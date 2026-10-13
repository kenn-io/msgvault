package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func setupCommandOAuth(t *testing.T) (*oauth.Manager, *invocation) {
	t.Helper()
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "client")
	cfg.OAuth.Tokens = config.OAuthTokenCommands(testutil.SecretStoreFixture(t))
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(t, err)
	mgr, err := oauth.NewManagerWithCredentials(t.Context(), source, cfg.TokensDir(), cfg.OAuth.Tokens, nil, oauth.Scopes)
	require.NoError(t, err)
	return mgr, testInvocationWithConfig(cfg)
}
func putCommandToken(t *testing.T, state *invocation, email string, scopes []string) {
	t.Helper()
	cfg := state.cfg
	data, err := json.Marshal(map[string]any{"access_token": "example-access", "refresh_token": "example-refresh", "expiry": time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "client_id": "example-client", "scopes": scopes})
	require.NoError(t, err)
	require.NoError(t, oauth.NewTokenStore(cfg.TokensDir(), cfg.OAuth.Tokens).Write(t.Context(), email, data))
}

func TestCommandGmailHeadlessRecoveryForcesBrowserAuthorization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	saveAddAccountFlags(t)
	const email = "reader@example.com"
	var out bytes.Buffer
	printCommandHeadlessInstructions(&out, email, oauth.HeadlessAccountArgs("add-account", email, "work", false, false), false)
	fixture := testutil.SecretCommand(t, "argv")[0]
	commands := 0
	for line := range strings.SplitSeq(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "msgvault add-account ") {
			fields := headlessNativeArgs(t, "cmd", fixture, line)
			cmd := newAddAccountCmd()
			require.NoError(cmd.ParseFlags(fields[1:]))
			require.NoError(cmd.Args(cmd, cmd.Flags().Args()))
			assert.Equal(commands == 0, forceReauth)
			assert.Equal("work", oauthAppName)
			commands++
		}
	}
	assert.Equal(2, commands)
}

func TestCommandCalendarHeadlessInstructionsUseUploadOnlyExport(t *testing.T) {
	var out bytes.Buffer
	printCommandHeadlessInstructions(&out, "reader@example.com",
		oauth.HeadlessAccountArgs("add-calendar", "reader@example.com", "work", false, true), true)

	assert.Contains(t, out.String(), "msgvault export-token reader@example.com --upload-only")
	assert.Contains(t, out.String(), "msgvault add-calendar reader@example.com --oauth-app work --write")
	assert.NotContains(t, out.String(), "msgvault add-account")
}

