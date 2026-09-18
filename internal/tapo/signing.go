package tapo

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

const (
	accessKey = "4d11b6b9d5ea4d19a829adbb9714b057"
	secretKey = "6ed7d97f3e73467f8a5bab90b577ba4c"
)

type signature struct {
	ContentMD5    string
	Authorization string
}

func sign(payload []byte, path string, now time.Time) (signature, error) {
	digest := md5.Sum(payload)
	contentMD5 := base64.StdEncoding.EncodeToString(digest[:])
	nonce, err := randomUUID()
	if err != nil {
		return signature{}, fmt.Errorf("generate signing nonce: %w", err)
	}
	timestamp := fmt.Sprintf("%d", now.Unix())
	input := contentMD5 + "\n" + timestamp + "\n" + nonce + "\n" + path
	mac := hmac.New(sha1.New, []byte(secretKey))
	_, _ = mac.Write([]byte(input))
	sum := hex.EncodeToString(mac.Sum(nil))
	return signature{
		ContentMD5: contentMD5,
		Authorization: "Timestamp=" + timestamp +
			", Nonce=" + nonce +
			", AccessKey=" + accessKey +
			", Signature=" + sum,
	}, nil
}

func randomUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32], nil
}
