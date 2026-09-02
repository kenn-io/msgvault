//go:build linux || darwin

package peoplesweep

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const credentialSecurityTestValue = "credential-security-test-value"

type credentialCleanupPathState struct {
	info os.FileInfo
	data []byte
}

func snapshotCredentialCleanupPaths(
	t *testing.T, paths ...string,
) map[string]credentialCleanupPathState {
	t.Helper()
	result := make(map[string]credentialCleanupPathState, len(paths))
	for _, path := range paths {
		info, err := os.Stat(path)
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		result[path] = credentialCleanupPathState{info: info, data: data}
	}
	return result
}

func assertCredentialCleanupPathsUnchanged(
	t *testing.T, before map[string]credentialCleanupPathState,
) {
	t.Helper()
	for path, want := range before {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.True(t, os.SameFile(want.info, info), "%s inode changed", path)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want.data, data, "%s content changed", path)
	}
}

func TestCredentialDeleteGuardIsOpaqueSingleUseAndBound(t *testing.T) {
	t.Run("opaque", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))

		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		_, err = json.Marshal(guard)
		require.ErrorContains(err, "cannot serialize")
		formatted := fmt.Sprintf("%v %#v %+v %q %x %p", guard, guard, guard, guard, guard, guard)
		assert.NotContains(formatted, credentialSecurityTestValue)
		assert.NotContains(formatted, tokensDir)
		assert.NotContains(formatted, "profile")
	})

	t.Run("closed", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		store := NewFileCredentialStore(t.TempDir())
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		require.NoError(guard.Close())

		err = store.Delete("profile", guard)
		require.ErrorContains(err, "closed")
		credential, loadErr := store.Load("profile")
		require.NoError(loadErr)
		assert.Equal(credentialSecurityTestValue, credential.Value())
	})

	t.Run("different store", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		err = NewFileCredentialStore(tokensDir).Delete("profile", guard)
		require.ErrorContains(err, "different store")
		err = store.Delete("profile", guard)
		require.ErrorContains(err, "already consumed")
		require.NoError(guard.Close())
		credential, loadErr := store.Load("profile")
		require.NoError(loadErr)
		assert.Equal(credentialSecurityTestValue, credential.Value())
	})

	t.Run("different profile", func(t *testing.T) {
		newRequire := require.New
		require := require.New(t)
		store := NewFileCredentialStore(t.TempDir())
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		require.NoError(store.Save("other", NewCredential(AuthBearer, credentialSecurityTestValue+"-other")))
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		err = store.Delete("other", guard)
		require.ErrorContains(err, "different profile")
		err = store.Delete("profile", guard)
		require.ErrorContains(err, "already consumed")
		require.NoError(guard.Close())
		for _, name := range []string{"profile", "other"} {
			_, loadErr := store.Load(name)
			require.NoError(loadErr)
		}
	})

	t.Run("reuse", func(t *testing.T) {
		newRequire := require.New
		require := require.New(t)
		store := NewFileCredentialStore(t.TempDir())
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		require.NoError(store.Delete("profile", guard))
		err = store.Delete("profile", guard)
		require.ErrorContains(err, "already consumed")
		require.NoError(guard.Close())
		_, loadErr := store.Load("profile")
		require.ErrorIs(loadErr, ErrCredentialNotFound)
	})
}

func TestCredentialCleanupGuardIsOpaqueSingleUseAndBound(t *testing.T) {
	t.Run("opaque and closed", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		guard, created, err := store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.NoError(err)
		require.True(created)
		require.NotNil(guard)

		_, err = json.Marshal(guard)
		require.ErrorContains(err, "cannot serialize")
		formatted := fmt.Sprintf("%v %#v %+v %q %x %p", guard, guard, guard, guard, guard, guard)
		assert.NotContains(formatted, credentialSecurityTestValue)
		assert.NotContains(formatted, tokensDir)
		assert.NotContains(formatted, "profile")
		require.NoError(guard.Close())

		err = store.CleanupNew("profile", guard)
		require.ErrorContains(err, "closed")
		credential, loadErr := store.Load("profile")
		require.NoError(loadErr)
		assert.Equal(credentialSecurityTestValue, credential.Value())
	})

	t.Run("different store and reuse", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		guard, created, err := store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.NoError(err)
		require.True(created)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		err = NewFileCredentialStore(tokensDir).CleanupNew("profile", guard)
		require.ErrorContains(err, "different store")
		err = store.CleanupNew("profile", guard)
		require.ErrorContains(err, "already consumed")
		credential, loadErr := store.Load("profile")
		require.NoError(loadErr)
		assert.Equal(credentialSecurityTestValue, credential.Value())
	})

	t.Run("different profile", func(t *testing.T) {
		newRequire := require.New
		require := require.New(t)
		store := NewFileCredentialStore(t.TempDir())
		guard, created, err := store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.NoError(err)
		require.True(created)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		err = store.CleanupNew("other", guard)
		require.ErrorContains(err, "different profile")
		err = store.CleanupNew("profile", guard)
		require.ErrorContains(err, "already consumed")
		_, loadErr := store.Load("profile")
		require.NoError(loadErr)
	})
}

