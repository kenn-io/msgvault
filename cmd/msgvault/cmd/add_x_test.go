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
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}
func newXRegistrationFixture(t *testing.T, ownerKeys ...string) *xRegistrationFixture {
	t.Helper()
	f := &xRegistrationFixture{st: testutil.NewTestStore(t), cfg: xTestConfig(t), newOwner: "1001"}
	if len(ownerKeys) > 0 {
		f.cfg.Server.APIKey = ownerKeys[0]
	}
	f.provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			require.NoError(t, r.ParseForm())
			assert.Equal(t, "authorization_code", r.Form.Get("grant_type"))
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"example-refresh","token_type":"bearer","expires_in":7200,"scope":"tweet.read users.read offline.access tweet.write"}`)
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
			w.WriteHeader(403)
			fmt.Fprint(w, `{"title":"Synthetic denial"}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"id":%q,"username":"example_user","name":"Example Author","protected":false}}`, owner)
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
	f := newXRegistrationFixture(t)
	local := f.clientConfig(t)
	output, err := f.run(t, local, "--client-id", "example-client", "--allow-send")
	require.NoError(t, err)
	sources, err := f.st.ListSources("x")
	require.NoError(t, err)
	assert.Empty(t, sources)
	assert.Equal(t, int32(1), f.meCalls.Load())
	assert.Contains(t, output, "--register")
	assert.Contains(t, output, "--user-id")
	assert.Contains(t, output, "daemon")
	assert.NotContains(t, output, "new-access")
	assert.NotContains(t, output, "example-refresh")
	_, err = xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	require.ErrorIs(t, err, os.ErrNotExist)
	copyXStage(t, local, f.cfg)
	output, err = f.run(t, local, "--register", "--user-id", "1001", "--json")
	require.NoError(t, err)
	var registered struct {
		SourceID   int64  `json:"source_id"`
		UserID     string `json:"user_id"`
		Generation int64  `json:"generation"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &registered))
	assert.Equal(t, "1001", registered.UserID)
	assert.Positive(t, registered.SourceID)
	source, err := f.st.GetSourceByID(registered.SourceID)
	require.NoError(t, err)
	assert.Equal(t, "x", source.SourceType)
	assert.Equal(t, "1001", source.Identifier)
	assert.Equal(t, "@example_user", source.DisplayName.String)
	assert.Equal(t, int32(2), f.meCalls.Load())
	identities, err := f.st.ListAccountIdentities(source.ID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	assert.Equal(t, "1001", identities[0].Address)
	var owners int
	require.NoError(t, f.st.DB().QueryRow(`SELECT count(*) FROM participant_identifiers WHERE identifier_type='x_user_id' AND identifier_value='1001'`).Scan(&owners))
	assert.Equal(t, 1, owners)
}