func TestCommandTokenExport(t *testing.T) {
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", oauth.ScopesGmailReadonly)
	var uploaded []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/token/reader@example.com" {
			uploaded, _ = io.ReadAll(r.Body)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	var out bytes.Buffer
	exporter := &tokenExporter{httpClient: server.Client(), tokensDir: cfg.TokensDir(), tokenCommands: cfg.OAuth.Tokens, stdout: &out, stderr: &out}
	_, err := exporter.export(t.Context(), "reader@example.com", server.URL, "example-key", true)
	require.NoError(t, err)
	assert.Contains(t, string(uploaded), "example-refresh")
	_, err = os.Stat(filepath.Join(cfg.TokensDir(), "reader@example.com.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestCommandMetadataUsesSelectedSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	_, state := setupCommandOAuth(t)
	cfg := state.cfg
	putCommandToken(t, state, "reader@example.com", []string{oauth.ScopeGmailReadonly, "https://www.googleapis.com/auth/drive.readonly"})
	cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-once")
	source, err := cfg.OAuth.CredentialsFor("")
	require.NoError(err)
	mgr, err := newCalendarOAuthManager(t.Context(), source, "reader@example.com", state)
	require.NoError(err)
	scopes := mgr.GrantedScopes(t.Context(), "reader@example.com")
	got := calendarEscalationScopes(scopes, calendarShouldPreserveGmail(true, mgr.HasScopeMetadata(t.Context(), "reader@example.com"), scopes))
	assert.ElementsMatch([]string{oauth.ScopeGmailReadonly, "https://www.googleapis.com/auth/drive.readonly", oauth.ScopeCalendarReadonly}, got)
	assert.False(addAccountTokenHasGmailScopes(t.Context(), mgr, "reader@example.com", false))
}

func TestCommandEscalationKeepsSelectionSnapshot(t *testing.T) {
	for _, calendar := range []bool{false, true} {
		t.Run(fmt.Sprintf("calendar=%v", calendar), func(t *testing.T) {
			_, state := setupCommandOAuth(t)
			const email = "reader@example.com"
			putCommandToken(t, state, email, oauth.Scopes)
			state.cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "client-once")
			source, err := state.cfg.OAuth.CredentialsFor("")
			require.NoError(t, err)
			ctx := withInvocation(t.Context(), state)
			var mgr *oauth.Manager
			if calendar {
				mgr, err = newCalendarOAuthManager(ctx, source, email, state)
			} else {
				_, mgr, err = deletionEscalationSelection(ctx, email, true, source)
			}
			require.NoError(t, err)
			putCommandToken(t, state, email, oauth.ScopesGmailReadonly)
			err = authorizeScopeEscalation(ctx, email, oauth.ScopesGmailCalendar, mgr)
			assert.ErrorIs(t, err, oauth.ErrTokenChanged)
		})
	}
}

func TestCommandOAuthReadsClientOnce(t *testing.T) {
	for _, calendar := range []bool{false, true} {
		t.Run(fmt.Sprintf("calendar=%v", calendar), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, state := setupCommandOAuth(t)
			state.cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "client-once")
			putCommandToken(t, state, "reader@example.com", []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly})
			credentials, err := state.cfg.OAuth.CredentialsFor("")
			require.NoError(err)
			var mgr *oauth.Manager
			if calendar {
				mgr, err = newCalendarOAuthManager(t.Context(), credentials, "reader@example.com", state, true)
			} else {
				mgr, err = newAddAccountOAuthManager(t.Context(), credentials, "reader@example.com", state)
			}
			require.NoError(err)
			flow, err := mgr.BeginWebAuthorization(t.Context(), "reader@example.com", "https://archive.example/")
			require.NoError(err)
			authURL, err := url.Parse(flow.URL)
			require.NoError(err)
			wantScopes := []string{oauth.ScopeGmailReadonly, oauth.ScopeCalendarReadonly}
			if calendar {
				wantScopes = append(wantScopes, oauth.ScopeCalendarEvents)
			}
			assert.ElementsMatch(wantScopes, strings.Fields(authURL.Query().Get("scope")))
		})
	}
}

func TestCommandTokenFailuresAvoidReauthorization(t *testing.T) {
	for _, clientFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("client=%v", clientFailure), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, state := setupCommandOAuth(t)
			if clientFailure {
				state.cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "fail")
			} else {
				state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "fail")
			}
			cache := oauthManagerCache(state)
			if !clientFailure {
				mgr, err := cache(t.Context(), "")
				require.NoError(err)
				_, err = getTokenSourceWithReauth(t.Context(), mgr, "reader@example.com", true, gmailReauthHint)
				require.Error(err)
				assert.NotContains(err.Error(), "add-account")
			}
			src := &store.Source{SourceType: "gmail", Identifier: "reader@example.com"}
			_, _, err := newDaemonGmailClient(withInvocation(t.Context(), state), src.Identifier, src, cache, state)
			require.Error(err)
			_, settingsHint := errors.AsType[*provideridentity.GmailCredentialError](err)
			assert.False(settingsHint)
			assert.NotContains(err.Error(), "add-account")
		})
	}
}

