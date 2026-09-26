package daemonclient_test

import (
	"database/sql"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	engine  *daemonclient.Engine
	st      *store.Store
	withRaw int64
	noRaw   int64
	raw     []byte
}

func newOriginalDaemon(t *testing.T) originalDaemon {
	t.Helper()
	st := testutil.NewTestStore(t)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	t.Cleanup(func() { _ = engine.Close() })
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{}, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler),
	}).Router())
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
		engine:  daemonclient.NewEngineAdapter(client),
		st:      st,
		withRaw: persist("daemon-raw", base, raw),
		noRaw:   persist("daemon-no-raw", base.Add(time.Hour), nil),
		raw:     raw,
	}
}

func TestEngineReadsOriginalMessagesThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	d := newOriginalDaemon(t)
	ctx := t.Context()

	got, err := d.engine.ReadOriginalMessage(ctx, query.MessageRef{SourceMessageID: "daemon-raw"})
	must.NoError(err)
	checks.Equal(d.raw, got.MIME)
	checks.Equal(d.withRaw, got.MessageID)
	checks.Equal("owner@example.com", got.Account)
	checks.Equal("thread-daemon", got.SourceConversationID)

	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{ID: d.noRaw})
	must.ErrorIs(err, query.ErrOriginalMIMEUnavailable)
	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{ID: 999999})
	must.ErrorIs(err, store.ErrMessageNotFound)
	_, err = d.engine.ReadOriginalMessage(ctx, query.MessageRef{})
	must.ErrorIs(err, query.ErrInvalidMessageRef)

	raw, err := d.engine.GetMessageRaw(ctx, d.noRaw)
	must.NoError(err, "missing raw follows the engine's nil, nil contract")
	checks.Nil(raw)
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

	_, err = d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "missing"})
	must.ErrorIs(err, query.ErrThreadNotFound)

	other, err := d.st.GetOrCreateSource("gmail", "second@example.com")
	must.NoError(err)
	_, err = d.st.EnsureConversation(other.ID, "thread-daemon", "Other")
	must.NoError(err)
	_, err = d.engine.ListThread(ctx, query.ThreadQuery{ThreadID: "thread-daemon"})
	must.ErrorIs(err, query.ErrAmbiguousReference)
	checks.Contains(err.Error(), "second@example.com")
}

func TestEngineOriginalMessageNeedsCurrentDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/health", r.URL.Path, "no export request reaches an old daemon")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.29.0"}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	engine := daemonclient.NewEngineAdapter(client)

	_, err = engine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: 1})
	require.ErrorIs(t, err, daemonclient.ErrNotSupported)
	_, err = engine.ListThread(t.Context(), query.ThreadQuery{ThreadID: "x"})
	require.ErrorIs(t, err, daemonclient.ErrNotSupported)
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

	_, err = daemonEngine.ReadOriginalMessage(t.Context(), query.MessageRef{ID: 1})
	must.ErrorIs(err, query.ErrOriginalExportUnsupported)
	_, err = daemonEngine.ListThread(t.Context(), query.ThreadQuery{ThreadID: "thread"})
	must.ErrorIs(err, query.ErrOriginalExportUnsupported)
}