func TestCredentialCleanupGuardRejectsValidIdentityReplacements(t *testing.T) {
	tests := []struct {
		name      string
		wantError string
		replace   func(t *testing.T, tokensDir, replacementTokensDir string) []string
	}{
		{
			name: "tokens root", wantError: "tokens directory changed",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) []string {
				t.Helper()
				retained := tokensDir + "-retained"
				require.NoError(t, os.Rename(tokensDir, retained))
				require.NoError(t, os.Rename(replacementTokensDir, tokensDir))
				return []string{filepath.Join(retained, credentialNamespace, "profile.json"),
					filepath.Join(tokensDir, credentialNamespace, "profile.json")}
			},
		},
		{
			name: "credential namespace", wantError: "credential directory changed",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) []string {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				retained := root + "-retained"
				require.NoError(t, os.Rename(root, retained))
				require.NoError(t, os.Rename(filepath.Join(replacementTokensDir, credentialNamespace), root))
				return []string{filepath.Join(retained, "profile.json"), filepath.Join(root, "profile.json")}
			},
		},
		{
			name: "lock marker", wantError: "credential lock changed",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) []string {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				marker := filepath.Join(root, ".credentials.lock")
				retained := marker + ".retained"
				require.NoError(t, os.Rename(marker, retained))
				require.NoError(t, os.Rename(
					filepath.Join(replacementTokensDir, credentialNamespace, ".credentials.lock"), marker,
				))
				return []string{filepath.Join(root, "profile.json"),
					filepath.Join(replacementTokensDir, credentialNamespace, "profile.json")}
			},
		},
		{
			name: "credential target", wantError: "credential changed",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) []string {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				target := filepath.Join(root, "profile.json")
				retained := target + ".retained"
				require.NoError(t, os.Rename(target, retained))
				require.NoError(t, os.Rename(
					filepath.Join(replacementTokensDir, credentialNamespace, "profile.json"), target,
				))
				return []string{retained, target}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newRequire := require.New
			assert := assert.New(t)
			require := require.New(t)
			parent := t.TempDir()
			tokensDir := filepath.Join(parent, "tokens")
			replacementTokensDir := filepath.Join(parent, "replacement-tokens")
			store := NewFileCredentialStore(tokensDir)
			guard, created, err := store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
			require.NoError(err)
			require.True(created)
			t.Cleanup(func() {
				require := newRequire(t)
				require.NoError(guard.Close())
			})
			replacementStore := NewFileCredentialStore(replacementTokensDir)
			require.NoError(replacementStore.Save("profile", NewCredential(
				AuthBearer, credentialSecurityTestValue+"-replacement",
			)))

			paths := test.replace(t, tokensDir, replacementTokensDir)
			before := snapshotCredentialCleanupPaths(t, paths...)
			err = store.CleanupNew("profile", guard)
			require.ErrorContains(err, test.wantError)
			assert.NotContains(err.Error(), credentialSecurityTestValue)
			assertCredentialCleanupPathsUnchanged(t, before)
		})
	}
}

func guardedSecurityCredentialDelete(store *FileCredentialStore, profileName string) error {
	guard, err := store.PreflightDelete(profileName)
	if err != nil {
		return err
	}
	return errors.Join(store.Delete(profileName, guard), guard.Close())
}

