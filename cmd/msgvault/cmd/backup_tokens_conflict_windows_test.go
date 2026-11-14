//go:build windows

package cmd

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/providercredentials"
	"golang.org/x/sys/windows"
)

func TestBackupRestoreConflictNoTokensAndPermissionRetry(t *testing.T) {
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

	target := filepath.Join(t.TempDir(), "restored&archive") + string(os.PathSeparator)
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
	assert.Contains(err.Error(), "selected token files were secured, but token directory permissions were not changed")
	assert.Contains(output, fmt.Sprintf("Restored snapshot %s to %s", manifest.SnapshotID, target))
	_, hint, ok := strings.Cut(err.Error(), "retry in PowerShell with: ")
	require.True(ok)
	syntheticTarget := `C:\a&b`
	require.Contains(hint, escapeRestoreArg(target))
	hint = strings.Replace(hint, escapeRestoreArg(target), escapeRestoreArg(syntheticTarget), 1)
	powershell := exec.CommandContext(t.Context(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `$tokens = $null; $errors = $null; $ast = [System.Management.Automation.Language.Parser]::ParseInput($env:MSGVAULT_RETRY_HINT, [ref]$tokens, [ref]$errors); $commands = @($ast.FindAll({ param($node) $node -is [System.Management.Automation.Language.CommandAst] }, $true)); if ($errors.Count -ne 0 -or $commands.Count -ne 1) { exit 1 }; $values = @($commands[0].CommandElements | ForEach-Object { if ($_ -is [System.Management.Automation.Language.StringConstantExpressionAst]) { $_.Value } else { $_.Extent.Text } }); ConvertTo-Json -InputObject $values -Compress`)
	powershell.Env = append(os.Environ(), "MSGVAULT_RETRY_HINT="+hint)
	parsed, parseErr := powershell.CombinedOutput()
	assert.NoError(parseErr, "%s", parsed)
	var args []string
	if assert.NoError(json.Unmarshal(parsed, &args), "%s", parsed) {
		assert.Equal([]string{"msgvault", "backup", "restore", manifest.SnapshotID, "--repo", repo.Root(), "--target", syntheticTarget, "--permissions-only"}, args)
	}

	configPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(configPath, []byte("# config"), 0o600))
	configSnapshot, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		Extras: backup.ExtrasSpec{Files: []backup.ExtrasFileSpec{{Path: configPath, RecordAs: "config.toml"}}}, IncludeConfig: true,
	})
	require.NoError(err)
	emptySnapshot, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
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
	for _, snapshot := range []string{configSnapshot.SnapshotID, emptySnapshot.SnapshotID} {
		output, err = runBackupTokenRestore(t, cfg, repo, snapshot, "--target", target, "--permissions-only")
		require.NoError(err)
		assert.Contains(output, "secured token files: 0")
		after, err := windows.GetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		require.NoError(err)
		assert.Equal(locked.String(), after.String())
	}
	require.NoError(windows.SetNamedSecurityInfo(tokens, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil))

	require.NoError(os.Rename(omitted, filepath.Join(t.TempDir(), "omitted")))
	output, err = runBackupTokenRestore(t, cfg, repo, manifest.SnapshotID, "--target", target, "--permissions-only")
	require.NoError(err)
	assert.Contains(output, "secured token files: 2")
	_, err = providercredentials.Read(tokens)
	require.NoError(err)
}

func TestPermissionRepairHonorsSnapshotAndLock(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, dbPath, attachments := backupTokenFixture(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	emptyManifest, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
	})
	require.NoError(err)
	manifest, err := backup.Create(t.Context(), repo, backupapp.New("test"), backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachments, DataDir: cfg.Data.DataDir,
		IncludeTokens: true, AllowPlaintextSecrets: true,
	})
	require.NoError(err)
	target := t.TempDir()
	prepareBackupTokenRestore(t)
	lock, err := repo.AcquireExclusiveLock("create", false)
	require.NoError(err)
	count, err := secureRestoredTokens(t.Context(), repo, emptyManifest.SnapshotID, target, false)
	require.NoError(err)
	assert.Zero(count)
	assert.FileExists(repo.Path("locks", "exclusive.json"))
	_, err = secureRestoredTokens(t.Context(), repo, manifest.SnapshotID, target, false)
	require.ErrorIs(err, backup.ErrRepoLocked)
	assert.FileExists(repo.Path("locks", "exclusive.json"))
	require.NoError(lock.Release())
	lock, err = repo.AcquireExclusiveLock("create", false)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(lock.Release()) })
	_, err = runBackupTokenRestore(t, cfg, repo, manifest.SnapshotID, "--target", target, "--permissions-only", "--force-unlock")
	require.NoError(err)
}
