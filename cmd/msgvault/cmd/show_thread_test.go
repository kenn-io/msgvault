package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
)

func TestLoadThreadMessagesBeyondRateLimitBurst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		limiter := api.NewRateLimiter(10, 20)
		defer limiter.Close()
		handler := api.RateLimitMiddleware(limiter, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal("/api/v1/cli/message", r.URL.Path)
			id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
			assert.NoError(err)
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{
				"id": id, "conversation_id": 7, "body_text": "Synthetic reply",
			}))
		}))
		srv := httptest.NewTestServer(t, handler)
		httpClient := srv.Client()
		client, err := daemonclient.New(daemonclient.Config{
			URL: srv.URL, AllowInsecure: true, HTTPClient: httpClient,
		})
		require.NoError(err)
		page := &query.ThreadPage{}
		page.ConversationID = 7
		for id := int64(1); id <= 100; id++ {
			page.Messages = append(page.Messages, query.ThreadMessage{ID: id})
		}

		messages, err := loadThreadMessages(t.Context(), client, page, false)
		require.NoError(err)
		require.Len(messages, 100)
		for i, message := range messages {
			assert.Equal(int64(i+1), message.ID)
			assert.Equal("Synthetic reply", message.BodyText)
		}
	})
}

func TestShowThreadPaginationAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		offset                       int
		asText                       bool
		detailID, detailConversation int64
		wantError                    string
	}{
		{name: "exhausted page", offset: 2},
		{name: "partial JSON page", detailID: 1, detailConversation: 7},
		{name: "partial text page", detailID: 1, detailConversation: 7, asText: true},
		{name: "detail id mismatch", detailID: 99, detailConversation: 7, wantError: "message"},
		{name: "conversation mismatch", detailID: 1, detailConversation: 99, wantError: "conversation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "3.3.0"}))
				case "/api/v1/cli/message/thread":
					messages := []map[string]any{}
					if tc.offset == 0 {
						messages = append(messages, map[string]any{"id": 1})
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"conversation_id": 7, "total": 2, "offset": tc.offset, "has_more": tc.offset == 0, "messages": messages}))
				case "/api/v1/cli/message":
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": tc.detailID, "conversation_id": tc.detailConversation, "body_text": "Synthetic reply", "from": []map[string]any{{"email": "alice@example.com", "name": "Alice"}}}))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			cfg := &config.Config{}
			cfg.Remote.URL = srv.URL
			cfg.Remote.AllowInsecure = true
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.AddCommand(newShowThreadCmd())
			args := []string{"show-thread", "7", "--offset", strconv.Itoa(tc.offset)}
			if !tc.asText {
				args = append(args, "--json")
			}
			root.SetArgs(args)
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			done := captureStdout(t)
			err := root.Execute()
			out := done()
			if tc.wantError != "" {
				require.ErrorContains(err, tc.wantError)
				assert.NotContains(stderr.String(), "More messages:")
				return
			}
			require.NoError(err)
			if tc.offset == 0 {
				assert.Contains(stderr.String(), "More messages: use --offset 1")
			} else {
				assert.Empty(stderr.String())
			}
			if tc.asText {
				assert.Contains(out, "Synthetic reply")
				assert.Contains(out, "Alice <alice@example.com>")
				return
			}
			var page struct {
				ConversationID int64 `json:"conversation_id"`
				Total          int64 `json:"total"`
				Offset         int   `json:"offset"`
				HasMore        bool  `json:"has_more"`
				Messages       []any `json:"messages"`
			}
			require.NoError(json.Unmarshal([]byte(out), &page))
			assert.Equal(int64(7), page.ConversationID)
			assert.Equal(int64(2), page.Total)
			assert.Equal(tc.offset, page.Offset)
			assert.Equal(tc.offset == 0, page.HasMore)
			if tc.offset == 0 {
				assert.Len(page.Messages, 1)
			} else {
				assert.Empty(page.Messages)
			}
		})
	}
}

func TestShowThreadProviderResolution(t *testing.T) {
	for _, tc := range []struct {
		name, code, wantError string
		status                int
		wantCalls             int
	}{
		{name: "numeric provider fallback", code: "message_not_found", status: 404, wantCalls: 2},
		{name: "ambiguous reference", code: "message_ambiguous", status: 409, wantError: "use an internal message ID", wantCalls: 1},
		{name: "server failure", code: "search_failed", status: 500, wantError: "get thread", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "3.3.0"}))
					return
				}
				calls++
				if calls == 1 {
					assert.Equal(t, "123", r.URL.Query().Get("id"))
					w.WriteHeader(tc.status)
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"error": tc.code, "message": "Synthetic reference response"}))
					return
				}
				assert.Equal(t, "123", r.URL.Query().Get("source_message_id"))
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"conversation_id": 7, "total": 0, "messages": []any{}}))
			}))
			t.Cleanup(srv.Close)
			cfg := &config.Config{}
			cfg.Remote.URL = srv.URL
			cfg.Remote.AllowInsecure = true
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.AddCommand(newShowThreadCmd())
			root.SetArgs([]string{"show-thread", "123", "--json"})
			done := captureStdout(t)
			err := root.Execute()
			_ = done()
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantCalls, calls)
		})
	}
}

// A request barrier proves overlap and the bound without asserting runner speed.
func TestLoadThreadMessagesBoundedAndOrdered(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	lastEntered := make(chan struct{})
	var active, maximum atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for prev := maximum.Load(); n > prev; prev = maximum.Load() {
			if maximum.CompareAndSwap(prev, n) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		assert.NoError(err)
		if id == 8 {
			close(lastEntered)
		}
		if id == 1 {
			select {
			case <-lastEntered:
			case <-r.Context().Done():
				return
			}
		}
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": id, "conversation_id": 7, "body_text": "Synthetic reply\n> old reply"}))
	}))
	defer srv.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true})
	require.NoError(err)
	defer func() { assert.NoError(client.Close()) }()
	page := &query.ThreadPage{}
	page.ConversationID = 7
	for id := int64(1); id <= 8; id++ {
		page.Messages = append(page.Messages, query.ThreadMessage{ID: id})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var messages []*query.MessageDetail
	var loadErr error
	done := make(chan struct{})
	go func() { messages, loadErr = loadThreadMessages(ctx, client, page, true); close(done) }()
	overlapped := true
	for range 4 {
		select {
		case <-entered:
		case <-ctx.Done():
			overlapped = false
		}
		if !overlapped {
			break
		}
	}
	close(release)
	<-done
	require.True(overlapped, "four detail requests must reach the barrier together")
	require.NoError(loadErr)
	require.Len(messages, 8)
	assert.Equal(int32(4), maximum.Load())
	for i, msg := range messages {
		assert.Equal(int64(i+1), msg.ID)
		assert.Equal("Synthetic reply", msg.BodyText)
	}
}
