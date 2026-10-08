package peoplesweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/providercredentials"
)

const credentialCanary = "test-credential-canary"

func writeLegacyCredential(t *testing.T, tokensDir, name, contents string) string {
	t.Helper()
	// Windows drops inherited access from older children when the store first
	// secures the tokens directory, so create the store first there; other OSes
	// keep the real upgrade order, with no store yet.
	if runtime.GOOS == "windows" {
		empty, err := providercredentials.Read(tokensDir)
		require.NoError(t, err)
		unused := providercredentials.PeopleProviderID("unused-fixture")
		absent, err := empty.Revision(unused)
		require.NoError(t, err)
		_, err = providercredentials.DeleteIfRevision(tokensDir, absent, unused, func() error { return nil })
		require.NoError(t, err)
	}
	path := filepath.Join(tokensDir, "people-providers", name+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

func saveStoredCredential(t *testing.T, store peoplesweep.StoredCredentials, name, endpoint, value string) string {
	t.Helper()
	revision, _, err := store.Revision(name, endpoint)
	require.NoError(t, err)
	saved, err := store.SaveIfRevision(name, endpoint, value, revision)
	require.NoError(t, err)
	return saved
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

	store := peoplesweep.NewStoredCredentials(t.TempDir())
	_, err = store.SaveIfRevision("../invalid", "https://provider.example.test/v1", credential.Value(), "")
	require.Error(err)
	assert.NotContains(err.Error(), credentialCanary)
}

func TestStoredCredentialsLifecycleUsesPerCredentialRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := peoplesweep.NewStoredCredentials(tokensDir)
	endpoint := "https://api.example.test/v1"

	absent, present, err := store.Revision("remote", endpoint)
	require.NoError(err)
	assert.False(present)
	_, err = store.Load("remote", endpoint)
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
	_, err = store.DeleteIfRevision("remote", absent)
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)

	saved, err := store.SaveIfRevision("remote", endpoint, credentialCanary, absent)
	require.NoError(err)
	current, present, err := store.Revision("remote", endpoint)
	require.NoError(err)
	assert.True(present)
	assert.Equal(saved, current)
	_, err = store.SaveIfRevision("remote", endpoint, "replacement", absent)
	require.ErrorIs(err, peoplesweep.ErrCredentialRevisionConflict)
	value, err := store.Load("remote", endpoint)
	require.NoError(err)
	assert.Equal(credentialCanary, value)

	_, err = store.DeleteIfRevision("remote", absent)
	require.ErrorIs(err, peoplesweep.ErrCredentialRevisionConflict)
	deleted, err := store.DeleteIfRevision("remote", current)
	require.NoError(err)
	assert.Equal(absent, deleted)
	_, err = store.Load("remote", endpoint)
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestStoredCredentialsImportsLegacyFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"x_api_key","value":"`+credentialCanary+`"}`)
	store := peoplesweep.NewStoredCredentials(tokensDir)

	value, err := store.Load("remote", "https://api.example.test/v1")
	require.NoError(err)
	assert.Equal(credentialCanary, value)
	assert.NoFileExists(path)

	snapshot, err := providercredentials.Read(tokensDir)
	require.NoError(err)
	id := providercredentials.PeopleProviderID("remote")
	stored, _, err := snapshot.Resolve(id, "https://api.example.test/other", "", nil)
	require.NoError(err)
	assert.Equal(credentialCanary, stored)
	_, _, err = snapshot.Resolve(id, "https://other.example.test/v1", "", nil)
	require.ErrorIs(err, providercredentials.ErrOriginMismatch)
}

func TestStoredCredentialsLegacyFileNeverReplacesOrResurrectsStoredKey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	endpoint := "https://api.example.test/v1"
	store := peoplesweep.NewStoredCredentials(tokensDir)
	current := saveStoredCredential(t, store, "remote", endpoint, "stored-value")

	path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"legacy-value"}`)
	value, err := store.Load("remote", endpoint)
	require.NoError(err)
	assert.Equal("stored-value", value)
	assert.NoFileExists(path)

	_, err = store.DeleteIfRevision("remote", current)
	require.NoError(err)
	_, err = store.Load("remote", endpoint)
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestStoredCredentialsIgnoresMalformedLegacyFileWhenKeyIsStored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	endpoint := "https://api.example.test/v1"
	store := peoplesweep.NewStoredCredentials(tokensDir)
	saveStoredCredential(t, store, "remote", endpoint, "stored-value")

	path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"x"} trailing`)
	value, err := store.Load("remote", endpoint)
	require.NoError(err)
	assert.Equal("stored-value", value)
	assert.NoFileExists(path)
}

func TestStoredCredentialsRetiresEmptyLegacyFileAsDeletedKey(t *testing.T) {
	for _, contents := range []string{"", " \n"} {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", contents)

		_, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
		require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
		assert.NoFileExists(path)
		snapshot, err := providercredentials.Read(tokensDir)
		require.NoError(err)
		assert.False(snapshot.Stored(providercredentials.PeopleProviderID("remote")))
	}
}

func TestStoredCredentialsLegacyImportFailureKeepsFile(t *testing.T) {
	t.Run("malformed legacy JSON", func(t *testing.T) {
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"`+credentialCanary+`"} trailing`)

		_, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
		require.ErrorContains(t, err, path)
		assert.NotContains(t, err.Error(), credentialCanary)
		assert.FileExists(t, path)
	})
	t.Run("unreadable shared store", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"`+credentialCanary+`"}`)
		require.NoError(os.WriteFile(filepath.Join(tokensDir, providercredentials.Filename), []byte("{not json"), 0o600))

		_, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
		require.ErrorContains(err, path)
		assert.NotContains(err.Error(), credentialCanary)
		assert.FileExists(path)
	})
}

func TestStoredCredentialsLegacyFileSizes(t *testing.T) {
	t.Run("a key over 16 KiB imports", func(t *testing.T) {
		tokensDir := t.TempDir()
		secret := strings.Repeat("k", 20<<10)
		path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"`+secret+`"}`)

		value, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
		require.NoError(t, err)
		assert.Equal(t, secret, value)
		assert.NoFileExists(t, path)
	})
	t.Run("an oversized file is refused and kept", func(t *testing.T) {
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"`+strings.Repeat("k", 1<<20)+`"}`)

		_, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
		require.ErrorContains(t, err, "too large")
		assert.FileExists(t, path)
	})
}

func TestStoredCredentialsRefusesSymlinkedLegacyFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	path := writeLegacyCredential(t, tokensDir, "remote", "")
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	require.NoError(os.WriteFile(target, []byte(`{"scheme":"bearer","value":"`+credentialCanary+`"}`), 0o600))
	require.NoError(os.Remove(path))
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := peoplesweep.NewStoredCredentials(tokensDir).Load("remote", "https://api.example.test/v1")
	require.ErrorContains(err, "not a regular file")
	snapshot, err := providercredentials.Read(tokensDir)
	require.NoError(err)
	assert.False(snapshot.Stored(providercredentials.PeopleProviderID("remote")))
	assert.FileExists(target)
}

func TestStoredCredentialsKeepImportedKeyWhenLegacyFileCannotBeRemoved(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the current user cannot write")
	}
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	endpoint := "https://api.example.test/v1"
	path := writeLegacyCredential(t, tokensDir, "remote", `{"scheme":"bearer","value":"`+credentialCanary+`"}`)
	legacyDir := filepath.Dir(path)
	require.NoError(os.Chmod(legacyDir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(legacyDir, 0o700) })
	store := peoplesweep.NewStoredCredentials(tokensDir)

	for range 2 {
		value, err := store.Load("remote", endpoint)
		require.NoError(err)
		assert.Equal(credentialCanary, value)
	}
	// The secret is gone from the leftover file, so a lost unlink cannot bring
	// a deleted key back.
	raw, err := os.ReadFile(path)
	require.NoError(err)
	assert.Empty(raw)

	revision, _, err := store.Revision("remote", endpoint)
	require.NoError(err)
	_, err = store.DeleteIfRevision("remote", revision)
	require.ErrorContains(err, path)
	value, err := store.Load("remote", endpoint)
	require.NoError(err)
	assert.Equal(credentialCanary, value)

	require.NoError(os.Chmod(legacyDir, 0o700))
	_, err = store.DeleteIfRevision("remote", revision)
	require.NoError(err)
	assert.NoFileExists(path)
	_, err = store.Load("remote", endpoint)
	require.ErrorIs(err, peoplesweep.ErrCredentialNotFound)
}

func TestStoredCredentialsBrokenLegacyFileCanBeReplacedOrDeleted(t *testing.T) {
	broken := `{"scheme":"bearer","value":"` + credentialCanary + `"} trailing`
	endpoint := "https://api.example.test/v1"
	t.Run("delete", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", broken)
		store := peoplesweep.NewStoredCredentials(tokensDir)

		revision, present, err := store.Revision("remote", endpoint)
		require.NoError(err)
		assert.True(present, "an unimported key file still counts as a stored key")
		_, err = store.DeleteIfRevision("remote", revision)
		require.NoError(err)
		assert.NoFileExists(path)
		_, present, err = store.Revision("remote", endpoint)
		require.NoError(err)
		assert.False(present)
	})
	t.Run("replace", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		tokensDir := t.TempDir()
		path := writeLegacyCredential(t, tokensDir, "remote", broken)
		store := peoplesweep.NewStoredCredentials(tokensDir)

		revision, _, err := store.Revision("remote", endpoint)
		require.NoError(err)
		_, err = store.SaveIfRevision("remote", endpoint, "replacement", revision)
		require.NoError(err)
		value, err := store.Load("remote", endpoint)
		require.NoError(err)
		assert.Equal("replacement", value)
		assert.NoFileExists(path)
	})
}

func TestStoredCredentialsUnconfiguredRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tokensDir := t.TempDir()
	store := peoplesweep.NewStoredCredentials(tokensDir)
	saveStoredCredential(t, store, "gone", "https://api.example.test/v1", credentialCanary)
	saveStoredCredential(t, store, "kept", "https://api.example.test/v1", "kept-value")
	path := writeLegacyCredential(t, tokensDir, "gone", `{"scheme":"bearer","value":"legacy"}`)

	revision, present, err := store.UnconfiguredRevision("gone")
	require.NoError(err)
	assert.True(present)
	assert.FileExists(path, "observing a key without an endpoint must not import the legacy file")
	_, err = store.DeleteIfRevision("gone", revision)
	require.NoError(err)
	assert.NoFileExists(path)
	snapshot, err := providercredentials.Read(tokensDir)
	require.NoError(err)
	assert.False(snapshot.Stored(providercredentials.PeopleProviderID("gone")))
	assert.True(snapshot.Stored(providercredentials.PeopleProviderID("kept")))

	_, present, err = store.UnconfiguredRevision("gone")
	require.NoError(err)
	assert.False(present)
	_, _, err = store.UnconfiguredRevision("../escape")
	require.Error(err)
}

func TestCredentialResolverUsesProfileScheme(t *testing.T) {
	tokensDir := t.TempDir()
	writeLegacyCredential(t, tokensDir, "stored-profile", `{"scheme":"bearer","value":"`+credentialCanary+`"}`)
	resolver := peoplesweep.NewCredentialResolver(peoplesweep.NewStoredCredentials(tokensDir), nil)

	credential, err := resolver.Resolve("stored-profile", credentialTestProfile(t,
		peoplesweep.CredentialStored, "stored-profile", peoplesweep.AuthXAPIKey))
	require.NoError(t, err)
	assert.Equal(t, peoplesweep.AuthXAPIKey, credential.Scheme)
	assert.Equal(t, credentialCanary, credential.Value())
}

func TestCredentialResolverUsesStoredEnvironmentAndNoneSources(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := peoplesweep.NewStoredCredentials(t.TempDir())
	saveStoredCredential(t, store, "stored-profile", "https://provider.example.test/v1", credentialCanary)
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
	store := peoplesweep.NewStoredCredentials(t.TempDir())
	saveStoredCredential(t, store, "mismatch", "https://elsewhere.example.test/v1", credentialCanary)
	resolver := peoplesweep.NewCredentialResolver(store, func(string) (string, bool) {
		return credentialCanary, false
	})

	_, err := resolver.Resolve("mismatch", credentialTestProfile(t,
		peoplesweep.CredentialStored, "mismatch", peoplesweep.AuthXAPIKey))
	require.ErrorIs(err, providercredentials.ErrOriginMismatch)
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
