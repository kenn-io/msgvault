package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	xapi "go.kenn.io/msgvault/internal/x"
)

type xRegistrationFixture struct {
	st               *store.Store
	cfg              *config.Config
	deps             xCommandDeps
	server, provider *httptest.Server
	meCalls          atomic.Int32
	mu               sync.Mutex
	newOwner         string
	rejectNew        bool
	entered, release chan struct{}
}

func xTestConfig(t *testing.T) *config.Config {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = dir
	cfg.Data.DataDir = dir
	cfg.Server.AgentAccess = true
	return cfg
}
func xTestRecord(access string) xapi.TokenRecord {
	r := xapi.NewTokenRecord("1001", "example-client", xapi.PublicClient, access, "example-refresh", "")
	r.Username = "example_user"
	r.GrantedScopes = []string{"tweet.read", "users.read", "offline.access", "tweet.write"}
	r.Expiry = time.Now().Add(time.Hour)
	return r
}
func xTestCallbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := address.Port
	require.NoError(t, listener.Close())
	return port
}
func newXRegistrationFixture(t *testing.T, ownerKeys ...string) *xRegistrationFixture {
	t.Helper()
	f := &xRegistrationFixture{st: testutil.NewTestStore(t), cfg: xTestConfig(t), newOwner: "1001"}
	if len(ownerKeys) > 0 {
		f.cfg.Server.APIKey = ownerKeys[0]
	}
	checks := assert.New(t)
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			if !checks.NoError(r.ParseForm()) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			checks.Equal("authorization_code", r.Form.Get("grant_type"))
			_, _ = fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"example-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access tweet.write"}`)
			return
		}
		assert.Equal(t, "/2/users/me", r.URL.Path)
		assert.Contains(t, []string{"Bearer old-access", "Bearer new-access", "Bearer latest-access"}, r.Header.Get("Authorization"))
		f.meCalls.Add(1)
		f.mu.Lock()
		owner, reject, entered, release := f.newOwner, f.rejectNew, f.entered, f.release
		f.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer old-access" {
			owner = "1001"
			reject = false
			entered = nil
		}
		if entered != nil {
			close(entered)
			<-release
		}
		if reject {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"title":"Synthetic denial"}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id":%q,"username":"example_user","name":"Example Author","protected":false}}`, owner)
	}))
	t.Cleanup(f.provider.Close)
	f.deps = xCommandDeps{apiBaseURL: f.provider.URL + "/2", httpClient: f.provider.Client(), oauthOptions: xapi.OAuthOptions{AuthURL: f.provider.URL + "/authorize", TokenURL: f.provider.URL + "/token", APIBaseURL: f.provider.URL + "/2", CallbackPort: xTestCallbackPort(t)}}
	f.deps.oauthOptions.OpenBrowser = func(_ context.Context, raw string) error {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		resp, err := http.Get(u.Query().Get("redirect_uri") + "?code=example-code&state=" + url.QueryEscape(u.Query().Get("state")))
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		return nil
	}
	adapter := &storeAPIAdapter{store: f.st, config: f.cfg, logger: slog.New(slog.DiscardHandler), xDeps: &f.deps}
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}
func (f *xRegistrationFixture) clientConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := xTestConfig(t)
	cfg.Remote.URL = f.server.URL
	cfg.Remote.AllowInsecure = true
	cfg.Remote.APIKey = f.cfg.Server.AuthenticationKey()
	return cfg
}
func (f *xRegistrationFixture) run(t *testing.T, cfg *config.Config, args ...string) (string, error) {
	t.Helper()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	root := &cobra.Command{Use: "msgvault"}
	root.SetContext(ctx)
	root.AddCommand(newAddXCmd(f.deps))
	silenceUsageInRunE(root)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"add-x"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}
