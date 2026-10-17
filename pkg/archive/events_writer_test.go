package archive_test

import (
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestLibraryWriteRecordsGapAfterEventsReenabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var open func() (*archive.Archive, error)
	if dsn := os.Getenv("MSGVAULT_TEST_DB"); store.IsPostgresURL(dsn) {
		admin, err := sql.Open("pgx", dsn)
		require.NoError(err)
		t.Cleanup(func() { assert.NoError(admin.Close()) })
		schema := "archive_" + strings.ToLower(rand.Text())
		t.Cleanup(func() {
			_, err := admin.Exec("DROP SCHEMA IF EXISTS " + pgx.Identifier{schema}.Sanitize() + " CASCADE")
			assert.NoError(err)
		})
		opts := archive.PostgreSQL{URL: dsn, Schema: schema}
		require.NoError(archive.Setup(t.Context(), opts))
		open = func() (*archive.Archive, error) { return archive.Open(t.Context(), opts) }
	} else {
		if !sqliteutil.Available {
			t.Skip("requires SQLite or a disposable PostgreSQL database")
		}
		path := filepath.Join(t.TempDir(), "archive.db")
		require.NoError(archive.SetupSQLite(t.Context(), path))
		open = func() (*archive.Archive, error) { return archive.OpenSQLite(t.Context(), path) }
	}

	daemonArchive, err := open()
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(daemonArchive.Close()) })
	daemon := daemonArchive.Store()
	source, err := daemon.GetOrCreateSource("gmail", "library-writer@example.test")
	require.NoError(err)
	conversation, err := daemon.EnsureConversation(source.ID, "library-writer-thread", "Library writer")
	require.NoError(err)
	cfg := store.MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	}
	_, err = daemon.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	library, err := open()
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(library.Close()) })
	// The library stays open while the daemon enables capture again.
	clock, err := daemon.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	now := time.Now().UTC()
	input := store.MCPSubscription{
		ID: "sub_library_writer", Principal: cfg.Principal, Name: "msgvault.message_archived",
		Arguments: []byte(`{"conversation_id":"` + strconv.FormatInt(conversation, 10) + `","include_from_me":false}`),
		ScopeKind: "conversation", ScopeID: conversation, CallbackURL: "https://receiver.example.net/events",
		SecretEnc: []byte("synthetic-secret"), SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(daemon.BindMCPSubscriptionScope(t.Context(), &input))
	active, _, err := daemon.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: input, Now: now})
	require.NoError(err)
	message := store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "captured-message", MessageType: "email"}
	_, err = daemon.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: now}).PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &message})
	require.NoError(err)
	pending, err := daemon.PrepareMCPDelivery(t.Context(), active.ID, active.Generation, now, func(store.MCPSubscription, store.MCPEvent) ([]byte, error) {
		return []byte(`{"eventId":"synthetic-pending"}`), nil
	})
	require.NoError(err)
	require.NotNil(pending)

	// A database failure must roll back the gap along with the mutation.
	message.SourceMessageID = "uncaptured-message"
	message.ConversationID = -1
	_, err = library.Store().PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &message})
	require.Error(err)
	unchanged, err := daemon.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(unchanged)
	assert.Equal("active", unchanged.State)
	assert.Equal(pending.Subscription.PendingEnvelope, unchanged.PendingEnvelope)
	var epoch int64
	var enabled bool
	require.NoError(daemon.DB().QueryRow(`SELECT capture_epoch, enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch, &enabled))
	assert.Equal(clock.Epoch, epoch)
	assert.True(enabled)

	message.ConversationID = conversation
	id, err := library.Store().PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &message})
	require.NoError(err)
	assert.Positive(id)
	var messages, events int
	require.NoError(daemon.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	require.NoError(daemon.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
	assert.Equal(2, messages)
	assert.Equal(1, events, "the library mutation is not captured")
	require.NoError(daemon.DB().QueryRow(`SELECT capture_epoch, enabled FROM mcp_event_clock WHERE singleton=1`).Scan(&epoch, &enabled))
	assert.Greater(epoch, clock.Epoch)
	assert.False(enabled)
	stopped, err := daemon.GetMCPSubscription(t.Context(), active.ID)
	require.NoError(err)
	require.NotNil(stopped)
	assert.Equal("stopped", stopped.State)
	assert.Equal("capture_gap", stopped.StopReason)
	assert.Empty(stopped.PendingEnvelope)
	assert.Zero(stopped.PendingSeq)
	assert.Greater(stopped.Generation, active.Generation)

	_, err = daemon.ConfigureMCPEvents(t.Context(), cfg)
	require.NoError(err)
	_, truncated, err := daemon.ActivateMCPSubscription(t.Context(), store.MCPActivation{
		Subscription: input, ExpectedGeneration: stopped.Generation, ExpectedState: stopped.State,
		ExpectedSecretRevision: stopped.SecretRevision, Now: now,
	})
	require.NoError(err)
	assert.True(truncated, "renewal must report the uncaptured library write")
}
