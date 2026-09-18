package tapo

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

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