func TestCommandSyncReadFailurePreservesSuccessfulAccount(t *testing.T) {
	for _, full := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			t.Run(fmt.Sprintf("full=%v/explicit=%v", full, explicit), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				_, state := setupCommandOAuth(t)
				state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "fail")
				t.Setenv("MSGVAULT_IMAP_PASSWORD", testutil.IMAPTestPassword)
				addr, _ := testutil.StartIMAPMemServerWithSpecialUse(t, map[string]int{"INBOX": 1}, nil)
				host, portText, err := net.SplitHostPort(addr)
				require.NoError(err)
				port, err := strconv.Atoi(portText)
				require.NoError(err)
				imapConfig := &imaplib.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
				require.NoError(imaplib.SaveCredentials(state.cfg.TokensDir(), imapConfig.Identifier(), testutil.IMAPTestPassword))
				encoded, err := imapConfig.ToJSON()
				require.NoError(err)
				st, err := store.Open(state.cfg.DatabaseDSN())
				require.NoError(err)
				require.NoError(st.InitSchema())
				src, err := st.GetOrCreateSource("gmail", "reader@example.com")
				require.NoError(err)
				require.NoError(st.UpdateSourceSyncCursor(src.ID, "1"))
				healthy, err := st.GetOrCreateSource("imap", imapConfig.Identifier())
				require.NoError(err)
				require.NoError(st.UpdateSourceSyncConfig(healthy.ID, encoded))
				require.NoError(st.Close())
				cmd := &cobra.Command{}
				cmd.SetContext(withInvocation(t.Context(), state))
				getOutput := captureStdout(t)
				args := []string(nil)
				if explicit {
					args = []string{"reader@example.com"}
				}
				if full {
					err = runSyncFullLocalForTest(cmd, args)
				} else {
					err = runSyncIncrementalLocal(cmd, args)
				}
				output := getOutput()
				require.Error(err)
				assert.Contains(err.Error(), "secret command exited with status 7")
				assert.Contains(err.Error(), "1 account(s) failed")
				if explicit {
					return
				}
				assert.Contains(output, "Sync complete!")
				st, err = store.Open(state.cfg.DatabaseDSN())
				require.NoError(err)
				defer func() { _ = st.Close() }()
				synced, err := st.GetSourceByIdentifier(healthy.Identifier)
				require.NoError(err)
				assert.True(synced.SyncCursor.Valid)
			})
		}
	}
}

