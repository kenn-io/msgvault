package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The real Gmail transport enforces at least a 30s quota pause. This one
// integration deliberately waits that protocol floor; no polling throughput or
// provider response latency is part of its correctness oracle.
func TestRapidEmailGmailPartialQuotaDebtReopensWithUnchangedHistory(t *testing.T) {
	var requests, histories, fetches, clients atomic.Int32
	var healthy atomic.Bool
	raw := base64.RawURLEncoding.EncodeToString([]byte("From: sender@example.test\r\nTo: sample@example.test\r\nSubject: Recovered fixture\r\nMessage-ID: <lost@example.test>\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nRecovered body\r\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if !assert.Equal(t, http.MethodGet, r.Method) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_, _ = w.Write([]byte(`{"emailAddress":"sample@example.test","historyId":"1001"}`))
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"}]}`))
		case "/gmail/v1/users/me/history":
			histories.Add(1)
			assert.Equal(t, "1000", r.URL.Query().Get("startHistoryId"))
			_, _ = w.Write([]byte(`{"historyId":"1001","history":[{"id":"1001","messagesAdded":[{"message":{"id":"lost","threadId":"thread-lost","labelIds":["INBOX"]}}]}]}`))
		case "/gmail/v1/users/me/messages/lost":
			fetches.Add(1)
			assert.Equal(t, "raw", r.URL.Query().Get("format"))
			if !healthy.Load() {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"lost","threadId":"thread-lost","labelIds":["INBOX"],"historyId":"1001","internalDate":"1704110400000","raw":%q}`, raw)
		default:
			assert.Fail(t, "unexpected synthetic Gmail operation", "%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	loopback, err := url.Parse(server.URL)
	require.NoError(t, err)
	originalTransport := http.DefaultTransport
	transport := &http.Transport{DisableKeepAlives: true}
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Scheme, clone.URL.Host = loopback.Scheme, loopback.Host
		return transport.RoundTrip(clone)
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport; transport.CloseIdleConnections() })

	dir := t.TempDir()
	path := filepath.Join(dir, "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	src, err := st.GetOrCreateSource("gmail", "sample@example.test")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncCursor(src.ID, "1000"))
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	state := invocationFromContext(ctx)
	results := make(chan error, 3)
	completed := make(chan struct{}, 1)
	s := scheduler.New(func(ctx context.Context, identifier string) error {
		selected, err := rapidEmailSource(st, identifier)
		if err == nil {
			clients.Add(1)
			client := gmail.NewClient(nil, gmail.WithConcurrency(1))
			client.DeferProviderRetries()
			err = runRapidGmailSource(ctx, selected, st, client, state)
			_ = client.Close()
		}
		results <- err
		return err
	}).WithLogger(state.logger).WithCompletion(func() { completed <- struct{}{} })
	defer func() { <-s.Stop().Done() }()
	require.NoError(t, s.AddAccount(src.Identifier, "0 0 1 1 *"))
	require.NoError(t, s.SetAccountPollInterval(src.Identifier, 15*time.Second))
	require.NoError(t, s.TriggerSync(src.Identifier, true))
	first := <-results
	var partial *scheduler.AcquisitionError
	require.ErrorAs(t, first, &partial)
	assert.Equal(t, scheduler.AcquisitionIncomplete, partial.Outcome)
	require.Eventually(t, func() bool { return !s.Status()[0].Running }, 5*time.Second, time.Millisecond)
	status := s.Status()[0]
	assert.True(t, status.LastCompleted.IsZero(), "a completed History run with fetch debt is not complete acquisition")
	assert.False(t, status.LastAttempted.IsZero())
	assert.False(t, status.NextEligiblePoll.Before(partial.RetryNotBefore()))
	debt, err := st.ListUnresolvedSyncItemErrors(ctx, src.ID)
	require.NoError(t, err)
	require.Len(t, debt, 1)
	assert.Equal(t, "lost", debt[0].SourceMessageID)
	current, err := st.GetSourceByID(src.ID)
	require.NoError(t, err)
	assert.Equal(t, "1001", current.SyncCursor.String)

	before := requests.Load()
	require.NoError(t, s.TriggerSync(src.Identifier, true))
	require.Eventually(t, func() bool { return !s.Status()[0].Running }, 5*time.Second, time.Millisecond)
	assert.EqualValues(t, 1, clients.Load(), "the retained floor stops a new acquisition client before profile reads")
	assert.Equal(t, before, requests.Load())
	assert.Empty(t, completed)
	require.NoError(t, st.Close())
	st, err = store.OpenForTest(path)
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	debt, err = st.ListUnresolvedSyncItemErrors(ctx, src.ID)
	require.NoError(t, err)
	require.Len(t, debt, 1, "fetch debt survives a fresh Store")
	healthy.Store(true)

	// Ordinary demand may bypass adaptive jitter after the provider floor;
	// startup retains that already accepted manual request and its completion.
	time.Sleep(max(time.Duration(0), time.Until(partial.RetryNotBefore())))
	s.Start()
	select {
	case <-completed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "reopened unchanged-History acquisition did not complete")
	}
	require.NoError(t, <-results)
	assert.EqualValues(t, 2, clients.Load())
	assert.EqualValues(t, 1, histories.Load(), "unchanged History must settle old debt without a new event")
	assert.EqualValues(t, 2, fetches.Load())
	debt, err = st.ListUnresolvedSyncItemErrors(ctx, src.ID)
	require.NoError(t, err)
	assert.Empty(t, debt)
	ids, err := st.MessageExistsBatch(src.ID, []string{"lost"})
	require.NoError(t, err)
	require.Contains(t, ids, "lost")
	assert.False(t, s.Status()[0].LastCompleted.IsZero())
	<-s.Stop().Done() // join before the reopened Store is closed
}
