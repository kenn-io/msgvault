// Package tiktokwebhook verifies and durably queues TikTok Business Messaging deliveries.
package tiktokwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

func VerifySignature(secret []byte, header string, body []byte, now time.Time) bool {
	var timestamp, signature string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return false
		}
		switch key {
		case "t":
			if timestamp != "" {
				return false
			}
			timestamp = value
		case "s":
			if signature != "" {
				return false
			}
			signature = value
		default:
			return false
		}
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || len(secret) == 0 {
		return false
	}
	age := now.Sub(time.Unix(seconds, 0))
	if age < 0 || age > 5*time.Second {
		return false
	}
	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}
