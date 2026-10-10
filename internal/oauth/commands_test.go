package oauth

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func secretCommand(t *testing.T, mode string) []string {
	t.Helper()
	return testutil.SecretCommand(t, mode)
}
func secretStoreFixture(t *testing.T) config.OAuthTokenCommands {
	t.Helper()
	return config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
}

func TestCommandTokenStoreRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := secretStoreFixture(t)
	dir := t.TempDir()
	store := NewTokenStore(dir, commands)
	_, err := store.Read(t.Context(), "reader@example.com")
	require.ErrorIs(err, os.ErrNotExist)
	data := []byte(`{"access_token":"example-token","scopes":["read"],"client_id":"example-client"}`)
	require.NoError(store.Write(t.Context(), "reader@example.com", data))
	got, err := store.Read(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal(data, got)
	accounts, err := store.List(t.Context())
	require.NoError(err)
	assert.Equal([]string{"reader@example.com"}, accounts)
	_, err = os.Stat(filepath.Join(dir, "reader@example.com.json"))
	require.ErrorIs(err, os.ErrNotExist)
	other := store.Namespace("contacts")
	_, err = other.Read(t.Context(), "reader@example.com")
	require.ErrorIs(err, os.ErrNotExist)
	accounts, err = other.List(t.Context())
	require.NoError(err)
	assert.Empty(accounts)
	require.NoError(store.Delete(t.Context(), "reader@example.com"))
	require.NoError(store.Delete(t.Context(), "reader@example.com"))
	_, err = store.Read(t.Context(), "reader@example.com")
	require.ErrorIs(err, os.ErrNotExist)
	require.NoError(store.Write(t.Context(), "reader@example.com", data))
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(dir, "reader@example.com.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err = store.Read(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal(data, got)
}

func TestSecretCommandLiteralArgvAndStdin(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	secretStoreFixture(t)
	argv := append(secretCommand(t, "echo"), "literal $HOME; `value` with spaces")
	got, err := runSecretCommand(t.Context(), argv, nil, nil)
	require.NoError(err)
	assert.Equal("literal $HOME; `value` with spaces", string(got))
	got, err = runSecretCommand(t.Context(), secretCommand(t, "stdin"), nil, []byte("example-input\n"))
	require.NoError(err)
	assert.Equal("example-input\n", string(got))
}

func TestSecretCommandFailuresAreBoundedAndRedacted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	secretStoreFixture(t)
	_, err := runSecretCommand(t.Context(), append(secretCommand(t, "fail"), "example-private-argument"), nil, nil)
	require.Error(err)
	assert.Contains(err.Error(), "7")
	for _, private := range []string{"example-private-output", "example-private-error", "example-private-argument"} {
		assert.NotContains(err.Error(), private)
	}
	_, err = runSecretCommand(t.Context(), secretCommand(t, "overflow"), nil, nil)
	require.ErrorIs(err, errSecretOutputLimit)
	assert.NotContains(err.Error(), strings.Repeat("x", 100))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runSecretCommand(ctx, secretCommand(t, "wait"), nil, nil)
	assert.ErrorIs(err, context.Canceled)
}

func TestSecretCommandCancelsRunningProcess(t *testing.T) {
	require := require.New(t)
	secretStoreFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	argv := secretCommand(t, "wait")
	go func() {
		_, err := runSecretCommand(ctx, argv, nil, nil)
		result <- err
	}()
	// Observe process startup before cancellation; polling throughput is irrelevant.
	require.Eventually(func() bool {
		_, err := os.Stat(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "started"))
		return err == nil
	}, time.Minute, 25*time.Millisecond)
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(time.Minute):
		require.Fail("credential command did not return after cancellation")
	}
}

func TestSecretCommandBoundsInheritedStdout(t *testing.T) {
	require := require.New(t)
	secretStoreFixture(t)
	root := os.Getenv("MSGVAULT_TEST_SECRET_ROOT")
	t.Cleanup(func() {
		data, err := os.ReadFile(filepath.Join(root, "child-pid"))
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil {
				process, err := os.FindProcess(pid)
				if err == nil {
					_ = process.Kill()
				}
			}
		}
	})
	result := make(chan error, 1)
	argv := secretCommand(t, "hold-pipe")
	go func() { _, err := runSecretCommand(t.Context(), argv, nil, nil); result <- err }()
	select {
	case err := <-result:
		require.Error(err)
		var exit *commandExitError
		require.NotErrorAs(err, &exit, "parent succeeded; an inherited pipe caused the failure")
	case <-time.After(time.Minute):
		require.Fail("credential command waited for its stdout-holding descendant")
	}
}

// Records are keyed by namespace and account, so moving the data directory or
// sharing a store between machines keeps finding the same tokens.
func TestCommandTokenKeysIgnoreLocalTokenDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commands := secretStoreFixture(t)
	data := []byte(`{"access_token":"example-token","client_id":"example-client"}`)
	require.NoError(NewTokenStore(t.TempDir(), commands).Write(t.Context(), "reader@example.com", data))

	moved := NewTokenStore(filepath.Join(t.TempDir(), "moved"), commands)
	got, err := moved.Read(t.Context(), "reader@example.com")
	require.NoError(err)
	assert.Equal(data, got)
	accounts, err := moved.List(t.Context())
	require.NoError(err)
	assert.Equal([]string{"reader@example.com"}, accounts)

	_, err = moved.Namespace("carddav-google/example").Read(t.Context(), "reader@example.com")
	require.ErrorIs(err, os.ErrNotExist)
}

func TestSecretCommandStartFailureIsConfiguration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	missing := filepath.Join(t.TempDir(), "example-missing-wrapper")
	_, err := runSecretCommand(t.Context(), []string{missing}, nil, nil)
	require.ErrorIs(err, ErrSecretCommandStart)
	assert.NotContains(err.Error(), missing)

	_, err = NewManagerWithCredentials(t.Context(), config.OAuthApp{ClientSecretsCommand: []string{missing}},
		t.TempDir(), config.OAuthTokenCommands{}, nil, Scopes)
	require.ErrorIs(err, ErrClientConfig)
	require.ErrorIs(err, ErrSecretCommandStart)
	assert.NotContains(err.Error(), missing)
}
