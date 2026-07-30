package cmd

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fakevault generator's shape and validation guarantees are covered in
// internal/fakevault; this test only proves the CLI flags reach the generator.
func TestFakeVaultCommandPassesScaleFlagsToGenerator(t *testing.T) {
	requirementsForTest := require.New(t)
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
	assert.Contains(t, stdout.String(), "Messages: 120")

	db, err := sql.Open("sqlite3", filepath.Join(output, "msgvault.db")+"?mode=ro")
	requirementsForTest.NoError(err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	var participants int64
	requirementsForTest.NoError(db.QueryRow("SELECT count(*) FROM participants").Scan(&participants))
	assert.Equal(t, int64(17), participants)
}
