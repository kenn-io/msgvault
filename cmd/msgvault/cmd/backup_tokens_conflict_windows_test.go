//go:build windows

package cmd

import (
	"bytes"
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
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)

	const endpoint = "https://provider.example.test/v1"
	const providerKey = "synthetic-provider-key"
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorEmbeddingsID, endpoint, providerKey)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(cfg.TokensDir(), ".provider-credentials.lock")))

	repo := createBackupTokenSnapshot(t, cfg, dbPath, attachments)

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
	entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_READ, AccessMode: windows.SET_ACCESS, Inheritance: windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)}})
	acl, err := windows.ACLFromEntries(entries, nil)
	require.NoError(err)
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	omitted := filepath.Join(tokens, "oauth-token.json.lock")
	require.NoError(os.WriteFile(omitted, []byte("omitted"), 0o600))
	err = runBackupTokenRestore(t, cfg, repo, target, true)
	require.ErrorContains(err, "was restored to")
	require.ErrorContains(err, "permissions conflict")
	manifest, readErr := repo.LatestSnapshot()
	require.NoError(readErr)
	other := filepath.Join(tokens, "other-snapshot.json")
	require.NoError(os.WriteFile(filepath.Join(cfg.TokensDir(), "other-snapshot.json"), []byte("other"), 0o600))
	captureBackupTokenSnapshot(t, repo, cfg, dbPath, attachments)
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
	var output bytes.Buffer
	rootCmd.SetOut(&output)
	rootCmd.SetArgs([]string{"--home", cfg.HomeDir, "--no-log-file", "backup", "restore", manifest.SnapshotID, "--repo", repo.Root(), "--target", target, "--permissions-only"})
	require.NoError(executeRootContext(t.Context(), rootCmd))
	assert.Contains(output.String(), "secured token files: 1")
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
}
