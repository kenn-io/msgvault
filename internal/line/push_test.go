package line_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const replyRetryKey = "123e4567-e89b-42d3-a456-426614174000"
const replyUser = "U00000000000000000000000000000002"

func TestPushTextReceiptAndDedupe(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client, fake := newClient(t)
	request := line.PushTextRequest{To: replyUser, Text: "Thanks! 🌍", QuoteToken: "synthetic-parent-quote"}
	receipt, err := client.PushText(t.Context(), replyRetryKey, request)
	require.NoError(err)
	assert.True(receipt.Accepted)
	assert.Equal("900000000000000001", receipt.MessageID)
	assert.Equal(200, receipt.StatusCode)
	repeated, err := client.PushText(t.Context(), replyRetryKey, request)
	require.NoError(err)
	assert.True(repeated.Accepted)
	assert.Equal(409, repeated.StatusCode)
	assert.Equal(receipt.MessageID, repeated.MessageID)
	assert.Equal(receipt.QuoteToken, repeated.QuoteToken)
	assert.Equal(receipt.RequestID, repeated.RequestID)
	assert.Equal(1, fake.AcceptedPushes())
	for _, r := range fake.Requests() {
		if r.Path != "/v2/bot/message/push" {
			continue
		}
		assert.Equal(replyRetryKey, r.Header.Get("X-Line-Retry-Key"))
		assert.JSONEq(`{"to":"U00000000000000000000000000000002","messages":[{"type":"text","text":"Thanks! 🌍","quoteToken":"synthetic-parent-quote"}]}`, string(r.Body))
	}
}

func TestPushRefreshKeepsRetryKeyAndPayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client, fake := newClient(t)
	fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 401})
	receipt, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
	require.NoError(err)
	assert.True(receipt.Accepted)
	assert.Equal(2, fake.Count("POST", "/oauth2/v3/token"))
	assert.Equal(2, fake.Count("POST", "/v2/bot/message/push"))
	var bodies [][]byte
	for _, r := range fake.Requests() {
		if r.Path == "/v2/bot/message/push" {
			assert.Equal(replyRetryKey, r.Header.Get("X-Line-Retry-Key"))
			bodies = append(bodies, r.Body)
		}
	}
	require.Len(bodies, 2)
	assert.Equal(bodies[0], bodies[1])
	assert.JSONEq(`{"to":"U00000000000000000000000000000002","messages":[{"type":"text","text":"hello"}]}`, string(bodies[0]))
}

func TestPushMalformedAcceptanceAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response lineapi.Response
		accepted bool
	}{
		{"empty 200", lineapi.Response{Status: 200, Body: `{"sentMessages":[]}`}, true},
		{"numeric ID", lineapi.Response{Status: 200, Body: `{"sentMessages":[{"id":100}]}`}, true},
		{"blank ID", lineapi.Response{Status: 200, Body: `{"sentMessages":[{"id":" "}]}`}, true},
		{"multiple IDs", lineapi.Response{Status: 200, Body: `{"sentMessages":[{"id":"100"},{"id":"101"}]}`}, true},
		{"malformed 200", lineapi.Response{Status: 200, Body: `private provider text`}, true},
		{"accepted 409 without ID", lineapi.Response{Status: 409, Header: http.Header{"X-Line-Accepted-Request-Id": {"original-request"}}, Body: `{}`}, true},
		{"409 missing accepted header", lineapi.Response{Status: 409, Body: `{"sentMessages":[{"id":"100"}]}`}, false},
		{"bad request", lineapi.Response{Status: 400, Body: `private provider text`}, false},
		{"server error", lineapi.Response{Status: 503, Body: `private provider text`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			client, fake := newClient(t)
			fake.Enqueue("POST", "/v2/bot/message/push", tc.response)
			receipt, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
			require.Error(t, err)
			assert.NotContains(err.Error(), "private provider text")
			assert.Equal(tc.accepted, receipt.Accepted)
			assert.Equal(tc.response.Status, receipt.StatusCode)
			assert.Empty(receipt.MessageID)
		})
	}
}

func TestPushRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, status := range []int{429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client, fake := newClient(t, line.WithClock(func() time.Time { return now }))
			fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: status, Header: http.Header{"Retry-After": {"180"}}})
			receipt, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
			require.Error(t, err)
			assert.Equal(t, now.Add(3*time.Minute), receipt.RetryAfter)
		})
	}
}

func TestPushLostResponseRecoversSameMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	client, fake := newClient(t)
	fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Accept: true, Disconnect: true})
	_, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
	require.Error(err)
	assert.Equal(1, fake.AcceptedPushes())
	receipt, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
	require.NoError(err)
	assert.Equal(409, receipt.StatusCode)
	assert.Equal("900000000000000001", receipt.MessageID)
	assert.Equal(1, fake.AcceptedPushes())
}

func TestPushRejectsInvalidInputBeforeTokenIO(t *testing.T) {
	for _, tc := range []struct{ key, to, text string }{
		{"", replyUser, "hello"}, {"not-a-uuid", replyUser, "hello"},
		{replyRetryKey, "not-a-line-id", "hello"}, {replyRetryKey, replyUser, " "},
	} {
		client, fake := newClient(t)
		_, err := client.PushText(t.Context(), tc.key, line.PushTextRequest{To: tc.to, Text: tc.text})
		require.Error(t, err)
		assert.Empty(t, fake.Requests())
	}
}

func TestPushDoesNotFollowRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls++ }))
	defer target.Close()
	client, fake := newClient(t)
	fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: 302, Header: http.Header{"Location": {target.URL}}})
	_, err := client.PushText(t.Context(), replyRetryKey, line.PushTextRequest{To: replyUser, Text: "hello"})
	require.Error(t, err)
	assert.Zero(t, targetCalls)
}

func TestPushFakeExpiresKeysAfter24Hours(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	client, fake := newClient(t, line.WithClock(func() time.Time { return now }))
	fake.SetClock(func() time.Time { return now })
	request := line.PushTextRequest{To: replyUser, Text: "hello"}
	first, err := client.PushText(t.Context(), replyRetryKey, request)
	require.NoError(err)
	now = now.Add(24 * time.Hour)
	second, err := client.PushText(t.Context(), replyRetryKey, request)
	require.NoError(err)
	assert.NotEqual(first.MessageID, second.MessageID)
	assert.Equal(2, fake.AcceptedPushes())
}

func TestValidateReplyText(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		valid      bool
	}{
		{"empty", "", false}, {"blank", " \t\n\u3000", false}, {"NUL", "a\x00b", false}, {"invalid UTF8", string([]byte{0xff}), false},
		{"BMP boundary", strings.Repeat("a", 5000), true}, {"BMP over", strings.Repeat("a", 5001), false},
		{"astral boundary", strings.Repeat("🌍", 2500), true}, {"astral over", strings.Repeat("🌍", 2501), false},
		{"mixed boundary", strings.Repeat("a", 4998) + "🌍", true}, {"mixed over", strings.Repeat("a", 4999) + "🌍", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := line.ValidateReplyText(tc.text)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
