package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/twilio"
)

func TestResolveTwilioSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := &config.Config{Twilio: []config.TwilioSource{{Identifier: "personal"}, {Identifier: "work"}}}
	sources, err := resolveTwilioSources(nil, false, cfg)
	require.NoError(err)
	assert.Len(sources, 2)
	_, err = resolveTwilioSources(nil, true, cfg)
	require.ErrorContains(err, "pass an identifier")
	sources, err = resolveTwilioSources([]string{"WORK"}, false, cfg)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("work", sources[0].Identifier)
}

func TestTwilioProbeDoesNotPrintCallEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal("1", r.URL.Query().Get("PageSize"))
		_, _ = fmt.Fprint(w, `{"recordings":[{"sid":"RE00000000000000000000000000000001","account_sid":"AC00000000000000000000000000000001","call_sid":"CA00000000000000000000000000000001","status":"completed"}],"next_page_uri":"/2010-04-01/Accounts/AC00000000000000000000000000000001/Recordings.json?Page=1&PageSize=1"}`)
	}))
	t.Cleanup(server.Close)
	client, err := twilio.NewClient(twilio.Options{AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token", Endpoints: map[string]string{"voice": server.URL}})
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(runTwilioProbe(t.Context(), &out, client))
	assert.Contains(out.String(), "Recordings on first page (sample): 1")
	assert.NotContains(out.String(), "RE0000")
	assert.NotContains(out.String(), "CA0000")
	assert.NotContains(out.String(), "synthetic-token")
	assert.Equal(1, requests, "the probe reads one page")
}

// Caches refresh after a sync that succeeded or still wrote something.
func TestFinishTwilioSyncRefreshesAfterWrites(t *testing.T) {
	failure := errors.New("recording failed")
	for _, tc := range []struct {
		name      string
		err       error
		written   *twilio.ImportSummary
		refreshed bool
	}{
		{"success", nil, &twilio.ImportSummary{}, true},
		{"failure with audio written", failure, &twilio.ImportSummary{AttachmentsStored: 1}, true},
		{"failure with nothing written", failure, &twilio.ImportSummary{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refreshed := false
			err := finishCallSync(tc.err, tc.written, func() error { refreshed = true; return nil })
			assert.Equal(t, tc.refreshed, refreshed)
			if tc.err == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.err)
			}
		})
	}
}

func TestTwilioCommandsUseDaemonRunner(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, "")
	for _, command := range []*cobra.Command{addTwilioCmd, syncTwilioCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(t, []string{command.Name(), "work"}, req.Args)
			}, `{"type":"stdout","data":"Twilio operation completed\n"}`, `{"type":"complete"}`)
			cmd := &cobra.Command{Use: command.Use}
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			var out bytes.Buffer
			cmd.SetOut(&out)
			require.NoError(t, command.RunE(cmd, []string{"work"}))
			assert.Equal(t, int32(1), requests.Load())
			assert.Equal(t, "Twilio operation completed\n", out.String())
		})
	}
}