func TestCredentialDeleteGuardRejectsValidIdentityReplacements(t *testing.T) {
	tests := []struct {
		name      string
		wantError string
		replace   func(t *testing.T, tokensDir, replacementTokensDir string) (string, string)
	}{
		{
			name:      "tokens root",
			wantError: "tokens directory changed during guarded deletion",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) (string, string) {
				t.Helper()
				retained := tokensDir + "-retained"
				require.NoError(t, os.Rename(tokensDir, retained))
				require.NoError(t, os.Rename(replacementTokensDir, tokensDir))
				return filepath.Join(retained, credentialNamespace, "profile.json"),
					filepath.Join(tokensDir, credentialNamespace, "profile.json")
			},
		},
		{
			name:      "credential namespace",
			wantError: "credential directory changed during guarded deletion",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) (string, string) {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				retained := root + "-retained"
				require.NoError(t, os.Rename(root, retained))
				require.NoError(t, os.Rename(filepath.Join(replacementTokensDir, credentialNamespace), root))
				return filepath.Join(retained, "profile.json"), filepath.Join(root, "profile.json")
			},
		},
		{
			name:      "lock marker",
			wantError: "credential lock changed during guarded deletion",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) (string, string) {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				marker := filepath.Join(root, ".credentials.lock")
				retained := marker + ".retained"
				require.NoError(t, os.Rename(marker, retained))
				require.NoError(t, os.Rename(
					filepath.Join(replacementTokensDir, credentialNamespace, ".credentials.lock"), marker,
				))
				return filepath.Join(root, "profile.json"),
					filepath.Join(replacementTokensDir, credentialNamespace, "profile.json")
			},
		},
		{
			name:      "credential target",
			wantError: "credential changed during guarded deletion",
			replace: func(t *testing.T, tokensDir, replacementTokensDir string) (string, string) {
				t.Helper()
				root := filepath.Join(tokensDir, credentialNamespace)
				target := filepath.Join(root, "profile.json")
				retained := target + ".retained"
				require.NoError(t, os.Rename(target, retained))
				require.NoError(t, os.Rename(
					filepath.Join(replacementTokensDir, credentialNamespace, "profile.json"), target,
				))
				return retained, target
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newRequire := require.New
			require := require.New(t)
			parent := t.TempDir()
			tokensDir := filepath.Join(parent, "tokens")
			replacementTokensDir := filepath.Join(parent, "replacement-tokens")
			store := NewFileCredentialStore(tokensDir)
			replacementStore := NewFileCredentialStore(replacementTokensDir)
			require.NoError(store.Save("profile", NewCredential(
				AuthBearer, credentialSecurityTestValue,
			)))
			require.NoError(replacementStore.Save("profile", NewCredential(
				AuthBearer, credentialSecurityTestValue+"-replacement",
			)))

			originalBefore := snapshotCredentialSecurityPath(t,
				filepath.Join(tokensDir, credentialNamespace, "profile.json"))
			replacementBefore := snapshotCredentialSecurityPath(t,
				filepath.Join(replacementTokensDir, credentialNamespace, "profile.json"))
			guard, err := store.PreflightDelete("profile")
			require.NoError(err)
			t.Cleanup(func() {
				require := newRequire(t)
				require.NoError(guard.Close())
			})

			originalPath, replacementPath := test.replace(t, tokensDir, replacementTokensDir)
			err = store.Delete("profile", guard)
			require.ErrorContains(err, test.wantError)
			assertCredentialSecurityPathMatches(t, originalBefore, originalPath)
			assertCredentialSecurityPathMatches(t, replacementBefore, replacementPath)
		})
	}
}

type credentialSecurityPathSnapshot struct {
	info          os.FileInfo
	entries       []string
	contentDigest [sha256.Size]byte
}

func snapshotCredentialSecurityPath(t *testing.T, path string) credentialSecurityPathSnapshot {
	t.Helper()
	info, err := os.Lstat(path)
	require.NoError(t, err)
	snapshot := credentialSecurityPathSnapshot{info: info}
	if info.IsDir() {
		entries, readErr := os.ReadDir(path)
		require.NoError(t, readErr)
		for _, entry := range entries {
			snapshot.entries = append(snapshot.entries, entry.Name())
		}
	} else if info.Mode().IsRegular() {
		contents, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		snapshot.contentDigest = sha256.Sum256(contents)
	}
	return snapshot
}

