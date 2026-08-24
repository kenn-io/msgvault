package httpretry

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRetryAfterBoundsProviderDelay(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		attempt int
		maximum time.Duration
		want    time.Duration
	}{
		{name: "negative falls back", header: "-1", attempt: 2, maximum: ProviderMaxRetryAfter, want: 4 * time.Second},
		{name: "valid long value within provider cap", header: "600", maximum: ProviderMaxRetryAfter, want: 10 * time.Minute},
		{name: "valid value above provider cap", header: "3600", maximum: ProviderMaxRetryAfter, want: ProviderMaxRetryAfter},
		{name: "beeper cap", header: "120", maximum: DefaultMaxRetryAfter, want: DefaultMaxRetryAfter},
		{name: "duration overflow falls back", header: "9223372037", attempt: 5, maximum: ProviderMaxRetryAfter, want: 32 * time.Second},
		{name: "uint64 overflow falls back", header: "18446744073709551616", attempt: 4, maximum: ProviderMaxRetryAfter, want: 16 * time.Second},
		{name: "malformed falls back", header: "1s", attempt: 3, maximum: ProviderMaxRetryAfter, want: 8 * time.Second},
		{name: "zero is immediate", header: "0", attempt: 4, maximum: ProviderMaxRetryAfter, want: 0},
		{name: "positive value honors subsecond maximum", header: "1", maximum: 10 * time.Millisecond, want: 10 * time.Millisecond},
		{name: "zero maximum uses safe default", header: "120", want: DefaultMaxRetryAfter},
		{name: "negative maximum uses safe default", header: "120", maximum: -time.Second, want: DefaultMaxRetryAfter},
		{name: "fallback caps", attempt: 6, want: DefaultMaxRetryAfter},
		{name: "fallback honors maximum", attempt: 6, maximum: 10 * time.Millisecond, want: 10 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RetryAfter(tt.header, tt.attempt, tt.maximum))
		})
	}
}

func TestRetryAfterAtHonorsHTTPDate(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		header  string
		maximum time.Duration
		want    time.Duration
	}{
		{name: "future", header: now.Add(37 * time.Second).Format(http.TimeFormat), maximum: time.Minute, want: 37 * time.Second},
		{name: "past is immediate", header: now.Add(-time.Second).Format(http.TimeFormat), maximum: time.Minute, want: 0},
		{name: "future capped", header: now.Add(2 * time.Hour).Format(http.TimeFormat), maximum: time.Minute, want: time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, RetryAfterAt(tt.header, 3, tt.maximum, now))
		})
	}
}

func TestRetryAfterFallbackAttemptBounds(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: time.Second},
		{attempt: 0, want: time.Second},
		{attempt: 1, want: 2 * time.Second},
		{attempt: 5, want: 32 * time.Second},
		{attempt: 6, want: DefaultMaxRetryAfter},
		{attempt: 100, want: DefaultMaxRetryAfter},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.attempt), func(t *testing.T) {
			assert.Equal(t, tt.want, RetryAfter("", tt.attempt, ProviderMaxRetryAfter))
		})
	}
}
