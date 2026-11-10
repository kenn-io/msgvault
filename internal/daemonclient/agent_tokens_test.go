package daemonclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIssueAgentTokenRoundTripReadsIDForRevoke verifies that IssueAgentToken
// returns a result whose ID field is populated (requiring the server schema to
// include it) and that RevokeAgentToken accepts that ID.
func TestIssueAgentTokenRoundTripReadsIDForRevoke(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)

	const (
		wantID     = "tok_roundtrip01"
		wantSecret = "mva1_dGVzdHNlY3JldA"
	)

	var (
		revokedID        string
		issueContentType string
		issueBody        string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.32.0"}`))
	})
	mux.HandleFunc("/api/v1/agent-tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		issueContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		issueBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          wantID,
			"label":       "round-trip-test",
			"permissions": []string{"draft.create"},
			"sources": []map[string]any{
				{"id": 1, "type": "imap", "identifier": "alice@example.com"},
			},
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"secret":     wantSecret,
			"daemon_url": "",
		})
	})
	mux.HandleFunc("/api/v1/agent-tokens/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		revokedID = r.URL.Path[len("/api/v1/agent-tokens/"):]
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "owner-key", AllowInsecure: true})
	requirementsForTest.NoError(err)

	result, err := c.IssueAgentToken(t.Context(), "round-trip-test", []string{"draft.create"}, []int64{1}, map[int64][]string{1: {"alice@example.com"}}, nil, time.Time{})
	requirementsForTest.NoError(err)
	requirementsForTest.NotNil(result)
	assertionsForTest.Equal("application/json", issueContentType, "issue requests must identify their JSON body")
	assertionsForTest.Contains(issueBody, "sender_selections")
	assertionsForTest.Contains(issueBody, "alice@example.com")
	assertionsForTest.Equal(wantID, result.ID, "id must be decoded from the 201 body")
	assertionsForTest.Equal(wantSecret, result.Secret)

	err = c.RevokeAgentToken(t.Context(), result.ID)
	requirementsForTest.NoError(err)
	assertionsForTest.Equal(wantID, revokedID, "revoke must use the id from the issue response")
}

// TestAgentTokenBusyResponseRetriesAndNotifies proves the P2 fix: a 503
// operation_in_progress response from the daemon triggers the busy-retry seam
// (notify + delay + retry) rather than returning a hard error immediately.
func TestAgentTokenBusyResponseRetriesAndNotifies(t *testing.T) {
	t.Cleanup(func() { operationBusyRetryDelay = time.Second })
	operationBusyRetryDelay = time.Millisecond

	var (
		callCount   int
		notifyCount int
		notifyMsg   string
	)

	const busyBody = `{"error":"operation_in_progress","message":"msgvault sync alice@example.com has been running for 4m12s"}`

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-tokens/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		callCount++
		if callCount < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(busyBody))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "owner-key", AllowInsecure: true})
	require.New(t).NoError(err)
	c.SetBusyNotifier(func(msg string) {
		notifyCount++
		notifyMsg = msg
	})

	err = c.RevokeAgentToken(t.Context(), "tok_abc")
	require.New(t).NoError(err, "must succeed after retries")
	assert.Equal(t, 3, callCount, "must have retried twice before succeeding")
	assert.GreaterOrEqual(t, notifyCount, 1, "busy notifier must have fired at least once")
	assert.Contains(t, notifyMsg, "msgvault sync", "notifier message must name the holder")
}

func TestIssueAgentTokenSenderSelectionRefusesOldDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	postCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.31.0"}`))
	})
	mux.HandleFunc("/api/v1/agent-tokens", func(w http.ResponseWriter, r *http.Request) {
		postCount++
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := New(Config{URL: srv.URL, APIKey: "owner-key", AllowInsecure: true})
	requirements.NoError(err)
	_, err = client.IssueAgentToken(t.Context(), "old-daemon", []string{"draft.create"}, []int64{1}, map[int64][]string{1: {"alice@example.com"}}, nil, time.Time{})
	requirements.Error(err)
	assertions.Contains(err.Error(), "requires daemon API schema 2.32.0")
	assertions.Zero(postCount)
}

func TestIssueAgentTokenExpiryRefusesOldDaemon(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	posts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.8.0"}`))
	})
	mux.HandleFunc("/api/v1/agent-tokens", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusCreated)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirementsForTest.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	_, err = client.IssueAgentToken(t.Context(), "reader", []string{"stats.read"}, []int64{1}, nil, nil, time.Now().Add(time.Hour))
	requirementsForTest.Error(err)
	assertionsForTest.Contains(err.Error(), "requires daemon API schema 3.9.0")
	assertionsForTest.Zero(posts)
}

func TestIssueIncomingSenderRestrictionsRefusesOldDaemon(t *testing.T) {
	for _, version := range []string{"5.0.0", "5.1.0"} {
		t.Run(version, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": version}))
					return
				}
				posts++
				var body map[string]any
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					return
				}
				assert.Equal(t, map[string]any{"1": []any{"codes@example.test"}}, body["incoming_senders"])
				assert.NotContains(t, body, "sender_selections")
				w.WriteHeader(http.StatusCreated)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "secret": "fixture-secret"}))
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, APIKey: "fixture-owner", AllowInsecure: true})
			requirementsForTest.NoError(err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			_, err = client.IssueAgentToken(t.Context(), "archive", []string{"message.archive"}, []int64{1}, nil, map[int64][]string{1: {"codes@example.test"}}, time.Time{})
			if version == "5.0.0" {
				requirementsForTest.ErrorContains(err, "incoming sender restrictions require")
				assertionsForTest.Zero(posts)
			} else {
				requirementsForTest.NoError(err)
				assertionsForTest.Equal(1, posts)
			}
		})
	}
}