func assertCredentialSecurityPathMatches(
	t *testing.T,
	want credentialSecurityPathSnapshot,
	path string,
) {
	t.Helper()
	got, err := os.Lstat(path)
	require.NoError(t, err, path)
	assert.True(t, os.SameFile(want.info, got), "%s inode changed", path)
	assert.Equal(t, want.info.Mode(), got.Mode(), "%s mode changed", path)
	assert.Equal(t, want.info.Size(), got.Size(), "%s size changed", path)
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

func TestCredentialStoreSaveUsesPinnedNamespaceAfterPathSwap(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	retained := filepath.Join(tokensDir, "retained-people-providers")
	external := t.TempDir()
	var once sync.Once
	store.hooks = &credentialStoreHooks{beforeOperation: func(operation string) {
		if operation != "save" {
			return
		}
		once.Do(func() {
			require := newRequire(t)
			require.NoError(os.Rename(root, retained))
			require.NoError(os.Symlink(external, root))
		})
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.NoError(err)
	assert.NoFileExists(filepath.Join(external, "profile.json"))
	contents, err := os.ReadFile(filepath.Join(retained, "profile.json"))
	require.NoError(err)
	assert.NotEmpty(contents, "credential was not published in the pinned namespace")
}

func TestCredentialStoreLoadUsesPinnedEntryAfterSwap(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "replacement.json")
	require.NoError(os.WriteFile(external, []byte(`{"scheme":"bearer","value":"replacement"}`), 0o600))
	var once sync.Once
	store.hooks = &credentialStoreHooks{afterCredentialOpen: func(operation string) {
		if operation != "load" {
			return
		}
		once.Do(func() {
			require := newRequire(t)
			require.NoError(os.Rename(filepath.Join(root, "profile.json"), original))
			require.NoError(os.Symlink(external, filepath.Join(root, "profile.json")))
		})
	}}

	credential, err := store.Load("profile")
	require.NoError(err)
	assert.Equal(credentialSecurityTestValue, credential.Value(),
		"load did not read the pinned credential entry")
}

func TestCredentialStorePreflightDeleteRejectsEntrySwapAfterOpen(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "replacement.json")
	require.NoError(os.WriteFile(external, []byte("replacement-test-value"), 0o600))
	var swapped atomic.Bool
	store.hooks = &credentialStoreHooks{afterCredentialOpen: func(operation string) {
		require := newRequire(t)
		if operation != "preflight-delete" {
			return
		}
		require.NoError(os.Rename(filepath.Join(root, "profile.json"), original))
		require.NoError(os.Symlink(external, filepath.Join(root, "profile.json")))
		swapped.Store(true)
	}}

	_, err := store.PreflightDelete("profile")
	require.ErrorContains(err, "changed during deletion preflight")
	assert.True(swapped.Load(), "deletion preflight did not reach the pinned-entry boundary")
	assert.NotContains(err.Error(), credentialSecurityTestValue)
	originalContents, readErr := os.ReadFile(original)
	require.NoError(readErr)
	assert.Contains(string(originalContents), credentialSecurityTestValue)
	externalContents, readErr := os.ReadFile(external)
	require.NoError(readErr)
	assert.Equal("replacement-test-value", string(externalContents))
}

func TestCredentialStoreSaveRefusesSwappedCandidate(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(os.WriteFile(external, []byte("unchanged"), 0o600))
	var retained string
	store.hooks = &credentialStoreHooks{afterCandidateOpen: func(candidateName string) {
		require := newRequire(t)
		retained = filepath.Join(root, candidateName+".retained")
		require.NoError(os.Rename(filepath.Join(root, candidateName), retained))
		require.NoError(os.Symlink(external, filepath.Join(root, candidateName)))
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.ErrorContains(err, "candidate changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(readErr)
	assert.Equal("unchanged", string(contents))
	assert.FileExists(retained)
	assert.NoFileExists(filepath.Join(root, "profile.json"))
}

func TestCredentialStoreSaveDetectsCandidateSwapAtPublicationBoundary(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(os.WriteFile(external, []byte("unchanged"), 0o600))
	var retained string
	store.hooks = &credentialStoreHooks{beforeCandidatePublish: func(candidateName string) {
		require := newRequire(t)
		retained = filepath.Join(root, candidateName+".retained")
		require.NoError(os.Rename(filepath.Join(root, candidateName), retained))
		require.NoError(os.Symlink(external, filepath.Join(root, candidateName)))
	}}

	err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.ErrorContains(err, "candidate changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(readErr)
	assert.Equal("unchanged", string(contents))
	retainedInfo, statErr := os.Stat(retained)
	require.NoError(statErr)
	assert.Zero(retainedInfo.Size(), "the pinned candidate was not wiped after the publication race")
	_, loadErr := store.Load("profile")
	require.Error(loadErr)
}

func TestCredentialStoreRejectsHardLinkedMutationTargets(t *testing.T) {
	t.Run("candidate", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
		root := filepath.Join(tokensDir, "people-providers")
		external := filepath.Join(t.TempDir(), "external")
		const externalContents = "external file must remain unchanged"
		require.NoError(os.WriteFile(external, []byte(externalContents), 0o600))
		require.NoError(os.Link(external, filepath.Join(root, unixCredentialStagingName)))

		err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.ErrorContains(err, "links")
		contents, readErr := os.ReadFile(external)
		require.NoError(readErr)
		assert.Equal(externalContents, string(contents), "candidate reuse modified an external hard link")
		assert.NoFileExists(filepath.Join(root, "profile.json"))
	})

	t.Run("candidate linked after open", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
		root := filepath.Join(tokensDir, "people-providers")
		external := filepath.Join(t.TempDir(), "external")
		const externalContents = "opened candidate must remain unchanged"
		store.hooks = &credentialStoreHooks{afterCandidateOpen: func(candidateName string) {
			require := newRequire(t)
			candidatePath := filepath.Join(root, candidateName)
			require.NoError(os.WriteFile(candidatePath, []byte(externalContents), 0o600))
			require.NoError(os.Link(candidatePath, external))
		}}

		err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.ErrorContains(err, "links")
		contents, readErr := os.ReadFile(external)
		require.NoError(readErr)
		assert.Equal(externalContents, string(contents),
			"candidate write or cleanup modified a hard link added after open")
	})

	t.Run("credential deletion", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
		root := filepath.Join(tokensDir, "people-providers")
		external := filepath.Join(t.TempDir(), "external")
		const externalContents = "external file must remain unchanged"
		require.NoError(os.WriteFile(external, []byte(externalContents), 0o600))
		require.NoError(os.Link(external, filepath.Join(root, "profile.json")))

		err := guardedSecurityCredentialDelete(store, "profile")
		require.ErrorContains(err, "links")
		contents, readErr := os.ReadFile(external)
		require.NoError(readErr)
		assert.Equal(externalContents, string(contents), "credential deletion modified an external hard link")
	})

	t.Run("credential linked before wipe", func(t *testing.T) {
		newRequire := require.New
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		store := NewFileCredentialStore(tokensDir)
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		root := filepath.Join(tokensDir, "people-providers")
		credentialPath := filepath.Join(root, "profile.json")
		external := filepath.Join(t.TempDir(), "external")
		before, statErr := os.Stat(credentialPath)
		require.NoError(statErr)
		store.hooks = &credentialStoreHooks{beforeCredentialRetire: func() {
			require := newRequire(t)
			require.NoError(os.Link(credentialPath, external))
		}}
		guard, err := store.PreflightDelete("profile")
		require.NoError(err)
		t.Cleanup(func() {
			require := newRequire(t)
			require.NoError(guard.Close())
		})

		err = store.Delete("profile", guard)
		require.ErrorContains(err, "changed")
		after, statErr := os.Stat(external)
		require.NoError(statErr)
		assert.Equal(before.Size(), after.Size(), "credential wipe modified a hard link added at the boundary")
	})
}

func TestValidateUnixCredentialStatRejectsForeignOwner(t *testing.T) {
	foreignUID := uint32(os.Geteuid()) ^ 1
	stat := unix.Stat_t{
		Mode:  unix.S_IFREG | 0o600,
		Nlink: 1,
		Uid:   foreignUID,
	}

	err := validateUnixCredentialStat(stat, "file")
	require.ErrorContains(t, err, "owner")
}

func TestCredentialStoreDeleteDetectsSwapAtRemovalBoundary(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	original := filepath.Join(root, "profile.original")
	external := filepath.Join(t.TempDir(), "must-remain")
	require.NoError(os.WriteFile(external, []byte("unchanged"), 0o600))
	targetBefore := snapshotCredentialSecurityPath(t, filepath.Join(root, "profile.json"))
	store.hooks = &credentialStoreHooks{beforeCredentialRetire: func() {
		require := newRequire(t)
		require.NoError(os.Rename(filepath.Join(root, "profile.json"), original))
		require.NoError(os.Symlink(external, filepath.Join(root, "profile.json")))
	}}
	guard, err := store.PreflightDelete("profile")
	require.NoError(err)
	t.Cleanup(func() {
		require := newRequire(t)
		require.NoError(guard.Close())
	})

	err = store.Delete("profile", guard)
	require.ErrorContains(err, "changed")
	contents, readErr := os.ReadFile(external)
	require.NoError(readErr)
	assert.Equal("unchanged", string(contents))
	assertCredentialSecurityPathMatches(t, targetBefore, original)
}

func TestCredentialStoreRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, operation := range []string{"save", "load", "preflight-delete", "delete"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			tokensDir := t.TempDir()
			store := NewFileCredentialStore(tokensDir)
			require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
			fifo := filepath.Join(tokensDir, "people-providers", "profile.json")
			require.NoError(unix.Mkfifo(fifo, 0o600))

			result := make(chan error, 1)
			go func() {
				switch operation {
				case "save":
					result <- store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
				case "load":
					_, err := store.Load("profile")
					result <- err
				case "preflight-delete":
					_, err := store.PreflightDelete("profile")
					result <- err
				case "delete":
					result <- guardedSecurityCredentialDelete(store, "profile")
				}
			}()

			select {
			case err := <-result:
				require.ErrorContains(err, "not a regular file")
			case <-time.After(time.Second):
				assert.Fail("credential operation blocked while inspecting a FIFO")
			}
		})
	}
}

