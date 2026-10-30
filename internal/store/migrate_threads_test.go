package store_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestThreadsSchemaUpgrade(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	s, ctx := f.Store, t.Context()
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		require.True(s.IsPostgreSQL())
	}
	messageID := f.CreateMessage("preserved")
	// Remove the feature's entire schema and data seed to exercise the real
	// previous version, rather than changing only its completion marker.
	_, err := s.DB().ExecContext(ctx, `DROP TABLE IF EXISTS threads_reply_sends`)
	require.NoError(err)
	_, err = s.DB().ExecContext(ctx, `DELETE FROM applied_migrations WHERE name = 'communication_services_threads_v1'`)
	require.NoError(err)
	_, err = s.DB().ExecContext(ctx, `DELETE FROM communication_services WHERE slug = 'threads'`)
	require.NoError(err)
	_, err = s.DB().ExecContext(ctx, `UPDATE archive_metadata SET value = '15' WHERE key = 'schema_version'`)
	require.NoError(err)
	require.Error(s.ValidateSchemaContext(ctx))
	require.NoError(s.InitSchemaContext(ctx))
	require.NoError(s.ValidateSchemaContext(ctx))
	var preserved int64
	require.NoError(s.DB().QueryRowContext(ctx, s.Rebind(`SELECT id FROM messages WHERE id = ?`), messageID).Scan(&preserved))
	assert.Equal(messageID, preserved)
	service, err := s.ResolveCommunicationServiceContext(ctx, "threads")
	require.NoError(err)
	assert.Equal("Threads", service.DisplayLabel)
	assert.Equal(store.ScopePolicyNone, service.ScopePolicy)
	assert.Equal(store.NormalizationStripAtLower, service.Normalization)
	require.NotNil(service.ProfileURLTemplate)
	assert.Equal("https://www.threads.com/@{username}", *service.ProfileURLTemplate)
	_, err = s.DB().ExecContext(ctx, `UPDATE communication_services SET display_label = 'My Threads' WHERE slug = 'threads'`)
	require.NoError(err)
	require.NoError(s.InitSchemaContext(ctx))
	service, err = s.ResolveCommunicationServiceContext(ctx, "threads")
	require.NoError(err)
	assert.Equal("My Threads", service.DisplayLabel)
	_, err = s.DB().ExecContext(ctx, `UPDATE archive_metadata SET value = '999' WHERE key = 'schema_version'`)
	require.NoError(err)
	require.Error(s.ValidateSchemaContext(ctx))
}

func TestThreadsSchemaReceiptLifetime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	s, ctx := f.Store, t.Context()
	_, err := s.DB().ExecContext(ctx, s.Rebind(`INSERT INTO chat_drafts
		(draft_id, source_id, conversation_id, source_conversation_id, conversation_type, body, revision)
		VALUES ('receipt-draft', ?, ?, '100', 'channel', 'Hello', 1)`), f.Source.ID, f.ConvID)
	require.NoError(err)
	insert := s.Rebind(`INSERT INTO threads_reply_sends
		(draft_id, source_id, source_identifier, conversation_id, draft_revision, root_media_id,
		parent_media_id, body, body_hash, state)
		VALUES ('receipt-draft', ?, 'threads:10:20', ?, 1, '100', '101', 'Hello', 'hash', 'claimed')`)
	_, err = s.DB().ExecContext(ctx, insert, f.Source.ID, f.ConvID)
	require.NoError(err)
	_, err = s.DB().ExecContext(ctx, insert, f.Source.ID, f.ConvID)
	require.Error(err, "one receipt per draft")
	_, err = s.DB().ExecContext(ctx, `DELETE FROM chat_drafts WHERE draft_id = 'receipt-draft'`)
	require.NoError(err)
	var count int
	require.NoError(s.DB().QueryRowContext(ctx, `SELECT count(*) FROM threads_reply_sends`).Scan(&count))
	assert.Equal(1, count)
	_, err = s.DB().ExecContext(ctx, `UPDATE threads_reply_sends SET state = 'invented'`)
	require.Error(err)
	_, err = s.DB().ExecContext(ctx, `UPDATE threads_reply_sends SET draft_revision = 0`)
	require.Error(err)
	_, err = s.DB().ExecContext(ctx, s.Rebind(`DELETE FROM sources WHERE id = ?`), f.Source.ID)
	require.NoError(err)
	require.NoError(s.DB().QueryRowContext(ctx, `SELECT count(*) FROM threads_reply_sends`).Scan(&count))
	assert.Zero(count)
}

func TestThreadsIdentifierClassification(t *testing.T) {
	f := storetest.New(t)
	for _, tc := range []struct{ kind, value string }{
		{"threads_user_id", "threads:10:20"},
		{"threads_reply_author", "threads:10:20:media:100"},
	} {
		_, err := f.Store.EnsureParticipantByIdentifier(tc.kind, tc.value, "Synthetic author")
		require.NoError(t, err)
		assertParticipantIdentifierClassification(t, f.Store, tc.kind, tc.value, "threads", "", "")
	}
}

// Hegel minimized the absent service classification to app=owner=media=1.
func TestThreadsOccurrenceOneNamespaceRegression(t *testing.T) {
	f := storetest.New(t)
	const key = "threads:1:1:media:1"
	id, err := f.Store.EnsureParticipantByIdentifier("threads_reply_author", key, "Unknown Threads author")
	require.NoError(t, err)
	replayed, err := f.Store.EnsureParticipantByIdentifier("threads_reply_author", key, "Renamed author")
	require.NoError(t, err)
	assert.Equal(t, id, replayed)
	assertParticipantIdentifierClassification(t, f.Store, "threads_reply_author", key, "threads", "", "")
}
