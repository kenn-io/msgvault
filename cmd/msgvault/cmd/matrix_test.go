package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReadMatrixSecretFilePreservesPasswordWhitespace(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "password")
	require.NoError(os.WriteFile(path, []byte("  password with spaces  \r\n"), 0o600))

	password, err := readMatrixSecretFile(path, false)
	require.NoError(err)
	assert.Equal(t, "  password with spaces  ", password)
	token, err := readMatrixSecretFile(path, true)
	require.NoError(err)
	assert.Equal(t, "password with spaces", token)
}

func TestRunConfiguredMatrixSyncRefreshesCacheAfterFailedAttempt(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	original := rebuildMatrixCacheAfterScheduledSync
	t.Cleanup(func() { rebuildMatrixCacheAfterScheduledSync = original })
	refreshErr := errors.New("synthetic cache refresh failure")
	var calls int
	rebuildMatrixCacheAfterScheduledSync = func(gotCtx context.Context, label string) error {
		calls++
		assert.Equal(t, "matrix", label)
		assert.NoError(t, gotCtx.Err())
		return refreshErr
	}

	err := runConfiguredMatrixSync(ctx, st)
	require.ErrorContains(err, "no Matrix accounts registered")
	require.ErrorIs(err, refreshErr)
	assert.Equal(t, 1, calls)
}

func retiredMatrixSyncFixture(t *testing.T) (*store.Store, *store.Source) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	retired, err := st.GetOrCreateSource(sourceTypeMatrix, "@history:example.org")
	require.NoError(err)
	active, err := st.GetOrCreateSource(sourceTypeMatrix, "@active:example.org")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: retired.ID,
		IntoSourceID: active.ID,
	})
	require.NoError(err)
	return st, retired
}

func TestRunMatrixSyncSkipsRetiredSourcesForAllAccounts(t *testing.T) {
	require := require.New(t)
	st, _ := retiredMatrixSyncFixture(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	active, err := st.GetSourceByTypeAndIdentifier(sourceTypeMatrix, "@active:example.org")
	require.NoError(err)
	var syncRequests int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncRequests++
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@active:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{
		Homeserver: server.URL, UserID: active.Identifier, DeviceID: "device", AccessToken: "token",
	}))

	err = runMatrixSync(t.Context(), st, cfg, "", false, nil, io.Discard)
	require.NoError(err)
	assert.Equal(t, 1, syncRequests, "only the active source is synced")
}

func TestRunMatrixSyncRejectsExplicitRetiredAccountBeforeCredentials(t *testing.T) {
	require := require.New(t)
	st, retired := retiredMatrixSyncFixture(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}

	err := runMatrixSync(t.Context(), st, cfg, retired.Identifier, false, nil, io.Discard)
	require.ErrorIs(err, store.ErrSourceRetired)
}

func TestAddMatrixRenewsExistingAccountInPlace(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var loggedOut []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@archive:example.org","device_id":"NEW","access_token":"fresh"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v3/logout", func(w http.ResponseWriter, r *http.Request) {
		loggedOut = append(loggedOut, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{
		Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "revoked",
	}))
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
	require.NoError(err)
	convID, err := st.EnsureConversation(source.ID, "!room:example.org", "Room")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{ConversationID: convID, SourceID: source.ID, SourceMessageID: "$kept", MessageType: sourceTypeMatrix})
	require.NoError(err)
	require.NoError(st.Close())

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvMatrixLoginSecret, "password")
	root := newTestRootCmd()
	root.AddCommand(newAddMatrixCmd())
	root.SetArgs([]string{"add-matrix", "--homeserver", server.URL, "--user-id", "@archive:example.org", "--no-default-identity"})
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	require.NoError(root.Execute())

	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), "@archive:example.org")
	require.NoError(err)
	assert.Equal("NEW", creds.DeviceID)
	assert.Equal([]string{"Bearer revoked"}, loggedOut)
	st, err = store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { _ = st.Close() }()
	renewed, err := st.GetOrCreateSource(sourceTypeMatrix, "@archive:example.org")
	require.NoError(err)
	assert.Equal(source.ID, renewed.ID)
	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(1), count)
}

