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
)

func TestActivityUpgradeRepairsMaildirDraftAccountAttribution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		activityEvent bool
	}{
		{name: "without_activity_event"},
		{name: "with_activity_event", activityEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newAttrFixture(t, "maildir", "drafts@example.test")
			f.confirm("drafts@example.test")
			mail := attrMail{from: []string{"drafts@example.test"}, to: []string{"drafts@example.test"}}
			messageID := f.persist(mail)
			address, path := attribution(t, f.st, messageID)
			require.Equal("drafts@example.test", address.String)
			require.Equal("inbound", path.String)

			if tc.activityEvent {
				at := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
				_, err := activityExec(f.st, `
					INSERT INTO activity_events (
						message_id, ref_kind, source_id, channel, occurred_at, date_origin,
						date_precision, timezone, utc_offset_minutes, local_date, direction,
						owner_address, projected_last_modified, projected_identity_revision,
						projected_account_identity_revision
					) VALUES (?, 'message', ?, 'email', ?, 'sent_at', 'timestamp',
						'UTC', 0, '2026-07-30', 'inbound', ?, ?, 1, 1)
				`, messageID, f.source.ID, at, "drafts@example.test", at)
				require.NoError(err)
			}

			draft, err := f.st.EnsureLabel(f.source.ID, "DRAFT", "Drafts", "system")
			require.NoError(err)
			// Older attribution ignored Maildir's DRAFT label. Attach it without
			// current attribution hooks to preserve that pre-upgrade state.
			_, err = activityExec(f.st, `INSERT INTO message_labels(message_id, label_id) VALUES (?, ?)`, messageID, draft)
			require.NoError(err)
			_, err = activityExec(f.st, `DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v5'`)
			require.NoError(err)
			require.NoError(f.st.InitSchema())
			_, err = f.st.RepairAccountAttributionContext(t.Context(), f.source.ID, nil)
			require.NoError(err)

			// Fresh attribution must agree with attribution repaired by the upgrade.
			mail.labels = []int64{draft}
			freshID := f.persist(mail)
			_, freshPath := attribution(t, f.st, freshID)
			require.Equal("sent", freshPath.String)
			_, path = attribution(t, f.st, messageID)
			assert.Equal("sent", path.String, "upgrade and repair must rederive the previously inbound Maildir draft")
		})
	}
}

func TestActivityUpgradeRetractsUnsentContact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		merged bool
		flags  string
		unsent bool
	}{
		{name: "ordinary_draft", unsent: true},
		{name: "old_merge_seen", merged: true, flags: `["\\Seen"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if tc.merged {
				original, err := f.Store.GetOrCreateSource("imap", "retired@example.test")
				require.NoError(err)
				_, err = activityExec(f.Store, `INSERT INTO source_merge_messages(message_id, original_source_id, original_message_id)
					SELECT id, ?, source_message_id FROM messages WHERE id = ?`, original.ID, messageID)
				require.NoError(err)
				_, err = activityExec(f.Store, `INSERT INTO imap_message_memberships(source_id, mailbox, uidvalidity, uid, message_id, flags)
					VALUES (?, 'Scratch', 7, 1, ?, ?)`, original.ID, messageID, tc.flags)
				require.NoError(err)
			} else {
				labels := f.EnsureLabels(map[string]string{"DRAFT": "Drafts"}, "system")
				// Reproduce an older projector's event without running attribution hooks.
				_, err = activityExec(f.Store, `INSERT INTO message_labels(message_id, label_id) VALUES (?, ?)`, messageID, labels["DRAFT"])
				require.NoError(err)
			}
			var revisionBefore int64
			require.NoError(activityQueryRow(f.Store, `SELECT revision FROM activity_projection_queue WHERE message_id = ?`, messageID).Scan(&revisionBefore))
			_, err = activityExec(f.Store, `DELETE FROM applied_migrations WHERE name = 'activity_projection_triggers_v5'`)
			require.NoError(err)
			require.NoError(f.Store.InitSchema())
			queued, err := f.Store.LoadQueuedActivityCandidatesContext(t.Context(), 10)
			require.NoError(err)
			if tc.unsent {
				require.Len(queued, 1)
				assert.Equal(messageID, queued[0].MessageID)
				assert.False(queued[0].Eligible)
				assert.Greater(queued[0].Queue.Revision, revisionBefore)
				projection := activityProjectionFromCandidate(t, queued[0], personID, newerAt)
				projection.Event = nil
				_, err = f.Store.ProjectActivityBatchContext(t.Context(), []store.ActivityProjection{projection})
				require.NoError(err)
			} else {
				assert.Empty(queued)
				var revisionAfter int64
				require.NoError(activityQueryRow(f.Store, `SELECT revision FROM activity_projection_queue WHERE message_id = ?`, messageID).Scan(&revisionAfter))
				assert.Equal(revisionBefore, revisionAfter)
			}
			state, err := f.Store.ContactStateContext(t.Context(), personID, newerAt)
			require.NoError(err)
			require.NotNil(state.LastContactAt)
			if tc.unsent {
				assert.Equal(olderAt, state.LastContactAt.UTC())
				assert.Equal(int64(1), state.InteractionCount)
			} else {
				assert.Equal(newerAt, state.LastContactAt.UTC())
				assert.Equal(int64(2), state.InteractionCount)
			}
		})
	}
}

// The helper initializes an archive through the combined message and attachment
// trigger migration, then this test proves that activity triggers are installed.
func TestPostgresActivityTriggersInstallAfterCombinedTriggerMigration(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") &&
		!strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("PostgreSQL trigger migration regression")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newActivityQueryPostgresStores(t, dbURL)

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
	//nolint:gosec // The query is a fixed test literal; Rebind only changes placeholders.
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT revision
		FROM activity_projection_queue
		WHERE message_id = ?
	`), messageID).Scan(&revision)
	require.NoError(err,
		"activity trigger migration must install queue triggers on an upgraded archive")
	assert.Equal(int64(1), revision)
}
