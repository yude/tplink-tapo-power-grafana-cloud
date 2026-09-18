package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("TAPO_USERNAME", "collector@example.invalid")
	t.Setenv("TAPO_PASSWORD", "password")
	t.Setenv("TAPO_TERMINAL_ID", "00000000-0000-4000-8000-000000000001")
	t.Setenv("GRAFANA_OTLP_ENDPOINT", "https://otlp.example.invalid/otlp/")
	t.Setenv("GRAFANA_OTLP_INSTANCE_ID", "123")
	t.Setenv("GRAFANA_CLOUD_TOKEN", "token")
	t.Setenv("COLLECTION_INTERVAL", "10m")
	t.Setenv("HISTORY_INTERVAL", "0")
	t.Setenv("TAPO_DEVICE_IDS", "one, two,one")

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.CollectionInterval != 10*time.Minute || got.HistoryInterval != 0 {
		t.Fatalf("unexpected intervals: %#v", got)
	}
	if got.GrafanaMetricsURL() != "https://otlp.example.invalid/otlp/v1/metrics" {
		t.Fatalf("unexpected metrics URL: %s", got.GrafanaMetricsURL())
	}
	if len(got.DeviceIDs) != 2 {
		t.Fatalf("unexpected device IDs: %#v", got.DeviceIDs)
	}
}

func TestLoadRejectsHTTPGrafanaEndpoint(t *testing.T) {
	t.Setenv("TAPO_USERNAME", "collector@example.invalid")
	t.Setenv("TAPO_PASSWORD", "password")
	t.Setenv("TAPO_TERMINAL_ID", "terminal")
	t.Setenv("GRAFANA_OTLP_ENDPOINT", "http://otlp.example.invalid")
	t.Setenv("GRAFANA_OTLP_INSTANCE_ID", "123")
	t.Setenv("GRAFANA_CLOUD_TOKEN", "token")
	if _, err := Load(); err == nil {
		t.Fatal("expected HTTP endpoint rejection")
	}
}