func TestAddMatrixKeepsOldLoginWhenPreviousLogoutFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var loggedOut []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@archive:example.org","device_id":"NEW","access_token":"fresh"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v3/logout", func(w http.ResponseWriter, r *http.Request) {
		loggedOut = append(loggedOut, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer still-valid" {
			http.Error(w, `{"errcode":"M_UNKNOWN","error":"temporary failure"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	old := matrixsource.Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "still-valid"}
	require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), old))

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvMatrixLoginSecret, "password")
	root := newTestRootCmd()
	root.AddCommand(newAddMatrixCmd())
	root.SetArgs([]string{"add-matrix", "--homeserver", server.URL, "--user-id", "@archive:example.org", "--no-default-identity"})
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	require.ErrorContains(root.Execute(), "login unchanged")

	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), "@archive:example.org")
	require.NoError(err)
	assert.Equal(old, creds, "the old login stays so revocation can be retried")
	assert.Equal([]string{"Bearer still-valid"}, loggedOut, "the new device is kept for the retry")
	pending, ok, err := matrixsource.LoadPendingCredentials(cfg.TokensDir(), "@archive:example.org")
	require.NoError(err)
	require.True(ok)
	assert.Equal("NEW", pending.DeviceID)
}

func TestAddMatrixStopsWhenExistingLoginIsUnreadable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var loggedOut []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"@archive:example.org","device_id":"NEW","access_token":"fresh"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v3/logout", func(w http.ResponseWriter, r *http.Request) {
		loggedOut = append(loggedOut, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{
		Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "old",
	}))
	matches, err := filepath.Glob(filepath.Join(cfg.TokensDir(), "matrix_*.json"))
	require.NoError(err)
	require.Len(matches, 1)
	require.NoError(os.WriteFile(matches[0], []byte("not json"), 0o600))

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv(clirun.EnvMatrixLoginSecret, "password")
	root := newTestRootCmd()
	root.AddCommand(newAddMatrixCmd())
	root.SetArgs([]string{"add-matrix", "--homeserver", server.URL, "--user-id", "@archive:example.org", "--no-default-identity"})
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	require.ErrorContains(root.Execute(), "existing Matrix login")

	data, err := os.ReadFile(matches[0])
	require.NoError(err)
	assert.Equal("not json", string(data), "the unreadable file is left for the user to fix")
	assert.Equal([]string{"Bearer fresh"}, loggedOut, "only the new device is logged out again")
}

// renewalHomeserver issues device NEW and records logouts. A logged-out token
// then answers M_UNKNOWN_TOKEN, as a real homeserver does.
type renewalHomeserver struct {
	*httptest.Server

	mu         sync.Mutex
	logins     int
	loggedOut  []string
	revoked    map[string]bool
	hangLogout bool
	release    chan struct{}
	releaseOne sync.Once
}

func newRenewalHomeserver(t *testing.T) *renewalHomeserver {
	t.Helper()
	h := &renewalHomeserver{revoked: map[string]bool{}, release: make(chan struct{})}
	unknownToken := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"revoked"}`))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/login", func(w http.ResponseWriter, _ *http.Request) {
		h.mu.Lock()
		h.logins++
		delete(h.revoked, "Bearer fresh")
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"user_id":"@archive:example.org","device_id":"NEW","access_token":"fresh"}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/account/whoami", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.revoked[auth] || auth != "Bearer fresh" {
			unknownToken(w)
			return
		}
		_, _ = w.Write([]byte(`{"user_id":"@archive:example.org","device_id":"NEW"}`))
	})
	mux.HandleFunc("POST /_matrix/client/v3/logout", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		h.mu.Lock()
		h.loggedOut = append(h.loggedOut, auth)
		hang := h.hangLogout
		h.mu.Unlock()
		if hang {
			// Outlast the client's timeout without relying on disconnect detection.
			<-h.release
			return
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.revoked[auth] {
			unknownToken(w)
			return
		}
		h.revoked[auth] = true
		_, _ = w.Write([]byte(`{}`))
	})
	h.Server = httptest.NewServer(mux)
	t.Cleanup(h.Close)
	t.Cleanup(h.releaseHang)
	return h
}