func TestCredentialStoreFailedSavesKeepArtifactsBounded(t *testing.T) {
	newRequire := require.New
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	target := filepath.Join(root, "profile.json")
	store.hooks = &credentialStoreHooks{beforeCandidatePublish: func(string) {
		require := newRequire(t)
		require.NoError(os.Mkdir(target, 0o700))
	}}

	for range 12 {
		err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
		require.Error(err)
		require.NoError(os.Remove(target))
	}

	entries, err := os.ReadDir(root)
	require.NoError(err)
	assert.LessOrEqual(len(entries), 3, "failed saves accumulated credential-store artifacts")
}

func TestCredentialStoreSaveReportsCandidateWipeFailure(t *testing.T) {
	tests := []struct {
		name            string
		configure       func(*credentialStoreHooks, *atomic.Bool)
		wantError       string
		wantZeroed      bool
		wantSyncAttempt bool
	}{
		{
			name: "truncate",
			configure: func(hooks *credentialStoreHooks, syncAttempted *atomic.Bool) {
				hooks.failedCandidateTruncate = func() error {
					return errors.New("injected truncate failure")
				}
				hooks.failedCandidateSync = func() error {
					syncAttempted.Store(true)
					return nil
				}
			},
			wantError:       "wipe failed",
			wantSyncAttempt: true,
		},
		{
			name: "sync",
			configure: func(hooks *credentialStoreHooks, syncAttempted *atomic.Bool) {
				hooks.failedCandidateSync = func() error {
					syncAttempted.Store(true)
					return errors.New("injected sync failure")
				}
			},
			wantError:       "durable wipe failed",
			wantZeroed:      true,
			wantSyncAttempt: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newRequire := require.New
			assert := assert.New(t)
			require := require.New(t)
			tokensDir := t.TempDir()
			store := NewFileCredentialStore(tokensDir)
			require.NoError(store.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
			root := filepath.Join(tokensDir, "people-providers")
			target := filepath.Join(root, "profile.json")
			var candidateName string
			hooks := &credentialStoreHooks{beforeCandidatePublish: func(name string) {
				require := newRequire(t)
				candidateName = name
				require.NoError(os.Mkdir(target, 0o700))
			}}
			var syncAttempted atomic.Bool
			test.configure(hooks, &syncAttempted)
			store.hooks = hooks

			err := store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
			require.ErrorContains(err, "publish")
			require.ErrorContains(err, test.wantError)
			assert.NotContains(err.Error(), "injected")
			if strings.Contains(err.Error(), credentialSecurityTestValue) {
				assert.Fail("failed candidate cleanup disclosed the credential value")
			}
			candidateInfo, statErr := os.Stat(filepath.Join(root, candidateName))
			require.NoError(statErr)
			if test.wantZeroed {
				assert.Zero(candidateInfo.Size(), "candidate truncate did not complete before sync failed")
			} else {
				assert.Positive(candidateInfo.Size(), "injected truncate failure unexpectedly zeroed the candidate")
			}
			assert.Equal(test.wantSyncAttempt, syncAttempted.Load(),
				"candidate sync cleanup attempt did not remain independent")
		})
	}
}

