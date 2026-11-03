package cmd

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func TestBroadcastErrorsFollowCLIGrammar(t *testing.T) {
	requirements := require.New(t)

	cfg, st, sourceID := broadcastWebhookConfig(t)
	cfg.MessengerBroadcasts[0].CampaignDays = 7
	sub, err := st.ApplyConsentWebhookContext(t.Context(), sourceID, store.ConsentWebhook{Token: "synthetic-token", PSID: "300", At: time.Now(), EvidenceRef: "fixture:0"})
	requirements.NoError(err)
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: sourceID, Name: "Offers", MessageJSON: []byte(`{"text":"Example"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
	requirements.NoError(err)
	a := &storeAPIAdapter{store: st, config: cfg}
	for _, tc := range []struct {
		name string
		args []string
		exit int
	}{
		{"usage", []string{"send", b.ID}, 2},
		{"invalid value", []string{"plan", b.ID, "--revision", "-1"}, 3},
		{"missing profile", []string{"list", "missing"}, 4},
		{"missing broadcast", []string{"show", "broadcast-missing"}, 4},
		{"changed plan", []string{"plan", b.ID, "--revision", "2"}, 6},
		{"disabled sends", []string{"send", b.ID, "--revision", "1", "--plan-digest", "fixture"}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			err := a.runCLIBroadcast(t.Context(), api.CLIRunRequest{Args: append([]string{"broadcast"}, tc.args...)}, nil)
			requirements.Error(err)
			var classified interface{ ExitCode() int }
			requirements.ErrorAs(err, &classified)
			assertions.Equal(tc.exit, classified.ExitCode())
			var hinted interface{ Hint() string }
			requirements.ErrorAs(err, &hinted)
			assertions.Contains(hinted.Hint(), "msgvault ")
		})
	}
	cfg.MessengerBroadcasts[0].PageID = "101"
	err = a.runCLIBroadcast(t.Context(), api.CLIRunRequest{Args: []string{"broadcast", "list", cfg.MessengerBroadcasts[0].Identifier}}, nil)
	requirements.Error(err)
	var classified interface{ ExitCode() int }
	requirements.ErrorAs(err, &classified)
	assert.Equal(t, 5, classified.ExitCode())
}

func TestBroadcastCobraLocalDaemonRoundTrip(t *testing.T) {
	cfg, st, _ := broadcastWebhookConfig(t)
	cfg.Server.APIKey = "synthetic-owner-key"
	autoStart := false
	cfg.Server.DaemonAutoStart = &autoStart
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg}, nil, testLoggerValue()).Router())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	runtime := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(cfg.Server.APIKey))
	_, err := daemonRuntimeStore(cfg.Data.DataDir).Write(runtime.Record)
	require.NoError(t, err)
	for _, tc := range []struct {
		args []string
		exit int
	}{
		{[]string{"list", cfg.MessengerBroadcasts[0].Identifier}, 0},
		{[]string{"list", "missing"}, 4},
		{[]string{"show", "broadcast-missing"}, 4},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
			root.AddCommand(newBroadcastCommand())
			var stdout bytes.Buffer
			root.SetOut(&stdout)
			root.SetArgs(append(append([]string{"broadcast"}, tc.args...), "--json"))
			err := root.ExecuteContext(withStoreResolverConfig(t, cfg))
			if tc.exit == 0 {
				requirements.NoError(err)
				assertions.JSONEq(`[]`, stdout.String())
				return
			}
			requirements.Error(err)
			var classified interface{ ExitCode() int }
			requirements.ErrorAs(err, &classified)
			assertions.Equal(tc.exit, classified.ExitCode())
			assertions.Empty(stdout.String())
		})
	}
}

// This fixture proves the external daemon stream contract; it never calls Meta.
func TestBroadcastCobraGroupedWireAndErrorClass(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, []string{"broadcast", "send", "--json", "--plan-digest=fixture", "--revision=1", "broadcast-example"}, req.Args)
		assert.Empty(t, req.Env)
		assert.Empty(t, req.Cwd)
	}, `{"type":"error","error":"campaign_warming"}`)
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(newBroadcastCommand())
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	root.SetArgs([]string{"broadcast", "send", "broadcast-example", "--revision", "1", "--plan-digest", "fixture", "--json"})
	err := root.ExecuteContext(configureRemoteDaemonForTest(t, server.URL))
	requirements.Error(err)
	var classified interface{ ExitCode() int }
	requirements.ErrorAs(err, &classified)
	assertions.Equal(6, classified.ExitCode())
	assertions.Equal(int32(1), requests.Load())
	assertions.Empty(stderr.String())
	assertions.ErrorContains(err, "campaign_warming")
}

func TestBroadcastCobraValidatesBeforeDaemon(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	server, requests := newDaemonCLIRunnerTestServer(t, nil)
	for _, tc := range []struct {
		args []string
		exit int
	}{
		{[]string{"send", "broadcast-example"}, 2},
		{[]string{"plan", "broadcast-example", "--revision", "-1"}, 3},
	} {
		root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
		root.AddCommand(newBroadcastCommand())
		root.SetArgs(append([]string{"broadcast"}, tc.args...))
		err := root.ExecuteContext(configureRemoteDaemonForTest(t, server.URL))
		requirements.Error(err)
		var classified interface{ ExitCode() int }
		requirements.ErrorAs(err, &classified)
		assertions.Equal(tc.exit, classified.ExitCode())
	}
	assertions.Zero(requests.Load())
}
