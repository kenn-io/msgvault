package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
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
	conversationID, err := st.EnsureConversation(source.ID, "baseline", "Baseline")
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
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP
		WHERE id IN (?, ?)
	`), reconciledID, labeledID)
	requirements.NoError(err)
	labelID, err := st.EnsureLabel(source.ID, "Legacy", "Legacy", "user")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageLabels(labeledID, []int64{labelID}))

	affected := make(map[int64]struct{})
	requirements.NoError(st.withTx(func(tx *loggedTx) error {
		return captureUntrackedIMAPMessageIDs(tx, source.ID, true, affected)
	}))
	got := make([]int64, 0, len(affected))
	for messageID := range affected {
		got = append(got, messageID)
	}
	slices.Sort(got)
	assertions.Equal([]int64{liveID, labeledID}, got)
	assertions.NotContains(got, reconciledID)

	var deletedAt sql.NullTime
	requirements.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), reconciledID).Scan(&deletedAt))
	assertions.True(deletedAt.Valid)
}

func TestUntrackedIMAPMessagesUsesStampIndexSQLite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "imap-capture-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+untrackedIMAPMessagesQuery(true, true), 1, "2026-01-01 00:00:00.000")
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "SEARCH messages USING INDEX idx_messages_content_changed_at")
	assertions.NotContains(plan, "idx_messages_deleted")
}

func TestIMAPMembershipGenerationSeekUsesPrimaryKeySQLite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "imap-generation-plan.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	requirements.NoError(st.InitSchema())
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+imapMembershipGenerationSeekQuery(), 1, "INBOX", 1)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		requirements.NoError(rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	requirements.NoError(rows.Err())
	plan := strings.Join(details, "\n")
	assertions.Contains(plan, "SEARCH imap_message_memberships USING COVERING INDEX sqlite_autoindex_imap_message_memberships_1")
	assertions.NotContains(plan, "SCAN imap_message_memberships")
	assertions.NotContains(plan, "USE TEMP B-TREE")
}
