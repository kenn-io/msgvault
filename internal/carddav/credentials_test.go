package carddav

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCardDAVConnectionBindingsAreIndependent(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	defaultDir, err := ConnectionTokenDir(root, "default")
	require.NoError(err)
	assertions.Equal(root, defaultDir)
	workDir, err := ConnectionTokenDir(root, "work")
	require.NoError(err)
	googleDir, err := ConnectionTokenDir(root, "google")
	require.NoError(err)
	assertions.Equal(filepath.Join(root, "carddav-connections", "work"), workDir)
	password := Credential{Password: "synthetic-password", BaseURL: "https://contacts.example/dav", Username: "person@example.com", ConnectionGeneration: 2}
	google := Credential{Google: true, OAuthApp: "contacts", BaseURL: "https://contacts.example/dav", Username: "person@example.com", ConnectionGeneration: 3}
	require.NoError(SaveCredential(defaultDir, password))
	require.NoError(SaveCredential(workDir, password))
	require.NoError(SaveCredential(googleDir, google))
	require.NoError(RemoveCredential(workDir))
	got, err := LoadCredential(defaultDir)
	require.NoError(err)
	assertions.Equal(password, got)
	got, err = LoadCredential(googleDir)
	require.NoError(err)
	assertions.Equal(google, got, "sharing an OAuth identity must not share a discovery binding")
	for _, name := range []string{"", "../work", "work/path", `work\path`, "Work", "wörk", strings.Repeat("a", 65)} {
		_, err := ConnectionTokenDir(root, name)
		assertions.Error(err, name)
	}
}

func FuzzCardDAVConnectionTokenPath(f *testing.F) {
	root := filepath.Join(f.TempDir(), "tokens")
	for _, name := range []string{"default", "a", "work-2", "../work", "", "wörk", strings.Repeat("a", 64), strings.Repeat("a", 65)} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		assertions := assert.New(t)
		require := require.New(t)

		path, err := ConnectionTokenDir(root, name)
		valid := len(name) >= 1 && len(name) <= 64
		if valid {
			valid = name[0] >= 'a' && name[0] <= 'z'
			for i := 1; i < len(name); i++ {
				c := name[i]
				valid = valid && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-')
			}
		}
		if !valid {
			require.Error(err)
			assertions.Empty(path)
			return
		}
		require.NoError(err)
		if name == "default" {
			assertions.Equal(root, path)
			return
		}
		relative, err := filepath.Rel(root, path)
		require.NoError(err)
		assertions.Equal(filepath.Join("carddav-connections", name), relative)
		assertions.False(filepath.IsAbs(relative))
		assertions.False(strings.HasPrefix(relative, ".."+string(filepath.Separator)))
	})
}

type injectedCredentialPermissionFailure struct {
	delegate       credentialPermissionBackend
	directoryError error
	secureFileCall int
	secureFileErr  error
	verifyError    error
}

func (f *injectedCredentialPermissionFailure) secureDirectory(path string) error {
	if f.directoryError != nil {
		return f.directoryError
	}
	return f.delegate.secureDirectory(path)
}

func (f *injectedCredentialPermissionFailure) secureFile(file *os.File) error {
	f.secureFileCall--
	if f.secureFileCall == 0 {
		return f.secureFileErr
	}
	return f.delegate.secureFile(file)
}

func (f *injectedCredentialPermissionFailure) verifyFile(file *os.File) error {
	if f.verifyError != nil {
		return f.verifyError
	}
	return f.delegate.verifyFile(file)
}

func TestCardDAVCredentialsRoundTripInPrivateTokenFile(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	require.NoError(SavePassword(testCredentialTokenDir(home), "first-secret"))
	require.NoError(SavePassword(testCredentialTokenDir(home), "replacement-secret"))

	password, err := LoadPassword(testCredentialTokenDir(home))
	require.NoError(err)
	assertions.Equal("replacement-secret", password)
	if runtime.GOOS == "windows" {
		return
	}

	path := filepath.Join(home, "tokens", "carddav.json")
	info, err := os.Stat(path)
	require.NoError(err)
	assertions.Equal(os.FileMode(0o600), info.Mode().Perm())
	tokensInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(err)
	assertions.Zero(tokensInfo.Mode().Perm() & 0o077)
}

