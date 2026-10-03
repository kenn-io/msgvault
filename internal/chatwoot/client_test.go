package chatwoot

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestClientAccountContracts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		assert.Equal("synthetic-token", r.Header.Get("Api_access_token"))
		switch r.URL.Path {
		case "/support/api/v1/accounts/9/inboxes":
			_, _ = io.WriteString(w, `{"payload":[{"id":7,"name":"Example inbox","channel_type":"Channel::TwilioSms","auth_token":"secret-must-not-survive"}]}`)
		case "/support/api/v1/accounts/9/agents":
			_, _ = io.WriteString(w, `[{"id":201,"name":"Example Agent","email":"agent@example.com","role":"agent"}]`)
		case "/support/api/v1/accounts/9/conversations":
			assert.Equal("all", r.URL.Query().Get("status"))
			assert.Equal("all", r.URL.Query().Get("assignee_type"))
			assert.Equal("created_at_asc", r.URL.Query().Get("sort_by"))
			assert.Equal("2", r.URL.Query().Get("page"))
			assert.Equal("7", r.URL.Query().Get("inbox_id"))
			_, _ = io.WriteString(w, `{"data":{"meta":{"all_count":1},"payload":[{"id":42,"account_id":9,"inbox_id":7,"status":"resolved","updated_at":1801526448.123,"meta":{"sender":{"id":101,"name":"Example Contact","phone_number":"+12025550101","type":"contact"},"assignee":{"id":202,"name":"Another Agent"}},"messages":[{"private":true,"content":"excluded note"}],"last_non_activity_message":{"private":true,"content":"excluded note"}}]}}`)
		case "/support/api/v1/accounts/9/conversations/42/messages":
			assert.Equal("1003", r.URL.Query().Get("after"))
			assert.Equal("1004", r.URL.Query().Get("before"))
			_, _ = io.WriteString(w, `{"meta":{},"payload":[{"id":1003,"content":"Voice call","inbox_id":7,"conversation_id":42,"message_type":0,"content_type":"voice_call","created_at":1801526402,"private":false,"sender":{"id":101,"type":"contact","name":"Example Contact"},"attachments":[{"id":301,"file_type":"audio","data_url":"https://chatwoot.example.com/note.ogg","transcribed_text":"Note transcript","file_size":1024,"width":null}],"content_attributes":{"data":{"call_id":501,"call_direction":"inbound","accepted_by":{"id":201,"name":"Example Agent"}},"unknown":{"original":true}},"call":{"id":501,"provider":"twilio","direction":"incoming","status":"completed","duration_seconds":0,"accepted_by_agent_id":201,"accepted_by_agent_name":"Example Agent","started_at":null,"ended_at":null,"recording_url":null,"transcript":"Call transcript"},"future_field":"retained"}]}`)
		default:
			assert.Fail("unexpected route", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/support/", 9, "synthetic-token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	inboxes, err := c.ListInboxes(t.Context())
	require.NoError(err)
	require.Len(inboxes, 1)
	assert.Equal(int64(7), inboxes[0].ID)
	assert.Equal("Example inbox", inboxes[0].Name)
	safeInbox, err := json.Marshal(inboxes[0])
	require.NoError(err)
	assert.NotContains(string(safeInbox), "secret-must-not-survive")
	agents, err := c.ListAgents(t.Context())
	require.NoError(err)
	require.Len(agents, 1)
	assert.Equal("agent@example.com", agents[0].Email)
	assert.Equal("user", agents[0].Type)
	conversations, err := c.ListConversations(t.Context(), 2, 7)
	require.NoError(err)
	require.Len(conversations, 1)
	assert.Equal(int64(42), conversations[0].ID)
	assert.Equal("+12025550101", conversations[0].Meta.Sender.PhoneNumber)
	safeContext, err := json.Marshal(conversations[0])
	require.NoError(err)
	assert.NotContains(string(safeContext), "excluded note")
	msgs, err := c.ListMessages(t.Context(), 42, 1003, 1004)
	require.NoError(err)
	require.Len(msgs, 1)
	m := msgs[0]
	assert.Equal(int64(1003), m.ID)
	assert.Equal(int64(1801526402), m.CreatedAt)
	require.NotNil(m.Call)
	assert.Equal(int64(201), m.Call.AcceptedByAgentID)
	require.NotNil(m.Call.DurationSeconds)
	assert.Zero(*m.Call.DurationSeconds)
	assert.Equal("Call transcript", m.Call.Transcript)
	require.Len(m.Attachments, 1)
	assert.Equal("Note transcript", m.Attachments[0].TranscribedText)
	assert.Contains(string(m.Raw), `"future_field":"retained"`)
	assert.Contains(string(m.Raw), `"unknown":{"original":true}`)
}

func TestClientNullAndUnknownSender(t *testing.T) {
	assert := assert.New(t)

	var m Message
	require.NoError(t, json.Unmarshal([]byte(`{"id":1,"content":null,"sender":null,"call":null,"attachments":null,"content_attributes":{},"created_at":1,"message_type":2,"content_type":"unknown_type"}`), &m))
	assert.Empty(m.Content)
	assert.Nil(m.Sender)
	assert.Nil(m.Call)
	assert.Equal("unknown_type", m.ContentType)
	assert.NotEmpty(m.Raw)
}

func TestClientValidation(t *testing.T) {
	for _, raw := range []string{"", "ftp://example.com", "https://user:pass@example.com", "https://example.com?token=secret", "https://example.com#fragment", "https:///path"} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewClient(raw, 9, "token")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "pass")
			assert.NotContains(t, err.Error(), "secret")
		})
	}
	assert := assert.New(t)
	require := require.New(t)
	_, err := NewClient("https://chatwoot.example.com", 0, "token")
	require.Error(err)
	_, err = NewClient("https://chatwoot.example.com", 9, "")
	require.Error(err)
	canonical, err := CanonicalURL("HTTPS://CHATWOOT.EXAMPLE.COM:443/support/")
	require.NoError(err)
	assert.Equal("https://chatwoot.example.com/support", canonical)
	assert.Equal(SourceIdentifier(canonical, 9, 7), SourceIdentifier("https://chatwoot.example.com/support/", 9, 7))
	assert.NotEqual(SourceIdentifier(canonical, 9, 7), SourceIdentifier(canonical, 10, 7))
	assert.NotEqual(SourceIdentifier(canonical, 9, 7), SourceIdentifier(canonical, 9, 8))
}

