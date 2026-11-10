package cmd

import (
	"bytes"
	"database/sql"
	"fmt"
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
	for _, asOf := range []string{"default", "now"} {
		t.Run(asOf, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			before := time.Now().UTC().Truncate(time.Hour)
			dir := t.TempDir()
			command := newFakeVaultCommand()
			var out bytes.Buffer
			command.SetOut(&out)
			args := []string{"--scenario", "agent-demo", "--output", dir, "--quiet"}
			if asOf == "now" {
				args = append(args, "--as-of", "now")
			}
			command.SetArgs(args)
			require.NoError(command.ExecuteContext(t.Context()))
			after := time.Now().UTC().Truncate(time.Hour)
			m, err := agentdemo.Load(dir)
			require.NoError(err)
			assert.Contains(out.String(), "Agent demo archive:")
			assert.Contains(out.String(), "Manifest:")
			// Count membership independently: the CLI reports every unbound participant,
			// while expect.unsaved_participants selects only feature candidates.
			bound := map[int64]bool{}
			for _, person := range m.Persons {
				for _, id := range person.ParticipantIDs {
					bound[id] = true
				}
			}
			for _, handle := range []string{"owner", "owner_phone", "owner_slack"} {
				require.Contains(m.Participants, handle)
				bound[m.Participants[handle].ID] = true
			}
			unsaved := 0
			for _, participant := range m.Participants {
				if !bound[participant.ID] {
					unsaved++
				}
			}
			assert.Contains(out.String(), fmt.Sprintf("unsaved participants: %d", unsaved))
			if asOf == "now" {
				assert.True(m.AsOf.Equal(before) || m.AsOf.Equal(after))
			} else {
				assert.Equal(agentdemo.DefaultAsOf, m.AsOf)
			}
		})
	}
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
func TestFakeVaultAgentDemoRefusesExistingArchive(t *testing.T) {
	for _, name := range []string{"live directory", "symlinked live directory", "existing database"} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			live := t.TempDir()
			output := live
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = live
			switch name {
			case "symlinked live directory":
				output = filepath.Join(t.TempDir(), "alias")
				require.NoError(os.Symlink(live, output))
			case "existing database":
				output = t.TempDir()
				require.NoError(os.WriteFile(filepath.Join(output, "msgvault.db"), []byte("keep me"), 0600))
			}
			inv := newInvocation()
			inv.cfg = cfg
			command := newFakeVaultCommand()
			command.SetArgs([]string{"--scenario", "agent-demo", "--output", output})
			require.ErrorContains(command.ExecuteContext(withInvocation(t.Context(), inv)), "refusing to write a demo archive into an existing archive directory")
			if name != "existing database" {
				assert.NoFileExists(t, filepath.Join(live, "msgvault.db"))
			}
		})
	}
}