func TestCredentialStoreRepeatedSaveDeleteKeepsArtifactsBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)

	for range 12 {
		require.NoError(store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
		require.NoError(guardedSecurityCredentialDelete(store, "profile"))
		_, err := store.Load("profile")
		require.ErrorIs(err, ErrCredentialNotFound)
	}

	entries, err := os.ReadDir(filepath.Join(tokensDir, "people-providers"))
	require.NoError(err)
	assert.LessOrEqual(len(entries), 2, "save/delete cycles accumulated credential-store artifacts")

	_, err = store.Load("profile")
	require.ErrorIs(err, ErrCredentialNotFound, "deleted credential did not remain logically absent")
}

func TestCredentialStoreLockReplacementCannotSplitExclusion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	first := NewFileCredentialStore(tokensDir)
	require.NoError(first.Save("seed", NewCredential(AuthBearer, credentialSecurityTestValue)))
	root := filepath.Join(tokensDir, "people-providers")
	firstLocked := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	hookResult := make(chan error, 1)
	var replaceOnce sync.Once
	first.hooks = &credentialStoreHooks{afterLockAcquired: func() {
		replaceOnce.Do(func() {
			hookErr := os.Rename(filepath.Join(root, ".credentials.lock"),
				filepath.Join(root, ".credentials.lock.original"))
			if hookErr == nil {
				hookErr = os.WriteFile(filepath.Join(root, ".credentials.lock"), nil, 0o600)
			}
			hookResult <- hookErr
			close(firstLocked)
			<-releaseFirst
		})
	}}
	second := NewFileCredentialStore(tokensDir)
	second.hooks = &credentialStoreHooks{beforeOperation: func(string) {
		select {
		case <-secondEntered:
		default:
			close(secondEntered)
		}
	}}

	firstResult := make(chan error, 1)
	go func() {
		firstResult <- first.Save("first", NewCredential(AuthBearer, credentialSecurityTestValue))
	}()
	<-firstLocked
	require.NoError(<-hookResult)
	secondResult := make(chan error, 1)
	go func() {
		secondResult <- second.Save("second", NewCredential(AuthBearer, credentialSecurityTestValue))
	}()
	select {
	case <-secondEntered:
		assert.Fail("replacement lock admitted a concurrent credential operation")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(<-firstResult)
	require.NoError(<-secondResult)
}