func stageXRecord(t *testing.T, cfg *config.Config, record xapi.TokenRecord) {
	t.Helper()
	record.Generation = 0
	require.NoError(t, xapi.NewTokenManager(xStagedTokensDir(cfg)).Save(t.Context(), record))
}
func copyXStage(t *testing.T, from, to *config.Config) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(xStagedTokensDir(from), "x_1001.json"))
	require.NoError(t, err)
	require.NoError(t, fileutil.SecureMkdirAll(xStagedTokensDir(to), 0700))
	require.NoError(t, fileutil.SecureReplaceFile(filepath.Join(xStagedTokensDir(to), "x_1001.json"), data, 0600))
}
func TestAddXRemoteAuthorizationDoesNotRegisterLocalPath(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	local := f.clientConfig(t)
	output, err := f.run(t, local, "--client-id", "example-client", "--allow-send")
	_testRequire.NoError(err)
	sources, err := f.st.ListSources("x")
	_testRequire.NoError(err)
	_testAssert.Empty(sources)
	_testAssert.Equal(int32(1), f.meCalls.Load())
	_testAssert.Contains(output, "--register")
	_testAssert.Contains(output, "--user-id")
	_testAssert.Contains(output, "daemon")
	_testAssert.NotContains(output, "new-access")
	_testAssert.NotContains(output, "example-refresh")
	_, err = xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	_testRequire.ErrorIs(err, os.ErrNotExist)
	copyXStage(t, local, f.cfg)
	output, err = f.run(t, local, "--register", "--user-id", "1001", "--json")
	_testRequire.NoError(err)
	var registered struct {
		SourceID   int64  `json:"source_id"`
		UserID     string `json:"user_id"`
		Generation int64  `json:"generation"`
	}
	_testRequire.NoError(json.Unmarshal([]byte(output), &registered))
	_testAssert.Equal("1001", registered.UserID)
	_testAssert.Positive(registered.SourceID)
	source, err := f.st.GetSourceByID(registered.SourceID)
	_testRequire.NoError(err)
	_testAssert.Equal("x", source.SourceType)
	_testAssert.Equal("1001", source.Identifier)
	_testAssert.Equal("@example_user", source.DisplayName.String)
	_testAssert.Equal(int32(2), f.meCalls.Load())
	identities, err := f.st.ListAccountIdentities(source.ID)
	_testRequire.NoError(err)
	_testRequire.Len(identities, 1)
	_testAssert.Equal("1001", identities[0].Address)
	var owners int
	_testRequire.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1001'`).Scan(&owners))
	_testAssert.Equal(1, owners)
}

func TestRegisterXGenerationRecoveryPreservesHistory(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(strconv.FormatBool(copied), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXRegistrationFixture(t)
			source, err := f.st.GetOrCreateSource("x", "1001")
			_testRequire.NoError(err)
			alias := "example-x-owner"
			_, err = f.st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
			_testRequire.NoError(err)
			state, err := f.st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			generation := int64(100)
			credentialError := "invalid_grant"
			spend := []store.XSpendBucket{{Month: "2026-10", SpentMicros: 1234, ReservedMicros: 15000}}
			reservations := []store.XReservation{{ID: "reservation-1", Kind: "timeline", Month: "2026-10", AmountMicros: 15000, State: "unknown"}}
			walk := store.XWalk{Endpoint: "own", Fingerprint: "frozen-window", Token: "saved-page", Watermark: "200"}
			_, err = f.st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError, Own: &walk, SpendBuckets: &spend, Reservations: &reservations})
			_testRequire.NoError(err)
			_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position) VALUES(?,?,?,?)`), source.ID, "200", "200", 0)
			_testRequire.NoError(err)
			_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), source.ID, "1001", "draft-example", "attempt-example", 1, "200", "200", "synthetic-digest", "remote_unknown", "reservation-1")
			_testRequire.NoError(err)
			record := xTestRecord("new-access")
			record.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
			if copied {
				_testRequire.NoError(xapi.NewTokenManager(f.cfg.TokensDir()).Save(t.Context(), record))
			} else {
				stageXRecord(t, f.cfg, record)
			}
			output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
			_testRequire.NoError(err)
			_testAssert.NotContains(output, "new-access")
			current, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
			_testRequire.NoError(err)
			_testAssert.Equal(source.ID, current.ID)
			_testAssert.Equal(alias, current.Alias)
			after, err := f.st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(int64(101), after.Generation)
			_testAssert.Empty(after.CredentialError)
			_testAssert.Equal(spend, after.SpendBuckets)
			_testAssert.Equal(reservations, after.Reservations)
			_testAssert.Equal(&walk, after.Own)
			loaded, err := xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
			_testRequire.NoError(err)
			_testAssert.Equal(int64(101), loaded.Generation)
			_testAssert.NotContains(loaded.GrantedScopes, "tweet.write")
			var aliases, attempts int
			_testRequire.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM x_post_versions`).Scan(&aliases))
			_testRequire.NoError(f.st.DB().QueryRow(`SELECT count(*) FROM x_reply_attempts`).Scan(&attempts))
			_testAssert.Equal(1, aliases)
			_testAssert.Equal(1, attempts)
			_, err = f.st.UpdateXControlContext(t.Context(), source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError})
			_testRequire.Error(err)
			fenced, err := f.st.LoadXControlContext(t.Context(), source.ID)
			_testRequire.NoError(err)
			_testAssert.Equal(after, fenced)
		})
	}
}

