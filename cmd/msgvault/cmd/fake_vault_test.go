package cmd

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFakeVaultCommandBuildsExactRelationshipScale(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)
	output := t.TempDir()
	command := newFakeVaultCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stdout)
	command.SetArgs([]string{
		"--output", output,
		"--messages", "120",
		"--participants", "17",
		"--participant-edges", "240",
		"--group-chat-members", "8",
		"--attachment-bytes", "0",
		"--seed", "1",
		"--quiet",
	})
	requirementsForTest.NoError(command.ExecuteContext(t.Context()))
	assertionsForTest.Contains(stdout.String(), "Messages: 120")

	db, err := sql.Open("sqlite3", filepath.Join(output, "msgvault.db")+"?mode=ro")
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var messages, participants, recipients, conversationMembers, groupMembers int64
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM participants").Scan(&participants))
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM message_recipients").Scan(&recipients))
	requirementsForTest.NoError(db.QueryRow(
		"SELECT count(*) FROM conversation_participants",
	).Scan(&conversationMembers))
	requirementsForTest.NoError(db.QueryRow(
		"SELECT participant_count FROM conversations WHERE conversation_type = 'group_chat'",
	).Scan(&groupMembers))
	assertionsForTest.Equal(int64(120), messages)
	assertionsForTest.Equal(int64(17), participants)
	assertionsForTest.Equal(int64(120), recipients)
	assertionsForTest.Equal(int64(12), conversationMembers)
	assertionsForTest.Equal(int64(8), groupMembers)
}

func TestFakeVaultCommandRejectsTooFewParticipantEdges(t *testing.T) {
	command := newFakeVaultCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{
		"--output", t.TempDir(),
		"--messages", "40",
		"--participants", "17",
		"--participant-edges", "39",
		"--attachment-bytes", "0",
		"--quiet",
	})
	err := command.ExecuteContext(t.Context())
	require.ErrorContains(t, err,
		"participant edge count must be at least the message count")
}
