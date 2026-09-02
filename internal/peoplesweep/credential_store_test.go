//go:build linux || darwin

package peoplesweep_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"golang.org/x/sys/unix"
)

const credentialCanary = "test-credential-canary"

type credentialPathSnapshot struct {
	exists        bool
	info          os.FileInfo
	entries       []string
	contentDigest [sha256.Size]byte
}

func snapshotCredentialPaths(t *testing.T, paths ...string) map[string]credentialPathSnapshot {
	t.Helper()
	snapshot := make(map[string]credentialPathSnapshot, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshot[path] = credentialPathSnapshot{}
			continue
		}
		require.NoError(t, err)
		state := credentialPathSnapshot{exists: true, info: info}
		if info.IsDir() {
			entries, readErr := os.ReadDir(path)
			require.NoError(t, readErr)
			state.entries = make([]string, 0, len(entries))
			for _, entry := range entries {
				state.entries = append(state.entries, entry.Name())
			}
		} else if info.Mode().IsRegular() {
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			state.contentDigest = sha256.Sum256(contents)
		}
		snapshot[path] = state
	}
	return snapshot
}

func assertCredentialPathsUnchanged(
	t *testing.T,
	before map[string]credentialPathSnapshot,
) {
	t.Helper()
	for path, want := range before {
		gotInfo, err := os.Lstat(path)
		if !want.exists {
			require.ErrorIs(t, err, os.ErrNotExist, path)
			continue
		}
		require.NoError(t, err, path)
		assert.True(t, os.SameFile(want.info, gotInfo), "%s inode changed", path)
		assert.Equal(t, want.info.Mode(), gotInfo.Mode(), "%s mode changed", path)
		assert.Equal(t, want.info.Size(), gotInfo.Size(), "%s size changed", path)
		assert.Equal(t, want.info.ModTime(), gotInfo.ModTime(), "%s mtime changed", path)
		if want.info.IsDir() {
			entries, readErr := os.ReadDir(path)
			require.NoError(t, readErr, path)
			gotEntries := make([]string, 0, len(entries))
			for _, entry := range entries {
				gotEntries = append(gotEntries, entry.Name())
			}
			assert.Equal(t, want.entries, gotEntries, "%s entries changed", path)
		} else if want.info.Mode().IsRegular() {
			contents, readErr := os.ReadFile(path)
			require.NoError(t, readErr, path)
			assert.Equal(t, want.contentDigest, sha256.Sum256(contents), "%s contents changed", path)
		}
	}
}

func validateCredentialDeletePreflight(store peoplesweep.CredentialStore) error {
	guard, err := store.PreflightDelete("profile")
	if err != nil {
		return err
	}
	return guard.Close()
}

func guardedCredentialDelete(
	store peoplesweep.CredentialStore,
	profileName string,
) error {
	guard, err := store.PreflightDelete(profileName)
	if err != nil {
		return err
	}
	return errors.Join(store.Delete(profileName, guard), guard.Close())
}

func createExistingCredentialPreflightFixture(t *testing.T, tokensDir string) []string {
	t.Helper()
	root := filepath.Join(tokensDir, "people-providers")
	lockPath := filepath.Join(root, ".credentials.lock")
	credentialPath := filepath.Join(root, "profile.json")
	require.NoError(t, os.Chmod(tokensDir, 0o700))
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.WriteFile(credentialPath, []byte(credentialCanary), 0o600))
	return []string{tokensDir, root, lockPath, credentialPath}
}

func TestValidateProviderProfileNameUsesOneSafeGrammar(t *testing.T) {
	for _, name := range []string{"a", "Alpha_1", "profile.with-dots", strings.Repeat("z", 64)} {
		require.NoError(t, peoplesweep.ValidateProviderProfileName(name), name)
	}
	for _, name := range []string{"", "--help", "--json", " leading", "trailing ", "bad\nname", strings.Repeat("z", 65)} {
		err := peoplesweep.ValidateProviderProfileName(name)
		require.Error(t, err, name)
		if name != "" {
			assert.NotContains(t, err.Error(), name, "unsafe input must not be reflected")
		}
	}
}

