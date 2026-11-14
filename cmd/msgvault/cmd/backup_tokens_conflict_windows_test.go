//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/carddav"
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

	nested := filepath.Join(cfg.TokensDir(), "carddav-connections", "example")
	credential := carddav.Credential{Password: "synthetic-password", BaseURL: "https://carddav.example.test", Username: "person@example.test", ConnectionGeneration: 1}
	require.NoError(carddav.SaveCredential(nested, credential))

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
	var before, rootBefore *windows.SECURITY_DESCRIPTOR
	if compliant {
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
	err = runBackupTokenRestore(t, cfg, repo, target, true)
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
		data, readErr := os.ReadFile(omitted)
		require.NoError(readErr)
		assert.Equal("omitted", string(data))
		after, readErr := windows.GetNamedSecurityInfo(omitted, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		assert.Equal(before.String(), after.String())
		restoredCredential, readErr := carddav.LoadCredential(nestedTarget)
		require.NoError(readErr)
		assert.Equal(credential, restoredCredential)
	} else {
		require.ErrorContains(err, "was restored to")
		require.ErrorContains(err, "permissions conflict")
		manifest, readErr := repo.LatestSnapshot()
		require.NoError(readErr)
		other := filepath.Join(tokens, "other-snapshot.json")
		require.NoError(os.WriteFile(filepath.Join(cfg.TokensDir(), "other-snapshot.json"), []byte("other"), 0o600))
		extras, readErr := backupExtrasSpec(cfg)
		require.NoError(readErr)
		_, readErr = backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir, Extras: extras, IncludeTokens: true, AllowPlaintextSecrets: true})
		require.NoError(readErr)
		require.NoError(os.WriteFile(other, []byte("other"), 0o600))
		otherAccess, readErr := windows.ACLFromEntries(entries[:1], nil)
		require.NoError(readErr)
		require.NoError(windows.SetNamedSecurityInfo(other, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, otherAccess, nil))
		otherACL, readErr := windows.GetNamedSecurityInfo(other, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		attachment := filepath.Join(target, "attachments", "unrelated")
		require.NoError(os.MkdirAll(filepath.Dir(attachment), 0o700))
		require.NoError(os.WriteFile(attachment, []byte("attachment"), 0o600))
		attachmentInfo, readErr := os.Stat(attachment)
		require.NoError(readErr)
		db := filepath.Join(target, "msgvault.db")
		before, readErr := os.ReadFile(db)
		require.NoError(readErr)
		info, readErr := os.Stat(db)
		require.NoError(readErr)
		require.NoError(os.Rename(omitted, filepath.Join(t.TempDir(), "omitted")))
		rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", manifest.SnapshotID, "--repo", repo.Root(), "--target", target, "--permissions-only"})
		require.NoError(executeRootContext(t.Context(), rootCmd))
		after, readErr := os.ReadFile(db)
		require.NoError(readErr)
		afterInfo, readErr := os.Stat(db)
		require.NoError(readErr)
		assert.Equal(before, after)
		assert.Equal(info.ModTime(), afterInfo.ModTime())
		attachmentBytes, readErr := os.ReadFile(attachment)
		require.NoError(readErr)
		afterAttachment, readErr := os.Stat(attachment)
		require.NoError(readErr)
		assert.Equal("attachment", string(attachmentBytes))
		assert.Equal(attachmentInfo.ModTime(), afterAttachment.ModTime())
		otherAfter, readErr := windows.GetNamedSecurityInfo(other, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(readErr)
		assert.Equal(otherACL.String(), otherAfter.String())
		restoredCredential, readErr := carddav.LoadCredential(nestedTarget)
		require.NoError(readErr)
		assert.Equal(credential, restoredCredential)
		snapshot, readErr := providercredentials.Read(tokens)
		require.NoError(readErr)
		value, _, readErr := snapshot.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
		require.NoError(readErr)
		assert.Equal(providerKey, value)
		key, readErr := providercredentials.ReadSecretFile(filepath.Join(tokens, providercredentials.ServerKeyFilename))
		require.NoError(readErr)
		assert.Equal(serverKey, key)
	}
	assert.FileExists(filepath.Join(target, "msgvault.db"))
}
