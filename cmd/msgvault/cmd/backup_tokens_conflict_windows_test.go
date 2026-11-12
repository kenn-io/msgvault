//go:build windows

package cmd

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/sys/windows"
)

func TestBackupRestorePermissionConflictAfterPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: t.TempDir()}
	cfg.Data.DataDir = dataDir
	dbPath := filepath.Join(dataDir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	require.NoError(st.Close())
	attachments := filepath.Join(dataDir, "attachments")
	require.NoError(os.MkdirAll(attachments, 0o700))

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(cfg.TokensDir(), ".provider-credentials.lock")))

	nested := filepath.Join(cfg.TokensDir(), "carddav-connections", "example", "carddav.json")
	require.NoError(os.MkdirAll(filepath.Dir(nested), 0o700))
	require.NoError(os.WriteFile(nested, []byte("synthetic-carddav"), 0o600))

	oldInclude, oldConfig, oldPlain := backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets
	backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = true, false, true
	t.Cleanup(func() {
		backupCreateIncludeTokens, backupCreateIncludeConfig, backupCreateAllowPlaintextSecrets = oldInclude, oldConfig, oldPlain
	})
	extras, err := backupExtrasSpec(cfg)
	require.NoError(err)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	_, err = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: dataDir,
		Extras: extras, IncludeTokens: true, AllowPlaintextSecrets: true,
	})
	require.NoError(err)

	oldRepo, oldTarget, oldOverwrite := backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite
	oldForce, oldJobs := backupRestoreForceUnlock, backupRestoreJobs
	oldLoose, oldIntegrity := backupRestoreLooseAttachments, backupRestoreIntegrityCheck
	oldOut, oldErr, oldLogger := rootCmd.OutOrStdout(), rootCmd.ErrOrStderr(), slog.Default()
	changed := make(map[string]bool)
	for _, name := range []string{"repo", "target", "jobs", "overwrite"} {
		changed[name] = backupRestoreCmd.Flags().Lookup(name).Changed
	}
	t.Cleanup(func() {
		backupRestoreRepo, backupRestoreTarget, backupRestoreOverwrite = oldRepo, oldTarget, oldOverwrite
		backupRestoreForceUnlock, backupRestoreJobs = oldForce, oldJobs
		backupRestoreLooseAttachments, backupRestoreIntegrityCheck = oldLoose, oldIntegrity
		for name, wasChanged := range changed {
			backupRestoreCmd.Flags().Lookup(name).Changed = wasChanged
		}
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.SetArgs(nil)
		slog.SetDefault(oldLogger)
	})
	target := filepath.Join(t.TempDir(), "restored")
	tokens := filepath.Join(target, "tokens")
	nestedTarget := filepath.Join(tokens, "carddav-connections", "example")
	require.NoError(os.MkdirAll(nestedTarget, 0o700))
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS,
		Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
	}}, nil)
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(nestedTarget, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	omitted := filepath.Join(nestedTarget, "omitted")
	require.NoError(os.WriteFile(omitted, []byte("omitted"), 0o600))
	before, err := windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1", "--overwrite"})
	err = executeRootContext(t.Context(), rootCmd)
	require.ErrorContains(err, "was restored to")
	require.ErrorContains(err, "permissions conflict")
	assert.FileExists(filepath.Join(target, "msgvault.db"))
	data, err := os.ReadFile(omitted)
	require.NoError(err)
	assert.Equal("omitted", string(data))
	after, err := windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	assert.Equal(before.String(), after.String())
	data, err = os.ReadFile(filepath.Join(nestedTarget, "carddav.json"))
	require.NoError(err)
	assert.Equal("synthetic-carddav", string(data))
}
