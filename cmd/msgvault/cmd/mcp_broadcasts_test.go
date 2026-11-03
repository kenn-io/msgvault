package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

func TestMCPBroadcastRunnerArgvHasGroup(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	var captured []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.CLIRunRequest
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &req)) {
			return
		}
		captured = req.Args
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, err := w.Write([]byte("{\"type\":\"stdout\",\"data\":\"{}\\n\"}\n{\"type\":\"complete\"}\n"))
		assert.NoError(t, err)
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	_, err = (daemonMCPDraftRunner{client: client}).RunDraftCommand(context.Background(), mcpserver.DraftCommandRequest{Command: "broadcast-send", Positional: "broadcast-abc", Flags: map[string][]string{"revision": {"1"}, "plan-digest": {"digest"}}})
	requirements.NoError(err)
	requirements.GreaterOrEqual(len(captured), 2)
	assertions.Equal([]string{"broadcast", "send"}, captured[:2])
	parsed, err := api.ParseCLIRunBroadcast(api.CLIRunRequest{Args: captured})
	requirements.NoError(err)
	assertions.Equal("digest", parsed.PlanDigest)
	assertions.False(agentDelegatedCommand("broadcast-send"))
}

func TestBroadcastDaemonClientReadRoundTrip(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	cfg, st, sourceID := broadcastWebhookConfig(t)
	cfg.Server.APIKey = "synthetic-owner-key"
	sub, err := st.ApplyConsentWebhookContext(t.Context(), sourceID, store.ConsentWebhook{Token: "synthetic-mm-token-1", PSID: "300", At: time.Now(), EvidenceRef: "fixture:0"})
	requirements.NoError(err)
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: sourceID, AdAccountID: "700", Name: "Offers", MessageJSON: []byte(`{"text":"Example offer"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg}
	server := httptest.NewServer(api.NewServer(cfg, adapter, nil, testLoggerValue()).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	detail, err := client.GetBroadcast(t.Context(), b.ID)
	requirements.NoError(err)
	assertions.Equal("100", detail.PageID)
	assertions.JSONEq(`{"text":"Example offer"}`, string(detail.MessageJSON))
	requirements.Len(detail.Recipients, 1)
	list, err := client.ListBroadcasts(t.Context(), cfg.MessengerBroadcasts[0].Identifier)
	requirements.NoError(err)
	requirements.Len(list.Broadcasts, 1)
	assertions.Equal(b.ID, list.Broadcasts[0].ID)
	subs, err := client.ListBroadcastSubscriptions(t.Context(), cfg.MessengerBroadcasts[0].Identifier, 0, "active")
	requirements.NoError(err)
	requirements.Len(subs.Subscriptions, 1)
	assertions.Equal(sub.Fingerprint, subs.Subscriptions[0].TokenFingerprint)
}
func TestMCPBroadcastBackendVersionWiring(t *testing.T) {
	for _, version := range []string{"3.29.0", "3.36.0", "3.37.0", mcpserver.BroadcastMinAPISchemaVersion} {
		client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/health" {
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"status": "ok", "api_schema_version": version}))
				return
			}
			http.NotFound(w, r)
		})
		opts := daemonMCPServeOptions(t.Context(), client, invocationFromContext(t.Context()))
		assert.Equal(t, version == mcpserver.BroadcastMinAPISchemaVersion, opts.Broadcasts != nil)
	}
}
