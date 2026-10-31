package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXRestoreRefusesMissingCurrentRemovalRecord(t *testing.T) {
	_testRequire := require.New(t)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	_, err = st.PersistXPostContext(t.Context(), store.XPersistPostInput{
		SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002",
		ConversationID: "100", EditHistory: []string{"100"},
		Text: "synthetic aurora payload", Raw: []byte(`{"post":{"id":"100"}}`),
		CreatedAt: now, FetchedAt: now, Availability: "public",
		Author: &store.XAuthorInput{SourceID: source.ID, UserID: "1002", Username: "example_author", DisplayName: "Example Author", FetchedAt: now},
	})
	_testRequire.NoError(err)
	attachments := filepath.Join(dataDir, "attachments")
	_testRequire.NoError(os.MkdirAll(attachments, 0o700))
	repoPath := filepath.Join(t.TempDir(), "repository")
	repo, err := backup.Init(repoPath)
	_testRequire.NoError(err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: dataDir,
	})
	_testRequire.NoError(err)

	savedRepo, savedTarget := backupRestoreRepo, backupRestoreTarget
	savedOverwrite, savedForce := backupRestoreOverwrite, backupRestoreForceUnlock
	savedJobs, savedLoose, savedIntegrity := backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck
	t.Cleanup(func() {
		backupRestoreRepo, backupRestoreTarget = savedRepo, savedTarget
		backupRestoreOverwrite, backupRestoreForceUnlock = savedOverwrite, savedForce
		backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck = savedJobs, savedLoose, savedIntegrity
	})
	backupRestoreRepo = repoPath
	backupRestoreTarget = filepath.Join(t.TempDir(), "isolated")
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreJobs, backupRestoreLooseAttachments, backupRestoreIntegrityCheck = 1, false, true
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = filepath.Join(t.TempDir(), "live")
	command := &cobra.Command{Use: "restore"}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	var output bytes.Buffer
	command.SetOut(&output)
	err = runBackupRestore(command, nil)
	_testRequire.Error(err, "an isolated X snapshot cannot prove the current removal set by itself")
	assert.NoFileExists(t, filepath.Join(backupRestoreTarget, "msgvault.db"))
}

func TestXRestoreIdentitySurvivesReconnectButNotSourceRecreation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	stageXRecord(t, f.cfg, xTestRecord("old-access"))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	first, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(first.InstanceID, 64)
	stageXRecord(t, f.cfg, xTestRecord("new-access"))
	_, err = f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	reconnected, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(first.InstanceID, reconnected.InstanceID)
	_testAssert.Greater(reconnected.Generation, first.Generation)
	_testRequire.NoError(f.st.RemoveSource(source.ID))
	stageXRecord(t, f.cfg, xTestRecord("latest-access"))
	_, err = f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	recreated, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	current, err := f.st.LoadXControlContext(t.Context(), recreated.ID)
	_testRequire.NoError(err)
	_testRequire.Len(current.InstanceID, 64)
	_testAssert.NotEqual(first.InstanceID, current.InstanceID)
	_testAssert.Equal(int32(3), f.meCalls.Load(), "incarnation binding adds no provider reads to verified registration")
}