func TestCommandDraftScopeGateUsesCheckedSnapshot(t *testing.T) {
	for _, mode := range []string{"read-once", "fail", "empty-object"} {
		t.Run(mode, func(t *testing.T) {
			_, state := setupCommandOAuth(t)
			putCommandToken(t, state, "reader@example.com", []string{oauth.ScopeGmailModify})
			state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, mode)
			source := &store.Source{SourceType: "gmail", Identifier: "reader@example.com"}
			err := gmailDraftScopeGate(withInvocation(t.Context(), state), state.cfg, source, oauth.ScopesGmailDraftWrite)
			if mode == "read-once" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestCommandCredentialOperationsHonorCancellation(t *testing.T) {
	mgr, state := setupCommandOAuth(t)
	credentials, err := state.cfg.OAuth.CredentialsFor("")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(withInvocation(t.Context(), state))
	cancel()
	for name, call := range map[string]func() error{
		"cached client": func() error { _, err := oauthManagerCache(state)(ctx, ""); return err },
		"Gmail client": func() error {
			_, err := newAddAccountOAuthManager(ctx, credentials, "reader@example.com", state)
			return err
		},
		"Calendar client": func() error {
			_, err := newCalendarOAuthManager(ctx, credentials, "reader@example.com", state)
			return err
		},
		"Drive client": func() error {
			_, err := newSynctechSMSDriveOAuthManager(ctx, state.cfg, state.logger, credentials)
			return err
		},
		"deletion selection": func() error {
			_, err := deleteStagedScopeEscalationForSource(ctx, "reader@example.com", &store.Source{SourceType: "gmail"}, false, credentials, state)
			return err
		},
		"web sign-in": func() error {
			_, err := mgr.BeginWebAuthorization(ctx, "reader@example.com", "https://archive.example/")
			return err
		},
		"token cleanup":      func() error { return mgr.DeleteToken(ctx, "reader@example.com") },
		"readonly selection": func() error { return refuseReadonlyUnderAliasSpelling(ctx, mgr, "reader@example.com", "") },
		"export": func() error {
			exporter := &tokenExporter{tokensDir: state.cfg.TokensDir(), tokenCommands: state.cfg.OAuth.Tokens}
			_, err := exporter.export(ctx, "reader@example.com", "https://archive.example", "example-key", false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) { require.ErrorIs(t, call(), context.Canceled) })
	}
}

func TestCommandForcePreflightFinalReadHonorsCancellation(t *testing.T) {
	require := require.New(t)
	_, state := setupCommandOAuth(t)
	saveAddAccountFlags(t)
	cmd := newAddAccountCmd()
	require.NoError(cmd.ParseFlags([]string{"--force"}))
	const email = "reader@example.com"
	putCommandToken(t, state, email, oauth.Scopes)
	st, err := store.Open(state.cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("gmail", email)
	require.NoError(err)
	startStoreQueryAPIDaemon(t, state.cfg.Data.DataDir, st)
	state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-then-wait")
	ctx, cancel := context.WithCancel(withInvocation(t.Context(), state))
	defer cancel()
	cmd.SetContext(ctx)
	result := make(chan error, 1)
	go func() { _, err := preflightAddAccountAuthorize(cmd, email); result <- err }()
	const credentialCommandBudget = 15 * time.Second
	require.Eventually(func() bool {
		_, err := os.Stat(filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "started"))
		return err == nil
	}, credentialCommandBudget, 10*time.Millisecond)
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(credentialCommandBudget):
		require.FailNow("authorization read did not honor cancellation")
	}
}

func TestCommandGrantDecisionReadsExactTokenOnce(t *testing.T) {
	for _, tc := range []struct {
		name            string
		readonly, alias bool
		failedRead      bool
	}{
		{"default", false, false, false}, {"readonly", true, false, false}, {"readonly alias", true, true, false}, {"failed read", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			_, state := setupCommandOAuth(t)
			saveAddAccountFlags(t)
			readonlyGrant = tc.readonly
			const email = "user.name@gmail.com"
			putCommandToken(t, state, email, oauth.ScopesGmailReadonly)
			if tc.alias {
				putCommandToken(t, state, "username@gmail.com", oauth.ScopesGmailReadonly)
			}
			state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "read-once")
			credentials, err := state.cfg.OAuth.CredentialsFor("")
			require.NoError(err)
			mgr, err := newAddAccountOAuthManager(t.Context(), credentials, email, state)
			require.NoError(err)
			if tc.failedRead {
				state.cfg.OAuth.Tokens.ReadCommand = testutil.SecretCommand(t, "fail")
				mgr, err = oauth.NewManagerWithCredentials(t.Context(), credentials, state.cfg.TokensDir(), state.cfg.OAuth.Tokens, nil, oauth.Scopes)
				require.NoError(err)
			}
			var out bytes.Buffer
			err = applyAddAccountGrantDecision(t.Context(), &out, mgr, email, "")
			if tc.failedRead {
				require.ErrorContains(err, "status 7")
			} else if tc.alias {
				require.ErrorContains(err, "hold stored tokens for the same Google account")
				assert.NotContains(err.Error(), "status 7")
			} else {
				require.NoError(err)
			}
		})
	}
}

