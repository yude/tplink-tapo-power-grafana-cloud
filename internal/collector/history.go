package collector

import (
	"strings"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/metric"
	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/tapo"
)

var usageHistoryFields = map[string]struct{}{
	"past24h": {}, "past7d": {}, "past30d": {}, "past1y": {},
}

func historyField(path []string) bool {
	for _, component := range path {
		if _, ok := usageHistoryFields[strings.ToLower(component)]; ok {
			return true
		}
	}
	return false
}

func (c *Collector) collectUsageHistory(batch *metric.Batch, thing tapo.Thing, usage map[string]any, fallback time.Time) {
	energy := usage
	if nested, ok := usage["energy_usage"].(map[string]any); ok {
		energy = nested
	}
	anchor := usageLocalTime(energy, fallback.In(c.location))
	addPowerHistory(batch, thing, "past24h", flattenNumbers(energy["past24h"]), anchor)
	addPowerHistory(batch, thing, "past7d", flattenNumbers(energy["past7d"]), anchor)
	addEnergyHistory(batch, thing, "past30d", flattenNumbers(energy["past30d"]), anchor)
	addEnergyHistory(batch, thing, "past1y", flattenNumbers(energy["past1y"]), anchor)
}

func addPowerHistory(batch *metric.Batch, thing tapo.Thing, window string, values []float64, anchor time.Time) {
	if len(values) == 0 {
		return
	}
	end := time.Date(anchor.Year(), anchor.Month(), anchor.Day()+1, 0, 0, 0, 0, anchor.Location())
	for index, value := range values {
		attributes := historyAttributes(thing, window, "hourly")
		batch.Add(metric.Point{
			Name: "tapo_power_bucket_watts", Unit: "W", Description: "Average power in a historical hourly bucket",
			Value: value, Timestamp: end.Add(-time.Duration(len(values)-index) * time.Hour), Attributes: attributes,
		})
	}
}

func addEnergyHistory(batch *metric.Batch, thing tapo.Thing, window string, values []float64, anchor time.Time) {
	if len(values) == 0 {
		return
	}
	for index, value := range values {
		var timestamp time.Time
		resolution := "daily"
		if window == "past1y" {
			resolution = "monthly"
			timestamp = time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, anchor.Location()).AddDate(0, -(len(values) - index - 1), 0)
		} else {
			timestamp = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, anchor.Location()).AddDate(0, 0, -(len(values) - index - 1))
		}
		batch.Add(metric.Point{
			Name: "tapo_energy_bucket_watt_hours", Unit: "Wh", Description: "Energy consumed in a historical time bucket",
			Value: value, Timestamp: timestamp, Attributes: historyAttributes(thing, window, resolution),
		})
	}
}

func historyAttributes(thing tapo.Thing, window, resolution string) map[string]string {
	attributes := deviceAttributes(thing, "thing_usage_history")
	attributes["tapo.energy.window"] = window
	attributes["tapo.energy.resolution"] = resolution
	return attributes
}

func usageLocalTime(energy map[string]any, fallback time.Time) time.Time {
	text, _ := energy["local_time"].(string)
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, text, fallback.Location()); err == nil {
			return parsed
		}
	}
	return fallback
}

func flattenNumbers(value any) []float64 {
	values := make([]float64, 0)
	walkNumbers(value, nil, func(_ []string, number float64) { values = append(values, number) })
	return values
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
