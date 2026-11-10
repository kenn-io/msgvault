package cmd

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fakevault/agentdemo"
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
	requirementsForTest.NoError(db.QueryRowContext(t.Context(), "SELECT count(*) FROM participants").Scan(&participants))
	assert.Equal(t, int64(17), participants)
}

func TestFakeVaultAgentDemoWritesManifest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()
	command := newFakeVaultCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--scenario", "agent-demo", "--output", dir, "--quiet"})
	require.NoError(command.ExecuteContext(t.Context()))
	assert.Contains(out.String(), "Agent demo archive:")
	assert.Contains(out.String(), "Manifest:")
	assert.Contains(out.String(), "unsaved participants: 11")
	_, err := agentdemo.Load(dir)
	require.NoError(err)
}
func TestFakeVaultAgentDemoRejectsScaleFlags(t *testing.T) {
	for _, args := range [][]string{{"--messages", "10"}, {"--participants", "3"}, {"--participant-edges", "9"}, {"--group-chat-members", "3"}, {"--attachment-bytes", "1MB"}, {"--append"}} {
		t.Run(args[0], func(t *testing.T) {
			dir := t.TempDir()
			cmd := newFakeVaultCommand()
			cmd.SetArgs(append([]string{"--scenario", "agent-demo", "--output", dir}, args...))
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), args[0])
			assert.NoFileExists(t, filepath.Join(dir, "msgvault.db"))
		})
	}
}
func TestFakeVaultAgentDemoRejectsBadAsOf(t *testing.T) {
	for _, s := range []string{"yesterday", ""} {
		t.Run(s, func(t *testing.T) {
			dir := t.TempDir()
			cmd := newFakeVaultCommand()
			cmd.SetArgs([]string{"--scenario", "agent-demo", "--output", dir, "--as-of", s})
			require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "--as-of")
			assert.NoFileExists(t, filepath.Join(dir, "msgvault.db"))
		})
	}
}
func TestFakeVaultUnknownScenario(t *testing.T) {
	cmd := newFakeVaultCommand()
	cmd.SetArgs([]string{"--scenario", "nope", "--output", t.TempDir()})
	require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "scale, agent-demo")
}
func TestFakeVaultAgentDemoRefusesLiveDataDir(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	inv := newInvocation()
	inv.cfg = cfg
	cmd := newFakeVaultCommand()
	cmd.SetArgs([]string{"--scenario", "agent-demo", "--output", dir})
	require.ErrorContains(t, cmd.ExecuteContext(withInvocation(t.Context(), inv)), "refusing to write a demo archive into an existing archive directory")
	assert.NoFileExists(t, filepath.Join(dir, "msgvault.db"))
}

func TestFakeVaultAgentDemoAsOfNow(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	before := time.Now().UTC().Truncate(time.Hour)
	dir := t.TempDir()
	cmd := newFakeVaultCommand()
	cmd.SetOut(io.Discard)
	cmd.SetArgs([]string{"--scenario", "agent-demo", "--output", dir, "--as-of", "now", "--quiet"})
	require.NoError(cmd.ExecuteContext(t.Context()))
	m, err := agentdemo.Load(dir)
	require.NoError(err)
	after := time.Now().UTC().Truncate(time.Hour)
	assert.True(m.AsOf.Equal(before) || m.AsOf.Equal(after))
	assert.Equal(m.AsOf.Truncate(time.Hour), m.AsOf)
}
func TestFakeVaultAgentDemoRefusesSymlinkedLiveDataDir(t *testing.T) {
	live := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(live, alias))
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = live
	inv := newInvocation()
	inv.cfg = cfg
	cmd := newFakeVaultCommand()
	cmd.SetArgs([]string{"--scenario", "agent-demo", "--output", alias})
	require.ErrorContains(t, cmd.ExecuteContext(withInvocation(t.Context(), inv)), "refusing to write a demo archive into an existing archive directory")
	assert.NoFileExists(t, filepath.Join(live, "msgvault.db"))
}
func TestFakeVaultAgentDemoRefusesExistingDatabase(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "msgvault.db")
	require.NoError(os.WriteFile(path, []byte("keep me"), 0600))
	cmd := newFakeVaultCommand()
	cmd.SetArgs([]string{"--scenario", "agent-demo", "--output", dir})
	require.ErrorContains(cmd.ExecuteContext(t.Context()), "refusing to write a demo archive into an existing archive directory")
	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, "keep me", string(data))
}
