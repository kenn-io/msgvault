//go:build windows

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/fileutil"
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
	if compliant {
		targetEmpty, readErr := providercredentials.Read(tokens)
		require.NoError(readErr)
		_, readErr = providercredentials.Put(tokens, targetEmpty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
		require.NoError(readErr)
		require.NoError(os.Remove(filepath.Join(tokens, ".provider-credentials.lock")))
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileutil.FileAllAccess, AccessMode: windows.SET_ACCESS,
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
	if compliant {
		_, err := providercredentials.Read(tokens)
		require.NoError(err)
	}
	omitted := filepath.Join(tokens, "oauth-token.json.lock")
	require.NoError(os.WriteFile(omitted, []byte("omitted"), 0o600))
	var before, rootBefore, deniedBefore *windows.SECURITY_DESCRIPTOR
	var deniedHandle windows.Handle
	if compliant {
		denied := filepath.Join(tokens, "unrelated")
		require.NoError(os.Mkdir(denied, 0o700))
		deniedPath, pathErr := windows.UTF16PtrFromString(denied)
		require.NoError(pathErr)
		deniedHandle, err = windows.CreateFile(deniedPath, windows.READ_CONTROL|windows.WRITE_DAC,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		require.NoError(err)
		t.Cleanup(func() {
			assert.NoError(windows.SetSecurityInfo(deniedHandle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
			assert.NoError(windows.CloseHandle(deniedHandle))
		})
		denyEntries := []windows.EXPLICIT_ACCESS{
			{AccessPermissions: windows.FILE_LIST_DIRECTORY, AccessMode: windows.DENY_ACCESS, Trustee: entries[0].Trustee},
			{AccessPermissions: fileutil.FileAllAccess, AccessMode: windows.GRANT_ACCESS, Trustee: entries[0].Trustee},
		}
		denyACL, aclErr := windows.ACLFromEntries(denyEntries, nil)
		require.NoError(aclErr)
		require.NoError(windows.SetSecurityInfo(deniedHandle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, denyACL, nil))
		_, readErr := os.ReadDir(denied)
		require.ErrorIs(readErr, windows.ERROR_ACCESS_DENIED)
		deniedBefore, err = windows.GetSecurityInfo(deniedHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(err)
		before, err = windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(err)
		dacl, _, err := before.DACL()
		require.NoError(err)
		require.NotNil(dacl)
		inherited := false
		for i := range uint32(dacl.AceCount) {
			var ace *windows.ACCESS_ALLOWED_ACE
			require.NoError(windows.GetAce(dacl, i, &ace))
			inherited = inherited || ace.Header.AceFlags&windows.INHERITED_ACE != 0
		}
		require.True(inherited)
		rootBefore, err = windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(err)
	}
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
		rootAfter, readErr := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		assert.Equal(rootBefore.String(), rootAfter.String())
		deniedAfter, readErr := windows.GetSecurityInfo(deniedHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		assert.Equal(deniedBefore.String(), deniedAfter.String())
		data, readErr := os.ReadFile(omitted)
		require.NoError(readErr)
		assert.Equal("omitted", string(data))
		after, readErr := windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		assert.Equal(before.String(), after.String())
		data, readErr = os.ReadFile(filepath.Join(nestedTarget, "carddav.json"))
		require.NoError(readErr)
		assert.Equal("synthetic-carddav", string(data))
	} else {
		require.ErrorContains(err, "was restored to")
		require.ErrorContains(err, "permissions conflict")
	}
	assert.FileExists(filepath.Join(target, "msgvault.db"))
}
