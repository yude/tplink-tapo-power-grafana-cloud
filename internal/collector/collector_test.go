package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/metric"
	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/tapo"
)

type fakeTapo struct {
	failAll bool
}

func (f *fakeTapo) ListThings(context.Context) ([]tapo.Thing, error) {
	return []tapo.Thing{{
		ThingName: "device-1", Nickname: "RGVzaw==", Model: "P110M(JP)", Category: "plug.switch",
	}}, nil
}

func (f *fakeTapo) ReadUsage(_ context.Context, _ tapo.Thing) (map[string]any, error) {
	if !f.failAll {
		return map[string]any{"energy_usage": map[string]any{"current_power": float64(1250), "today_energy": float64(10)}}, nil
	}
	return nil, errors.New("usage unavailable")
}

func (f *fakeTapo) ReadShadow(_ context.Context, _ tapo.Thing) (map[string]any, error) {
	if !f.failAll {
		return map[string]any{"on": true}, nil
	}
	return nil, errors.New("shadow unavailable")
}

func TestCollectReturnsErrorWhenEveryEnergyReadFails(t *testing.T) {
	api := &fakeTapo{failAll: true}
	sink := &fakeSink{}
	collector := New(api, sink, nil, time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	result, err := collector.Collect(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "all 1 selected Tapo devices failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.DevicesSucceeded != 0 || result.DevicesFailed != 1 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if len(sink.points) != 2 {
		t.Fatalf("failure status metrics were not pushed: %#v", sink.points)
	}
}

func TestSafeErrorPreservesBoundedDiagnosticSuffix(t *testing.T) {
	value := strings.Repeat("x", 500) + " diagnostic reason"
	if got := safeError(value); !strings.HasSuffix(got, " diagnostic reason") {
		t.Fatalf("diagnostic suffix was truncated: %q", got)
	}
	if got := safeError(strings.Repeat("x", 1100)); len(got) != 1000 {
		t.Fatalf("safeError length = %d", len(got))
	}
}

type fakeSink struct{ points []metric.Point }

func (f *fakeSink) Push(_ context.Context, batch *metric.Batch) error {
	f.points = batch.Points()
	return nil
}

func TestCollectNormalizesPowerAndNeverMutatesDevices(t *testing.T) {
	api := &fakeTapo{}
	sink := &fakeSink{}
	collector := New(api, sink, nil, time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	collector.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	result, err := collector.Collect(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.DevicesSucceeded != 1 || result.DevicesFailed != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	var powerFound, onlineFound bool
	for _, point := range sink.points {
		switch point.Name {
		case "tapo_power_watts":
			powerFound = point.Value == 1.25 && point.Attributes["tapo.device.name"] == "Desk"
		case "tapo_device_online":
			onlineFound = point.Value == 1
		}
	}
	if !powerFound || !onlineFound {
		t.Fatalf("expected metrics not found: %#v", sink.points)
	}
}