func TestCredentialStoreSyncsNamespaceParentAfterCreation(t *testing.T) {
	store := NewFileCredentialStore(t.TempDir())
	var parentSynced atomic.Bool
	store.hooks = &credentialStoreHooks{afterNamespaceParentSync: func() {
		parentSynced.Store(true)
	}}
	require.NoError(t, store.Save("profile", NewCredential(AuthBearer, credentialSecurityTestValue)))
	assert.True(t, parentSynced.Load(), "credential namespace parent was not synced after creation")
}

func TestCredentialStoreSaveNewRollsBackPublicationWhenCleanupPinFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := NewFileCredentialStore(tokensDir)
	store.hooks = &credentialStoreHooks{failedCleanupPin: func() error {
		return errors.New("injected cleanup pin failure")
	}}

	guard, created, err := store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.Error(err)
	require.ErrorContains(err, "injected cleanup pin failure")
	assert.False(created)
	assert.Nil(guard)
	assert.NotContains(err.Error(), credentialSecurityTestValue)

	_, loadErr := store.Load("profile")
	require.ErrorIs(loadErr, ErrCredentialNotFound)

	// Retirement policy: a durable empty record remains instead of an unlink.
	data, readErr := os.ReadFile(filepath.Join(tokensDir, credentialNamespace, "profile.json"))
	require.NoError(readErr)
	assert.Empty(data)

	// The failed publication no longer blocks a retry from creating.
	store.hooks = nil
	guard, created, err = store.SaveNew("profile", NewCredential(AuthBearer, credentialSecurityTestValue))
	require.NoError(err)
	assert.True(created)
	require.NoError(guard.Close())
	credential, loadErr := store.Load("profile")
	require.NoError(loadErr)
	assert.Equal(credentialSecurityTestValue, credential.Value())
}
