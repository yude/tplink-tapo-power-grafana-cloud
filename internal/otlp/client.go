package otlp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/yude/tplink-tapo-power-grafana-cloud/internal/metric"
)

type Client struct {
	endpoint   string
	instanceID string
	token      string
	service    string
	version    string
	instance   string
	http       *http.Client
}

func New(endpoint, instanceID, token, service, version, instance string, timeout time.Duration) *Client {
	return &Client{
		endpoint: endpoint, instanceID: instanceID, token: token,
		service: service, version: version, instance: instance,
		http: &http.Client{Timeout: timeout},
	}
}

func (c *Client) Push(ctx context.Context, batch *metric.Batch) error {
	if batch.Len() == 0 {
		return nil
	}
	payload, err := json.Marshal(batch.OTLP(c.service, c.version, c.instance))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.instanceID+":"+c.token)))
	response, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("push OTLP metrics: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Grafana Cloud OTLP endpoint returned HTTP %d", response.StatusCode)
	}
	return nil
}
