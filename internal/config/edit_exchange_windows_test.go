//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func writeSecureWindowsTestConfig(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o600))
	// os.OpenFile never requests WRITE_DAC, which SetSecurityInfo requires,
	// so open the fixture with an explicit access mask. The deferred close
	// keeps a failed hardening attempt from leaking a handle that would block
	// TempDir cleanup.
	encoded, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(
		encoded,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	require.NoError(t, err)
	file := os.NewFile(uintptr(handle), path)
	defer func() { require.NoError(t, file.Close()) }()
	require.NoError(t, secureConfigCandidate(file, path, 0o600))
}

func TestWindowsConfigReplacementSupportsVerifiedRollback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	candidate := filepath.Join(dir, "candidate.toml")
	writeSecureWindowsTestConfig(t, target, []byte("old"))
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))

	candidateBefore, err := readPhysicalConfigSnapshot(candidate)
	require.NoError(err)
	targetBefore, err := readPhysicalConfigSnapshot(target)
	require.NoError(err)
	replacement, err := beginConfigReplacement(candidate, target, candidateBefore, targetBefore)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(replacement.release()) })
	current, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal([]byte("new"), current)
	displaced, err := os.ReadFile(replacement.displacedPath)
	require.NoError(err)
	assert.Equal([]byte("old"), displaced)

	published, err := readPhysicalConfigSnapshot(target)
	require.NoError(err)
	replacement.published = published
	require.NoError(replacement.rollbackInstalledVersion())
	restored, err := os.ReadFile(target)
	require.NoError(err)
	assert.Equal([]byte("old"), restored)
	rolledBackCandidate, err := os.ReadFile(candidate)
	require.NoError(err)
	assert.Equal([]byte("new"), rolledBackCandidate)
}

func TestWindowsRollbackPreservesLaterWriter(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	candidate := filepath.Join(dir, "candidate.toml")
	writeSecureWindowsTestConfig(t, target, []byte("old"))
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))
	candidateBefore, err := readPhysicalConfigSnapshot(candidate)
	require.NoError(t, err)
	targetBefore, err := readPhysicalConfigSnapshot(target)
	require.NoError(t, err)
	replacement, err := beginConfigReplacement(candidate, target, candidateBefore, targetBefore)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, replacement.release()) })
	published, err := readPhysicalConfigSnapshot(target)
	require.NoError(t, err)
	replacement.published = published

	require.NoError(t, os.Remove(target))
	writeSecureWindowsTestConfig(t, target, []byte("later writer"))
	err = replacement.rollbackInstalledVersion()
	require.ErrorIs(t, err, ErrConfigChanged)
	require.ErrorIs(t, err, ErrConfigConflict)
	assert.Equal(t, []byte("later writer"), mustReadFile(t, target))
	assert.FileExists(t, replacement.displacedPath)
}

func TestWindowsRejectsReparseConfigPath(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	linkDir := filepath.Join(dir, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skip("creating a Windows symlink requires optional privileges")
	}
	_, err := ReadConfigFile(filepath.Join(linkDir, "config.toml"))
	require.ErrorIs(t, err, ErrUnsafeConfigTarget)
}

func TestWindowsRetainedAuthorityBlocksIntermediateRenameAndAllowsReplace(t *testing.T) {
	root := t.TempDir()
	intermediate := filepath.Join(root, "intermediate")
	parent := filepath.Join(intermediate, "config")
	require.NoError(t, os.MkdirAll(parent, 0o700))
	target := filepath.Join(parent, "config.toml")
	candidate := filepath.Join(parent, "candidate.toml")
	writeSecureWindowsTestConfig(t, target, []byte("old"))
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))
	targetBefore, err := readPhysicalConfigSnapshot(target)
	require.NoError(t, err)
	candidateBefore, err := readPhysicalConfigSnapshot(candidate)
	require.NoError(t, err)
	authority, err := pinWindowsConfigParent(target)
	require.NoError(t, err)
	// Release is idempotent; this cleanup only matters when an assertion
	// aborts the test before the explicit release below.
	t.Cleanup(func() { require.NoError(t, authority.Release()) })
	encodedParent, err := windows.UTF16PtrFromString(parent)
	require.NoError(t, err)
	deleter, err := windows.CreateFile(encodedParent, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.Error(t, err, "retained parent handle must deny delete-capable opens")
	assert.Equal(t, windows.InvalidHandle, deleter)
	assert.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)

	err = os.Rename(intermediate, filepath.Join(root, "redirected"))
	require.Error(t, err, "retained intermediate handle must deny rename")
	replacement, err := beginConfigReplacement(candidate, target, candidateBefore, targetBefore)
	require.NoError(t, err, "ReplaceFileW on a child must work while parent handles are retained")
	t.Cleanup(func() { _ = replacement.release() })
	assert.Equal(t, []byte("new"), mustReadFile(t, target))
	require.NotNil(t, replacement.cleanupDisplaced)
	require.NoError(t, replacement.cleanupDisplaced())
	tombstones, err := filepath.Glob(filepath.Join(parent, configRetiredPrefix+"*"))
	require.NoError(t, err)
	require.Len(t, tombstones, 1)
	assert.Equal(t, []byte("old"), mustReadFile(t, tombstones[0]))
	require.NoError(t, replacement.release())
	require.NoError(t, authority.Release())
	require.NoError(t, os.Rename(intermediate, filepath.Join(root, "redirected")),
		"rename must work after every retained handle is released")
}

