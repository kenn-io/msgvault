package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// The writer must acquire the identity fence before the Events clock.
func TestMCPEventsEnabledWriterFencesIdentityBeforeClock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		releaseIdentity := holdIdentityRow(t, f.Store)
		clockTx, err := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(err)
		defer func() { _ = clockTx.Rollback() }()
		_, err = clockTx.Exec(`SELECT head_seq FROM mcp_event_clock WHERE singleton=1 FOR UPDATE`)
		require.NoError(err)
		done := make(chan error, 1)
		go func() {
			_, err := f.Store.UpsertMessage(&store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "synthetic-fenced-message", MessageType: "email"})
			done <- err
		}()
		waitForLockWait(t, f.Store, "archive_metadata", "writer must first wait for identity")
		releaseIdentity()
		waitForLockWait(t, f.Store, "UPDATE mcp_event_clock", "writer must next wait for the Events clock")
		probe, err := f.Store.DB().BeginTx(t.Context(), nil)
		require.NoError(err)
		defer func() { _ = probe.Rollback() }()
		_, err = probe.Exec(`SELECT value FROM archive_metadata WHERE key='identity_revision' FOR UPDATE NOWAIT`)
		var pgErr *pgconn.PgError
		require.ErrorAs(err, &pgErr)
		assert.Equal("55P03", pgErr.Code, "writer must hold identity while waiting for the clock")
		require.NoError(probe.Rollback())
		require.NoError(clockTx.Commit())
		assertNoDeadlock(t, waitResult(t, done, "enabled writer"))
		return
	}
	// SQLite admits one writer; triggers observe the order within its transaction.
	_, err = f.Store.DB().Exec(`INSERT INTO archive_metadata (key,value) VALUES ('synthetic_mcp_identity_seen','0'),('synthetic_mcp_clock_seen','0')`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_identity AFTER UPDATE ON archive_metadata WHEN NEW.key='identity_revision' BEGIN UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_identity_seen'; END`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TRIGGER observe_mcp_clock BEFORE UPDATE ON mcp_event_clock BEGIN SELECT CASE WHEN (SELECT value FROM archive_metadata WHERE key='synthetic_mcp_identity_seen')='1' THEN NULL ELSE RAISE(ABORT,'identity fence must precede Events clock') END; UPDATE archive_metadata SET value='1' WHERE key='synthetic_mcp_clock_seen'; END`)
	require.NoError(err)
	assert.Positive(f.CreateMessage("synthetic-fenced-message"))
	var clockSeen string
	require.NoError(f.Store.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='synthetic_mcp_clock_seen'`).Scan(&clockSeen))
	assert.Equal("1", clockSeen)
}

func TestMCPEnabledWriterWithStaleCoverageRecordsCaptureGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	grantSyntheticSourceFixture(t, f)

	staleWriter, err := store.OpenForTest(store.DBPathForTest(f.Store))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(staleWriter.Close()) })
	narrow := mcpStoreConfig()
	_, err = staleWriter.ConfigureMCPEvents(t.Context(), narrow)
	require.NoError(err)
	broad := narrow
	broad.Capabilities = append(append([]store.MCPEventCapability(nil), narrow.Capabilities...), store.MCPEventCapability{
		Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"},
	})
	_, err = f.Store.ConfigureMCPEvents(t.Context(), broad)
	require.NoError(err)
	var expectedFingerprint string
	require.NoError(f.Store.DB().QueryRow(`SELECT coverage_fingerprint FROM mcp_event_clock WHERE singleton=1`).Scan(&expectedFingerprint))

	now := time.Now().UTC()
	input := store.MCPSubscription{
		ID: "sub_stale_writer_coverage_fence", Principal: "owner:synthetic", Name: "msgvault.source_message_archived",
		Arguments: []byte(`{"kinds":["message"]}`), ScopeKind: "source", ScopeID: f.Source.ID, SourceID: f.Source.ID,
		CallbackURL: "https://receiver.example.net/events", SecretEnc: []byte("synthetic-secret"),
		SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(f.Store.BindMCPSubscriptionScope(t.Context(), &input))
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	require.NotNil(active)
	assert.Equal("active", active.State)
	var beforeEpoch int64
	require.NoError(f.Store.DB().QueryRow(`SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&beforeEpoch))

	message := f.NewMessage().WithSourceMessageID("synthetic-stale-coverage-message").Build()
	_, err = staleWriter.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message})
	require.NoError(err)

	stopped, err := f.Store.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State, "a writer with incompatible coverage must stop subscriptions before it can omit arrivals")
	assert.Equal("capture_gap", stopped.StopReason)
	var afterEpoch int64
	require.NoError(f.Store.DB().QueryRow(`SELECT capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&afterEpoch))
	assert.Equal(beforeEpoch+1, afterEpoch, "the incompatible write records a new capture epoch")
	var actualFingerprint string
	require.NoError(f.Store.DB().QueryRow(`SELECT coverage_fingerprint FROM mcp_event_clock WHERE singleton=1`).Scan(&actualFingerprint))
	assert.Equal(expectedFingerprint, actualFingerprint, "a stale writer must not replace the current coverage")
	var captured int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=? AND source_id=?`), "msgvault.source_message_archived", f.Source.ID).Scan(&captured))
	assert.Zero(captured, "the stale writer does not emit the newly selected source family")

	_, err = f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Principal: "owner:synthetic"})
	require.NoError(err)
	disabledMessage := f.NewMessage().WithSourceMessageID("synthetic-stale-disabled-coverage-message").Build()
	_, err = staleWriter.WithIngestContext(store.IngestContext{Mode: store.IngestLive}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: disabledMessage})
	require.ErrorContains(err, "capture_configuration_changed", "a stale enabled writer must not turn disabled capture back on")
	var persisted int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id=?`), f.Source.ID, disabledMessage.SourceMessageID).Scan(&persisted))
	assert.Zero(persisted, "the incompatible mutation is rejected")
	var enabled bool
	require.NoError(f.Store.DB().QueryRow(`SELECT enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&enabled))
	assert.False(enabled, "stale writer configuration must not re-enable Events")
}