func TestCredentialNeverFormatsSecret(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	credential := peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)
	formatted := fmt.Sprintf("%v %#v %s %q %x %p", credential, credential, credential, credential, credential, credential)
	assert.NotContains(formatted, credentialCanary)

	var captured bytes.Buffer
	logger := log.New(&captured, "", 0)
	logger.Printf("credential=%v", credential)
	assert.NotContains(captured.String(), credentialCanary)

	profile := credentialTestProfile(t, peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer)
	profileJSON, err := json.Marshal(profile)
	require.NoError(err)
	assert.NotContains(string(profileJSON), credentialCanary)

	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	err = store.Save("../invalid", credential)
	require.Error(err)
	assert.NotContains(err.Error(), credentialCanary)
}

func TestCredentialStoreLifecycleUsesPrivateFilesAndExactDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("alpha", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)))
	require.NoError(store.Save("alpha.backup", peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, credentialCanary)))

	root := filepath.Join(tokensDir, "people-providers")
	rootInfo, err := os.Stat(root)
	require.NoError(err)
	assert.Equal(os.FileMode(0o700), rootInfo.Mode().Perm())
	for _, name := range []string{".credentials.lock", "alpha.json", "alpha.backup.json"} {
		info, statErr := os.Stat(filepath.Join(root, name))
		require.NoError(statErr)
		assert.Equal(os.FileMode(0o600), info.Mode().Perm(), name)
	}

	loaded, err := store.Load("alpha")
	require.NoError(err)
	assert.Equal(peoplesweep.AuthBearer, loaded.Scheme)
	assert.Equal(credentialCanary, loaded.Value(), "loaded credential differs")

	require.NoError(guardedCredentialDelete(store, "alpha"))
	_, err = store.Load("alpha")
	require.Error(err)
	remaining, err := store.Load("alpha.backup")
	require.NoError(err)
	assert.Equal(peoplesweep.AuthXAPIKey, remaining.Scheme)
	assert.Equal(credentialCanary, remaining.Value(), "remaining credential differs")
	require.ErrorIs(guardedCredentialDelete(store, "alpha"), peoplesweep.ErrCredentialNotFound)
}

func TestCredentialStoreDeleteRetiresOnlyExactPinnedTargetAsBoundedTombstone(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("profile", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, credentialCanary,
	)))
	require.NoError(store.Save("other", peoplesweep.NewCredential(
		peoplesweep.AuthXAPIKey, credentialCanary+"-other",
	)))
	guard, err := store.PreflightDelete("profile")
	require.NoError(err)
	t.Cleanup(func() {
		require := newRequire(t)
		require.NoError(guard.Close())
	})

	root := filepath.Join(tokensDir, "people-providers")
	lockPath := filepath.Join(root, ".credentials.lock")
	targetPath := filepath.Join(root, "profile.json")
	otherPath := filepath.Join(root, "other.json")
	targetBefore, err := os.Stat(targetPath)
	require.NoError(err)
	stableBefore := snapshotCredentialPaths(t, tokensDir, root, lockPath, otherPath)

	require.NoError(store.Delete("profile", guard))

	targetAfter, err := os.Lstat(targetPath)
	require.NoError(err)
	assert.True(os.SameFile(targetBefore, targetAfter), "deletion replaced the exact target inode")
	assert.True(targetAfter.Mode().IsRegular())
	assert.Equal(os.FileMode(0o600), targetAfter.Mode().Perm())
	assert.Zero(targetAfter.Size())
	var targetStat unix.Stat_t
	require.NoError(unix.Lstat(targetPath, &targetStat))
	assert.EqualValues(1, targetStat.Nlink)
	tombstone, err := os.ReadFile(targetPath)
	require.NoError(err)
	assert.Empty(tombstone, "credential tombstone retained bytes")
	assertCredentialPathsUnchanged(t, stableBefore)
	require.NoError(guard.Close())
	require.ErrorIs(validateCredentialDeletePreflight(store), peoplesweep.ErrCredentialNotFound)
}

