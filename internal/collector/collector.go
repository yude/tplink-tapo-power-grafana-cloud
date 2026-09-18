package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/metric"
	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/tapo"
)

type TapoClient interface {
	ListThings(context.Context) ([]tapo.Thing, error)
	ReadUsage(context.Context, tapo.Thing, bool) (map[string]any, error)
	ReadShadow(context.Context, tapo.Thing) (map[string]any, error)
}

type Sink interface {
	Push(context.Context, *metric.Batch) error
}

type Collector struct {
	tapo      TapoClient
	sink      Sink
	deviceIDs map[string]struct{}
	location  *time.Location
	logger    *slog.Logger
	now       func() time.Time
}

type Result struct {
	DevicesSelected  int
	DevicesSucceeded int
	DevicesFailed    int
	PointsSent       int
}

func New(client TapoClient, sink Sink, deviceIDs map[string]struct{}, location *time.Location, logger *slog.Logger) *Collector {
	return &Collector{
		tapo: client, sink: sink, deviceIDs: deviceIDs,
		location: location, logger: logger, now: time.Now,
	}
}

func (c *Collector) Collect(ctx context.Context, includeHistory bool) (Result, error) {
	things, err := c.tapo.ListThings(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list TP-Link things: %w", err)
	}
	selected := c.selectDevices(things)
	if len(selected) == 0 {
		return Result{}, fmt.Errorf("no Tapo energy devices selected from %d things", len(things))
	}
	now := c.now().In(c.location)
	var batch metric.Batch
	result := Result{DevicesSelected: len(selected)}
	for _, thing := range selected {
		pointsBefore := batch.Len()
		readErrors := make([]string, 0, 2)
		shadow, shadowErr := c.tapo.ReadShadow(ctx, thing)
		if shadowErr != nil {
			readErrors = append(readErrors, "shadow: "+safeError(shadowErr.Error()))
		} else {
			c.extract(&batch, thing, "thing_shadow", shadow, now, false)
		}
		usage, usageErr := c.tapo.ReadUsage(ctx, thing, includeHistory)
		if usageErr != nil {
			readErrors = append(readErrors, "usage: "+safeError(usageErr.Error()))
		} else {
			c.extract(&batch, thing, "thing_usage", usage, now, true)
			if includeHistory {
				c.collectUsageHistory(&batch, thing, usage, now)
			}
		}
		if batch.Len() > pointsBefore {
			result.DevicesSucceeded++
			c.statusMetrics(&batch, thing, now, true)
		} else {
			result.DevicesFailed++
			c.statusMetrics(&batch, thing, now, false)
			c.logger.Error("Tapo energy collection failed", "device", safe(thing.Name()), "read_errors", readErrors)
		}
	}
	if batch.Len() == 0 {
		return result, errors.New("collection produced no metrics")
	}
	if err := c.sink.Push(ctx, &batch); err != nil {
		return result, err
	}
	result.PointsSent = batch.Len()
	if result.DevicesSucceeded == 0 {
		return result, fmt.Errorf("all %d selected Tapo devices failed energy reads", result.DevicesSelected)
	}
	return result, nil
}

func (c *Collector) selectDevices(things []tapo.Thing) []tapo.Thing {
	selected := make([]tapo.Thing, 0)
	for _, thing := range things {
		if len(c.deviceIDs) > 0 {
			if _, ok := c.deviceIDs[thing.ID()]; !ok {
				continue
			}
		}
		kind := strings.ToUpper(thing.Kind())
		model := strings.ToUpper(thing.ModelName())
		if strings.Contains(kind, "PLUG") || strings.Contains(kind, "SWITCH") || strings.Contains(kind, "STRIP") ||
			strings.HasPrefix(model, "P") || strings.HasPrefix(model, "KP") || strings.HasPrefix(model, "EP") || strings.HasPrefix(model, "HS") {
			selected = append(selected, thing)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].ID() < selected[j].ID() })
	return selected
}

func (c *Collector) statusMetrics(batch *metric.Batch, thing tapo.Thing, timestamp time.Time, success bool) {
	value := 0.0
	if success {
		value = 1
	}
	batch.Add(metric.Point{
		Name: "tapo_device_online", Unit: "1", Description: "Whether the energy collector could reach the device",
		Value: value, Timestamp: timestamp, Attributes: deviceAttributes(thing, ""),
	})
	batch.Add(metric.Point{
		Name: "tapo_energy_collection_success", Unit: "1", Description: "Whether at least one usable energy read completed",
		Value: value, Timestamp: timestamp, Attributes: deviceAttributes(thing, ""),
	})
}

