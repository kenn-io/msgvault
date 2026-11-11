package cmd

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXProfileOnlyCleanupPersistsFenceAcrossReopen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := xTestConfig(t)
	cfg.Server.APIKey = "example-owner-key"
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("x", "1001")
	require.NoError(err)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "1001", InitializeInstance: true})
	require.NoError(err)
	at := time.Now().UTC().Add(-time.Hour)
	pid, err := st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: source.ID, UserID: "1001", Username: "former_handle", DisplayName: "Former Public Label", FetchedAt: at})
	require.NoError(err)
	require.NoError(st.QueueXAuthorRemovalContext(t.Context(), source.ID, "1001", at.Add(time.Minute)))
	pending, err := st.XCleanupPendingContext(t.Context())
	require.NoError(err)
	require.True(pending, "protected profile evidence must fence reads even without post aliases")
	_, _, err = st.ExportXRemovalRecordContext(t.Context())
	require.ErrorContains(err, "cleanup is pending", "an incomplete erasure cannot produce current removal authority")
	require.ErrorIs(st.RemoveSource(source.ID), store.ErrXRemovalPending, "source deletion cannot forget the erasure obligation")
	cleaner := NewXContentCleanup(st, cfg, nil)
	cleaner.beforePublish = func() error { return errors.New("synthetic publication failure") }
	require.ErrorContains(cleaner.Resume(t.Context()), "synthetic publication failure")
	var name sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
	assert.Empty(name.String, "SQL purge precedes the interrupted derived cleanup")
	pending, err = st.XCleanupPendingContext(t.Context())
	require.NoError(err)
	require.True(pending, "the durable marker survives after the last unsupported username is gone")
	require.NoError(st.Close())
	reopened, err := store.OpenContext(t.Context(), cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(reopened.Close()) })
	require.NoError(reopened.InitSchemaContext(t.Context()))
	recovery := NewXContentCleanup(reopened, cfg, nil)
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: reopened, config: cfg}, Engine: query.NewEngine(reopened.DB(), false), Logger: slog.New(slog.DiscardHandler), ContentReadGuard: recovery.AcquireRead})
	recovery.SetServer(server)
	read := func() *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cli/accounts", nil)
		r.Header.Set("X-Api-Key", cfg.Server.APIKey)
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, r)
		return out
	}
	out := read()
	assert.Equal(http.StatusServiceUnavailable, out.Code, out.Body.String())
	assert.Contains(out.Body.String(), "x_content_cleanup_pending")
	require.NoError(recovery.Resume(t.Context()))
	pending, err = reopened.XCleanupPendingContext(t.Context())
	require.NoError(err)
	assert.False(pending)
	out = read()
	assert.Equal(http.StatusOK, out.Code, out.Body.String())
	assert.NotContains(out.Body.String(), "Former Public Label")
	var anchors int
	require.NoError(reopened.DB().QueryRowContext(t.Context(), reopened.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id=? AND identifier_type='x_user_id'`), pid).Scan(&anchors))
	assert.Equal(1, anchors)
}

func TestXRestoreReplaysProtectedProfileWithoutPostRemovals(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXRestoreFixture(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	pid, err := f.st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: f.source.ID, UserID: "1001", Username: "former_handle", DisplayName: "Former Public Label", FetchedAt: at})
	require.NoError(err)
	snapshot := f.snapshot(t)
	require.NoError(f.st.QueueXAuthorRemovalContext(t.Context(), f.source.ID, "1001", at.Add(time.Minute)))
	require.NoError(NewXContentCleanup(f.st, f.cfg, nil).Resume(t.Context()))
	record, head := f.record(t)
	target, err := f.restore(t, snapshot, record, head)
	require.NoError(err)
	restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
	require.NoError(err)
	defer func() { assert.NoError(restored.Close()) }()
	var name sql.NullString
	require.NoError(restored.DB().QueryRowContext(t.Context(), restored.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
	assert.Empty(name.String, "current protected-author authority must erase the snapshot's unsupported profile")
	var usernames int
	require.NoError(restored.DB().QueryRowContext(t.Context(), restored.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND source='archive_observation'`), pid).Scan(&usernames))
	assert.Zero(usernames)
	assert.Zero(f.providerCalls.Load())
}

func TestXProfileCleanupRetainsNewerPublicEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newXRestoreFixture(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	pid, err := f.st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: f.source.ID, UserID: "1001", Username: "former_handle", DisplayName: "Former Public Label", FetchedAt: at})
	require.NoError(err)
	other, err := f.st.GetOrCreateSource("x", "3001")
	require.NoError(err)
	_, err = f.st.UpdateXControlContext(t.Context(), other.ID, 0, store.XControlPatch{OwnerID: "3001", InitializeInstance: true})
	require.NoError(err)
	_, err = f.st.RecordXAuthorContext(t.Context(), store.XAuthorInput{SourceID: other.ID, UserID: "1001", Username: "new_handle", DisplayName: "New Public Label", FetchedAt: at.Add(2 * time.Minute)})
	require.NoError(err)
	snapshot := f.snapshot(t)
	// The protected response arrives after a newer public response; its original
	// time still retires old labels without erasing the newer observation.
	require.NoError(f.st.QueueXAuthorRemovalContext(t.Context(), f.source.ID, "1001", at.Add(time.Minute)))
	require.NoError(NewXContentCleanup(f.st, f.cfg, nil).Resume(t.Context()))
	var oldRows, newRows int
	require.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND normalized_value='former_handle'`), pid).Scan(&oldRows))
	assert.Zero(oldRows)
	require.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND normalized_value='new_handle'`), pid).Scan(&newRows))
	assert.Equal(1, newRows)
	record, head := f.record(t)
	target, err := f.restore(t, snapshot, record, head)
	require.NoError(err)
	restored, err := store.OpenReadOnly(filepath.Join(target, "msgvault.db"))
	require.NoError(err)
	defer func() { assert.NoError(restored.Close()) }()
	var name sql.NullString
	require.NoError(restored.DB().QueryRowContext(t.Context(), restored.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
	assert.Empty(name.String, "a newer username alone cannot support the old display label")
	require.NoError(restored.DB().QueryRowContext(t.Context(), restored.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND normalized_value='former_handle'`), pid).Scan(&oldRows))
	assert.Zero(oldRows)
	require.NoError(restored.DB().QueryRowContext(t.Context(), restored.Rebind(`SELECT COUNT(*) FROM participant_contact_observations WHERE participant_id=? AND address_kind='username' AND normalized_value='new_handle'`), pid).Scan(&newRows))
	assert.Equal(1, newRows)
	assert.Zero(f.providerCalls.Load())
}