func TestTwilioAddRegistersPrimaryIdentity(t *testing.T) {
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.Twilio = []config.TwilioSource{{Identifier: "work", AccountEmail: "owner@example.com", AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"recordings":[],"next_page_uri":null}`)
	}))
	t.Cleanup(server.Close)
	previous := newTwilioClient
	t.Cleanup(func() { newTwilioClient = previous })
	newTwilioClient = func(opts twilio.Options) (*twilio.Client, error) {
		opts.Endpoints = map[string]string{"voice": server.URL}
		return twilio.NewClient(opts)
	}
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(addTwilioCmd.RunE(cmd, []string{"work"}))
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	source, err := st.GetSourceByTypeAndIdentifier("twilio", "work")
	require.NoError(err)
	identities, err := st.ListAccountIdentitiesContext(t.Context(), source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal(t, "owner@example.com", identities[0].Address)
}

// twilioSyncFixture registers a source whose account lists two calls across two
// pages. Transcript endpoints answer 404 so every run carries a coverage diagnostic.
func twilioSyncFixture(t *testing.T, recordingStatus string, mediaStatus int) *config.Config {
	t.Helper()
	require := require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.Twilio = []config.TwilioSource{{Identifier: "work", AccountEmail: "owner@example.com", AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token"}}
	const accountPath = "/2010-04-01/Accounts/AC00000000000000000000000000000001"
	recording := func(n int) string {
		return fmt.Sprintf(`{"sid":"RE0000000000000000000000000000000%d","account_sid":"AC00000000000000000000000000000001","call_sid":"CA0000000000000000000000000000000%d","status":"%s"}`, n, n, recordingStatus)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == accountPath+"/Recordings.json" && r.URL.Query().Get("Page") == "":
			_, _ = fmt.Fprintf(w, `{"recordings":[%s],"next_page_uri":"%s/Recordings.json?Page=1&PageSize=1000"}`, recording(1), accountPath)
		case r.URL.Path == accountPath+"/Recordings.json":
			_, _ = fmt.Fprintf(w, `{"recordings":[%s],"next_page_uri":null}`, recording(2))
		case strings.HasSuffix(r.URL.Path, "/Recordings.json"):
			_, _ = fmt.Fprint(w, `{"recordings":[],"next_page_uri":null}`)
		case strings.HasSuffix(r.URL.Path, ".wav"):
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(mediaStatus)
		case strings.HasPrefix(r.URL.Path, accountPath+"/Calls/"):
			n := 1
			if strings.Contains(r.URL.Path, "CA00000000000000000000000000000002") {
				n = 2
			}
			_, _ = fmt.Fprintf(w, `{"sid":"CA0000000000000000000000000000000%d","account_sid":"AC00000000000000000000000000000001"}`, n)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	previous := newTwilioClient
	t.Cleanup(func() { newTwilioClient = previous })
	newTwilioClient = func(opts twilio.Options) (*twilio.Client, error) {
		opts.Endpoints = map[string]string{"voice": server.URL, "intelligence": server.URL}
		return twilio.NewClient(opts)
	}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("twilio", "work")
	require.NoError(err)
	require.NoError(st.Close())
	return cfg
}

func runLimitedTwilioSync(t *testing.T, cfg *config.Config) (string, error) {
	t.Helper()
	previousLimit := syncTwilioLimit
	syncTwilioLimit = 1
	t.Cleanup(func() { syncTwilioLimit = previousLimit })
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := syncTwilioCmd.RunE(cmd, []string{"work"})
	return out.String(), err
}

// A limited run that stops early, or a failed one, says so, shows its
// diagnostics, and prints the command that resumes it.
func TestTwilioSyncPrintsResumeHint(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		audio        int
		wantErr      bool
		want         []string
	}{
		{"paused", "processing", http.StatusOK, false, []string{"Twilio sync paused with more calls to discover."}},
		{"failed", "completed", http.StatusServiceUnavailable, true, []string{"Twilio sync failed; later syncs retry calls that errored while they're within seven days; an older call that failed during a first sync or --full needs --full.", "legacy transcription coverage unavailable (HTTP 404)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			out, err := runLimitedTwilioSync(t, twilioSyncFixture(t, tc.status, tc.audio))
			assert.Equal(tc.wantErr, err != nil)
			for _, want := range tc.want {
				assert.Contains(out, want)
			}
			assert.Contains(out, "Run: msgvault sync-twilio work --limit 1")
			assert.NotContains(out, "sync complete")
		})
	}
}

// A source with bad credentials must not stop the others,
// and the cache refreshes once.
func TestSyncTwilioRunsEverySourceAndRefreshesOnce(t *testing.T) {
	assert := assert.New(t)
	cfg := twilioSyncFixture(t, "processing", http.StatusOK)
	noCredentials := cfg.Twilio[0]
	noCredentials.Identifier, noCredentials.AuthToken = "no-credentials", ""
	cfg.Twilio = []config.TwilioSource{noCredentials, cfg.Twilio[0]}
	previousRefresh := rebuildTwilioCacheAfterWrite
	t.Cleanup(func() { rebuildTwilioCacheAfterWrite = previousRefresh })
	refreshes := 0
	rebuildTwilioCacheAfterWrite = func(string, *invocation) error { refreshes++; return nil }
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := syncTwilioCmd.RunE(cmd, nil)
	require.ErrorContains(t, err, "twilio sync no-credentials failed")
	assert.Contains(out.String(), "Syncing Twilio calls for work")
	assert.Contains(out.String(), "Meetings added:     2")
	assert.Equal(1, refreshes)
}

func TestScheduledTwilioSyncLogsDiagnostics(t *testing.T) {
	require := require.New(t)
	cfg := twilioSyncFixture(t, "processing", http.StatusOK)
	previousRefresh := rebuildTwilioCacheAfterScheduledSync
	t.Cleanup(func() { rebuildTwilioCacheAfterScheduledSync = previousRefresh })
	rebuildTwilioCacheAfterScheduledSync = func(context.Context, string) error { return nil }
	var logs bytes.Buffer
	state := newInvocation()
	state.cfg = cfg
	state.logger = slog.New(slog.NewTextHandler(&logs, nil))
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(runConfiguredTwilioSync(withInvocation(t.Context(), state), st, cfg.Twilio[0]))
	assert.Contains(t, logs.String(), "legacy transcription coverage unavailable (HTTP 404)")
}

func TestTwilioDaemonDispatchesRegisteredScheduledSource(t *testing.T) {
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Server.APIPort = freeTCPPort(t)
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Analytics.AutoBuildCache = false
	cfg.Vector.Enabled = false
	cfg.Twilio = []config.TwilioSource{{Identifier: "work", AccountEmail: "owner@example.com", AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token", Enabled: true, Schedule: "0 0 1 1 *"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"recordings":[],"next_page_uri":null}`)
	}))
	t.Cleanup(server.Close)
	previous := newTwilioClient
	t.Cleanup(func() { newTwilioClient = previous })
	newTwilioClient = func(opts twilio.Options) (*twilio.Client, error) {
		opts.Endpoints = map[string]string{"voice": server.URL}
		return twilio.NewClient(opts)
	}
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("twilio", "work")
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := &cobra.Command{Use: serveCmd.Use}
	cmd.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	errCh := make(chan error, 1)
	go func() { errCh <- runServe(cmd, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			require.NoError(err)
		case <-time.After(serveLifecycleTestTimeout):
			require.FailNow("daemon did not stop")
		}
	})
	waitForServeHealth(t, cfg.Server.APIPort, errCh)
	base := fmt.Sprintf("http://127.0.0.1:%d", cfg.Server.APIPort)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post(base+"/api/v1/sync/work?source_type=twilio", "application/json", nil)
	require.NoError(err)
	require.NoError(response.Body.Close())
	require.Equal(http.StatusAccepted, response.StatusCode)
	var status api.SourceStatusResponse
	require.Eventually(func() bool {
		response, err := client.Get(base + "/api/v1/sources/status?source_type=twilio")
		if err != nil {
			return false
		}
		defer func() { assert.NoError(t, response.Body.Close()) }()
		return json.UnmarshalRead(response.Body, &status) == nil && len(status.Sources) == 1 && status.Sources[0].LastSuccessfulSync != nil && status.Sources[0].CanSync
	}, serveLifecycleTestTimeout, 20*time.Millisecond, "scheduled Twilio import did not finish")
	assert.Empty(t, status.Sources[0].SchedulerLastError, "scheduled refresh must receive daemon configuration")
}

func TestTwilioRemovalDoesNotRecreateScheduledSource(t *testing.T) {
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("twilio", "work")
	require.NoError(err)
	require.NoError(st.Close())
	root := newTestRootCmd()
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.AddCommand(newRemoveAccountLocalTestCmd())
	root.SetArgs([]string{"remove-account", "work", "--yes", "--type", "twilio"})
	require.NoError(root.Execute())
	check, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(check.Close()) })
	err = runConfiguredTwilioSync(t.Context(), check, config.TwilioSource{Identifier: "work"})
	require.ErrorContains(err, "add-twilio work")
	_, err = check.GetSourceByTypeAndIdentifier("twilio", "work")
	require.ErrorIs(err, store.ErrSourceNotFound)
}