func TestRegisterXFailedReplacementKeepsOldCredentials(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			f := newXRegistrationFixture(t)
			manager := xapi.NewTokenManager(f.cfg.TokensDir())
			_testRequire.NoError(manager.Save(t.Context(), xTestRecord("old-access")))
			_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
			_testRequire.NoError(err)
			before, err := manager.Load(t.Context(), "1001")
			_testRequire.NoError(err)
			stageXRecord(t, f.cfg, xTestRecord("new-access"))
			f.mu.Lock()
			if mismatch {
				f.newOwner = "1002"
			} else {
				f.rejectNew = true
			}
			f.mu.Unlock()
			output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
			_testRequire.Error(err)
			_testAssert.NotContains(output, "new-access")
			after, err := manager.Load(t.Context(), "1001")
			_testRequire.NoError(err)
			_testAssert.Equal(before.Generation, after.Generation)
			_testAssert.Equal("old-access", after.AccessToken())
			c := manager.Credentials("1001", f.provider.Client())
			_, err = c.Prepare(t.Context())
			_testRequire.NoError(err)
			owner, err := xapi.NewClient(f.provider.URL+"/2", c.HTTPClient()).Me(t.Context())
			_testRequire.NoError(err)
			_testAssert.Equal("1001", owner.ID)
			sources, err := f.st.ListSources("x")
			_testRequire.NoError(err)
			_testRequire.Len(sources, 1)
		})
	}
}

func TestRegisterXFencesCredentialReplacement(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	_testRequire.NoError(manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	_testRequire.NoError(err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	before, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	stageXRecord(t, f.cfg, xTestRecord("new-access"))
	entered, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.entered = entered
	f.release = release
	f.mu.Unlock()
	done := make(chan error, 1)
	clientConfig := f.clientConfig(t)
	go func() { _, err := f.run(t, clientConfig, "--register", "--user-id", "1001"); done <- err }()
	<-entered
	_, err = f.st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.ErrorIs(err, store.ErrSyncAlreadyActive)
	during, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal(before.Generation, during.Generation)
	close(release)
	_testRequire.NoError(<-done)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	after, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	state, err := f.st.LoadXControlContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	_testAssert.Equal(after.Generation, state.Generation)
}

func TestAddXRejectsUnsafeArgumentsAndHeadlessDoesNotAuthorize(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	for _, args := range [][]string{{"--register"}, {"--register", "--user-id", "01"}, {"--register", "--user-id", "1001", "--client-id", "example-client"}, {"--user-id", "1001"}, {"--token", "secret"}, {"--token-file", "/tmp/secret"}, {"--client-secret", "secret"}} {
		_, err := f.run(t, f.clientConfig(t), args...)
		_testRequire.Error(err)
	}
	output, err := f.run(t, f.clientConfig(t), "--headless")
	_testRequire.NoError(err)
	_testAssert.Contains(output, "--client-id")
	_testAssert.Contains(output, "--register")
	_testAssert.Zero(f.meCalls.Load())
}

func TestRegisterXHTTPRejectsEnvironmentCwdAndRemoteAuthorization(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	valid := []string{"add-x", "--register", "--user-id", "1001"}
	for _, request := range []api.CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{clirun.EnvDiscordToken: "synthetic-secret"}}, {Args: []string{"add-x", "--client-id", "example-client"}}, {Args: []string{"add-x", "--register", "--user-id", "1001", "--token-file", "/tmp/example"}}} {
		body, err := json.Marshal(request)
		_testRequire.NoError(err)
		response, err := http.Post(f.server.URL+"/api/v1/cli/run", "application/json", bytes.NewReader(body))
		_testRequire.NoError(err)
		_testRequire.NoError(response.Body.Close())
		_testAssert.Equal(http.StatusBadRequest, response.StatusCode)
	}
	_testAssert.Zero(f.meCalls.Load())
}

func TestRegisterXHTTPRejectsDelegatedGrant(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t, "example-owner-key")
	source, err := f.st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	body := fmt.Sprintf(`{"label":"example-agent","permissions":["draft.create"],"source_ids":[%d]}`, source.ID)
	ownerRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/agent-tokens", strings.NewReader(body))
	_testRequire.NoError(err)
	ownerRequest.Header.Set("Content-Type", "application/json")
	ownerRequest.Header.Set("X-Api-Key", "example-owner-key")
	response, err := http.DefaultClient.Do(ownerRequest)
	_testRequire.NoError(err)
	data, err := io.ReadAll(response.Body)
	_testRequire.NoError(err)
	_testRequire.NoError(response.Body.Close())
	_testRequire.Equal(http.StatusCreated, response.StatusCode, string(data))
	var grant struct {
		Secret string `json:"secret"`
	}
	_testRequire.NoError(json.Unmarshal(data, &grant))
	_testRequire.NotEmpty(grant.Secret)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", strings.NewReader(`{"args":["add-x","--register","--user-id","1001"]}`))
	_testRequire.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	response, err = http.DefaultClient.Do(request)
	_testRequire.NoError(err)
	_testRequire.NoError(response.Body.Close())
	_testAssert.Equal(http.StatusBadRequest, response.StatusCode)
	_testAssert.Zero(f.meCalls.Load())
}

