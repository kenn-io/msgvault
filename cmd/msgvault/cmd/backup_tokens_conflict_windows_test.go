//go:build windows

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/sys/windows"
)

func TestBackupRestorePermissionConflictAfterPublication(t *testing.T) {
	backupRestoreOmittedOAuthLock(t, false)
}

func TestBackupRestoreKeepsCompliantDirectoryAndOmittedOAuthLock(t *testing.T) {
	backupRestoreOmittedOAuthLock(t, true)
}

func backupRestoreOmittedOAuthLock(t *testing.T, compliant bool) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	serverKey, err := providercredentials.EnsureServerKey(cfg.TokensDir())
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(cfg.TokensDir(), ".provider-credentials.lock")))

	nested := filepath.Join(cfg.TokensDir(), "carddav-connections", "example", "carddav.json")
	require.NoError(os.MkdirAll(filepath.Dir(nested), 0o700))
	require.NoError(os.WriteFile(nested, []byte("synthetic-carddav"), 0o600))

	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)

	prepareBackupTokenRestore(t)

	target := filepath.Join(t.TempDir(), "restored")
	tokens := filepath.Join(target, "tokens")
	nestedTarget := filepath.Join(tokens, "carddav-connections", "example")
	require.NoError(os.MkdirAll(nestedTarget, 0o700))
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS,
		Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
	}}
	if !compliant {
		everyone, err := windows.StringToSid("S-1-1-0")
		require.NoError(err)
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_READ, AccessMode: windows.SET_ACCESS, Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)}})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	omitted := filepath.Join(tokens, "oauth-token.json.lock")
	require.NoError(os.WriteFile(omitted, []byte("omitted"), 0o600))
	before, err := windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	dacl, _, err := before.DACL()
	require.NoError(err)
	require.NotNil(dacl)
	inherited := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		require.NoError(windows.GetAce(dacl, i, &ace))
		inherited = inherited || ace.Header.AceFlags&windows.INHERITED_ACE != 0
	}
	require.True(inherited)
	rootBefore, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	backupRestoreOverwrite, backupRestoreForceUnlock = false, false
	backupRestoreLooseAttachments, backupRestoreIntegrityCheck = false, false
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", "--repo", repo.Root(), "--target", target, "--jobs", "1", "--overwrite"})
	err = executeRootContext(t.Context(), rootCmd)
	if compliant {
		require.NoError(err)
		snapshot, readErr := providercredentials.Read(tokens)
		require.NoError(readErr)
		value, _, readErr := snapshot.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
		require.NoError(readErr)
		assert.Equal(providerKey, value)
		key, readErr := providercredentials.ReadSecretFile(filepath.Join(tokens, providercredentials.ServerKeyFilename))
		require.NoError(readErr)
		assert.Equal(serverKey, key)
	} else {
		require.ErrorContains(err, "was restored to")
		require.ErrorContains(err, "permissions conflict")
	}
	rootAfter, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	assert.Equal(rootBefore.String(), rootAfter.String())
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
