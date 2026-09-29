package beeper

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeeperDraftWire(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var methods []string
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.EscapedPath())
		var body []byte
		body, _ = ioReadAll(r)
		bodies = append(bodies, string(body))
		_, _ = w.Write([]byte(`{"id":"!room:beeper.local","accountID":"signal","draft":{"text":"rich text"}}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	text := "hello **world**"
	chat, err := c.UpdateDraft(context.Background(), "!room:beeper.local", &text)
	requirements.NoError(err)
	assertions.Equal("!room:beeper.local", chat.ID)
	assertions.Equal([]string{"PATCH /v1/chats/%21room:beeper.local"}, methods)
	assertions.JSONEq(`{"draft":{"text":"hello **world**"}}`, bodies[0])

	_, err = c.UpdateDraft(context.Background(), "!room:beeper.local", nil)
	requirements.NoError(err)
	assertions.Equal(`{"draft":null}`, bodies[1])
}

func TestBeeperDraftWireFailuresAreSingleAttempt(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		disconnect bool
		state      DraftWriteState
		code       string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, body: `slow down`, state: DraftWriteUncertain, code: DraftWriteCodeUnknown},
		{name: "server failure", status: http.StatusBadGateway, body: `temporary`, state: DraftWriteUncertain, code: DraftWriteCodeUnknown},
		{name: "malformed success", status: http.StatusOK, body: `not json`, state: DraftWriteUncertain, code: DraftWriteCodeUnknown},
		{name: "disconnect", disconnect: true, state: DraftWriteUncertain, code: DraftWriteCodeUnknown},
		{name: "rejected", status: http.StatusBadRequest, body: `invalid`, state: DraftWriteRejected, code: DraftWriteCodeRejected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if tc.disconnect {
					hijacker, ok := w.(http.Hijacker)
					if !assertions.True(ok) {
						http.Error(w, "hijack unavailable", http.StatusInternalServerError)
						return
					}
					connection, _, hijackErr := hijacker.Hijack()
					if !assertions.NoError(hijackErr) {
						return
					}
					_ = connection.Close()
					return
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
			text := "candidate"
			_, err := c.UpdateDraft(t.Context(), "!room:beeper.local", &text)
			requirements.Error(err)
			var writeErr *DraftWriteError
			requirements.ErrorAs(err, &writeErr)
			assertions.Equal(tc.state, writeErr.State)
			assertions.Equal(tc.code, writeErr.Code)
			assertions.Equal(1, attempts)
		})
	}
}

func TestBeeperDraftWireReadsFullSuccessfulChat(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	largeTitle := strings.Repeat("x", maxErrorBodyBytes+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(struct {
			ID        string `json:"id"`
			AccountID string `json:"accountID"`
			Title     string `json:"title"`
			Draft     struct {
				Text string `json:"text"`
			} `json:"draft"`
		}{ID: "!room:beeper.local", AccountID: "signal", Title: largeTitle, Draft: struct {
			Text string `json:"text"`
		}{Text: "rich text"}})
		assertions.NoError(err)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	text := "candidate"
	chat, err := client.UpdateDraft(t.Context(), "!room:beeper.local", &text)
	requirements.NoError(err)
	assertions.Equal(largeTitle, chat.Title)
	assertions.Equal("rich text", mustDraftText(t, chat))
}

func TestBeeperDraftPreflightFailureDoesNotDispatch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	text := "candidate"
	client := NewClient(srv.URL, func(context.Context) (string, error) {
		return "", errors.New("token unavailable")
	}, 1000)
	_, err := client.UpdateDraft(t.Context(), "!room:beeper.local", &text)
	requirements.Error(err)
	var writeErr *DraftWriteError
	requirements.ErrorAs(err, &writeErr)
	assertions.Equal(DraftWriteRejected, writeErr.State)
	assertions.Equal(DraftWriteCodeToken, writeErr.Code)
	assertions.Zero(attempts)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client = NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	_, err = client.UpdateDraft(ctx, "!room:beeper.local", &text)
	requirements.Error(err)
	requirements.ErrorAs(err, &writeErr)
	assertions.Equal(DraftWriteRejected, writeErr.State)
	assertions.Equal(DraftWriteCodeRateLimit, writeErr.Code)
	assertions.Zero(attempts)
}

func TestBeeperDraftObservation(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		present     bool
		empty       bool
		attachments bool
		unknown     bool
	}{
		{name: "missing", raw: `{}`, present: false},
		{name: "null", raw: `{"draft":null}`, present: true, empty: true},
		{name: "attachment", raw: `{"draft":{"attachments":{"a":{"id":"a"}}}}`, present: true, attachments: true, unknown: true},
		{name: "empty attachment field", raw: `{"draft":{"text":"x","attachments":{}}}`, present: true, attachments: true, unknown: true},
		{name: "unknown", raw: `{"draft":{"text":"x","future":true}}`, present: true, unknown: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var chat Chat
			if tc.raw == `{}` {
				chat = Chat{}
			} else {
				requirements.NoError(jsonUnmarshal([]byte(tc.raw), &chat))
			}
			observation, err := chat.InspectDraft()
			requirements.NoError(err)
			assertions.Equal(tc.present, observation.Present)
			assertions.Equal(tc.empty, observation.Empty())
			assertions.Equal(tc.attachments, observation.AttachmentsPresent)
			assertions.Equal(tc.unknown, observation.Unknown)
		})
	}
}

// Keep the test's JSON helpers local so the test covers the same v2 decoder
// used by the provider structs without exposing a second production API.
func jsonUnmarshal(data []byte, out any) error  { return json.Unmarshal(data, out) }
func ioReadAll(r *http.Request) ([]byte, error) { return io.ReadAll(r.Body) }

func mustDraftText(t *testing.T, chat *Chat) string {
	t.Helper()
	observation, err := chat.InspectDraft()
	require.NoError(t, err)
	return observation.Text
}