func headlessNativeArgs(t *testing.T, shell, fixture, line string) []string {
	t.Helper()
	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("native shell unavailable: %s", shell)
	}
	tail := strings.TrimPrefix(line, "msgvault ")
	var command *exec.Cmd
	switch shell {
	case "cmd":
		command = exec.Command(path, "/d", "/c", ".\\"+filepath.Base(fixture)+" argv "+tail)
		command.Dir = filepath.Dir(fixture)
	case "pwsh":
		command = exec.Command(path, "-NoProfile", "-NonInteractive", "-Command", "& '"+strings.ReplaceAll(fixture, "'", "''")+"' argv "+tail)
	case "sh":
		command = exec.Command(path, "-c", "exec \"$1\" argv "+tail, "sh", filepath.ToSlash(fixture))
	}
	data, err := command.CombinedOutput()
	require.NoError(t, err, "%s", data)
	var args []string
	require.NoError(t, json.Unmarshal(data, &args))
	return args
}

func TestHeadlessCommandsPreserveNativeArguments(t *testing.T) {
	saveAddAccountFlags(t)
	oldWrite, oldApp := calAddWrite, calAddOAuthApp
	t.Cleanup(func() { calAddWrite, calAddOAuthApp = oldWrite, oldApp })
	fixture := testutil.SecretCommand(t, "argv")[0]
	for _, app := range []string{"work", "work team", "work's", "%PATH%", "$HOME", "work&team", "@work", "work\"team"} {
		for _, flow := range []string{"browser", "server", "calendar"} {
			t.Run(app+"/"+flow, func(t *testing.T) {
				command := "add-account"
				if flow == "calendar" {
					command = "add-calendar"
				}
				want := oauth.HeadlessAccountArgs(command, "reader+tag@example.com", app, true, true)
				if flow == "browser" {
					want = append(want, "--force")
				}
				var out bytes.Buffer
				oauth.PrintHeadlessCommand(&out, want...)
				var lines []string
				for line := range strings.SplitSeq(out.String(), "\n") {
					if strings.HasPrefix(strings.TrimSpace(line), "msgvault ") {
						lines = append(lines, strings.TrimSpace(line))
					}
				}
				require.NotEmpty(t, lines)
				for _, shell := range []string{"cmd", "sh", "pwsh"} {
					if shell == "cmd" && len(lines) == 2 {
						continue
					}
					t.Run(shell, func(t *testing.T) {
						assert := assert.New(t)
						require := require.New(t)
						line := lines[0]
						if shell == "pwsh" && len(lines) == 2 {
							line = lines[1]
						}
						got := headlessNativeArgs(t, shell, fixture, line)
						require.Equal(want[1:], got)
						cmd := newAddAccountCmd()
						if flow == "calendar" {
							cmd = newAddCalendarCmd()
						}
						require.NoError(cmd.ParseFlags(got[1:]))
						require.NoError(cmd.Args(cmd, cmd.Flags().Args()))
						require.NoError(validateAddAccountArgs(cmd, cmd.Flags().Args()))
						selected, err := cmd.Flags().GetString("oauth-app")
						require.NoError(err)
						assert.Equal(app, selected)
					})
				}
			})
		}
	}
}

const sharedLoadBudget = 15 * time.Second

// slowOAuthManagers returns a cache whose "slow" app blocks in
// client_secrets_command on its first run and succeeds afterwards. The channel
// reports each time a caller attaches to a "slow" load.
func slowOAuthManagers(t *testing.T) (*oauthManagers, <-chan struct{}) {
	t.Helper()
	_, state := setupCommandOAuth(t)
	state.cfg.OAuth.Apps = map[string]config.OAuthApp{
		"slow": {ClientSecretsCommand: testutil.SecretCommand(t, "client-after-wait")},
	}
	joined := make(chan struct{}, 8)
	cache := &oauthManagers{state: state, managers: map[string]*oauth.Manager{}, joined: func(appName string) {
		if appName == "slow" {
			joined <- struct{}{}
		}
	}}
	return cache, joined
}

func awaitSharedLoad(t *testing.T, joined <-chan struct{}) {
	t.Helper()
	select {
	case <-joined:
	case <-time.After(sharedLoadBudget):
		require.Fail(t, "caller did not attach to the shared load")
	}
}