func TestWindowsRetainedAuthorityAllowsMissingPublication(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.Mkdir(parent, 0o700))
	target := filepath.Join(parent, "config.toml")
	candidate := filepath.Join(parent, "candidate.toml")
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))
	before, err := ReadConfigFile(target)
	require.NoError(t, err)
	authority, err := pinWindowsConfigParent(target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, authority.Release()) })

	retained, err := retainWindowsConfigArtifact(candidate, mustConfigIdentity(t, candidate))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, retained.Close()) })
	publication, err := publishNewConfig(candidate, retained, before)
	require.NoError(t, err, "MoveFileExW on a child must work while parent handles are retained")
	t.Cleanup(func() { _ = publication.release() })
	assert.Equal(t, []byte("new"), mustReadFile(t, target))
	require.NoError(t, publication.release())
	require.NoError(t, authority.Release())
}

func TestWindowsAuthorityRejectsPreexistingDirectoryWriter(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.Mkdir(parent, 0o700))
	encoded, err := windows.UTF16PtrFromString(parent)
	require.NoError(t, err)
	writer, err := windows.CreateFile(encoded, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	require.NoError(t, err)
	defer windows.CloseHandle(writer)

	_, err = pinWindowsConfigParent(filepath.Join(parent, "config.toml"))
	require.Error(t, err)
	assert.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
}

func TestWindowsEditConfigFileFullTransactions(t *testing.T) {
	for _, existing := range []bool{true, false} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if existing {
				writeSecureWindowsTestConfig(t, path, []byte("[web]\ntheme = \"system\"\n"))
			}
			snapshot, err := ReadConfigFile(path)
			require.NoError(t, err)

			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(t, err)
			assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(after.Content))
			if !existing {
				require.NoError(t, verifyConfigOwnerOnly(after.Path))
			}
		})
	}
}

func TestWindowsReadAndEditConfigWithMissingFinalParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "missing", "nested")
	path := filepath.Join(parent, "config.toml")

	snapshot, err := ReadConfigFile(path)
	require.NoError(t, err)
	assert.False(t, snapshot.Exists)
	assert.Empty(t, snapshot.Content)
	assert.Equal(t, configETag(nil), snapshot.ETag)
	assert.Equal(t, path, snapshot.Path)
	assert.NoDirExists(t, parent, "reading must not create missing config directories")

	after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(t, err)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(after.Content))
	assert.FileExists(t, path)
	require.NoError(t, verifyConfigOwnerOnly(path))
}

func TestWindowsMissingParentCreationRejectsAncestorReplacement(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Join(root, "authority")
	require.NoError(t, os.Mkdir(ancestor, 0o700))
	path := filepath.Join(ancestor, "missing", "config.toml")

	snapshot, err := readConfigFileSnapshot(path)
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.parentIdentity)

	displaced := filepath.Join(root, "displaced")
	require.NoError(t, os.Rename(ancestor, displaced))
	require.NoError(t, os.Mkdir(ancestor, 0o700))

	err = ensureConfigParentDirectories(path, snapshot.parentIdentity)
	require.ErrorIs(t, err, ErrConfigConflict)
	assert.NoDirExists(t, filepath.Dir(path))
}

func TestWindowsConfigSaveProducesEditableOwnerOnlyFile(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing legacy inherited DACL"
		}
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "config.toml")
			if existing {
				require.NoError(t, os.WriteFile(path, []byte("[web]\ntheme = \"light\"\n"), 0o600))
			}
			cfg := NewDefaultConfig()
			cfg.HomeDir = home
			cfg.configPath = path
			cfg.Web.Theme = "system"
			require.NoError(t, cfg.Save())
			require.NoError(t, verifyConfigOwnerOnly(path))
			tombstones, globErr := filepath.Glob(filepath.Join(home, configRetiredPrefix+"*"))
			require.NoError(t, globErr)
			if existing {
				require.Len(t, tombstones, 1)
				assert.Equal(t, []byte("[web]\ntheme = \"light\"\n"), mustReadFile(t, tombstones[0]))
			} else {
				assert.Empty(t, tombstones)
			}

			snapshot, err := ReadConfigFile(path)
			require.NoError(t, err)
			after, err := EditConfigFile(path, snapshot.ETag, []Edit{{Key: "web.theme", Value: "dark"}})
			require.NoError(t, err)
			assert.Contains(t, string(after.Content), "theme = \"dark\"")
			require.NoError(t, verifyConfigOwnerOnly(path))
		})
	}
}