func TestRegisterXGenerationRecoveryPreservesHistory(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(fmt.Sprint(copied), func(t *testing.T) {
			f := newXRegistrationFixture(t)
			source, err := f.st.GetOrCreateSource("x", "1001")
			require.NoError(t, err)
			alias := "example-x-owner"
			_, err = f.st.UpdateSourceSettingsContext(t.Context(), source.ID, store.SourceSettingsUpdate{Alias: &alias})
			require.NoError(t, err)
			state, err := f.st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			generation := int64(100)
			credentialError := "invalid_grant"
			spend := []store.XSpendBucket{{Month: "2026-10", SpentMicros: 1234, ReservedMicros: 15000}}
			reservations := []store.XReservation{{ID: "reservation-1", Kind: "timeline", Month: "2026-10", AmountMicros: 15000, State: "unknown"}}
			walk := store.XWalk{Endpoint: "own", Fingerprint: "frozen-window", Token: "saved-page", Watermark: "200"}
			state, err = f.st.UpdateXControlContext(t.Context(), source.ID, state.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError, Own: &walk, SpendBuckets: &spend, Reservations: &reservations})
			require.NoError(t, err)
			_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_post_versions(source_id,post_id,canonical_id,version_position) VALUES(?,?,?,?)`), source.ID, "200", "200", 0)
			require.NoError(t, err)
			_, err = f.st.DB().Exec(f.st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), source.ID, "1001", "draft-example", "attempt-example", 1, "200", "200", "synthetic-digest", "remote_unknown", "reservation-1")
			require.NoError(t, err)
			record := xTestRecord("new-access")
			record.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
			if copied {
				require.NoError(t, xapi.NewTokenManager(f.cfg.TokensDir()).Save(t.Context(), record))
			} else {
				stageXRecord(t, f.cfg, record)
			}
			output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001", "--json")
			require.NoError(t, err)
			assert.NotContains(t, output, "new-access")
			current, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
			require.NoError(t, err)
			assert.Equal(t, source.ID, current.ID)
			assert.Equal(t, alias, current.Alias)
			after, err := f.st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.Equal(t, int64(101), after.Generation)
			assert.Empty(t, after.CredentialError)
			assert.Equal(t, spend, after.SpendBuckets)
			assert.Equal(t, reservations, after.Reservations)
			assert.Equal(t, &walk, after.Own)
			loaded, err := xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
			require.NoError(t, err)
			assert.Equal(t, int64(101), loaded.Generation)
			assert.NotContains(t, loaded.GrantedScopes, "tweet.write")
			var aliases, attempts int
			require.NoError(t, f.st.DB().QueryRow(`SELECT count(*) FROM x_post_versions`).Scan(&aliases))
			require.NoError(t, f.st.DB().QueryRow(`SELECT count(*) FROM x_reply_attempts`).Scan(&attempts))
			assert.Equal(t, 1, aliases)
			assert.Equal(t, 1, attempts)
			_, err = f.st.UpdateXControlContext(t.Context(), source.ID, after.Revision, store.XControlPatch{OwnerID: "1001", Generation: &generation, CredentialError: &credentialError})
			require.Error(t, err)
			fenced, err := f.st.LoadXControlContext(t.Context(), source.ID)
			require.NoError(t, err)
			assert.Equal(t, after, fenced)
		})
	}
}

func TestRegisterXFailedReplacementKeepsOldCredentials(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			f := newXRegistrationFixture(t)
			manager := xapi.NewTokenManager(f.cfg.TokensDir())
			require.NoError(t, manager.Save(t.Context(), xTestRecord("old-access")))
			_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
			require.NoError(t, err)
			before, err := manager.Load(t.Context(), "1001")
			require.NoError(t, err)
			stageXRecord(t, f.cfg, xTestRecord("new-access"))
			f.mu.Lock()
			if mismatch {
				f.newOwner = "1002"
			} else {
				f.rejectNew = true
			}
			f.mu.Unlock()
			output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
			require.Error(t, err)
			assert.NotContains(t, output, "new-access")
			after, err := manager.Load(t.Context(), "1001")
			require.NoError(t, err)
			assert.Equal(t, before.Generation, after.Generation)
			assert.Equal(t, "old-access", after.AccessToken())
			c := manager.Credentials("1001", f.provider.Client())
			_, err = c.Prepare(t.Context())
			require.NoError(t, err)
			owner, err := xapi.NewClient(f.provider.URL+"/2", c.HTTPClient()).Me(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "1001", owner.ID)
			sources, err := f.st.ListSources("x")
			require.NoError(t, err)
			require.Len(t, sources, 1)
		})
	}
}

func TestRegisterXFencesCredentialReplacement(t *testing.T) {
	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	require.NoError(t, err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	require.NoError(t, err)
	before, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	stageXRecord(t, f.cfg, xTestRecord("new-access"))
	entered, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.entered = entered
	f.release = release
	f.mu.Unlock()
	done := make(chan error, 1)
	go func() { _, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001"); done <- err }()
	<-entered
	_, err = f.st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
	during, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, before.Generation, during.Generation)
	close(release)
	require.NoError(t, <-done)
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), source.ID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	after, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	state, err := f.st.LoadXControlContext(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Equal(t, after.Generation, state.Generation)
}

func TestAddXRejectsUnsafeArgumentsAndHeadlessDoesNotAuthorize(t *testing.T) {
	f := newXRegistrationFixture(t)
	for _, args := range [][]string{{"--register"}, {"--register", "--user-id", "01"}, {"--register", "--user-id", "1001", "--client-id", "example-client"}, {"--user-id", "1001"}, {"--token", "secret"}, {"--token-file", "/tmp/secret"}, {"--client-secret", "secret"}} {
		_, err := f.run(t, f.clientConfig(t), args...)
		require.Error(t, err)
	}
	output, err := f.run(t, f.clientConfig(t), "--headless")
	require.NoError(t, err)
	assert.Contains(t, output, "--client-id")
	assert.Contains(t, output, "--register")
	assert.Zero(t, f.meCalls.Load())
}

func TestRegisterXHTTPRejectsEnvironmentCwdAndRemoteAuthorization(t *testing.T) {
	f := newXRegistrationFixture(t)
	valid := []string{"add-x", "--register", "--user-id", "1001"}
	for _, request := range []api.CLIRunRequest{{Args: valid, Cwd: "/tmp"}, {Args: valid, Env: map[string]string{clirun.EnvDiscordToken: "synthetic-secret"}}, {Args: []string{"add-x", "--client-id", "example-client"}}, {Args: []string{"add-x", "--register", "--user-id", "1001", "--token-file", "/tmp/example"}}} {
		body, err := json.Marshal(request)
		require.NoError(t, err)
		response, err := http.Post(f.server.URL+"/api/v1/cli/run", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	}
	assert.Zero(t, f.meCalls.Load())
}

func TestRegisterXHTTPRejectsDelegatedGrant(t *testing.T) {
	f := newXRegistrationFixture(t, "example-owner-key")
	source, err := f.st.GetOrCreateSource("x", "1001")
	require.NoError(t, err)
	body := fmt.Sprintf(`{"label":"example-agent","permissions":["draft.create"],"source_ids":[%d]}`, source.ID)
	ownerRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/agent-tokens", strings.NewReader(body))
	require.NoError(t, err)
	ownerRequest.Header.Set("Content-Type", "application/json")
	ownerRequest.Header.Set("X-Api-Key", "example-owner-key")
	response, err := http.DefaultClient.Do(ownerRequest)
	require.NoError(t, err)
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusCreated, response.StatusCode, string(data))
	var grant struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(data, &grant))
	require.NotEmpty(t, grant.Secret)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.server.URL+"/api/v1/cli/run", strings.NewReader(`{"args":["add-x","--register","--user-id","1001"]}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, grant.Secret)
	response, err = http.DefaultClient.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
	assert.Zero(t, f.meCalls.Load())
}

