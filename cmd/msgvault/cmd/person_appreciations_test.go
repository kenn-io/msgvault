package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func runAppreciations(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault"}
	root.AddCommand(newPersonAppreciationsCommand())
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"appreciations"}, args...))
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

const appreciationPayload = `{"appreciations":[{"suggestion":{"text":"Helped with café\u001b[31m","anchor_id":"a","quote":"exact peer quote","key":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","state":"suggested","prior_thanks":"unknown","thanked":null},"peer":{"source_lane":"chat","source_id":2,"message_id":42,"source_message_id":"native-42\u001b[31m","attachment_id":0,"occurrence_key":"o","chunk_key":"c","span_start":1,"span_end":8}},{"suggestion":{"text":"Shared notes","anchor_id":"b","quote":"peer two","key":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","state":"already_thanked","prior_thanks":"evidenced","thanked":{"evidence_id":"e","quote":"exact owner thanks"}},"peer":{"source_lane":"chat","source_id":2,"message_id":43,"source_message_id":"native-43"},"owner":{"source_lane":"chat","source_id":2,"message_id":44,"source_message_id":"native-44"}}],"future":"preserved"}`

func TestAppreciationsListWireAndOutput(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, appreciationPayload)
	out, err := runAppreciations(daemon.ctx, t, "list", "7")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, daemon.method)
	assert.Equal(t, "/api/v1/people/7/appreciations", daemon.path)
	assert.Empty(t, daemon.body)
	for _, value := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "café", "exact peer quote", "lane=chat", "message_id=42", "source_message_id=native-42", "suggested", "prior thanks unknown", "already_thanked", "prior thanks evidenced", "exact owner thanks", "message_id=44"} {
		assert.Contains(t, out, value)
	}
	assert.NotContains(t, out, "\x1b")
	out, err = runAppreciations(daemon.ctx, t, "list", "7", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, appreciationPayload, out)
	daemon.response = `{"appreciations":[]}`
	out, err = runAppreciations(daemon.ctx, t, "list", "7")
	require.NoError(t, err)
	assert.Contains(t, out, "No supported, undismissed appreciation suggestions.")
	daemon.status = http.StatusNotFound
	daemon.response = `{"error":"person_brief_not_found","message":"missing current brief"}`
	_, err = runAppreciations(daemon.ctx, t, "list", "7")
	require.Error(t, err)
	assert.ErrorContains(t, err, "missing current brief")
}

func TestAppreciationsDismissForwardsAuditAndErrors(t *testing.T) {
	key := strings.Repeat("a", 64)
	daemon := newPersonBriefTestDaemon(t, `{"contribution_key":"`+key+`","dismissed_by":"owner","dismissed_at":"2026-01-02T03:04:05Z","dismissed_person_uid":"uid","thanks_brief_id":9,"future":true}`)
	first, err := runAppreciations(daemon.ctx, t, "dismiss", "7", key)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, daemon.method)
	assert.Equal(t, "/api/v1/people/7/appreciations/"+key+"/dismiss", daemon.path)
	assert.Empty(t, daemon.body)
	for _, value := range []string{key, "owner", "2026-01-02T03:04:05Z", "uid", "9"} {
		assert.Contains(t, first, value)
	}
	second, err := runAppreciations(daemon.ctx, t, "dismiss", "7", key)
	require.NoError(t, err)
	// The fixed response checks forwarding, not server-side idempotence.
	assert.Equal(t, first, second)
	out, err := runAppreciations(daemon.ctx, t, "dismiss", "7", key, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"future":true`)
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		daemon.status = status
		daemon.response = `{"error":"failure","message":"dismissal failed"}`
		_, err = runAppreciations(daemon.ctx, t, "dismiss", "7", "invalid")
		require.Error(t, err)
		assert.ErrorContains(t, err, "dismissal failed")
	}
}

func TestAppreciationsRejectInvalidInputAndResponses(t *testing.T) {
	daemon := newPersonBriefTestDaemon(t, `null`)
	for _, args := range [][]string{{"list", "0"}, {"list", "999999999999999999999999"}, {"list"}, {"dismiss", "7", ""}, {"dismiss", "7"}} {
		_, err := runAppreciations(daemon.ctx, t, args...)
		require.Error(t, err)
	}
	assert.Zero(t, daemon.requests.Load())
	for _, body := range []string{"", "{", "null"} {
		daemon.response = body
		_, err := runAppreciations(daemon.ctx, t, "list", "7")
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(daemon.ctx)
	cancel()
	_, err := runAppreciations(ctx, t, "list", "7")
	require.Error(t, err)
}

// Exercise the generated client and the server's route segmentation and key
// validator together. The synthetic handler only records a valid write.
func TestAppreciationDismissEscapesPathDelimiters(t *testing.T) {
	key := strings.Repeat("a", 64)
	seen := make(chan string, 4)
	var writes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/people/{id}/appreciations/{key}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		received := r.PathValue("key")
		seen <- received
		// Empty audit fields stop the real Store method before it needs a database.
		_, err := (&store.Store{}).RecordPersonAppreciationDismissalContext(r.Context(), received, "", "", time.Time{})
		w.Header().Set("Content-Type", "application/json")
		if errors.Is(err, store.ErrInvalidPersonAppreciationKey) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"invalid_key","message":"invalid key"}`)
			return
		}
		if !errors.Is(err, store.ErrInvalidPersonAppreciationAudit) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writes.Add(1)
		_, _ = fmt.Fprintf(w, `{"contribution_key":%q}`, received)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	_, err := runAppreciations(ctx, t, "dismiss", "7", key)
	require.NoError(t, err)
	assert.Equal(t, key, <-seen)
	assert.EqualValues(t, 1, writes.Load())
	for _, suffix := range []string{"/dismiss#", "/dismiss?", "%2F"} {
		malformed := key + suffix
		_, err = runAppreciations(ctx, t, "dismiss", "7", malformed)
		require.Error(t, err)
		select {
		case received := <-seen:
			assert.Equal(t, malformed, received)
		default:
			assert.Fail(t, "request did not reach the dismissal route")
		}
		assert.EqualValues(t, 1, writes.Load())
	}
}