func awaitLoadResult(t *testing.T, result <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(sharedLoadBudget):
		require.Fail(t, what+" did not return")
		return nil
	}
}

// awaitSlowCommand waits until the first "slow" credential command is running.
func awaitSlowCommand(t *testing.T) {
	t.Helper()
	started := filepath.Join(os.Getenv("MSGVAULT_TEST_SECRET_ROOT"), "client-after-wait.started")
	require.Eventually(t, func() bool {
		_, err := os.Stat(started)
		return err == nil
	}, sharedLoadBudget, 10*time.Millisecond)
}

func startSlowLoad(ctx context.Context, t *testing.T, cache *oauthManagers, joined <-chan struct{}) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() { _, err := cache.get(ctx, "slow"); result <- err }()
	awaitSharedLoad(t, joined)
	return result
}

// A slow client_secrets_command for one OAuth app must not hold up managers
// for other apps, and every caller can stop waiting for its own reasons.
func TestOAuthManagerCacheLoadsAppsIndependently(t *testing.T) {
	require := require.New(t)
	cache, joined := slowOAuthManagers(t)
	loaderCtx, cancelLoader := context.WithCancel(t.Context())
	defer cancelLoader()
	loader := startSlowLoad(loaderCtx, t, cache, joined)
	awaitSlowCommand(t)

	// Another app loads while the slow command is still running.
	other := make(chan error, 1)
	go func() { _, err := cache.get(t.Context(), ""); other <- err }()
	require.NoError(awaitLoadResult(t, other, "the default app"))

	// A waiter for the slow app stops when its own context ends.
	waiterCtx, cancelWaiter := context.WithCancel(t.Context())
	waiter := startSlowLoad(waiterCtx, t, cache, joined)
	cancelWaiter()
	require.ErrorIs(awaitLoadResult(t, waiter, "a cancelled waiter"), context.Canceled)

	// When the loading caller is cancelled, a caller still attached loads again.
	survivor := startSlowLoad(t.Context(), t, cache, joined)
	cancelLoader()
	require.ErrorIs(awaitLoadResult(t, loader, "the cancelled loader"), context.Canceled)
	require.NoError(awaitLoadResult(t, survivor, "the remaining caller"))
}

// expiringContext reaches its deadline when the test says so, so a shared load
// can be shown to end by deadline without depending on timing.
type expiringContext struct {
	context.Context

	done chan struct{}
}

func (c expiringContext) Done() <-chan struct{} { return c.done }

func (c expiringContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// A sync that started the shared load and then ran out of time must not fail
// other syncs still waiting for the same app.
func TestOAuthManagerCacheRetriesAfterLoaderDeadline(t *testing.T) {
	require := require.New(t)
	cache, joined := slowOAuthManagers(t)
	loaderCtx := expiringContext{Context: t.Context(), done: make(chan struct{})}
	loader := startSlowLoad(loaderCtx, t, cache, joined)
	awaitSlowCommand(t)
	survivor := startSlowLoad(t.Context(), t, cache, joined)

	close(loaderCtx.done)
	require.ErrorIs(awaitLoadResult(t, loader, "the expired loader"), context.DeadlineExceeded)
	require.NoError(awaitLoadResult(t, survivor, "the waiting caller"),
		"the waiting caller must retry under its own context")
}

// A load that starts just after another one published must reuse that manager
// instead of running client_secrets_command again.
func TestOAuthManagerLoadReusesPublishedManager(t *testing.T) {
	require := require.New(t)
	mgr, state := setupCommandOAuth(t)
	state.cfg.OAuth.ClientSecretsCommand = testutil.SecretCommand(t, "fail")
	cache := &oauthManagers{state: state, managers: map[string]*oauth.Manager{"": mgr}}
	loaded, err := cache.load(t.Context(), "")
	require.NoError(err)
	require.Same(mgr, loaded)
}