func (h *renewalHomeserver) releaseHang() {
	h.releaseOne.Do(func() { close(h.release) })
}

func (h *renewalHomeserver) snapshot() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logins, append([]string(nil), h.loggedOut...)
}

func setupMatrixRenewal(t *testing.T) (*renewalHomeserver, *config.Config, matrixsource.Credentials) {
	t.Helper()
	server := newRenewalHomeserver(t)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	old := matrixsource.Credentials{Homeserver: server.URL, UserID: "@archive:example.org", DeviceID: "OLD", AccessToken: "old"}
	require.NoError(t, matrixsource.SaveCredentials(cfg.TokensDir(), old))
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	return server, cfg, old
}

func runAddMatrixForRenewal(t *testing.T, cfg *config.Config, homeserver, secret string) error {
	t.Helper()
	t.Setenv(clirun.EnvMatrixLoginSecret, secret)
	root := newTestRootCmd()
	root.AddCommand(newAddMatrixCmd())
	root.SetArgs([]string{"add-matrix", "--homeserver", homeserver, "--user-id", "@archive:example.org", "--no-default-identity"})
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err != nil {
		return fmt.Errorf("add-matrix: %w", err)
	}
	return nil
}

// failMatrixCredentialWrite makes one credential writer fail; an ErrPublished
// failure still writes the file first, like a failed directory sync.
func failMatrixCredentialWrite(t *testing.T, target *func(string, matrixsource.Credentials) error, fail error) {
	t.Helper()
	original := *target
	t.Cleanup(func() { *target = original })
	*target = func(dir string, creds matrixsource.Credentials) error {
		if errors.Is(fail, atomicfile.ErrPublished) {
			if err := original(dir, creds); err != nil {
				return err
			}
		}
		return fail
	}
}

func restoreMatrixCredentialWrites() {
	saveMatrixCredentials = matrixsource.SaveCredentials
	savePendingMatrixCredentials = matrixsource.SavePendingCredentials
}

func TestAddMatrixRenewalStagingFailureKeepsOldLogin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, cfg, old := setupMatrixRenewal(t)
	failMatrixCredentialWrite(t, &savePendingMatrixCredentials, errors.New("synthetic disk full"))

	require.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, "password"), "login unchanged")

	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal(old, creds)
	_, loggedOut := server.snapshot()
	assert.Equal([]string{"Bearer fresh"}, loggedOut, "only the unsaved new device is revoked")
	pending, err := matrixsource.PendingCredentialsExist(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.False(pending)
}

func TestAddMatrixRenewalResumesAfterPublishFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail error
	}{
		{"not published", errors.New("synthetic disk full")},
		{"published but not durable", fmt.Errorf("synthetic directory sync failure: %w", atomicfile.ErrPublished)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			server, cfg, old := setupMatrixRenewal(t)
			failMatrixCredentialWrite(t, &saveMatrixCredentials, tc.fail)

			require.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, "password"), "retry add-matrix to finish")
			pending, ok, err := matrixsource.LoadPendingCredentials(cfg.TokensDir(), old.UserID)
			require.NoError(err)
			require.True(ok, "the new login stays recoverable")
			assert.Equal("NEW", pending.DeviceID)
			_, loggedOut := server.snapshot()
			assert.Equal([]string{"Bearer old"}, loggedOut, "the saved new device is kept")

			restoreMatrixCredentialWrites()
			require.NoError(runAddMatrixForRenewal(t, cfg, server.URL, ""), "a retry needs no new login secret")
			creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
			require.NoError(err)
			assert.Equal(pending, creds)
			logins, loggedOut := server.snapshot()
			assert.Equal(1, logins, "the retry reuses the saved device")
			assert.NotContains(loggedOut, "Bearer fresh")
			exists, err := matrixsource.PendingCredentialsExist(cfg.TokensDir(), old.UserID)
			require.NoError(err)
			assert.False(exists)
		})
	}
}

