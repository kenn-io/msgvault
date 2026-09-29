package daemonclient_test

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type originalDaemon struct {
	engine         *daemonclient.Engine
	st             *store.Store
	withRaw        int64
	noRaw          int64
	raw            []byte
	healthRequests *atomic.Int64
	failHealth     *atomic.Bool
}

func newOriginalDaemon(t *testing.T) originalDaemon {
	t.Helper()
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	router := api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler),
	}).Router()
	healthRequests := new(atomic.Int64)
	failHealth := new(atomic.Bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			healthRequests.Add(1)
			if failHealth.Swap(false) {
				http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				return
			}
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })

	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(src.ID, "thread-daemon", "Daemon")
	require.NoError(t, err)
	raw := []byte("Subject: daemon\r\n\r\n\x00\xff bytes\n")
	persist := func(sourceMessageID string, sentAt time.Time, rawMIME []byte) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				SourceID: src.ID, ConversationID: convID, SourceMessageID: sourceMessageID,
				MessageType: "email", SentAt: sql.NullTime{Time: sentAt, Valid: true},
			},
			RawMIME: rawMIME,
		})
		require.NoError(t, err)
		return id
	}
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return originalDaemon{
		engine:         daemonclient.NewEngineAdapter(client),
		st:             st,
		withRaw:        persist("daemon-raw", base, raw),
		noRaw:          persist("daemon-no-raw", base.Add(time.Hour), nil),
		raw:            raw,
		healthRequests: healthRequests,
		failHealth:     failHealth,
	}
}

func TestEngineReadsOriginalMessagesThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	d := newOriginalDaemon(t)
	ctx := t.Context()

	got, err := d.engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "daemon-raw"}, 0)
	must.NoError(err)
	checks.Equal(d.raw, got.MIME)
	checks.Equal(d.withRaw, got.MessageID)
	checks.Equal("owner@example.com", got.Account)
	checks.Equal("thread-daemon", got.SourceConversationID)

	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{ID: d.noRaw}, 0)
	must.ErrorIs(err, query.ErrOriginalMIMEUnavailable)
	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{ID: 999999}, 0)
	must.ErrorIs(err, store.ErrMessageNotFound)
	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{}, 0)
	must.ErrorIs(err, query.ErrInvalidMessageRef)

	raw, err := d.engine.GetMessageRaw(ctx, d.withRaw)
	must.NoError(err)
	checks.Equal(d.raw, raw)

	raw, err = d.engine.GetMessageRaw(ctx, d.noRaw)
	must.NoError(err, "missing raw follows the engine's nil, nil contract")
	checks.Nil(raw)
}

func TestEngineOriginalMessageByteLimitThroughDaemon(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	d := newOriginalDaemon(t)
	ref := query.MessageRef{ID: d.withRaw}
	_, err := d.engine.ReadOriginalMessage(t.Context(), ref, int64(len(d.raw)-1))
	must.ErrorIs(err, query.ErrOriginalMessageTooLarge)
	got, err := d.engine.ReadOriginalMessage(t.Context(), ref, int64(len(d.raw)))
	must.NoError(err)
	checks.Equal(d.raw, got.MIME)
	got, err = d.engine.ReadOriginalMessage(t.Context(), ref, 0)
	must.NoError(err)
	checks.Equal(d.raw, got.MIME, "unrestricted CLI reads remain available")
}

func TestEngineListsThreadThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	d := newOriginalDaemon(t)
	ctx := t.Context()

	page, err := d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "thread-daemon", Limit: 1, Offset: 1})
	must.NoError(err)
	must.Len(page.Messages, 1)
	checks.Equal(d.noRaw, page.Messages[0].ID)
	checks.False(page.Messages[0].HasRaw)
	checks.Equal(int64(2), page.Total)
	checks.Equal(1, page.Offset)
	checks.False(page.HasMore)

	all, err := d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "thread-daemon", All: true, Limit: 1, Offset: 1})
	must.NoError(err)
	must.Len(all.Messages, 2)
	checks.Equal(d.withRaw, all.Messages[0].ID)
	checks.Equal(d.noRaw, all.Messages[1].ID)
	checks.Equal(int64(2), all.Total)
	checks.False(all.HasMore)

	_, err = d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "missing"})
	must.ErrorIs(err, query.ErrThreadNotFound)

	other, err := d.st.GetOrCreateSource("gmail", "second@example.com")
	must.NoError(err)
	otherConv, err := d.st.EnsureConversation(other.ID, "thread-daemon", "Other")
	must.NoError(err)
	_, err = d.st.UpsertMessage(&store.Message{
		SourceID: other.ID, ConversationID: otherConv, SourceMessageID: "other-message", MessageType: "email",
	})
	must.NoError(err)
	_, err = d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "thread-daemon"})
	must.ErrorIs(err, query.ErrAmbiguousReference)
	checks.Contains(err.Error(), "second@example.com")
	checks.Equal(1, strings.Count(err.Error(), "matches several accounts"))
	checks.NotContains(err.Error(), "pass account", "presentation layers supply their own hint")
}

func TestEngineOriginalCapabilityRetriesFailureAndCachesSuccess(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	d := newOriginalDaemon(t)
	d.failHealth.Store(true)
	_, err := d.engine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: d.withRaw}, 0)
	must.Error(err)
	_, err = d.engine.ListThread(t.Context(), query.ThreadQuery{ID: d.withRaw})
	must.NoError(err)
	for range 3 {
		original, err := d.engine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: d.withRaw}, 0)
		must.NoError(err)
		checks.Equal(d.raw, original.MIME)
	}
	checks.Equal(int64(2), d.healthRequests.Load(), "failed capability checks retry; successful ones are reused")
}

func TestEngineRawIDDoesNotFallBackAfterDedupPurge(t *testing.T) {
	must := require.New(t)
	d := newOriginalDaemon(t)
	_, err := d.st.MergeDuplicates(d.noRaw, []int64{d.withRaw}, "strict-id-test")
	must.NoError(err)
	_, err = d.st.DeleteDedupedBatch("strict-id-test")
	must.NoError(err)
	src, err := d.st.GetOrCreateSource("gmail", "other@example.com")
	must.NoError(err)
	conv, err := d.st.EnsureConversation(src.ID, "unrelated-thread", "Unrelated")
	must.NoError(err)
	_, err = d.st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: strconv.FormatInt(d.withRaw, 10), MessageType: "email"},
		RawMIME: []byte("Subject: unrelated\r\n\r\nOther message"),
	})
	must.NoError(err)
	raw, err := d.engine.GetMessageRaw(t.Context(), d.withRaw)
	must.ErrorIs(err, store.ErrMessageNotFound)
	assert.Empty(t, raw)
}

func TestEngineOriginalMessageNeedsCurrentDaemon(t *testing.T) {
	must := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/health", r.URL.Path, "no export request reaches an old daemon")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.32.0"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	must.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	engine := daemonclient.NewEngineAdapter(client)

	_, err = engine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: 1}, 0)
	must.ErrorIs(err, daemonclient.ErrNotSupported)
	_, err = engine.GetMessageRaw(t.Context(), 1)
	must.ErrorIs(err, daemonclient.ErrNotSupported)
	_, err = engine.ListThread(t.Context(), query.ThreadQuery{ThreadID: "x"})
	must.ErrorIs(err, daemonclient.ErrNotSupported)
}

func TestEngineMapsUnsupportedOriginalExportThroughDaemon(t *testing.T) {
	must := require.New(t)
	st := testutil.NewTestStore(t)
	engine, err := query.NewDuckDBEngine("", "", nil)
	must.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	must.NoError(err)
	t.Cleanup(func() { must.NoError(client.Close()) })
	daemonEngine := daemonclient.NewEngineAdapter(client)

	_, err = daemonEngine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: 1}, 0)
	must.ErrorIs(err, query.ErrOriginalExportUnsupported)
	_, err = daemonEngine.ListThread(t.Context(), query.ThreadQuery{ThreadID: "thread"})
	must.ErrorIs(err, query.ErrOriginalExportUnsupported)
}