func TestAddXStagesReauthorizationWithoutHoldingSourceOwnership(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	_testRequire.NoError(manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	_testRequire.NoError(err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	before, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	original := f.deps.oauthOptions.OpenBrowser
	f.deps.oauthOptions.OpenBrowser = func(ctx context.Context, url string) error {
		current, err := manager.Load(ctx, "1001")
		require.NoError(t, err)
		assert.Equal(t, before.Generation, current.Generation)
		assert.Equal(t, "old-access", current.AccessToken())
		execution, err := f.st.AcquireSyncExecutionContext(ctx, source.ID)
		require.NoError(t, err)
		require.NoError(t, execution.Release())
		return original(ctx, url)
	}
	local := *f.cfg
	local.Remote.URL = f.server.URL
	local.Remote.AllowInsecure = true
	_, err = f.run(t, &local, "--client-id", "example-client")
	_testRequire.NoError(err)
	current, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal(before.Generation, current.Generation)
	_testAssert.Equal("old-access", current.AccessToken())
	staged, err := xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal("new-access", staged.AccessToken())
}

func TestRegisterXRejectsChangedStagedAuthorization(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	_testRequire.NoError(manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	_testRequire.NoError(err)
	before, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	stageXRecord(t, f.cfg, xTestRecord("new-access"))
	entered, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.entered = entered
	f.release = release
	f.mu.Unlock()
	done := make(chan error, 1)
	client := f.clientConfig(t)
	go func() { _, err := f.run(t, client, "--register", "--user-id", "1001"); done <- err }()
	<-entered
	stageXRecord(t, f.cfg, xTestRecord("latest-access"))
	close(release)
	_testRequire.Error(<-done)
	after, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal(before.Generation, after.Generation)
	_testAssert.Equal("old-access", after.AccessToken())
	staged, err := xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal("latest-access", staged.AccessToken())
}

func TestRegisterXDifferentOwnerCreatesSeparateSource(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	_testRequire.NoError(manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	_testRequire.NoError(err)
	original, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	_testRequire.NoError(err)
	before, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	f.mu.Lock()
	f.newOwner = "1002"
	f.mu.Unlock()
	replacement := xTestRecord("new-access")
	replacement.OwnerID = "1002"
	stageXRecord(t, f.cfg, replacement)
	output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1002", "--json")
	_testRequire.NoError(err)
	var registered xRegistrationOutput
	_testRequire.NoError(json.Unmarshal([]byte(output), &registered))
	_testAssert.Equal("1002", registered.UserID)
	_testAssert.NotEqual(original.ID, registered.SourceID)
	sources, err := f.st.ListSources("x")
	_testRequire.NoError(err)
	_testAssert.Len(sources, 2)
	current, err := manager.Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_testAssert.Equal(before.Generation, current.Generation)
	_testAssert.Equal("old-access", current.AccessToken())
	_, err = xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1002")
	_testRequire.ErrorIs(err, os.ErrNotExist)
}

func TestAddXDelegatedAuthorizationDoesNotContactProvider(t *testing.T) {
	f := newXRegistrationFixture(t)
	ctx := testInvocationContext(t.Context(), f.clientConfig(t), invocationOptions{agentURL: "http://127.0.0.1:1", agentTokenFile: "unused-example-token"})
	command := newAddXCmd(f.deps)
	command.SetArgs([]string{"--client-id", "example-client"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.Error(t, command.ExecuteContext(ctx))
	assert.Zero(t, f.meCalls.Load())
}

func TestAddXLocalAuthorizationRegistersThroughDiscoveredDaemon(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	f := newXRegistrationFixture(t)
	runtime := daemonRuntimeForHTTPServer(t, f.server, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(f.cfg.Data.DataDir).Write(runtime.Record)
	_testRequire.NoError(err)
	output, err := f.run(t, f.cfg, "--client-id", "example-client")
	_testRequire.NoError(err)
	_testAssert.Contains(output, "Registered X account")
	sources, err := f.st.ListSources("x")
	_testRequire.NoError(err)
	_testRequire.Len(sources, 1)
	_testAssert.Equal("1001", sources[0].Identifier)
	_testAssert.Equal(int32(2), f.meCalls.Load())
	_, err = xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	_testRequire.NoError(err)
	_, err = xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	_testRequire.ErrorIs(err, os.ErrNotExist)
}

func TestAddXHeadlessPreservesCallbackPort(t *testing.T) {
	_testAssert := assert.New(t)

	f := newXRegistrationFixture(t)
	output, err := f.run(t, f.clientConfig(t), "--headless", "--client-id", "example-client", "--callback-port", "9876")
	require.NoError(t, err)
	_testAssert.Contains(output, "--callback-port")
	_testAssert.Contains(output, "9876")
	_testAssert.Zero(f.meCalls.Load())
}
