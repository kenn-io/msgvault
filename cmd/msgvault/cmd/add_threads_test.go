package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/threads"
)

type threadsConnectFixture struct {
	server      *httptest.Server
	mu          sync.Mutex
	now         time.Time
	appID       string
	ownerID     string
	scopes      []string
	attempts    int
	beforeDebug func()
	beforeShort func()
	readHandler func(http.ResponseWriter, *http.Request) bool
}

func newThreadsConnectFixture(t *testing.T, owner string) *threadsConnectFixture {
	t.Helper()
	f := &threadsConnectFixture{now: time.Now().UTC().Add(time.Hour), ownerID: owner, scopes: []string{"threads_basic", "threads_read_replies"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.attempts++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/access_token":
			if f.beforeShort != nil {
				f.beforeShort()
			}
			if !assert.NoError(t, r.ParseForm()) {
				return
			}
			assert.Equal(t, "synthetic-app-secret", r.Form.Get("client_secret"))
			f.appID = r.Form.Get("client_id")
			_, _ = fmt.Fprintf(w, `{"access_token":"synthetic-short-token","user_id":%q}`, f.ownerID)
		case "/access_token":
			_, _ = fmt.Fprint(w, `{"access_token":"synthetic-long-token","expires_in":5184000}`)
		case "/me":
			_, _ = fmt.Fprintf(w, `{"id":%q,"username":"archive_owner","name":"Archive Owner"}`, f.ownerID)
		case "/debug_token":
			if f.beforeDebug != nil {
				f.beforeDebug()
			}
			expected := "TH|" + f.appID + "|synthetic-app-secret"
			assert.Equal(t, expected, r.URL.Query().Get("access_token"))
			scopes, err := json.Marshal(f.scopes)
			if !assert.NoError(t, err) {
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"app_id":%q,"user_id":%q,"is_valid":true,"issued_at":%d,"expires_at":%d,"data_access_expires_at":%d,"scopes":%s}}`, f.appID, f.ownerID, f.now.Add(-48*time.Hour).Unix(), f.now.Add(60*24*time.Hour).Unix(), f.now.Add(90*24*time.Hour).Unix(), scopes)
		default:
			if f.readHandler != nil && f.readHandler(w, r) {
				return
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestAddThreadsReauthorizationCannotRecreateRemovedSource(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err := executeAddThreadsFixture(t, deps, "123")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	before, err := deps.tokenManager().Load("123", "20")
	require.NoError(err)
	payload, err := json.Marshal(threadsOAuthPayload{AppID: "123", RedirectURI: "https://example.com/threads-callback", Code: "new-synthetic-code", AppSecret: "synthetic-app-secret"})
	require.NoError(err)
	deps.oauthPayload = func() string { return string(payload) }
	entered := make(chan struct{})
	provider.mu.Lock()
	provider.beforeShort = func() { close(entered) }
	provider.mu.Unlock()
	done := make(chan error, 1)
	err = deps.tokenManager().WithSourceLock(t.Context(), source.Identifier, func() error {
		go func() {
			cmd := newAddThreadsLocalCmd(deps)
			cmd.SetArgs([]string{"--app-id", "123", "--redirect-uri", "https://example.com/threads-callback"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			done <- cmd.ExecuteContext(t.Context())
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			require.FailNow("reauthorization did not reach owner discovery")
		}
		// The discovered binding now waits for this lifecycle lock. Removal wins
		// before it can obtain execution ownership or exchange a long-lived token.
		_, _, err := st.RemoveSourceSerialized(t.Context(), source.ID)
		return err
	})
	require.NoError(err)
	select {
	case err := <-done:
		require.ErrorContains(err, "changed while connecting")
	case <-time.After(5 * time.Second):
		require.FailNow("reauthorization did not finish after removal")
	}
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, source.Identifier)
	require.ErrorIs(err, store.ErrSourceNotFound)
	after, err := deps.tokenManager().Load("123", "20")
	require.NoError(err)
	assert.Equal(before.AccessToken(), after.AccessToken())
}
func (f *threadsConnectFixture) clock() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.now }
func (f *threadsConnectFixture) deps(st *store.Store, dir string) threadsCommandDeps {
	manager := threads.NewTokenManager(dir)
	return threadsCommandDeps{
		openStore: func() (*store.Store, func(), error) { return st, func() {}, nil }, tokenManager: func() *threads.TokenManager { return manager },
		oauthOptions: func() threads.OAuthClientOptions {
			return threads.OAuthClientOptions{HTTPClient: f.server.Client(), OAuthBaseURL: f.server.URL, Now: f.clock}
		},
		registerSource: registerThreadsSource, postSourceMigrations: func(*store.Store) error { return nil },
	}
}
func executeAddThreadsFixture(t *testing.T, deps threadsCommandDeps, appID string) (string, error) {
	t.Helper()
	payload, err := json.Marshal(threadsOAuthPayload{AppID: appID, RedirectURI: "https://example.com/threads-callback", Code: "synthetic-code", AppSecret: "synthetic-app-secret"})
	require.NoError(t, err)
	t.Setenv(clirun.EnvThreadsOAuth, string(payload))
	cmd := newAddThreadsLocalCmd(deps)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	args := []string{"--app-id", appID, "--redirect-uri", "https://example.com/threads-callback"}

	cmd.SetArgs(args)
	err = cmd.Execute()
	return output.String(), err
}

func TestAddThreadsDaemonExchange(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		require.True(st.IsPostgreSQL(), "connection lane must exercise PostgreSQL")
	}
	f := newThreadsConnectFixture(t, "9007199254740993123")
	deps := f.deps(st, t.TempDir())
	original := deps.registerSource
	deps.registerSource = func(ctx context.Context, s *store.Store, r threads.TokenRecord) (*store.Source, error) {
		saved, err := deps.tokenManager().Load(r.AppID, r.OwnerID)
		require.NoError(err)
		assert.NotEmpty(saved.AccessToken(), "credential must be durable before registration")
		return original(ctx, s, r)
	}
	out, err := executeAddThreadsFixture(t, deps, "123")
	require.NoError(err)
	const identifier = "threads:123:9007199254740993123"
	assert.Contains(out, identifier)
	assert.Contains(out, f.clock().Add(60*24*time.Hour).Format(time.RFC3339))
	assert.NotContains(out, "synthetic-app-secret")
	assert.NotContains(out, "synthetic-code")
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, identifier)
	require.NoError(err)
	assert.Equal("Archive Owner", source.DisplayName.String)
	identities, err := st.ListAccountIdentitiesContext(t.Context(), source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal(identifier, identities[0].Address)
	originalConfig := source.SyncConfig
	assert.False(originalConfig.Valid)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "Chosen name"))
	out, err = executeAddThreadsFixture(t, deps, "123")
	require.NoError(err)
	again, err := st.GetSourceByID(source.ID)
	require.NoError(err)
	assert.Equal(originalConfig, again.SyncConfig)
	assert.Equal("Chosen name", again.DisplayName.String)
	assert.Contains(out, identifier)
	_, err = executeAddThreadsFixture(t, deps, "124")
	require.NoError(err)
	other, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:124:9007199254740993123")
	require.NoError(err)
	assert.NotEqual(source.ID, other.ID)
}

func TestAddThreadsRegistrationFailureRecovery(t *testing.T) {
	for _, stage := range []string{"before source", "prior token", "after source", "setup"} {
		t.Run(stage, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			provider := newThreadsConnectFixture(t, "20")
			deps := provider.deps(st, t.TempDir())
			if stage == "prior token" {
				_, err := executeAddThreadsFixture(t, deps, "123")
				require.NoError(err)
				source, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:123:20")
				require.NoError(err)
				_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
				require.NoError(err)
			}
			if stage == "setup" {
				deps.postSourceMigrations = func(*store.Store) error { return errors.New("synthetic setup failure") }
			} else {
				deps.registerSource = func(ctx context.Context, st *store.Store, record threads.TokenRecord) (*store.Source, error) {
					if stage == "after source" {
						if _, err := registerThreadsSource(ctx, st, record); err != nil {
							return nil, err
						}
					}
					return nil, errors.New("synthetic registration failure")
				}
			}
			_, err := executeAddThreadsFixture(t, deps, "123")
			require.ErrorContains(err, "rerun add-threads")
			_, tokenErr := deps.tokenManager().Load("123", "20")
			_, sourceErr := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeThreads, "threads:123:20")
			if stage == "before source" {
				require.ErrorIs(tokenErr, threads.ErrTokenNotFound)
				require.ErrorIs(sourceErr, store.ErrSourceNotFound)
			} else {
				require.NoError(tokenErr)
				if stage == "prior token" {
					require.ErrorIs(sourceErr, store.ErrSourceNotFound)
				} else {
					require.NoError(sourceErr)
				}
			}
			deps.registerSource = registerThreadsSource
			deps.postSourceMigrations = func(*store.Store) error { return nil }
			_, err = executeAddThreadsFixture(t, deps, "123")
			require.NoError(err)
			sources, err := st.ListSourcesContext(t.Context(), sourceTypeThreads)
			require.NoError(err)
			assert.Len(sources, 1)
		})
	}
}

func TestAddThreadsRespectsDefaultIdentityOptOut(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(sourceTypeThreads, "threads:123:20")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(t.Context(), source.ID, `{"no_default_identity":true}`))
	provider := newThreadsConnectFixture(t, "20")
	deps := provider.deps(st, t.TempDir())
	_, err = executeAddThreadsFixture(t, deps, "123")
	require.NoError(err)
	identities, err := st.ListAccountIdentitiesContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Empty(t, identities)
}
