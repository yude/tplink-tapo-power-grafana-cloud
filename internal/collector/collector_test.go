package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/metric"
	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/tapo"
)

type fakeTapo struct {
	methods []string
}

func (f *fakeTapo) ListThings(context.Context) ([]tapo.Thing, error) {
	return []tapo.Thing{{
		ThingName: "device-1", Nickname: "RGVzaw==", Model: "P110M(JP)", Category: "plug.switch",
	}}, nil
}

func (f *fakeTapo) Call(_ context.Context, _ tapo.Thing, method string, _ any) (map[string]any, error) {
	f.methods = append(f.methods, method)
	if method == "get_energy_usage" {
		return map[string]any{"current_power": float64(1250), "today_energy": float64(10)}, nil
	}
	return nil, errors.New("unsupported")
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
	for _, method := range api.methods {
		if method == "set_device_info" || method == "set_relay_state" || method == "turn_on" || method == "turn_off" {
			t.Fatalf("mutation method called: %s", method)
		}
	}
}
