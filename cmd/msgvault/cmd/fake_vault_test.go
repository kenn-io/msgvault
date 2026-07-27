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
		"--messages", "40",
		"--participants", "17",
		"--participant-edges", "96",
		"--attachment-bytes", "0",
		"--seed", "1",
		"--quiet",
	})
	requirementsForTest.NoError(command.ExecuteContext(t.Context()))
	assertionsForTest.Contains(stdout.String(), "Messages: 40")

	db, err := sql.Open("sqlite3", filepath.Join(output, "msgvault.db")+"?mode=ro")
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var messages, participants, recipients int64
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM messages").Scan(&messages))
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM participants").Scan(&participants))
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM message_recipients").Scan(&recipients))
	assertionsForTest.Equal(int64(40), messages)
	assertionsForTest.Equal(int64(17), participants)
	assertionsForTest.Equal(int64(56), recipients)
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
