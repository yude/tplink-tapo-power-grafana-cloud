package metric

import (
	"testing"
	"time"
)

func TestOTLPUsesExactNanosecondsAndSortedAttributes(t *testing.T) {
	var batch Batch
	timestamp := time.Unix(1_700_000_000, 123_000_000)
	batch.Add(Point{
		Name: "power", Unit: "W", Value: 1.25, Timestamp: timestamp,
		Attributes: map[string]string{"z": "last", "a": "first"},
	})
	payload := batch.OTLP("collector", "test", "instance")
	point := payload.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Gauge.DataPoints[0]
	if point.TimeUnixNano != "1700000000123000000" {
		t.Fatalf("timeUnixNano = %s", point.TimeUnixNano)
	}
	if point.Attributes[0].Key != "a" || point.Attributes[1].Key != "z" {
		t.Fatalf("attributes are not sorted: %#v", point.Attributes)
	}
}
