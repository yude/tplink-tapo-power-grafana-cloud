package tapo

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

func decodeBase64Text(value string) string {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil && utf8.Valid(decoded) {
		text := strings.TrimSpace(string(decoded))
		if text != "" {
			return text
		}
	}
	return value
}
