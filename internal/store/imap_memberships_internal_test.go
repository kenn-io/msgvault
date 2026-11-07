package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCaptureInitialIMAPBaselineSkipsReconciledHistory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := Open(filepath.Join(t.TempDir(), "imap-baseline.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("imap", "baseline-internal@example.test")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(t.Context(), source.ID, "baseline", "Baseline")
	requirements.NoError(err)

	createMessage := func(sourceMessageID string) int64 {
		messageID, createErr := st.UpsertMessage(&Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: sourceMessageID, MessageType: "email",
		})
		requirements.NoError(createErr)
		return messageID
	}
	liveID := createMessage("live")
	reconciledID := createMessage("reconciled")
	labeledID := createMessage("labeled")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP
		WHERE id IN (?, ?)
	`), reconciledID, labeledID)
	requirements.NoError(err)
	labelID, err := st.EnsureLabel(t.Context(), source.ID, "Legacy", "Legacy", "user")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(labeledID, []int64{labelID}))

	affected := make(map[int64]struct{})
	requirements.NoError(st.withTx(func(tx *loggedTx) error {
		return st.captureUntrackedIMAPMessageIDs(t.Context(), tx, source.ID, false, affected)
	}))
	got := make([]int64, 0, len(affected))
	for messageID := range affected {
		got = append(got, messageID)
	}
	slices.Sort(got)
	assertions.Equal([]int64{liveID, labeledID}, got)
	assertions.NotContains(got, reconciledID)

	var deletedAt sql.NullTime
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), reconciledID).Scan(&deletedAt))
	assertions.True(deletedAt.Valid)
}

func TestUntrackedIMAPMessagesUsesStampIndexSQLite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "imap-capture-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	plan := explainPlan(t, st, untrackedIMAPMessagesQuery(true, true), 1, "2026-01-01 00:00:00.000")
	assertions.Contains(plan, "SEARCH messages USING INDEX idx_messages_content_changed_at")
	assertions.NotContains(plan, "idx_messages_deleted")
}

func TestIMAPMembershipGenerationSeekUsesPrimaryKeySQLite(t *testing.T) {
	requirements := require.New(t)
	st, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "imap-generation-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	for _, test := range []struct {
		name  string
		query string
		args  []any
	}{
		{"next epoch", imapMembershipGenerationSeekQuery(), []any{1, "INBOX", 1}},
		{"next mailbox", imapMembershipMailboxSeekQuery(), []any{1, "INBOX"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertions := assert.New(t)
			plan := explainPlan(t, st, test.query, test.args...)
			assertions.Contains(plan, "SEARCH imap_message_memberships USING COVERING INDEX sqlite_autoindex_imap_message_memberships_1")
			assertions.NotContains(plan, "SCAN imap_message_memberships")
			assertions.NotContains(plan, "USE TEMP B-TREE")
		})
	}
}

func TestIMAPMembershipMailboxesUsesMessageIndexWithStaleStatsSQLite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "imap-mailboxes-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	seedLiveMessages(t, st, 2000)
	_, err = st.db.Exec(`INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id)
		VALUES (1, 'INBOX', 1, 1, 1)`)
	requirements.NoError(err)
	_, err = st.db.Exec(`ANALYZE imap_message_memberships`)
	requirements.NoError(err)
	// Keep the statistics from the small membership table after a large import.
	_, err = st.db.Exec(`INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id)
		SELECT 1, 'INBOX', 1, id, id FROM messages WHERE id > 1`)
	requirements.NoError(err)
	plan := explainPlan(t, st, imapMembershipMailboxesQuery(true), 1, 2000)
	assertions.Contains(plan, "USING INDEX idx_imap_message_memberships_source_message (source_id=? AND message_id=?)")
	assertions.NotContains(plan, "SCAN imap_message_memberships")
}
