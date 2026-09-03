package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// TestRunRepairListIDsLocalDryRunApplyAndNoop catches a repair command that
// mutates without --apply, omits an operator-useful summary, or advances the
// cache revision for an already-current archive. The fixture uses the archive's
// real zlib MIME rows rather than a repair stub.
func TestRunRepairListIDsLocalDryRunApplyAndNoop(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	messageIDs := newListIDRepairArchive(t)
	_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	const pendingMigration = "person_sweep_change_triggers_v5"
	markListIDRepairMigrationPending(t, pendingMigration)
	assert.False(listIDRepairMigrationApplied(t, pendingMigration))
	beforeRevision := listIDRepairArchiveRevision(t)
	beforeCacheState, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	require.NoError(err)
	assert.Equal(beforeRevision, beforeCacheState.DerivedDataRevision)

	var dryRunOut bytes.Buffer
	dryRunCmd := &cobra.Command{}
	dryRunCmd.SetContext(context.Background())
	dryRunCmd.SetOut(&dryRunOut)
	require.NoError(runRepairListIDsLocal(dryRunCmd, false))
	assert.Equal(
		"List-Id repair dry run: scanned=3 found=1 changed=2 undecodable=1\n"+
			"Dry run: no rows were modified. Re-run with --apply to write repairs.\n",
		dryRunOut.String())
	assert.Equal(sql.NullString{}, listIDRepairArchiveValue(t, messageIDs.missing))
	assert.Equal(
		sql.NullString{String: "<stale.example.test>", Valid: true},
		listIDRepairArchiveValue(t, messageIDs.stale))
	assert.Equal(beforeRevision, listIDRepairArchiveRevision(t))
	assert.False(listIDRepairMigrationApplied(t, pendingMigration),
		"dry run must not initialize schema or apply pending migrations")

	var applyOut bytes.Buffer
	applyCmd := &cobra.Command{}
	applyCmd.SetContext(context.Background())
	applyCmd.SetOut(&applyOut)
	require.NoError(runRepairListIDsLocal(applyCmd, true))
	assert.Equal(
		"List-Id repair applied: scanned=3 found=1 changed=2 undecodable=1\n",
		applyOut.String())
	assert.Equal(
		sql.NullString{String: "<announce.example.test>", Valid: true},
		listIDRepairArchiveValue(t, messageIDs.missing))
	assert.Equal(sql.NullString{}, listIDRepairArchiveValue(t, messageIDs.stale))
	assert.Equal(beforeRevision+1, listIDRepairArchiveRevision(t))
	afterCacheState, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	require.NoError(err)
	assert.Equal(beforeRevision+1, afterCacheState.DerivedDataRevision)

	var noChangeOut bytes.Buffer
	noChangeCmd := &cobra.Command{}
	noChangeCmd.SetContext(context.Background())
	noChangeCmd.SetOut(&noChangeOut)
	require.NoError(runRepairListIDsLocal(noChangeCmd, true))
	assert.Equal(
		"List-Id repair applied: scanned=3 found=1 changed=0 undecodable=1\n",
		noChangeOut.String())
	assert.Equal(beforeRevision+1, listIDRepairArchiveRevision(t))
}

func TestRunRepairListIDsLocalApplySurfacesCacheRefreshFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	messageIDs := newListIDRepairArchive(t)
	require.NoError(os.WriteFile(cfg.AnalyticsDir(), []byte("not a directory"), 0o600))

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	err := runRepairListIDsLocal(cmd, true)
	require.Error(err)
	require.ErrorContains(err, "refresh analytics cache")
	assert.Equal(
		sql.NullString{String: "<announce.example.test>", Valid: true},
		listIDRepairArchiveValue(t, messageIDs.missing))
}

func markListIDRepairMigrationPending(t *testing.T, migration string) {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`), migration)
	require.NoError(t, err)
	require.NoError(t, st.Close())
}

func listIDRepairMigrationApplied(t *testing.T, migration string) bool {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	applied, err := st.IsMigrationApplied(migration)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	return applied
}

// TestRepairListIDsCommandRoutesThroughDaemonCLIRunner catches bypassing the
// daemon-owned writer path from a normal CLI parent process.
func TestRepairListIDsCommandRoutesThroughDaemonCLIRunner(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"repair-list-ids", "--apply"}, req.Args)
		},
		`{"type":"stdout","data":"List-Id repair applied: scanned=1 found=1 changed=1 undecodable=0\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	cmd := newRepairListIDsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--apply"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "List-Id repair applied: scanned=1 found=1 changed=1 undecodable=0\n", stdout.String())
}

// TestRepairListIDsCommandHelpAndFlagValidation executes Cobra's public
// command surface, catching a command that is not registered or accepts an
// accidental mutation flag.
func TestRepairListIDsCommandHelpAndFlagValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := newTestRootCmd()
	root.AddCommand(newRepairListIDsCmd())
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetArgs([]string{"repair-list-ids", "--help"})
	require.NoError(root.Execute())
	assert.Contains(help.String(), "msgvault repair-list-ids [--apply]")
	assert.Contains(help.String(), "--apply")

	root = newTestRootCmd()
	root.AddCommand(newRepairListIDsCmd())
	root.SetArgs([]string{"repair-list-ids", "--force"})
	err := root.Execute()
	require.Error(err)
	assert.ErrorContains(err, "unknown flag: --force")
}

type listIDRepairArchiveMessageIDs struct {
	missing int64
	stale   int64
}

func newListIDRepairArchive(t *testing.T) listIDRepairArchiveMessageIDs {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("gmail", "archive@example.test")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "list-id-repair-thread", "List-Id repair")
	require.NoError(t, err)

	newMessage := func(sourceMessageID string, listID sql.NullString) int64 {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     "email",
			ListID:          listID,
		})
		require.NoError(t, err)
		return messageID
	}

	missing := newMessage("list-id-missing", sql.NullString{})
	require.NoError(t, st.UpsertMessageRaw(missing,
		[]byte("List-Id: Announcements <announce.example.test>\r\n\r\nbody")))
	stale := newMessage("list-id-stale", sql.NullString{String: "<stale.example.test>", Valid: true})
	require.NoError(t, st.UpsertMessageRaw(stale, []byte("Subject: no list\r\n\r\nbody")))
	undecodable := newMessage("list-id-undecodable", sql.NullString{})
	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		VALUES (?, ?, 'mime', 'zlib')`), undecodable, []byte("not a zlib stream"))
	require.NoError(t, err)
	require.NoError(t, st.Close())

	return listIDRepairArchiveMessageIDs{missing: missing, stale: stale}
}

func listIDRepairArchiveValue(t *testing.T, messageID int64) sql.NullString {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	var value sql.NullString
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT list_id FROM messages WHERE id = ?`), messageID).Scan(&value))
	return value
}

func listIDRepairArchiveRevision(t *testing.T) int64 {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	revision, err := st.DerivedDataRevision()
	require.NoError(t, err)
	return revision
}