func TestWindowsConfigSavePreservesPublishedCandidateWhenRetirementFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	require.NoError(os.WriteFile(path, []byte("[web]\ntheme = \"light\"\n"), 0o600))
	cfg := NewDefaultConfig()
	cfg.HomeDir = home
	cfg.configPath = path
	cfg.Web.Theme = "system"
	displacedSubstitute := []byte("displaced later writer")
	candidateSubstitute := []byte("candidate later writer")
	var candidatePath string
	var displacedPath string
	var movedDisplacedPath string

	err := cfg.saveWithHooks(configSaveHooks{
		beforeExistingRetirement: func(replacement configReplacement) error {
			displacedPath = replacement.displacedPath
			candidatePath = strings.TrimSuffix(displacedPath, ".displaced")
			movedDisplacedPath = displacedPath + ".moved"
			require.NoError(os.Rename(displacedPath, movedDisplacedPath))
			writeSecureWindowsTestConfig(t, displacedPath, displacedSubstitute)
			writeSecureWindowsTestConfig(t, candidatePath, candidateSubstitute)
			return nil
		},
	})
	require.ErrorIs(err, ErrConfigChanged)
	assert.Contains(string(mustReadFile(t, path)), "theme = \"system\"")
	assert.Equal([]byte("[web]\ntheme = \"light\"\n"), mustReadFile(t, movedDisplacedPath))
	assert.Equal(displacedSubstitute, mustReadFile(t, displacedPath))
	assert.Equal(candidateSubstitute, mustReadFile(t, candidatePath))
}

func TestWindowsEditMigratesCurrentUserInheritedDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[web]\ntheme = \"system\"\n"), 0o600))
	world, err := windows.StringToSid("S-1-1-0")
	require.NoError(t, err)
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(world),
		},
	}}, nil)
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, acl, nil))
	after, err := EditConfigFile(path, configETag([]byte("[web]\ntheme = \"system\"\n")), []Edit{{Key: "web.theme", Value: "dark"}})
	require.NoError(t, err)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(after.Content))
	require.NoError(t, verifyConfigOwnerOnly(path))
	content, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "[web]\ntheme = \"dark\"\n", string(content))
}

func TestWindowsSnapshotIdentityDetectsRecreatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := []byte("[web]\ntheme = \"system\"\n")
	writeSecureWindowsTestConfig(t, path, content)
	first, err := ReadConfigFile(path)
	require.NoError(t, err)
	second, err := ReadConfigFile(path)
	require.NoError(t, err)
	assert.Equal(t, first.identity, second.identity)

	require.NoError(t, os.Remove(path))
	writeSecureWindowsTestConfig(t, path, content)
	recreated, err := ReadConfigFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, first.identity, recreated.identity)
}

func TestWindowsReplaceFilePartialFailureRestoresOriginal(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	candidate := filepath.Join(dir, "candidate.toml")
	writeSecureWindowsTestConfig(t, target, []byte("old"))
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))
	targetBefore, err := readPhysicalConfigSnapshot(target)
	require.NoError(t, err)
	candidateBefore, err := readPhysicalConfigSnapshot(candidate)
	require.NoError(t, err)
	backup := candidate + ".displaced"
	require.NoError(t, os.Rename(target, backup))
	replacement := configReplacement{
		displacedPath:     backup,
		preserveCandidate: true,
		recoveryPaths:     []string{target, candidate, backup},
	}

	_, err = reconcileReplaceFileFailure(replacement, targetBefore, candidateBefore,
		windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrConfigChanged)
	old, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("old"), old)
	newCandidate, readErr := os.ReadFile(candidate)
	require.NoError(t, readErr)
	assert.Equal(t, []byte("new"), newCandidate)
}

func TestWindowsReplaceFilePartialFailurePreservesRecoveryArtifacts(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.toml")
	candidate := filepath.Join(dir, "candidate.toml")
	writeSecureWindowsTestConfig(t, target, []byte("old"))
	writeSecureWindowsTestConfig(t, candidate, []byte("new"))
	targetBefore, err := readPhysicalConfigSnapshot(target)
	require.NoError(t, err)
	candidateBefore, err := readPhysicalConfigSnapshot(candidate)
	require.NoError(t, err)
	backup := candidate + ".displaced"
	require.NoError(t, os.Rename(target, backup))
	require.NoError(t, os.Mkdir(target, 0o700))
	replacement := configReplacement{
		displacedPath:     backup,
		preserveCandidate: true,
		recoveryPaths:     []string{target, candidate, backup},
	}

	replacement, err = reconcileReplaceFileFailure(replacement, targetBefore, candidateBefore,
		windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2)
	require.ErrorIs(t, err, ErrConfigChanged)
	assert.True(t, replacement.preserveCandidate)
	assert.FileExists(t, candidate)
	assert.FileExists(t, backup)
}
