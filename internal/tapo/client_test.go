package tapo

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNormalizePublicHost(t *testing.T) {
	tests := map[string]string{
		"https://n-aps1-wap-gw.tplinkcloud.com/": "https://aps1-wap-gw.tplinkcloud.com",
		"https://n-aps1-wap.i.tplinkcloud.com/":  "https://aps1-wap.tplinkcloud.com",
		"https://use1-wap.tplinkcloud.com":       "https://use1-wap.tplinkcloud.com",
	}
	for input, want := range tests {
		got, err := normalizePublicHost(input)
		if err != nil {
			t.Fatalf("normalizePublicHost(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizePublicHost(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := normalizePublicHost("https://evil.example"); err == nil {
		t.Fatal("expected non-TP-Link host rejection")
	}
}

func TestEmbeddedTPLinkCloudServerCA(t *testing.T) {
	block, rest := pem.Decode(tpLinkCloudServerCA)
	if block == nil || len(rest) != 0 {
		t.Fatal("embedded TP-Link CA is not one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Subject.CommonName != "TP-Link Cloud Server CA" {
		t.Fatalf("unexpected CA subject: %q", certificate.Subject.CommonName)
	}
	if !certificate.IsCA || certificate.NotAfter.Before(time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("invalid or unexpectedly short-lived CA: isCA=%v notAfter=%s", certificate.IsCA, certificate.NotAfter)
	}
}

func TestNormalizeThingHost(t *testing.T) {
	want := "https://aps1-app-server.iot.i.tplinkcloud.com"
	got, err := normalizeThingHost(want + "/")
	if err != nil || got != want {
		t.Fatalf("normalizeThingHost = %q, %v", got, err)
	}
	for _, invalid := range []string{
		"https://aps1-wap.i.tplinkcloud.com",
		"https://aps1-app-server.iot.i.tplinkcloud.com.evil.example",
		"http://aps1-app-server.iot.i.tplinkcloud.com",
	} {
		if _, err := normalizeThingHost(invalid); err == nil {
			t.Fatalf("expected rejection for %q", invalid)
		}
	}
}

func TestDecodeBase64Text(t *testing.T) {
	if got := decodeBase64Text("RGVzaw=="); got != "Desk" {
		t.Fatalf("decoded name = %q", got)
	}
	if got := decodeBase64Text("plain-name"); got != "plain-name" {
		t.Fatalf("plain name changed to %q", got)
	}
}

func TestAPIErrorCodeReadsNestedResult(t *testing.T) {
	response := map[string]any{
		"error_code": float64(0),
		"result":     map[string]any{"errorCode": "-20571", "errorMsg": "Device is offline"},
	}
	if got := apiCode(response); got != -20571 {
		t.Fatalf("apiCode = %d", got)
	}
}

func TestClientRejectsDeviceControlMethods(t *testing.T) {
	client := &Client{}
	for _, method := range []string{"set_device_info", "device_on", "device_off", "toggle"} {
		if _, err := client.Call(context.Background(), Thing{}, method, nil); err == nil {
			t.Fatalf("expected %q to be rejected", method)
		}
	}
}

func TestAuthenticateLogsMFATransitionsWithoutSecrets(t *testing.T) {
	responses := []string{
		`{"error_code":0,"result":{"appServerUrl":"https://aps1-wap-gw.tplinkcloud.com"}}`,
		`{"error_code":-20677,"result":{"MFAProcessId":"secret-process"}}`,
		`{"error_code":0}`,
		`{"error_code":0,"result":{"token":"secret-token","refreshToken":"secret-refresh"}}`,
	}
	requestIndex := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		if requestIndex >= len(responses) {
			t.Fatal("unexpected extra HTTP request")
		}
		response := responses[requestIndex]
		requestIndex++
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(response)),
			Header:     make(http.Header),
		}, nil
	})}
	var logs bytes.Buffer
	client := &Client{
		username: "private@example.invalid", password: "secret-password",
		terminalID:  "00000000-0000-4000-8000-000000000001",
		mfaProvider: FileMFACodeProvider{Immediate: "123456"},
		publicHTTP:  httpClient,
		now:         func() time.Time { return time.Unix(1_700_000_000, 0) },
		logger:      slog.New(slog.NewJSONHandler(&logs, nil)),
	}
	if err := client.Authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requestIndex != len(responses) {
		t.Fatalf("made %d requests, want %d", requestIndex, len(responses))
	}
	output := logs.String()
	for _, message := range []string{
		"TP-Link terminal verification required",
		"TP-Link verification email requested; waiting for code",
		"TP-Link verification code received; completing terminal verification",
		"TP-Link authentication completed",
	} {
		if !strings.Contains(output, message) {
			t.Errorf("missing log message %q in %s", message, output)
		}
	}
	for _, secret := range []string{"private@example.invalid", "secret-password", "123456", "secret-process", "secret-token", "secret-refresh"} {
		if strings.Contains(output, secret) {
			t.Errorf("secret %q appeared in logs", secret)
		}
	}
}

func TestDoJSONRedactsQueryParameters(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Header:     make(http.Header),
		}, nil
	})}
	request, err := http.NewRequest(http.MethodGet, "https://example.invalid/path?token=secret-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = doJSON(httpClient, request, &map[string]any{})
	if err == nil {
		t.Fatal("expected HTTP status error")
	}
	if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "?token") {
		t.Fatalf("query parameter leaked in error: %v", err)
	}
	if !strings.Contains(err.Error(), "https://example.invalid/path") {
		t.Fatalf("safe request URL missing from error: %v", err)
	}
}
