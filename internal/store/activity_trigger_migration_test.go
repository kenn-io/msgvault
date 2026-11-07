package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestActivityTriggerMigrationDefersConversationMemberFanout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("membership-upgrade")
	participantID := f.EnsureParticipant("member@example.com", "Member", "example.com")
	// Restore the old fanout trigger, as an archive awaiting the v6 repair has it.
	if f.Store.IsPostgreSQL() {
		_, err := f.Store.DB().ExecContext(t.Context(), `CREATE OR REPLACE FUNCTION queue_activity_conversation_people_insert() RETURNS trigger AS $$
		BEGIN
		    PERFORM enqueue_activity_projection_conversation(NEW.conversation_id);
		    RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`)
		require.NoError(err)
	} else {
		_, err := f.Store.DB().ExecContext(t.Context(), `DROP TRIGGER trg_activity_queue_conversation_people_insert;
		CREATE TRIGGER trg_activity_queue_conversation_people_insert
		AFTER INSERT ON conversation_participants FOR EACH ROW BEGIN
		    INSERT INTO activity_projection_queue (message_id, revision, queued_at)
		    SELECT id, 1, CURRENT_TIMESTAMP FROM messages WHERE conversation_id = NEW.conversation_id
		    ON CONFLICT(message_id) DO UPDATE SET revision = activity_projection_queue.revision + 1;
		END;`)
		require.NoError(err)
	}
	_, err := f.Store.DB().ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v6'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchema())
	before := activityQueueRevision(t, f.Store, messageID)
	require.NoError(f.Store.EnsureConversationParticipantContext(t.Context(), f.ConvID, participantID, "member"))
	assert.Equal(before, activityQueueRevision(t, f.Store, messageID))
	var conversationID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT conversation_id FROM activity_projection_conversation_queue`).Scan(&conversationID))
	assert.Equal(f.ConvID, conversationID)
}

func TestActivityTriggerMigrationRequeuesProjectedUnsentMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := activityProjectionFixture(t)
	occurredAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	messageID := f.NewMessage().WithSentAt(occurredAt).Create(t, f.Store)
	projection := activityProjectionForMessage(t, f, messageID, personID, occurredAt)
	_, err := f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	labels := f.EnsureLabels(map[string]string{"DRAFT": "Drafts"}, "system")
	// Bypass attribution hooks to reproduce a draft projected by an older daemon.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`), messageID, labels["DRAFT"])
	require.NoError(err)
	queued, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Empty(queued)
	_, err = f.Store.DB().ExecContext(t.Context(), `DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v6'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchema())
	queued, err = f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(queued, 1)
	assert.Equal(messageID, queued[0].MessageID)
	assert.False(queued[0].Eligible)
}

func TestActivityUpgradeRetractsUnsentContact(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, personID := activityProjectionFixture(t)
	olderAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	newerAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	olderID := f.NewMessage().WithSentAt(olderAt).Create(t, f.Store)
	messageID := f.NewMessage().WithSentAt(newerAt).Create(t, f.Store)
	_, err := f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{
		activityProjectionForMessage(t, f, olderID, personID, olderAt),
		activityProjectionForMessage(t, f, messageID, personID, newerAt),
	})
	require.NoError(err)
	labels := f.EnsureLabels(map[string]string{"DRAFT": "Drafts"}, "system")
	// Reproduce an older projector's event without running attribution hooks.
	_, err = activityExec(t.Context(), f.Store, `INSERT INTO message_labels(message_id, label_id) VALUES (?, ?)`, messageID, labels["DRAFT"])
	require.NoError(err)
	var revisionBefore int64
	require.NoError(activityQueryRow(t.Context(), f.Store, `SELECT revision FROM activity_projection_queue WHERE message_id = ?`, messageID).Scan(&revisionBefore))
	_, err = activityExec(t.Context(), f.Store, `DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v6'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchema())
	queued, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
	require.NoError(err)
	require.Len(queued, 1)
	assert.Equal(messageID, queued[0].MessageID)
	assert.False(queued[0].Eligible)
	assert.Greater(queued[0].Queue.Revision, revisionBefore)
	projection := activityProjectionFromCandidate(t, queued[0], personID, newerAt)
	projection.Event = nil
	_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{projection})
	require.NoError(err)
	state, err := f.Store.ContactStateContext(t.Context(), personID, newerAt)
	require.NoError(err)
	require.NotNil(state.LastContactAt)
	assert.Equal(olderAt, state.LastContactAt.UTC())
	assert.Equal(int64(1), state.InteractionCount)
}

// A fresh archive has both the combined message and attachment trigger
// migration and the activity triggers installed.
func TestPostgresActivityTriggersInstallAfterCombinedTriggerMigration(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") &&
		!strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("PostgreSQL trigger migration regression")
	}
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	var watermarkMigrationApplied bool
	err := st.DB().QueryRowContext(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM applied_migrations
			WHERE name = 'message_and_attachment_triggers_v12'
		)
	`).Scan(&watermarkMigrationApplied)
	require.NoError(err)
	assert.True(watermarkMigrationApplied)

	source, err := st.GetOrCreateSource("gmail", "activity-trigger@example.invalid")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		t.Context(),
		source.ID, "activity-trigger-thread", "Activity trigger")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "activity-trigger-message",
		MessageType:     "email",
		SentAt:          sql.NullTime{Valid: false},
	})
	require.NoError(err)

	var revision int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT revision
		FROM activity_projection_queue
		WHERE message_id = ?
	`), messageID).Scan(&revision)
	require.NoError(err,
		"activity trigger migration must install queue triggers on a fresh archive")
	assert.Equal(int64(1), revision)
}