func TestCardDAVCredentialBindsPasswordToConnectionIdentity(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	want := Credential{
		Password:             "synthetic-secret",
		BaseURL:              "https://contacts.example/dav",
		Username:             "alice",
		ConnectionGeneration: 3,
	}
	require.NoError(SaveCredential(testCredentialTokenDir(home), want))

	got, err := LoadCredential(testCredentialTokenDir(home))
	require.NoError(err)
	assertions.Equal(want, got)
	assertions.NotContains(string(mustReadCredentialFile(t, testCredentialTokenDir(home))), "connection_generation\":0")
}

func TestCardDAVCredentialRejectsLegacyUnboundPassword(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, SavePassword(testCredentialTokenDir(home), "synthetic-secret"))

	_, err := LoadCredential(testCredentialTokenDir(home))
	require.ErrorContains(t, err, "not bound to a connection")
}

func testCredentialTokenDir(home string) string {
	return filepath.Join(home, "tokens")
}

func mustReadCredentialFile(t *testing.T, tokenDir string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(tokenDir, cardDAVTokenFilename))
	require.NoError(t, err)
	return content
}

func TestCardDAVCredentialsRejectExposedTokenFile(t *testing.T) {
	require := require.New(t)

	home := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(home, "tokens"), 0o700))
	path := filepath.Join(home, "tokens", "carddav.json")
	require.NoError(os.WriteFile(path, []byte(`{"password":"secret"}`), 0o644))
	// The retained Linux verifier runs with a private umask, so WriteFile can
	// narrow the requested mode. Chmod establishes the exposed fixture exactly.
	require.NoError(os.Chmod(path, 0o644))

	_, err := LoadPassword(testCredentialTokenDir(home))
	require.ErrorContains(err, "permissions")
	assert.NotContains(t, err.Error(), "secret")
}

func TestCardDAVCredentialsRequireUnixTokenMode0600(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows token privacy is verified from the DACL")
	}
	home := t.TempDir()
	require.NoError(t, SavePassword(testCredentialTokenDir(home), "secret"))
	path := filepath.Join(home, "tokens", cardDAVTokenFilename)
	require.NoError(t, os.Chmod(path, 0o400))

	_, err := LoadPassword(testCredentialTokenDir(home))
	require.ErrorContains(t, err, "permissions")
}

func TestCardDAVCredentialsPropagatePermissionBackendFailures(t *testing.T) {
	injected := errors.New("injected owner-only permission failure")
	for _, tc := range []struct {
		name        string
		permissions *injectedCredentialPermissionFailure
	}{
		{name: "token directory", permissions: &injectedCredentialPermissionFailure{delegate: nativeCredentialPermissions{}, directoryError: injected}},
		{name: "temporary token", permissions: &injectedCredentialPermissionFailure{delegate: nativeCredentialPermissions{}, secureFileCall: 1, secureFileErr: injected}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			require := require.New(t)

			home := t.TempDir()
			require.NoError(SavePassword(testCredentialTokenDir(home), "prior-secret"))
			err := savePasswordWithPermissions(testCredentialTokenDir(home), "replacement-secret", tc.permissions)
			require.ErrorIs(err, injected)
			assertions.NotContains(err.Error(), "prior-secret")
			assertions.NotContains(err.Error(), "replacement-secret")
			password, loadErr := LoadPassword(testCredentialTokenDir(home))
			require.NoError(loadErr)
			assertions.Equal("prior-secret", password, "pre-publication failure must preserve the prior token")
		})
	}
}

func TestCardDAVCredentialsCompleteHardeningBeforeReplacingPriorToken(t *testing.T) {
	require := require.New(t)

	home := t.TempDir()
	require.NoError(SavePassword(testCredentialTokenDir(home), "prior-secret"))
	permissions := &injectedCredentialPermissionFailure{
		delegate:       nativeCredentialPermissions{},
		secureFileCall: 2,
		secureFileErr:  errors.New("post-publication hardening must not run"),
	}

	err := savePasswordWithPermissions(testCredentialTokenDir(home), "replacement-secret", permissions)
	require.NoError(err)
	password, err := LoadPassword(testCredentialTokenDir(home))
	require.NoError(err)
	assert.Equal(t, "replacement-secret", password)
}

func TestCardDAVCredentialsLoadRequiresPermissionBackendVerification(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	home := t.TempDir()
	require.NoError(SavePassword(testCredentialTokenDir(home), "secret"))
	injected := errors.New("injected DACL verification failure")
	permissions := &injectedCredentialPermissionFailure{
		delegate: nativeCredentialPermissions{}, verifyError: injected,
	}

	password, err := loadPasswordWithPermissions(testCredentialTokenDir(home), permissions)
	require.ErrorIs(err, injected)
	assertions.Empty(password)
	assertions.NotContains(err.Error(), "secret")
}
