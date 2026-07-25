package mime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsPlausibleDate(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		date time.Time
		want bool
	}{
		{name: "zero", date: time.Time{}, want: false},
		{name: "before lower bound", date: time.Date(1989, 12, 31, 23, 59, 59, 0, time.UTC), want: false},
		{name: "lower bound", date: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC), want: true},
		{name: "current", date: now, want: true},
		{name: "future slack boundary", date: now.Add(30 * 24 * time.Hour), want: true},
		{name: "beyond future slack", date: now.Add(30*24*time.Hour + time.Second), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsPlausibleDate(tt.date, now))
		})
	}
}

func TestParseReceivedChainPreservesSourceOrder(t *testing.T) {
	headers := []string{
		"from relay.example.net by mx.example.net; Wed, 03 Jan 2007 15:04:05 +0000",
		"from sender.example.net by relay.example.net; Tue, 02 Jan 2007 15:04:05 +0000",
		"missing semicolon",
		"from broken.example.net; not-a-date",
	}

	got := ParseReceivedChain(headers)

	assert.Equal(t, []time.Time{
		time.Date(2007, 1, 3, 15, 4, 5, 0, time.UTC),
		time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC),
	}, got)
}

func TestResolveMessageDatePrecedence(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	header := time.Date(2020, 1, 1, 1, 0, 0, 0, time.UTC)
	oldestReceived := time.Date(2018, 1, 1, 1, 0, 0, 0, time.UTC)
	newestReceived := time.Date(2018, 1, 2, 1, 0, 0, 0, time.UTC)
	fallback := time.Date(2021, 1, 1, 1, 0, 0, 0, time.UTC)

	got, source := ResolveMessageDate(
		header,
		[]time.Time{newestReceived, oldestReceived},
		fallback,
		now,
	)
	assert.Equal(header, got)
	assert.Equal(DateSourceHeader, source)

	got, source = ResolveMessageDate(
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		[]time.Time{newestReceived, oldestReceived},
		fallback,
		now,
	)
	assert.Equal(oldestReceived, got)
	assert.Equal(DateSourceReceived, source)

	got, source = ResolveMessageDate(
		time.Date(2039, 1, 1, 0, 0, 0, 0, time.UTC),
		nil,
		fallback,
		now,
	)
	assert.Equal(fallback, got)
	assert.Equal(DateSourceFallback, source)

	got, source = ResolveMessageDate(time.Time{}, nil, time.Time{}, now)
	assert.True(got.IsZero())
	assert.Equal(DateSourceNone, source)
}

func TestParseCollectsReceivedDates(t *testing.T) {
	raw := []byte("From: sender@example.com\r\n" +
		"To: recipient@example.com\r\n" +
		"Date: Thu, 01 Jan 1970 00:00:00 +0000\r\n" +
		"Received: from relay.example.net by mx.example.net; Wed, 03 Jan 2007 15:04:05 +0000\r\n" +
		"Received: from sender.example.net by relay.example.net; Tue, 02 Jan 2007 15:04:05 +0000\r\n" +
		"Subject: test\r\n\r\nbody\r\n")

	msg, err := Parse(raw)

	require.NoError(t, err)
	assert.Equal(t, time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), msg.Date)
	assert.Equal(t, []time.Time{
		time.Date(2007, 1, 3, 15, 4, 5, 0, time.UTC),
		time.Date(2007, 1, 2, 15, 4, 5, 0, time.UTC),
	}, msg.ReceivedDates)
}
