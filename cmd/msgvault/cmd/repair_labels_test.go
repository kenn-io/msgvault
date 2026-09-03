package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// TestRunRepairLabelsLocalDryRunApplyAndNoop catches a repair command that
// mutates without --apply, omits an operator-useful summary, or advances the
// cache revision for an already-current archive. The fixture reproduces the
// issue #748 gap directly: an add-only label merge leaves a label no
// imap_message_memberships row backs.
func TestRunRepairLabelsLocalDryRunApplyAndNoop(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	messageID := newLabelRepairArchive(t, "labels@example.test")
	_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	beforeRevision := labelRepairArchiveRevision(t)
	beforeCacheState, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	require.NoError(err)
	assert.Equal(beforeRevision, beforeCacheState.DerivedDataRevision)

	var dryRunOut bytes.Buffer
	dryRunCmd := &cobra.Command{}
	dryRunCmd.SetContext(context.Background())
	dryRunCmd.SetOut(&dryRunOut)
	require.NoError(runRepairLabelsLocal(dryRunCmd, "", false))
	assert.Equal(
		"  labels@example.test: scanned=2 changed=1\n"+
			"Label repair dry run: scanned=2 changed=1\n"+
			"Dry run: no rows were modified. Re-run with --apply to write repairs.\n",
		dryRunOut.String())
	assert.Equal([]string{"INBOX", "Stray"}, labelRepairArchiveLabels(t, messageID))
	assert.Equal(beforeRevision, labelRepairArchiveRevision(t))

	var applyOut bytes.Buffer
	applyCmd := &cobra.Command{}
	applyCmd.SetContext(context.Background())
	applyCmd.SetOut(&applyOut)
	require.NoError(runRepairLabelsLocal(applyCmd, "", true))
	assert.Equal(
		"  labels@example.test: scanned=2 changed=1\n"+
			"Label repair applied: scanned=2 changed=1\n",
		applyOut.String())
	assert.Equal([]string{"INBOX"}, labelRepairArchiveLabels(t, messageID))
	assert.Equal(beforeRevision+1, labelRepairArchiveRevision(t))
	afterCacheState, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	require.NoError(err)
	assert.Equal(beforeRevision+1, afterCacheState.DerivedDataRevision)

	var noChangeOut bytes.Buffer
	noChangeCmd := &cobra.Command{}
	noChangeCmd.SetContext(context.Background())
	noChangeCmd.SetOut(&noChangeOut)
	require.NoError(runRepairLabelsLocal(noChangeCmd, "", true))
	assert.Equal(
		"  labels@example.test: scanned=2 changed=0\n"+
			"Label repair applied: scanned=2 changed=0\n",
		noChangeOut.String())
	assert.Equal(beforeRevision+1, labelRepairArchiveRevision(t))
}

// TestRunRepairLabelsLocalIdentifierScopesToOneSource catches a repair
// command that ignores the optional identifier and touches every source.
func TestRunRepairLabelsLocalIdentifierScopesToOneSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	newLabelRepairArchive(t, "one@example.test")

	var out bytes.Buffer
	repairCmd := &cobra.Command{}
	repairCmd.SetContext(context.Background())
	repairCmd.SetOut(&out)
	require.NoError(runRepairLabelsLocal(repairCmd, "someone-else@example.test", true))
	assert.Equal("Label repair applied: scanned=0 changed=0\n", out.String())
}

// TestRepairLabelsCommandRoutesThroughDaemonCLIRunner catches bypassing the
// daemon-owned writer path from a normal CLI parent process.
func TestRepairLabelsCommandRoutesThroughDaemonCLIRunner(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"repair-labels", "--apply"}, req.Args)
		},
		`{"type":"stdout","data":"Label repair applied: scanned=1 changed=1\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	cmd := newRepairLabelsCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{"--apply"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "Label repair applied: scanned=1 changed=1\n", stdout.String())
}

// TestRepairLabelsCommandHelpAndFlagValidation executes Cobra's public
// command surface, catching a command that is not registered or accepts an
// accidental mutation flag.
func TestRepairLabelsCommandHelpAndFlagValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := newTestRootCmd()
	root.AddCommand(newRepairLabelsCmd())
	var help bytes.Buffer
	root.SetOut(&help)
	root.SetArgs([]string{"repair-labels", "--help"})
	require.NoError(root.Execute())
	assert.Contains(help.String(), "msgvault repair-labels [identifier] [--apply]")
	assert.Contains(help.String(), "--apply")

	root = newTestRootCmd()
	root.AddCommand(newRepairLabelsCmd())
	root.SetArgs([]string{"repair-labels", "--force"})
	err := root.Execute()
	require.Error(err)
	assert.ErrorContains(err, "unknown flag: --force")
}

// newLabelRepairArchive seeds one IMAP source with two messages in INBOX,
// then reproduces the issue #748 gap: an add-only label merge on the first
// message leaves a "Stray" label no membership row backs. Returns that
// message's ID.
func newLabelRepairArchive(t *testing.T, identifier string) int64 {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("imap", identifier)
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(source.ID, "label-repair-thread", "Label repair")
	require.NoError(t, err)

	newMessage := func(sourceMessageID string) int64 {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     "email",
		})
		require.NoError(t, err)
		return messageID
	}
	messageID1 := newMessage("label-repair-1")
	newMessage("label-repair-2")

	require.NoError(t, st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 3, HighestModSeq: 1},
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 1, UID: 1, SourceMessageID: "label-repair-1"},
			{Mailbox: "INBOX", UIDValidity: 1, UID: 2, SourceMessageID: "label-repair-2"},
		},
	}}))

	strayLabelID, err := st.EnsureLabel(source.ID, "stray-source-label", "Stray", "user")
	require.NoError(t, err)
	_, err = st.ReconcileMessageLabels(messageID1, []int64{strayLabelID}, false)
	require.NoError(t, err)

	require.NoError(t, st.Close())
	return messageID1
}

func labelRepairArchiveLabels(t *testing.T, messageID int64) []string {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	rows, err := st.DB().Query(st.Rebind(`
		SELECT l.name
		FROM labels l
		JOIN message_labels ml ON ml.label_id = l.id
		WHERE ml.message_id = ?
		ORDER BY l.name
	`), messageID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var labels []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		labels = append(labels, name)
	}
	require.NoError(t, rows.Err())
	return labels
}

func labelRepairArchiveRevision(t *testing.T) int64 {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	revision, err := st.DerivedDataRevision()
	require.NoError(t, err)
	return revision
}
