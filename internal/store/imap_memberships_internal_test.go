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
		return captureUntrackedIMAPMessageIDs(tx, source.ID, affected)
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
