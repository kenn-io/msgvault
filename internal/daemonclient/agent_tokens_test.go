package daemonclient

import (
	"context"
	"encoding/json"
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
	require := require.New(t)
	assert := assert.New(t)

	const (
		wantID     = "tok_roundtrip01"
		wantSecret = "mva1_dGVzdHNlY3JldA"
	)

	var (
		revokedID        string
		issueContentType string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent-tokens", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		issueContentType = r.Header.Get("Content-Type")
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
	require.NoError(err)

	result, err := c.IssueAgentToken(context.Background(), "round-trip-test", []string{"draft.create"}, []int64{1})
	require.NoError(err)
	require.NotNil(result)
	assert.Equal("application/json", issueContentType, "issue requests must identify their JSON body")
	assert.Equal(wantID, result.ID, "id must be decoded from the 201 body")
	assert.Equal(wantSecret, result.Secret)

	err = c.RevokeAgentToken(context.Background(), result.ID)
	require.NoError(err)
	assert.Equal(wantID, revokedID, "revoke must use the id from the issue response")
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

	err = c.RevokeAgentToken(context.Background(), "tok_abc")
	require.New(t).NoError(err, "must succeed after retries")
	assert.Equal(t, 3, callCount, "must have retried twice before succeeding")
	assert.GreaterOrEqual(t, notifyCount, 1, "busy notifier must have fired at least once")
	assert.Contains(t, notifyMsg, "msgvault sync", "notifier message must name the holder")
}