// TestCredentialStoreDeleteConsumesGuardWhenProfileNameIsInvalid also pins the
// guard contract that caused a CI deadlock: PreflightDelete holds the
// credential namespace flock until Close, and flock is bound to the open file
// description, so ordinary store operations from the same process block on
// an unclosed guard. The post-Close load therefore must run bounded so a
// future Close that leaks the lock fails this test quickly instead of
// hanging the package until the go test timeout fires.
func TestCredentialStoreDeleteConsumesGuardWhenProfileNameIsInvalid(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(store.Save("profile", peoplesweep.NewCredential(
		peoplesweep.AuthBearer, credentialCanary,
	)))
	guard, err := store.PreflightDelete("profile")
	require.NoError(err)
	t.Cleanup(func() {
		require := newRequire(t)
		require.NoError(guard.Close())
	})

	err = store.Delete("../invalid", guard)
	require.ErrorContains(err, "invalid people provider credential profile name")
	err = store.Delete("profile", guard)
	require.ErrorContains(err, "already consumed")
	require.NoError(guard.Close())

	loaded := make(chan error, 1)
	go func() {
		_, loadErr := store.Load("profile")
		loaded <- loadErr
	}()
	select {
	case loadErr := <-loaded:
		require.NoError(loadErr)
	case <-time.After(10 * time.Second):
		require.Fail("guard Close must release the credential namespace lock before ordinary operations")
	}
	credential, err := store.Load("profile")
	require.NoError(err)
	assert.Equal(credentialCanary, credential.Value())
}

func TestCredentialStorePreflightDeleteValidatesWithoutReadingOrChangingSecret(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	paths := createExistingCredentialPreflightFixture(t, tokensDir)
	credentialPath := filepath.Join(tokensDir, "people-providers", "profile.json")
	beforeContents, err := os.ReadFile(credentialPath)
	require.NoError(err)
	before := snapshotCredentialPaths(t, paths...)

	err = validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
	require.NoError(err)
	afterContents, err := os.ReadFile(credentialPath)
	require.NoError(err)
	assert.Equal(beforeContents, afterContents)
	assertCredentialPathsUnchanged(t, before)
}

