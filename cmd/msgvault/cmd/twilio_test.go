package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	"go.kenn.io/msgvault/internal/testutil"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		_, _ = fmt.Fprint(w, `{"recordings":[{"sid":"RE00000000000000000000000000000001","account_sid":"AC00000000000000000000000000000001","call_sid":"CA00000000000000000000000000000001","status":"completed"}],"next_page_uri":null}`)
	}))
	t.Cleanup(server.Close)
	client, err := twilio.NewClient(twilio.Options{AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token", Endpoints: map[string]string{"voice": server.URL}})
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(runTwilioProbe(t.Context(), &out, client, false))
	assert.Contains(out.String(), "Recordings on first page (sample): 1")
	assert.NotContains(out.String(), "RE0000")
	assert.NotContains(out.String(), "CA0000")
	assert.NotContains(out.String(), "synthetic-token")
}

func TestTwilioProbeDoesNotFetchSubsequentRecordingPages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(http.MethodGet, r.Method)
		if requests == 1 {
			_, _ = fmt.Fprint(w, `{"recordings":[{"sid":"RE00000000000000000000000000000001","account_sid":"AC00000000000000000000000000000001","call_sid":"CA00000000000000000000000000000001","status":"completed"}],"next_page_uri":"/2010-04-01/Accounts/AC00000000000000000000000000000001/Recordings.json?Page=2&PageSize=1000"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"recordings":[{"sid":"RE00000000000000000000000000000002","account_sid":"AC00000000000000000000000000000001","call_sid":"CA00000000000000000000000000000002","status":"completed"}],"next_page_uri":null}`)
	}))
	t.Cleanup(server.Close)
	client, err := twilio.NewClient(twilio.Options{AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token", Endpoints: map[string]string{"voice": server.URL}})
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(runTwilioProbe(t.Context(), &out, client, false))
	assert.Equal(1, requests)
}

func TestTwilioProbeUsesSingleCallPage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	callRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method)
		if strings.HasSuffix(r.URL.Path, "/Recordings.json") {
			_, _ = fmt.Fprint(w, `{"recordings":[],"next_page_uri":null}`)
			return
		}
		assert.True(strings.HasSuffix(r.URL.Path, "/Calls.json"), r.URL.Path)
		callRequests++
		if callRequests == 1 {
			_, _ = fmt.Fprint(w, `{"calls":[{"sid":"CA00000000000000000000000000000001","account_sid":"AC00000000000000000000000000000001","date_created":"Thu, 01 Oct 2026 00:00:00 +0000"}],"next_page_uri":"/2010-04-01/Accounts/AC00000000000000000000000000000001/Calls.json?Page=2&PageSize=1000"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"calls":[{"sid":"CA00000000000000000000000000000002","account_sid":"AC00000000000000000000000000000001","date_created":"Fri, 02 Oct 2026 00:00:00 +0000"}],"next_page_uri":null}`)
	}))
	t.Cleanup(server.Close)
	client, err := twilio.NewClient(twilio.Options{AccountSID: "AC00000000000000000000000000000001", AuthToken: "synthetic-token", Endpoints: map[string]string{"voice": server.URL}})
	require.NoError(err)
	var out bytes.Buffer
	require.NoError(runTwilioProbe(t.Context(), &out, client, true))
	assert.Equal(1, callRequests)
	assert.Contains(out.String(), "Calls on first page (sample): 1")
}

func TestFinishTwilioImportRefreshesPartialAudioWrites(t *testing.T) {
	failure := errors.New("recording failed")
	refreshFailure := errors.New("cache refresh failed")
	for _, summary := range []*twilio.ImportSummary{{MeetingsAdded: 1}, {MeetingsUpdated: 1}, {AttachmentsStored: 1}} {
		refreshed := 0
		err := finishTwilioImport("work", summary, failure, func() error { refreshed++; return refreshFailure })
		require.ErrorIs(t, err, failure)
		require.ErrorIs(t, err, refreshFailure)
		assert.Equal(t, 1, refreshed)
	}
}

func TestConfiguredTwilioSyncRefusesRemovedSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	err := runConfiguredTwilioSync(context.Background(), st, config.TwilioSource{Identifier: "removed"})
	require.ErrorContains(t, err, "add-twilio removed")
	_, err = st.GetSourceByTypeAndIdentifier("twilio", "removed")
	require.ErrorIs(t, err, store.ErrSourceNotFound)
}

func TestTwilioDaemonCacheAndAttachmentClassification(t *testing.T) {
	assert := assert.New(t)
	assert.True(manualSyncCLICommand([]string{"sync-twilio", "work"}))
	assert.False(manualSyncCLICommand([]string{"sync-twilio", "--probe"}))
	assert.False(manualSyncCLICommand([]string{"sync-twilio", "--probe=true"}))
	assert.True(attachmentProducingCommand([]string{"sync-twilio", "work"}))
	assert.False(attachmentProducingCommand([]string{"sync-twilio", "--probe"}))
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
