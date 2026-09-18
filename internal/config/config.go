package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TapoUsername       string
	TapoPassword       string
	TapoTerminalID     string
	TapoMFACode        string
	TapoMFACodeFile    string
	GrafanaEndpoint    string
	GrafanaInstanceID  string
	GrafanaToken       string
	DeviceIDs          map[string]struct{}
	CollectionInterval time.Duration
	HistoryInterval    time.Duration
	HistoryOnStart     bool
	RequestTimeout     time.Duration
	MFAWaitTimeout     time.Duration
	ListenAddress      string
	Timezone           string
}

func Load() (Config, error) {
	c := Config{
		TapoUsername:      strings.TrimSpace(os.Getenv("TAPO_USERNAME")),
		TapoPassword:      os.Getenv("TAPO_PASSWORD"),
		TapoTerminalID:    strings.TrimSpace(os.Getenv("TAPO_TERMINAL_ID")),
		TapoMFACode:       strings.TrimSpace(os.Getenv("TAPO_MFA_CODE")),
		TapoMFACodeFile:   strings.TrimSpace(os.Getenv("TAPO_MFA_CODE_FILE")),
		GrafanaEndpoint:   strings.TrimRight(strings.TrimSpace(os.Getenv("GRAFANA_OTLP_ENDPOINT")), "/"),
		GrafanaInstanceID: strings.TrimSpace(os.Getenv("GRAFANA_OTLP_INSTANCE_ID")),
		GrafanaToken:      os.Getenv("GRAFANA_CLOUD_TOKEN"),
		ListenAddress:     envOr("LISTEN_ADDRESS", ":8080"),
		Timezone:          envOr("TZ", "Asia/Tokyo"),
		DeviceIDs:         splitSet(os.Getenv("TAPO_DEVICE_IDS")),
	}

	var err error
	if c.CollectionInterval, err = durationEnv("COLLECTION_INTERVAL", 5*time.Minute, false); err != nil {
		return Config{}, err
	}
	if c.HistoryInterval, err = durationEnv("HISTORY_INTERVAL", 24*time.Hour, true); err != nil {
		return Config{}, err
	}
	if c.RequestTimeout, err = durationEnv("REQUEST_TIMEOUT", 30*time.Second, false); err != nil {
		return Config{}, err
	}
	if c.MFAWaitTimeout, err = durationEnv("TAPO_MFA_WAIT_TIMEOUT", 10*time.Minute, false); err != nil {
		return Config{}, err
	}
	if c.HistoryOnStart, err = boolEnv("HISTORY_ON_START", true); err != nil {
		return Config{}, err
	}

	missing := make([]string, 0, 6)
	for name, value := range map[string]string{
		"TAPO_USERNAME":            c.TapoUsername,
		"TAPO_PASSWORD":            c.TapoPassword,
		"TAPO_TERMINAL_ID":         c.TapoTerminalID,
		"GRAFANA_OTLP_ENDPOINT":    c.GrafanaEndpoint,
		"GRAFANA_OTLP_INSTANCE_ID": c.GrafanaInstanceID,
		"GRAFANA_CLOUD_TOKEN":      c.GrafanaToken,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if !strings.HasPrefix(c.GrafanaEndpoint, "https://") {
		return Config{}, errors.New("GRAFANA_OTLP_ENDPOINT must use HTTPS")
	}
	if c.CollectionInterval < time.Minute {
		return Config{}, errors.New("COLLECTION_INTERVAL must be at least 1m")
	}
	return c, nil
}

func (c Config) GrafanaMetricsURL() string {
	if strings.HasSuffix(c.GrafanaEndpoint, "/v1/metrics") {
		return c.GrafanaEndpoint
	}
	return c.GrafanaEndpoint + "/v1/metrics"
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration, allowZero bool) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if duration < 0 || (!allowZero && duration == 0) {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}

func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s: %w", name, err)
	}
	return parsed, nil
}

func splitSet(value string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}