func TestAddMatrixRenewalResumesPublishedPendingLogin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, cfg, old := setupMatrixRenewal(t)
	failMatrixCredentialWrite(t, &savePendingMatrixCredentials, fmt.Errorf("synthetic directory sync failure: %w", atomicfile.ErrPublished))

	require.Error(runAddMatrixForRenewal(t, cfg, server.URL, "password"))
	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal(old, creds, "the old login is untouched")
	_, loggedOut := server.snapshot()
	assert.Empty(loggedOut, "the published pending device is kept")

	// A resumed login is rewritten durably before the old device is revoked.
	failMatrixCredentialWrite(t, &savePendingMatrixCredentials, errors.New("synthetic disk full"))
	require.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, ""), "login unchanged")
	_, loggedOut = server.snapshot()
	assert.Empty(loggedOut, "both logins survive a second staging failure")

	restoreMatrixCredentialWrites()
	require.NoError(runAddMatrixForRenewal(t, cfg, server.URL, ""))
	creds, err = matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal("NEW", creds.DeviceID)
	logins, loggedOut := server.snapshot()
	assert.Equal(1, logins)
	assert.Equal([]string{"Bearer old"}, loggedOut)
}

func TestAddMatrixRenewalDiscardsRevokedPendingLogin(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, cfg, old := setupMatrixRenewal(t)
	revoked := matrixsource.Credentials{Homeserver: server.URL, UserID: old.UserID, DeviceID: "GONE", AccessToken: "gone"}
	require.NoError(matrixsource.SavePendingCredentials(cfg.TokensDir(), revoked))

	require.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, ""), "run add-matrix again")
	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal(old, creds, "a revoked pending login never replaces the working one")
	_, loggedOut := server.snapshot()
	assert.Empty(loggedOut)
	exists, err := matrixsource.PendingCredentialsExist(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.False(exists)

	require.NoError(runAddMatrixForRenewal(t, cfg, server.URL, "password"))
	creds, err = matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal("NEW", creds.DeviceID)
}

func TestAddMatrixRenewalLogoutTimeoutKeepsBothLoginsRecoverable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server, cfg, old := setupMatrixRenewal(t)
	original := matrixRequestTimeout
	t.Cleanup(func() { matrixRequestTimeout = original })
	matrixRequestTimeout = 50 * time.Millisecond
	server.mu.Lock()
	server.hangLogout = true
	server.mu.Unlock()

	require.ErrorContains(runAddMatrixForRenewal(t, cfg, server.URL, "password"), "login unchanged")
	creds, err := matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal(old, creds, "the old login keeps working")
	pending, ok, err := matrixsource.LoadPendingCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	require.True(ok)
	assert.Equal("NEW", pending.DeviceID)

	server.mu.Lock()
	server.hangLogout = false
	server.mu.Unlock()
	server.releaseHang()
	matrixRequestTimeout = original
	require.NoError(runAddMatrixForRenewal(t, cfg, server.URL, ""))
	creds, err = matrixsource.LoadCredentials(cfg.TokensDir(), old.UserID)
	require.NoError(err)
	assert.Equal(pending, creds)
	logins, loggedOut := server.snapshot()
	assert.Equal(1, logins)
	assert.NotContains(loggedOut, "Bearer fresh")
}
