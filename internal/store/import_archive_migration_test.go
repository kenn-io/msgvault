package store_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// This is the pre-import production table shape. The fixture performs the
// actual schema upgrade with populated native receipts and resource children.
const legacyImportSubscriptionSQL = `CREATE TABLE mcp_subscriptions_legacy (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    name TEXT NOT NULL,
    arguments TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_id INTEGER NOT NULL,
    source_id INTEGER NOT NULL,
    scope_reference_id INTEGER NOT NULL DEFAULT 0,
    callback_url TEXT NOT NULL,
    secret_enc BLOB NOT NULL,
    previous_secret_enc BLOB,
    previous_secret_until TEXT,
    secret_revision INTEGER NOT NULL CHECK (secret_revision > 0),
    verified_revision INTEGER NOT NULL,
    verified_at TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    state TEXT NOT NULL CHECK (state IN ('active', 'expired', 'unsubscribed', 'gone', 'stopped')),
    stop_reason TEXT NOT NULL DEFAULT '',
    expires_at TEXT NOT NULL,
    cursor_epoch INTEGER NOT NULL,
    cursor_seq INTEGER NOT NULL CHECK (cursor_seq >= 0),
    pending_seq INTEGER,
    pending_envelope BLOB,
    pending_generation INTEGER,
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 12),
    next_attempt_at TEXT,
    from_me_window_start TEXT,
    from_me_window_count INTEGER NOT NULL DEFAULT 0 CHECK (from_me_window_count >= 0),
    loop_guard_skips INTEGER NOT NULL DEFAULT 0 CHECK (loop_guard_skips >= 0),
    dead_letter_count INTEGER NOT NULL DEFAULT 0 CHECK (dead_letter_count >= 0),
    last_outcome TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((pending_seq IS NULL AND pending_envelope IS NULL AND pending_generation IS NULL)
        OR (pending_seq IS NOT NULL AND pending_seq > 0
            AND pending_envelope IS NOT NULL
            AND pending_generation IS NOT NULL AND pending_generation > 0))
);`