func TestAddXStagesReauthorizationWithoutHoldingSourceOwnership(t *testing.T) {
	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	require.NoError(t, err)
	source, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	require.NoError(t, err)
	before, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
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
	require.NoError(t, err)
	current, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, before.Generation, current.Generation)
	assert.Equal(t, "old-access", current.AccessToken())
	staged, err := xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, "new-access", staged.AccessToken())
}

func TestRegisterXRejectsChangedStagedAuthorization(t *testing.T) {
	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	require.NoError(t, err)
	before, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
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
	require.Error(t, <-done)
	after, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, before.Generation, after.Generation)
	assert.Equal(t, "old-access", after.AccessToken())
	staged, err := xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, "latest-access", staged.AccessToken())
}

func TestRegisterXDifferentOwnerCreatesSeparateSource(t *testing.T) {
	f := newXRegistrationFixture(t)
	manager := xapi.NewTokenManager(f.cfg.TokensDir())
	require.NoError(t, manager.Save(t.Context(), xTestRecord("old-access")))
	_, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1001")
	require.NoError(t, err)
	original, err := f.st.GetSourceByTypeAndIdentifier("x", "1001")
	require.NoError(t, err)
	before, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	f.mu.Lock()
	f.newOwner = "1002"
	f.mu.Unlock()
	replacement := xTestRecord("new-access")
	replacement.OwnerID = "1002"
	stageXRecord(t, f.cfg, replacement)
	output, err := f.run(t, f.clientConfig(t), "--register", "--user-id", "1002", "--json")
	require.NoError(t, err)
	var registered xRegistrationOutput
	require.NoError(t, json.Unmarshal([]byte(output), &registered))
	assert.Equal(t, "1002", registered.UserID)
	assert.NotEqual(t, original.ID, registered.SourceID)
	sources, err := f.st.ListSources("x")
	require.NoError(t, err)
	assert.Len(t, sources, 2)
	current, err := manager.Load(t.Context(), "1001")
	require.NoError(t, err)
	assert.Equal(t, before.Generation, current.Generation)
	assert.Equal(t, "old-access", current.AccessToken())
	_, err = xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1002")
	require.ErrorIs(t, err, os.ErrNotExist)
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
	f := newXRegistrationFixture(t)
	runtime := daemonRuntimeForHTTPServer(t, f.server, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(f.cfg.Data.DataDir).Write(runtime.Record)
	require.NoError(t, err)
	output, err := f.run(t, f.cfg, "--client-id", "example-client")
	require.NoError(t, err)
	assert.Contains(t, output, "Registered X account")
	sources, err := f.st.ListSources("x")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "1001", sources[0].Identifier)
	assert.Equal(t, int32(2), f.meCalls.Load())
	_, err = xapi.NewTokenManager(f.cfg.TokensDir()).Load(t.Context(), "1001")
	require.NoError(t, err)
	_, err = xapi.NewTokenManager(xStagedTokensDir(f.cfg)).Load(t.Context(), "1001")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAddXHeadlessPreservesCallbackPort(t *testing.T) {
	f := newXRegistrationFixture(t)
	output, err := f.run(t, f.clientConfig(t), "--headless", "--client-id", "example-client", "--callback-port", "9876")
	require.NoError(t, err)
	assert.Contains(t, output, "--callback-port")
	assert.Contains(t, output, "9876")
	assert.Zero(t, f.meCalls.Load())
}
