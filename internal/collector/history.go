package collector

import (
	"context"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/metric"
	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/tapo"
)

type historyWindow struct {
	Resolution      string
	IntervalMinutes int
	Start           time.Time
	End             time.Time
}

func (c *Collector) collectHistory(ctx context.Context, batch *metric.Batch, thing tapo.Thing, now time.Time) {
	for _, window := range historyWindows(now) {
		if err := c.collectHistoryWindow(ctx, batch, thing, window); err != nil {
			c.logger.Debug("Tapo energy history unavailable", "device", safe(thing.Name()), "resolution", window.Resolution, "error", safe(err.Error()))
		}
	}
}

func (c *Collector) collectHistoryWindow(ctx context.Context, batch *metric.Batch, thing tapo.Thing, window historyWindow) error {
	requestedEnd := window.End.Unix()
	nextStart := window.Start.Unix()
	seen := make(map[int64]struct{})
	for page := 0; page < 12 && nextStart < requestedEnd; page++ {
		payload, err := c.tapo.Call(ctx, thing, "get_energy_data", map[string]any{
			"start_timestamp": nextStart,
			"end_timestamp":   requestedEnd,
			"interval":        window.IntervalMinutes,
		})
		if err != nil {
			return err
		}
		data, ok := payload["data"].([]any)
		if !ok || len(data) == 0 {
			return nil
		}
		pageStart := int64Number(payload["start_timestamp"], nextStart)
		for index, raw := range data {
			value, ok := floatNumber(raw)
			if !ok {
				continue
			}
			timestamp := historyTimestamp(pageStart, window, index)
			if _, duplicate := seen[timestamp.UnixNano()]; duplicate {
				continue
			}
			seen[timestamp.UnixNano()] = struct{}{}
			attributes := deviceAttributes(thing, "thing_history")
			attributes["tapo.energy.resolution"] = window.Resolution
			batch.Add(metric.Point{
				Name: "tapo_energy_bucket_watt_hours", Unit: "Wh",
				Description: "Energy consumed in a historical time bucket",
				Value:       value, Timestamp: timestamp, Attributes: attributes,
			})
		}
		returnedEnd := int64Number(payload["end_timestamp"], requestedEnd)
		if returnedEnd <= nextStart || returnedEnd >= requestedEnd {
			return nil
		}
		nextStart = returnedEnd
	}
	return nil
}

func historyWindows(now time.Time) []historyWindow {
	year, month, day := now.Date()
	location := now.Location()
	startOfDay := time.Date(year, month, day, 0, 0, 0, 0, location)
	quarterMonth := time.Month(((int(month)-1)/3)*3 + 1)
	return []historyWindow{
		{Resolution: "hourly", IntervalMinutes: 60, Start: startOfDay, End: startOfDay.AddDate(0, 0, 1)},
		{Resolution: "daily", IntervalMinutes: 1440, Start: time.Date(year, quarterMonth, 1, 0, 0, 0, 0, location), End: time.Date(year, quarterMonth, 1, 0, 0, 0, 0, location).AddDate(0, 3, 0)},
		{Resolution: "monthly", IntervalMinutes: 43200, Start: time.Date(year, 1, 1, 0, 0, 0, 0, location), End: time.Date(year+1, 1, 1, 0, 0, 0, 0, location)},
	}
}

func historyTimestamp(start int64, window historyWindow, index int) time.Time {
	base := time.Unix(start, 0).In(window.Start.Location())
	if window.Resolution == "monthly" {
		return time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, base.Location()).AddDate(0, index, 0)
	}
	return time.Unix(start+int64(index*window.IntervalMinutes*60), 0).In(window.Start.Location())
}

func floatNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	default:
		return 0, false
	}
}

func int64Number(value any, fallback int64) int64 {
	if number, ok := floatNumber(value); ok {
		return int64(number)
	}
	return fallback
}
