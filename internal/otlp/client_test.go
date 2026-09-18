package otlp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/metric"
)

func TestPushUsesBasicAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("123:secret"))
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected content type: %q", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	var batch metric.Batch
	batch.Add(metric.Point{Name: "power", Value: 1, Timestamp: time.Now()})
	client := New(server.URL, "123", "secret", "collector", "test", "instance", time.Second)
	if err := client.Push(context.Background(), &batch); err != nil {
		t.Fatal(err)
	}
}