func installPreImportSubscriptions(t *testing.T, st *store.Store) {
	t.Helper()
	if st.IsPostgreSQL() {
		rows, err := st.DB().QueryContext(t.Context(), `SELECT conname FROM pg_constraint WHERE conrelid='mcp_event_subscriptions'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%scope_kind%'`)
		Require.NoError(t, err)
		defer func() { Require.NoError(t, rows.Close()) }()
		var constraints []string
		for rows.Next() {
			var name string
			Require.NoError(t, rows.Scan(&name))
			constraints = append(constraints, name)
		}
		Require.NoError(t, rows.Err())
		for _, name := range constraints {
			_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE mcp_event_subscriptions DROP CONSTRAINT "`+strings.ReplaceAll(name, `"`, `""`)+`"`)
			Require.NoError(t, err)
		}
		_, err = st.DB().ExecContext(t.Context(), `ALTER TABLE mcp_event_subscriptions ALTER COLUMN source_id SET NOT NULL`)
		Require.NoError(t, err)
	} else {
		conn, err := st.DB().Conn(t.Context())
		Require.NoError(t, err)
		defer func() { Assert.NoError(t, conn.Close()) }()
		_, err = conn.ExecContext(t.Context(), `PRAGMA foreign_keys=OFF`)
		Require.NoError(t, err)
		defer func() {
			_, err := conn.ExecContext(context.WithoutCancel(t.Context()), `PRAGMA foreign_keys=ON`)
			Assert.NoError(t, err)
		}()
		tx, err := conn.BeginTx(t.Context(), nil)
		Require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		for _, statement := range []string{legacyImportSubscriptionSQL, `INSERT INTO mcp_subscriptions_legacy SELECT * FROM mcp_event_subscriptions`, `DROP TABLE mcp_event_subscriptions`, `ALTER TABLE mcp_subscriptions_legacy RENAME TO mcp_event_subscriptions`} {
			_, err := tx.ExecContext(t.Context(), statement)
			Require.NoError(t, err)
		}
		Require.NoError(t, tx.Commit())
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM applied_migrations WHERE name=?`), "mcp_import_archive_subscriptions_v1")
	Require.NoError(t, err)
}

func TestImportArchiveUpgradePreservesOriginalReceiptsPendingBytesAndBindings(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)
	const owner = "owner:synthetic-import-upgrade"
	const family = "msgvault.import_message_archived"
	cfg := store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: family, SourceType: "eml", Kinds: []string{"message"}}, {Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}}
	_, err := f.Store.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	now := time.Now().UTC()
	_, originalBinding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.message_archived", "conversation", f.ConvID, []string{"message"}, "")
	require.NoError(err)
	activateOriginal := func(id string) *store.MCPSubscription {
		input := store.MCPSubscription{
			ID: id, Principal: owner, Name: "msgvault.message_archived", Arguments: []byte(`{"conversation_id":"1","include_from_me":false}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID,
			CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-ciphertext"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
		}
		Require.NoError(t, f.Store.BindMCPSubscriptionScope(t.Context(), &input))
		sub, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Now: now, ResourceBinding: originalBinding, Subscription: input})
		Require.NoError(t, err)
		return sub
	}
	original := activateOriginal("sub_synthetic_upgrade_pending")
	dead := activateOriginal("sub_synthetic_upgrade_dead")
	live := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now})
	messageID, err := live.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().WithSourceMessageID("synthetic-upgrade-live").Build(), BodyText: sql.NullString{String: "Synthetic original body", Valid: true}})
	require.NoError(err)
	envelope := func(_ store.MCPSubscription, _ store.MCPEvent) ([]byte, error) {
		return []byte(`{"synthetic_preserved":"exact bytes"}`), nil
	}
	pending, err := f.Store.PrepareMCPDelivery(t.Context(), original.ID, original.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(pending)
	deadPending, err := f.Store.PrepareMCPDelivery(t.Context(), dead.ID, dead.Generation, now, envelope)
	require.NoError(err)
	require.NotNil(deadPending)
	require.NoError(f.Store.FinishMCPDelivery(t.Context(), dead.ID, dead.Generation, deadPending.Event.Seq, now, 413, time.Time{}))

	installPreImportSubscriptions(t, f.Store)
	require.NoError(f.Store.InitSchemaContext(t.Context()))
	require.NoError(f.Store.InitSchemaContext(t.Context()), "upgrade is idempotent")
	after, err := f.Store.GetMCPSubscription(t.Context(), original.ID)
	require.NoError(err)
	require.NotNil(after)
	assert.Equal(pending.Subscription.PendingEnvelope, after.PendingEnvelope)
	assert.Equal(pending.Event.Seq, after.PendingSeq)
	assert.Equal(original.Generation, after.Generation)
	require.NoError(f.Store.CheckMCPSubscription(t.Context(), original.ID, original.Generation, now), "native resource binding survives rebuild")
	event, err := f.Store.GetMCPEvent(t.Context(), original.ID, pending.Event.Seq, owner, now)
	require.NoError(err)
	assert.Equal(messageID, event.MessageID)
	var deadCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_dead_letters`).Scan(&deadCount))
	assert.Equal(1, deadCount)

	_, archiveBinding, err := f.Store.ResolveMCPSubscriptionResource(t.Context(), family, "archive", 0, []string{"message"}, "")
	require.NoError(err)
	archive, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Now: now, ResourceBinding: archiveBinding, Subscription: store.MCPSubscription{
		ID: "sub_synthetic_upgrade_archive", Principal: owner, Name: family, Arguments: []byte(`{"include_from_me":false,"source_id":"*"}`), ScopeKind: "archive",
		CallbackURL: "https://receiver.example.net/imports", SecretEnc: []byte("synthetic-ciphertext"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}})
	require.NoError(err, "upgraded archive admits a deliberately null source")
	require.NotNil(archive)
	for _, invalid := range []struct {
		family, scope string
		source        any
		id            int64
	}{
		{"msgvault.message_archived", "conversation", nil, f.ConvID},
		{family, "source", nil, f.Source.ID},
		{family, "archive", f.Source.ID, 0},
		{"msgvault.message_archived", "archive", nil, 0},
		{family, "source", int64(0), f.Source.ID},
	} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE mcp_event_subscriptions SET name=?,scope_kind=?,source_id=?,scope_id=? WHERE id=?`), invalid.family, invalid.scope, invalid.source, invalid.id, archive.ID)
		require.Error(err, "nullable source is legal only for the exact import archive shape")
	}
	_, err = f.Store.DB().ExecContext(t.Context(), `INSERT INTO mcp_subscription_resource_bindings (subscription_id,source_generation,scope_generation,valid) VALUES ('sub_synthetic_missing','synthetic','synthetic',TRUE)`)
	require.Error(err, "rebuild restores foreign-key enforcement")
}
