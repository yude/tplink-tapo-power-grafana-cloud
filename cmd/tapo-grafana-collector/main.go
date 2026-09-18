package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/collector"
	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/config"
	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/otlp"
	"github.com/yude/tplink-tapo-power-grafana-cloud-gas/internal/tapo"
)

var version = "dev"

type healthState struct {
	mu      sync.RWMutex
	ready   bool
	lastRun time.Time
	lastErr string
}

func (s *healthState) update(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRun = time.Now()
	s.ready = err == nil
	if err == nil {
		s.lastErr = ""
	} else {
		s.lastErr = err.Error()
	}
}

func (s *healthState) handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	if r.URL.Path != "/readyz" {
		http.NotFound(w, r)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.ready {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("collector stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return fmt.Errorf("load timezone %q: %w", cfg.Timezone, err)
	}
	provider := tapo.FileMFACodeProvider{
		Immediate: cfg.TapoMFACode, Path: cfg.TapoMFACodeFile,
		PollInterval: 5 * time.Second, Timeout: cfg.MFAWaitTimeout,
	}
	tapoClient, err := tapo.NewClient(
		cfg.TapoUsername, cfg.TapoPassword, cfg.TapoTerminalID,
		provider, cfg.RequestTimeout,
	)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	sink := otlp.New(
		cfg.GrafanaMetricsURL(), cfg.GrafanaInstanceID, cfg.GrafanaToken,
		"tapo-grafana-collector", version, hostname, cfg.RequestTimeout,
	)
	energyCollector := collector.New(tapoClient, sink, cfg.DeviceIDs, location, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	state := &healthState{}
	server := &http.Server{Addr: cfg.ListenAddress, Handler: http.HandlerFunc(state.handler), ReadHeaderTimeout: 5 * time.Second}
	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("health server listening", "address", cfg.ListenAddress)
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serverErrors <- serveErr
		}
	}()

	runCollection := func(includeHistory bool) {
		result, collectErr := energyCollector.Collect(ctx, includeHistory)
		state.update(collectErr)
		if collectErr != nil {
			logger.Error("collection failed", "error", collectErr)
			tapoClient.ResetSession()
			return
		}
		logger.Info("collection completed",
			"devices_selected", result.DevicesSelected,
			"devices_succeeded", result.DevicesSucceeded,
			"devices_failed", result.DevicesFailed,
			"points_sent", result.PointsSent,
			"history", includeHistory,
		)
	}
	runCollection(cfg.HistoryOnStart)

	collectionTicker := time.NewTicker(cfg.CollectionInterval)
	defer collectionTicker.Stop()
	var historyTicker *time.Ticker
	var historyChannel <-chan time.Time
	if cfg.HistoryInterval > 0 {
		historyTicker = time.NewTicker(cfg.HistoryInterval)
		defer historyTicker.Stop()
		historyChannel = historyTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return server.Shutdown(shutdownCtx)
		case serveErr := <-serverErrors:
			return serveErr
		case <-collectionTicker.C:
			runCollection(false)
		case <-historyChannel:
			runCollection(true)
		}
	}
}