func TestClientRequiresHTTPSOutsideLoopback(t *testing.T) {
	for _, raw := range []string{
		"http://chatwoot.example.com",
		"http://192.0.2.10/support",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewClient(raw, 9, "synthetic-token")
			require.Error(t, err, "remote API tokens must not be sent over HTTP")
		})
	}
	for _, raw := range []string{
		"http://localhost:3000/support",
		"http://127.0.0.1:3000/support",
		"http://[::1]:3000/support",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewClient(raw, 9, "synthetic-token")
			require.NoError(t, err, "local Chatwoot development instances may use loopback HTTP")
		})
	}
}

func TestClientMediaAndAPIRedirectIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"))
		assert.Empty(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "audio/ogg")
		_, _ = io.WriteString(w, "synthetic audio")
	}))
	defer media.Close()
	mediaHits := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/audio" {
			mediaHits++
			assert.Empty(r.Header.Get("Api_access_token"))
			assert.Empty(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "audio/ogg")
			_, _ = io.WriteString(w, "synthetic audio")
			return
		}
		http.Redirect(w, r, media.URL+"/audio?signature=secret", http.StatusFound)
	}))
	defer api.Close()
	c, err := NewClient(api.URL, 9, "synthetic-token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = c.ListInboxes(t.Context())
	require.Error(err, "authenticated API requests must not redirect")
	assert.NotContains(err.Error(), "signature")
	assert.Zero(mediaHits, "authenticated API redirects must not fetch the target")
	reader, size, mime, err := c.OpenMedia(t.Context(), api.URL+"/audio", 1<<20)
	require.NoError(err)
	defer func() { require.NoError(reader.Close()) }()
	body, err := io.ReadAll(reader)
	require.NoError(err)
	assert.Equal("synthetic audio", string(body))
	assert.Equal(int64(len(body)), size)
	assert.Equal("audio/ogg", mime)
	invalidReader, _, _, err := c.OpenMedia(t.Context(), "file:///etc/passwd", 1<<20)
	require.Error(err)
	assert.Nil(invalidReader)
}

func TestClientRejectsUntrustedPrivateMediaHost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hits := 0
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = io.WriteString(w, "synthetic internal response")
	}))
	defer media.Close()

	c, err := NewClient("https://chatwoot.example.com", 9, "synthetic-token")
	require.NoError(err)
	body, _, _, err := c.OpenMedia(t.Context(), media.URL+"/internal", 1<<20)
	if body != nil {
		require.NoError(body.Close())
	}
	require.Error(err, "media URLs from message data must not reach an untrusted private destination")
	assert.Zero(hits)
}

func TestClientRejectsPrivateMediaRedirect(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	privateHits := 0
	privateServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		privateHits++
		_, _ = io.WriteString(w, "synthetic media response")
	}))
	defer privateServer.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, privateServer.URL+"/internal", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "synthetic media response")
	}))
	defer server.Close()
	baseURL := server.URL
	c, err := NewClient(baseURL, 9, "synthetic-token")
	require.NoError(err)
	body, _, _, err := c.OpenMedia(t.Context(), baseURL+"/start", 1<<20)
	if body != nil {
		require.NoError(body.Close())
	}
	require.Error(err, "every redirect target must pass the destination policy")
	assert.Zero(privateHits)
}

func TestClientFailuresAndRateLimit(t *testing.T) {
	for _, code := range []int{401, 403, 404, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "upstream-sensitive-message")
			}))
			defer srv.Close()
			c, err := NewClient(srv.URL, 9, "token")
			require.NoError(t, err)
			c.limiter = rate.NewLimiter(rate.Inf, 1)
			_, err = c.ListInboxes(t.Context())
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "upstream-sensitive-message")
		})
	}
	require := require.New(t)
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"payload":[]}`)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, 9, "token")
	require.NoError(err)
	c.limiter = rate.NewLimiter(rate.Inf, 1)
	_, err = c.ListInboxes(t.Context())
	require.NoError(err)
	assert.Equal(t, 2, attempts)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.ListInboxes(ctx)
	require.ErrorIs(err, context.Canceled)
}

func TestClientMalformedAndOversizeJSON(t *testing.T) {
	for _, body := range []string{`not-json`, `{"unexpected":[]}`, strings.Repeat(" ", maxAPIBytes+1)} {
		t.Run(strconv.Itoa(len(body)), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer srv.Close()
			c, err := NewClient(srv.URL, 9, "token")
			require.NoError(t, err)
			c.limiter = rate.NewLimiter(rate.Inf, 1)
			_, err = c.ListInboxes(t.Context())
			require.Error(t, err)
		})
	}
	// A real HTTP request blocked on response headers observes context cancellation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	c, err := NewClient(srv.URL, 9, "token")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = c.ListInboxes(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
