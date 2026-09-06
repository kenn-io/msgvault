package cmd

import (
	"bytes"
	"context"
	"errors"
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

// TestRunRepairLabelsLocalUnknownIdentifierErrors catches a repair command
// that silently matches no source instead of failing loudly when the given
// identifier does not resolve to one.
func TestRunRepairLabelsLocalUnknownIdentifierErrors(t *testing.T) {
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	newLabelRepairArchive(t, "one@example.test")

	repairCmd := &cobra.Command{}
	repairCmd.SetContext(context.Background())
	repairCmd.SetOut(&bytes.Buffer{})
	err := runRepairLabelsLocal(repairCmd, "someone-else@example.test", true)
	require.Error(err)
	require.ErrorContains(err, `no account found for "someone-else@example.test"`)
}

// TestRunRepairLabelsLocalIdentifierScopesByDisplayName catches matching only
// the raw connection-string identifier. A real IMAP source's Identifier is
// its imaps://user@host:port connection string, not the email a person types
// on the command line — the display name carries that email.
func TestRunRepairLabelsLocalIdentifierScopesByDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	scoped, err := st.GetOrCreateSource("imap", "imaps://scoped@example.test:993")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(scoped.ID, "scoped@example.test"))
	other, err := st.GetOrCreateSource("imap", "imaps://other@example.test:993")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(other.ID, "other@example.test"))
	require.NoError(st.Close())

	var out bytes.Buffer
	repairCmd := &cobra.Command{}
	repairCmd.SetContext(context.Background())
	repairCmd.SetOut(&out)
	require.NoError(runRepairLabelsLocal(repairCmd, "scoped@example.test", true))
	assert.Equal(
		"  imaps://scoped@example.test:993: scanned=0 changed=0\n"+
			"Label repair applied: scanned=0 changed=0\n",
		out.String())
}

// errAfterNWriter succeeds for the first n Write calls and fails every call
// after, without touching whatever already happened before those writes.
type errAfterNWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *errAfterNWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errors.New("simulated output failure")
	}
	w.n--
	// bytes.Buffer.Write never returns a non-nil error.
	n, _ := w.buf.Write(p)
	return n, nil
}

// TestRunRepairLabelsLocalRebuildsCacheDespitePartialFailure catches the
// failure mode roborev flagged on PR #754: each source's repair commits
// independently, but the analytics cache rebuild used to run only after the
// whole multi-source loop returned successfully. A later source's own
// commit still succeeds even when something after it fails (another
// source's repair, an output write, or a cancelled context) — so that
// commit must still reach the cache, not wait on the loop finishing clean.
// This test fails the second source's *output write*, the simplest of those
// three triggers to force deterministically; the important thing it proves
// is that a real, already-committed change is not lost from the cache just
// because the command as a whole reports an error.
func TestRunRepairLabelsLocalRebuildsCacheDespitePartialFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	t.Cleanup(func() { cfg = savedCfg })

	newLabelRepairArchive(t, "one@example.test")
	newLabelRepairArchive(t, "two@example.test")
	_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	beforeRevision := labelRepairArchiveRevision(t)

	// The first source's per-source line is the only Write call allowed to
	// succeed; the second source's own repair still runs and commits before
	// its line fails to print.
	out := &errAfterNWriter{n: 1}
	repairCmd := &cobra.Command{}
	repairCmd.SetContext(context.Background())
	repairCmd.SetOut(out)
	err = runRepairLabelsLocal(repairCmd, "", true)
	require.ErrorContains(err, "write label repair line")

	// Both sources actually committed (the stray label from each is gone),
	// bumping the revision twice, and the cache rebuild ran anyway and
	// caught up to it — despite the command itself returning an error.
	assert.Equal(beforeRevision+2, labelRepairArchiveRevision(t))
	cacheState, err := query.ReadCacheSyncState(cfg.AnalyticsDir())
	require.NoError(err)
	assert.Equal(beforeRevision+2, cacheState.DerivedDataRevision)
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