func TestCredentialStorePreflightDeleteRejectsMissingStateWithoutCreatingIt(t *testing.T) {
	t.Run("tokens root", func(t *testing.T) {
		parent := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		before := snapshotCredentialPaths(t, parent, tokensDir)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		before := snapshotCredentialPaths(t, tokensDir, root)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock marker", func(t *testing.T) {
		require := require.New(t)
		tokensDir := t.TempDir()
		require.NoError(os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		credentialPath := filepath.Join(root, "profile.json")
		require.NoError(os.Mkdir(root, 0o700))
		require.NoError(os.WriteFile(credentialPath, []byte(credentialCanary), 0o600))
		paths := []string{tokensDir, root, filepath.Join(root, ".credentials.lock"), credentialPath}
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential target", func(t *testing.T) {
		require := require.New(t)
		tokensDir := t.TempDir()
		require.NoError(os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		credentialPath := filepath.Join(root, "profile.json")
		require.NoError(os.Mkdir(root, 0o700))
		require.NoError(os.WriteFile(filepath.Join(root, ".credentials.lock"), nil, 0o600))
		paths := []string{tokensDir, root, filepath.Join(root, ".credentials.lock"), credentialPath}
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential tombstone", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		require.NoError(t, os.Truncate(paths[len(paths)-1], 0))
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorIs(t, err, peoplesweep.ErrCredentialNotFound)
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStoreDeleteRejectsMissingStateAfterPreflightWithoutRecreatingIt(t *testing.T) {
	t.Run("tokens root", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		parent := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(os.Mkdir(tokensDir, 0o700))
		createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		retained := tokensDir + ".retained"
		require.NoError(os.Rename(tokensDir, retained))
		before := snapshotCredentialPaths(t,
			parent,
			tokensDir,
			retained,
			filepath.Join(retained, "people-providers"),
			filepath.Join(retained, "people-providers", ".credentials.lock"),
			filepath.Join(retained, "people-providers", "profile.json"),
		)

		err = store.Delete("profile", guard)
		require.Error(err)
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		root := paths[1]
		retained := root + ".retained"
		require.NoError(os.Rename(root, retained))
		before := snapshotCredentialPaths(t,
			tokensDir,
			root,
			retained,
			filepath.Join(retained, ".credentials.lock"),
			filepath.Join(retained, "profile.json"),
		)

		err = store.Delete("profile", guard)
		require.Error(err)
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock marker", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		lockPath := paths[2]
		retained := lockPath + ".retained"
		require.NoError(os.Rename(lockPath, retained))
		before := snapshotCredentialPaths(t, paths[0], paths[1], lockPath, retained, paths[3])

		err = store.Delete("profile", guard)
		require.Error(err)
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential target", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		credentialPath := paths[3]
		retained := credentialPath + ".retained"
		require.NoError(os.Rename(credentialPath, retained))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], credentialPath, retained)

		err = store.Delete("profile", guard)
		require.ErrorContains(err, "credential changed")
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential tombstone", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		require.NoError(os.Truncate(paths[3], 0))
		before := snapshotCredentialPaths(t, paths...)

		err = store.Delete("profile", guard)
		require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStorePreflightDeleteRejectsWrongModesWithoutRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		mode  os.FileMode
	}{
		{name: "tokens root", index: 0, mode: 0o750},
		{name: "credential namespace", index: 1, mode: 0o750},
		{name: "lock marker", index: 2, mode: 0o640},
		{name: "credential target", index: 3, mode: 0o640},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokensDir := t.TempDir()
			paths := createExistingCredentialPreflightFixture(t, tokensDir)
			require.NoError(t, os.Chmod(paths[test.index], test.mode))
			before := snapshotCredentialPaths(t, paths...)

			err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
			require.ErrorContains(t, err, "permissions")
			if err != nil {
				assert.NotContains(t, err.Error(), credentialCanary)
			}
			assertCredentialPathsUnchanged(t, before)
		})
	}
}

func TestCredentialStoreDeleteRejectsWrongModesAfterPreflightWithoutRepair(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		mode  os.FileMode
	}{
		{name: "tokens root", index: 0, mode: 0o750},
		{name: "credential namespace", index: 1, mode: 0o750},
		{name: "lock marker", index: 2, mode: 0o640},
		{name: "credential target", index: 3, mode: 0o640},
	} {
		t.Run(test.name, func(t *testing.T) {
			newRequire := require.New
			assert := assert.New(t)
			require := require.New(t)
			tokensDir := t.TempDir()
			paths := createExistingCredentialPreflightFixture(t, tokensDir)
			store := peoplesweep.NewFileCredentialStore(tokensDir)
			guard, err := store.PreflightDelete("profile")
			require.NoError(err)
			t.Cleanup(func() {
				require := newRequire(t)
				require.NoError(guard.Close())
			})

			require.NoError(os.Chmod(paths[test.index], test.mode))
			before := snapshotCredentialPaths(t, paths...)

			err = store.Delete("profile", guard)
			require.ErrorContains(err, "changed")
			if err != nil {
				assert.NotContains(err.Error(), credentialCanary)
			}
			assertCredentialPathsUnchanged(t, before)
		})
	}
}

