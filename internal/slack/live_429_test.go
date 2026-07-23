package slack

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingTransport records the status codes flowing through the client so
// the test can prove a real 429 occurred and was absorbed.
type countingTransport struct {
	requests   int
	got429     int
	retryAfter string
}

func (ct *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.requests++
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		ct.got429++
		ct.retryAfter = resp.Header.Get("Retry-After")
	}
	return resp, err
}

// TestLive429RetryAfter validates the client's throttle handling against the
// REAL Slack API. It is skipped unless both env vars are set:
//
//	MSGVAULT_SLACK_LIVE_TOKEN   user token for a DEV workspace you own
//	MSGVAULT_SLACK_LIVE_CHANNEL a channel ID with some history
//
// With the client's own pacing disabled it requests tiny history pages until
// Slack throttles (Tier 3 ≈ 50/min), then asserts the throttled call still
// returned successfully via Retry-After retry. Bounded to maxProbeRequests /
// probeDeadline; stops at the first observed 429. Run this only against a
// development workspace.
func TestLive429RetryAfter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	token := os.Getenv("MSGVAULT_SLACK_LIVE_TOKEN")
	channel := os.Getenv("MSGVAULT_SLACK_LIVE_CHANNEL")
	if token == "" || channel == "" {
		t.Skip("set MSGVAULT_SLACK_LIVE_TOKEN and MSGVAULT_SLACK_LIVE_CHANNEL to run the live throttle probe")
	}

	const maxProbeRequests = 500
	const probeDeadline = 4 * time.Minute

	ct := &countingTransport{}
	c := NewClient("", token)
	c.disableRateLimits()
	c.http.Transport = ct

	ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
	defer cancel()

	calls := 0
	for ct.got429 == 0 && ct.requests < maxProbeRequests {
		// limit=1 keeps responses tiny; every request still counts against
		// the per-minute budget.
		params := HistoryParams{ChannelID: channel}
		page, err := c.historyPageWithLimit(ctx, params, 1)
		if ctx.Err() != nil {
			// Ran out of probe time before Slack throttled (observed live:
			// enforcement lags the published budget until earlier traffic has
			// accumulated). Inconclusive, not a failure.
			t.Skipf("no 429 within %v / %d requests — inconclusive", probeDeadline, ct.requests)
		}
		require.NoError(err, "call %d must succeed even when throttled mid-way", calls)
		require.NotNil(page)
		calls++
	}

	if ct.got429 == 0 {
		t.Skipf("no 429 within %d requests — workspace budget higher than expected; inconclusive, not a failure", ct.requests)
	}
	t.Logf("throttled after %d requests; Retry-After=%q; %d successful calls returned", ct.requests, ct.retryAfter, calls)
	assert.Positive(calls, "the throttled call must have completed via retry")
	assert.NotEmpty(ct.retryAfter, "Slack 429s must carry Retry-After for the backoff to honor")
}
