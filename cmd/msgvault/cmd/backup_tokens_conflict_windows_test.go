//go:build windows

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/sys/windows"
)

func TestBackupRestorePermissionConflictAfterPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)
	manifest, err := repo.LatestSnapshot()
	require.NoError(err)
	prepareBackupTokenRestore(t)

	target := filepath.Join(t.TempDir(), "restored")
	tokens := filepath.Join(target, "tokens")
	require.NoError(os.MkdirAll(tokens, 0o700))
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(err)
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: fileutil.FileAllAccess, AccessMode: windows.SET_ACCESS,
		Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid)},
	}}
	everyone, err := windows.StringToSid("S-1-1-0")
	require.NoError(err)
	entries = append(entries, windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ, AccessMode: windows.SET_ACCESS,
		Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE,
		Trustee:     windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)},
	})
	acl, err := windows.ACLFromEntries(entries, nil)
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	omitted := filepath.Join(tokens, "oauth-token.json.lock")
	require.NoError(os.WriteFile(omitted, []byte("omitted"), 0o600))
	output, err := runBackupTokenRestore(t, cfg, repo, manifest.SnapshotID, "--target", target, "--overwrite")
	require.Error(err)
	assert.NotContains(err.Error(), "was restored to")
	require.ErrorContains(err, "permissions conflict")
	assert.Contains(output, fmt.Sprintf("Restored snapshot %s to %s", manifest.SnapshotID, target))
	assert.Contains(err.Error(), "--permissions-only")

	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("# config"), 0o600))
	configSnapshot, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: configPath, RecordAs: "config.toml"}}}, IncludeConfig: true,
	})
	require.NoError(err)
	deny, err := windows.SecurityDescriptorFromString("D:P")
	require.NoError(err)
	emptyACL, _, err := deny.DACL()
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, emptyACL, nil))
	t.Cleanup(func() {
		assert.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	})
	locked, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	_, err = os.ReadDir(tokens)
	require.Error(err)
	output, err = runBackupTokenRestore(t, cfg, repo, configSnapshot.SnapshotID, "--target", target, "--permissions-only")
	require.NoError(err)
	assert.Contains(output, "secured token files: 0")
	after, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	require.NoError(err)
	assert.Equal(locked.String(), after.String())
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))

	require.NoError(os.Rename(omitted, filepath.Join(t.TempDir(), "omitted")))
	output, err = runBackupTokenRestore(t, cfg, repo, manifest.SnapshotID, "--target", target, "--permissions-only")
	require.NoError(err)
	assert.Contains(output, "secured token files: 2")
	restored, err := providercredentials.Read(tokens)
	require.NoError(err)
	value, _, err := restored.Resolve(providercredentials.VectorEmbeddingsID, endpoint, "", nil)
	require.NoError(err)
	assert.Equal(providerKey, value)
}