func (c *Collector) extract(batch *metric.Batch, thing tapo.Thing, source string, payload map[string]any, timestamp time.Time, includeRaw bool) {
	walkNumbers(payload, nil, func(path []string, value float64) {
		if historyField(path) {
			return
		}
		leaf := strings.ToLower(path[len(path)-1])
		switch leaf {
		case "err_code", "error_code", "start_timestamp", "end_timestamp", "local_time", "interval", "year", "month", "day":
			return
		}
		point, ok := normalize(path, value)
		point.Timestamp = timestamp
		point.Attributes = deviceAttributes(thing, source)
		if !ok {
			if !includeRaw {
				return
			}
			point = metric.Point{
				Name: "tapo_energy_raw_value", Unit: "1",
				Description: "Unnormalized numeric value returned by a Tapo energy API",
				Value:       value, Timestamp: timestamp, Attributes: deviceAttributes(thing, source),
			}
			point.Attributes["tapo.energy.field"] = strings.Join(path, ".")
		}
		batch.Add(point)
	})
}

func normalize(path []string, value float64) (metric.Point, bool) {
	leaf := strings.ToLower(path[len(path)-1])
	joined := strings.ToLower(strings.Join(path, "."))
	switch {
	case leaf == "voltage_mv":
		return normalized("tapo_voltage_volts", "V", "RMS voltage", value/1000), true
	case leaf == "voltage":
		return normalized("tapo_voltage_volts", "V", "RMS voltage", value), true
	case leaf == "current_ma":
		return normalized("tapo_current_amperes", "A", "RMS current", value/1000), true
	case leaf == "current":
		return normalized("tapo_current_amperes", "A", "RMS current", value), true
	case leaf == "power_mw":
		return normalized("tapo_power_watts", "W", "Instantaneous active power", value/1000), true
	case leaf == "current_power" && strings.Contains(joined, "energy_usage"):
		return normalized("tapo_power_watts", "W", "Instantaneous active power", value/1000), true
	case leaf == "current_power" || leaf == "power":
		return normalized("tapo_power_watts", "W", "Instantaneous active power", value), true
	case leaf == "today_energy":
		return normalized("tapo_energy_today_watt_hours", "Wh", "Energy used today", value), true
	case leaf == "month_energy":
		return normalized("tapo_energy_month_watt_hours", "Wh", "Energy used this month", value), true
	case leaf == "total_wh":
		return normalized("tapo_energy_total_watt_hours", "Wh", "Total energy reported by the device", value), true
	case leaf == "total":
		return normalized("tapo_energy_total_watt_hours", "Wh", "Total energy reported by a legacy meter", value*1000), true
	case leaf == "today_runtime":
		return normalized("tapo_runtime_today_seconds", "s", "Powered runtime today", value*60), true
	case leaf == "month_runtime":
		return normalized("tapo_runtime_month_seconds", "s", "Powered runtime this month", value*60), true
	case leaf == "energy_wh":
		return normalized("tapo_energy_watt_hours", "Wh", "Energy value returned by the device", value), true
	default:
		return metric.Point{}, false
	}
}

func normalized(name, unit, description string, value float64) metric.Point {
	return metric.Point{Name: name, Unit: unit, Description: description, Value: value}
}

func walkNumbers(value any, path []string, visitor func([]string, float64)) {
	switch typed := value.(type) {
	case float64:
		if !math.IsNaN(typed) && !math.IsInf(typed, 0) {
			visitor(path, typed)
		}
	case float32:
		visitor(path, float64(typed))
	case int:
		visitor(path, float64(typed))
	case int64:
		visitor(path, float64(typed))
	case jsonNumber:
		if parsed, err := strconv.ParseFloat(string(typed), 64); err == nil {
			visitor(path, parsed)
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			walkNumbers(typed[key], append(path, key), visitor)
		}
	case []any:
		for index, item := range typed {
			walkNumbers(item, append(path, strconv.Itoa(index)), visitor)
		}
	}
}

type jsonNumber string

func deviceAttributes(thing tapo.Thing, source string) map[string]string {
	attributes := map[string]string{
		"tapo.device.id":    safe(thing.ID()),
		"tapo.device.name":  safe(thing.Name()),
		"tapo.device.model": safe(thing.ModelName()),
	}
	if source != "" {
		attributes["tapo.data.source"] = source
	}
	return attributes
}

func safe(value string) string {
	return safeLimit(value, 200)
}

func safeError(value string) string {
	return safeLimit(value, 1000)
}

func safeLimit(value string, limit int) string {
	value = strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