func TestCredentialStoreDeleteRejectsUnsafeTargetAfterPreflightWithoutChangingIt(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		credentialPath := paths[3]
		retained := credentialPath + ".retained"
		external := filepath.Join(t.TempDir(), "external")
		require.NoError(os.Rename(credentialPath, retained))
		require.NoError(os.WriteFile(external, []byte("external-must-remain"), 0o600))
		require.NoError(os.Symlink(external, credentialPath))
		before := snapshotCredentialPaths(t,
			paths[0], paths[1], paths[2], credentialPath, retained, external,
		)

		err = store.Delete("profile", guard)
		require.ErrorContains(err, "changed")
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("hard link", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		linkPath := filepath.Join(t.TempDir(), "linked-credential")
		require.NoError(os.Link(paths[3], linkPath))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], paths[3], linkPath)

		err = store.Delete("profile", guard)
		require.ErrorContains(err, "changed")
		if err != nil {
			assert.NotContains(err.Error(), credentialCanary)
		}
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("FIFO", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		store := peoplesweep.NewFileCredentialStore(tokensDir)
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		retained := paths[3] + ".retained"
		require.NoError(os.Rename(paths[3], retained))
		require.NoError(unix.Mkfifo(paths[3], 0o600))
		before := snapshotCredentialPaths(t, paths[0], paths[1], paths[2], paths[3], retained)

		result := make(chan error, 1)
		go func() {
			result <- store.Delete("profile", guard)
		}()
		select {
		case err := <-result:
			require.ErrorContains(err, "changed")
			if err != nil {
				assert.NotContains(err.Error(), credentialCanary)
			}
		case <-time.After(time.Second):
			assert.Fail("credential deletion blocked while inspecting a FIFO")
		}
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStorePreflightDeleteRejectsUnsafeObjectsWithoutChangingThem(t *testing.T) {
	t.Run("tokens directory symlink", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(t, os.Symlink(target, tokensDir))
		before := snapshotCredentialPaths(t, parent, target, tokensDir)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential namespace symlink", func(t *testing.T) {
		tokensDir := t.TempDir()
		require.NoError(t, os.Chmod(tokensDir, 0o700))
		target := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(t, os.Symlink(target, root))
		before := snapshotCredentialPaths(t, tokensDir, target, root)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(t, err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("lock symlink", func(t *testing.T) {
		require := require.New(t)
		tokensDir := t.TempDir()
		require.NoError(os.Chmod(tokensDir, 0o700))
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(os.Mkdir(root, 0o700))
		external := filepath.Join(t.TempDir(), "external-lock")
		require.NoError(os.WriteFile(external, nil, 0o600))
		lockPath := filepath.Join(root, ".credentials.lock")
		require.NoError(os.Symlink(external, lockPath))
		before := snapshotCredentialPaths(t, tokensDir, root, lockPath, external)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(err)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential symlink", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		require.NoError(os.Remove(credentialPath))
		external := filepath.Join(t.TempDir(), "external-credential")
		require.NoError(os.WriteFile(external, []byte(credentialCanary), 0o600))
		require.NoError(os.Symlink(external, credentialPath))
		paths = append(paths, external)
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.Error(err)
		assert.NotContains(err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential hard link", func(t *testing.T) {
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		linkPath := filepath.Join(t.TempDir(), "credential-link")
		require.NoError(t, os.Link(credentialPath, linkPath))
		paths = append(paths, linkPath)
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorContains(t, err, "links")
		assert.NotContains(t, err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})

	t.Run("credential directory", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		paths := createExistingCredentialPreflightFixture(t, tokensDir)
		credentialPath := paths[len(paths)-1]
		require.NoError(os.Remove(credentialPath))
		require.NoError(os.Mkdir(credentialPath, 0o700))
		before := snapshotCredentialPaths(t, paths...)

		err := validateCredentialDeletePreflight(peoplesweep.NewFileCredentialStore(tokensDir))
		require.ErrorContains(err, "not a regular file")
		assert.NotContains(err.Error(), credentialCanary)
		assertCredentialPathsUnchanged(t, before)
	})
}

func TestCredentialStoreRotatesAtomically(t *testing.T) {
	require := require.New(t)
	tokensDir := t.TempDir()
	store := peoplesweep.NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Go(func() {
		for range 200 {
			credential, err := store.Load("rotate")
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			value := credential.Value()
			if value != credentialCanary+"-old" && value != credentialCanary+"-new" {
				select {
				case errCh <- errors.New("reader observed a partial credential"):
				default:
				}
				return
			}
		}
	})
	for range 100 {
		require.NoError(store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-new")))
		require.NoError(store.Save("rotate", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary+"-old")))
	}
	wg.Wait()
	select {
	case err := <-errCh:
		require.NoError(err)
	default:
	}
}

func TestCredentialStoreSerializesConcurrentSaves(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	const profiles = 16
	var wg sync.WaitGroup
	errors := make(chan error, profiles)
	for index := range profiles {
		wg.Go(func() {
			name := fmt.Sprintf("profile-%02d", index)
			errors <- store.Save(name, peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary))
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(err)
	}
	for index := range profiles {
		credential, err := store.Load(fmt.Sprintf("profile-%02d", index))
		require.NoError(err)
		assert.Equal(peoplesweep.AuthBearer, credential.Scheme)
		assert.Equal(credentialCanary, credential.Value(), "loaded credential differs")
	}
}

func TestCredentialStoreSaveNewNeverOverwritesConcurrentWinner(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	start := make(chan struct{})
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	for _, value := range []string{credentialCanary + "-first", credentialCanary + "-second"} {
		go func() {
			<-start
			guard, created, err := store.SaveNew("new-profile", peoplesweep.NewCredential(
				peoplesweep.AuthBearer, value,
			))
			if guard != nil {
				err = errors.Join(err, guard.Close())
			}
			results <- result{created: created, err: err}
		}()
	}
	close(start)
	created := 0
	for range 2 {
		result := <-results
		require.NoError(result.err)
		if result.created {
			created++
		}
	}
	assert.Equal(1, created)
	credential, err := store.Load("new-profile")
	require.NoError(err)
	assert.Contains([]string{credentialCanary + "-first", credentialCanary + "-second"}, credential.Value())
}

func TestCredentialStoreRejectsInvalidNamesAndMalformedJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	for _, name := range []string{"", ".hidden", "../escape", "slash/name", "space name", string(make([]byte, 65))} {
		err := store.Save(name, peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary))
		require.Error(err, name)
		assert.NotContains(err.Error(), credentialCanary)
	}

	root := filepath.Join(t.TempDir(), "people-providers")
	store = peoplesweep.NewFileCredentialStore(filepath.Dir(root))
	require.NoError(os.Mkdir(root, 0o700))
	require.NoError(os.WriteFile(filepath.Join(root, "broken.json"), []byte(`{"scheme":"bearer","value":`), 0o600))
	_, err := store.Load("broken")
	require.ErrorContains(err, "parse")
	assert.NotContains(err.Error(), credentialCanary)

	require.NoError(os.WriteFile(filepath.Join(root, "unknown.json"), []byte(fmt.Sprintf(
		`{"scheme":"bearer","value":"safe-test-value",%q:true}`, credentialCanary)), 0o600))
	_, err = store.Load("unknown")
	require.Error(err)
	if strings.Contains(err.Error(), credentialCanary) {
		assert.Fail("credential parse error disclosed an attacker-controlled field name")
	}
}

func TestCredentialStoreRejectsSymlinkedRootFileAndLock(t *testing.T) {
	credential := peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)

	t.Run("tokens directory", func(t *testing.T) {
		parent := t.TempDir()
		target := t.TempDir()
		tokensDir := filepath.Join(parent, "tokens")
		require.NoError(t, os.Symlink(target, tokensDir))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "directory")
		assert.NotContains(t, err.Error(), credentialCanary)
	})

	t.Run("root", func(t *testing.T) {
		tokensDir := t.TempDir()
		target := t.TempDir()
		require.NoError(t, os.Symlink(target, filepath.Join(tokensDir, "people-providers")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(t, err, "directory")
		assert.NotContains(t, err.Error(), credentialCanary)
	})

	t.Run("credential file", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(os.Mkdir(root, 0o700))
		target := filepath.Join(t.TempDir(), "target")
		require.NoError(os.WriteFile(target, []byte("unchanged"), 0o600))
		require.NoError(os.Symlink(target, filepath.Join(root, "profile.json")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(err, "regular file")
		contents, readErr := os.ReadFile(target)
		require.NoError(readErr)
		assert.Equal("unchanged", string(contents))
		assert.NotContains(err.Error(), credentialCanary)
	})

	t.Run("lock", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		root := filepath.Join(tokensDir, "people-providers")
		require.NoError(os.Mkdir(root, 0o700))
		target := filepath.Join(t.TempDir(), "lock-target")
		require.NoError(os.WriteFile(target, nil, 0o600))
		require.NoError(os.Symlink(target, filepath.Join(root, ".credentials.lock")))
		err := peoplesweep.NewFileCredentialStore(tokensDir).Save("profile", credential)
		require.ErrorContains(err, "lock")
		assert.NotContains(err.Error(), credentialCanary)
	})
}

func TestCredentialResolverUsesStoredEnvironmentAndNoneSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(store.Save("stored-profile", peoplesweep.NewCredential(peoplesweep.AuthXAPIKey, credentialCanary)))
	lookup := func(name string) (string, bool) {
		if name != "TEST_CREDENTIAL" {
			return "", false
		}
		return credentialCanary, true
	}
	resolver := peoplesweep.NewCredentialResolver(store, lookup)

	stored, err := resolver.Resolve("stored-profile", credentialTestProfile(t,
		peoplesweep.CredentialStored, "stored-profile", peoplesweep.AuthXAPIKey))
	require.NoError(err)
	assert.Equal(peoplesweep.AuthXAPIKey, stored.Scheme)
	assert.Equal(credentialCanary, stored.Value(), "stored credential differs")

	environment, err := resolver.Resolve("ignored-profile-name", credentialTestProfile(t,
		peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer))
	require.NoError(err)
	assert.Equal(peoplesweep.AuthBearer, environment.Scheme)
	assert.Equal(credentialCanary, environment.Value(), "environment credential differs")

	none, err := resolver.Resolve("local", credentialTestProfile(t,
		peoplesweep.CredentialNone, "", peoplesweep.AuthNone))
	require.NoError(err)
	assert.Equal(peoplesweep.AuthNone, none.Scheme)
	assert.Empty(none.Value())
}

func TestCredentialResolverFailsClosedWithoutLeakingSecrets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewFileCredentialStore(t.TempDir())
	require.NoError(store.Save("mismatch", peoplesweep.NewCredential(peoplesweep.AuthBearer, credentialCanary)))
	resolver := peoplesweep.NewCredentialResolver(store, func(string) (string, bool) {
		return credentialCanary, false
	})

	_, err := resolver.Resolve("mismatch", credentialTestProfile(t,
		peoplesweep.CredentialStored, "mismatch", peoplesweep.AuthXAPIKey))
	require.ErrorContains(err, "scheme")
	assert.NotContains(err.Error(), credentialCanary)

	_, err = resolver.Resolve("environment", credentialTestProfile(t,
		peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer))
	require.ErrorContains(err, "TEST_CREDENTIAL")
	assert.NotContains(err.Error(), credentialCanary)

	remote := credentialTestProfile(t, peoplesweep.CredentialEnv, "TEST_CREDENTIAL", peoplesweep.AuthBearer)
	remote.Credential = peoplesweep.CredentialNone
	remote.CredentialRef = ""
	remote.Auth = peoplesweep.AuthNone
	_, err = resolver.Resolve("remote", remote)
	require.Error(err)
	assert.NotContains(err.Error(), credentialCanary)
}

func credentialTestProfile(
	t *testing.T,
	source peoplesweep.CredentialSource,
	reference string,
	auth peoplesweep.AuthScheme,
) peoplesweep.ProviderProfile {
	t.Helper()
	config := validConfig()
	mutateActiveProvider(&config, func(provider *peoplesweep.ProviderConfig) {
		provider.Endpoint = "https://provider.example.test/v1"
		provider.Auth = auth
		provider.Credential = source
		provider.CredentialEnv = ""
		if source == peoplesweep.CredentialEnv {
			provider.CredentialEnv = reference
		}
		if source == peoplesweep.CredentialNone {
			provider.Endpoint = "http://127.0.0.1:11434/v1"
		}
	})
	if source == peoplesweep.CredentialStored {
		oldName := config.Provider.Name
		config.Provider.Name = reference
		provider := config.Providers[oldName]
		delete(config.Providers, oldName)
		config.Providers[reference] = provider
	}
	profile, err := config.Profile()
	require.NoError(t, err)
	return profile
}
