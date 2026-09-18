package tapo

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSign(t *testing.T) {
	payload := []byte(`{"method":"getDeviceList"}`)
	when := time.Unix(1_700_000_000, 0)
	got, err := sign(payload, "/api/v2/common/getDeviceListByPage", when)
	if err != nil {
		t.Fatal(err)
	}
	digest := md5.Sum(payload)
	wantMD5 := base64.StdEncoding.EncodeToString(digest[:])
	if got.ContentMD5 != wantMD5 {
		t.Fatalf("Content-MD5 = %q, want %q", got.ContentMD5, wantMD5)
	}
	parts := strings.Split(got.Authorization, ", ")
	if len(parts) != 4 {
		t.Fatalf("unexpected authorization: %q", got.Authorization)
	}
	nonce := strings.TrimPrefix(parts[1], "Nonce=")
	input := wantMD5 + "\n1700000000\n" + nonce + "\n/api/v2/common/getDeviceListByPage"
	mac := hmac.New(sha1.New, []byte(secretKey))
	_, _ = mac.Write([]byte(input))
	wantSignature := hex.EncodeToString(mac.Sum(nil))
	if !strings.HasSuffix(got.Authorization, "Signature="+wantSignature) {
		t.Fatalf("signature mismatch: %q", got.Authorization)
	}
}

func TestRandomUUIDIsVersion4(t *testing.T) {
	got, err := randomUUID()
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(got) {
		t.Fatalf("randomUUID() = %q", got)
	}
}
