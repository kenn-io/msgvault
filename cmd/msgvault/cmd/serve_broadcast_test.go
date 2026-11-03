package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunCLIBroadcastRefusesDelegatedGrant(t *testing.T) {
	a := &storeAPIAdapter{}
	err := a.runCLIBroadcast(t.Context(), api.CLIRunRequest{Args: []string{"broadcast", "list", "example"}, Grant: &agentgrant.Grant{}}, nil)
	require.Error(t, err)
	assert.Equal(t, "not_permitted", err.Error())
}
func TestRunCLIBroadcastMissingTokenEnv(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	cfg, st, _ := broadcastWebhookConfig(t)
	p := &cfg.MessengerBroadcasts[0]
	p.TokenEnv = "MSGVAULT_TEST_BROADCAST_PAGE_TOKEN"
	p.AdTokenEnv = "MSGVAULT_TEST_BROADCAST_AD_TOKEN"
	p.Actions = []string{"send"}
	t.Setenv(p.TokenEnv, "")
	t.Setenv(p.AdTokenEnv, "")
	a := &storeAPIAdapter{store: st, config: cfg}
	err := a.runCLIBroadcast(t.Context(), api.CLIRunRequest{Args: []string{"broadcast", "sync-subscriptions", p.Identifier}}, nil)
	requirements.Error(err)
	assertions.Equal(`broadcast_credentials_missing: set MSGVAULT_TEST_BROADCAST_PAGE_TOKEN in the daemon environment for profile "example-offers"`, err.Error())
	var classified interface{ ExitCode() int }
	requirements.ErrorAs(err, &classified)
	assertions.Equal(7, classified.ExitCode())
}
func TestBroadcastJSONOutputHasNoToken(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	cfg, st, sourceID := broadcastWebhookConfig(t)
	sub, err := st.ApplyConsentWebhookContext(t.Context(), sourceID, store.ConsentWebhook{Token: "synthetic-mm-token-1", PSID: "300", At: time.Now(), EvidenceRef: "fixture:0"})
	requirements.NoError(err)
	a := &storeAPIAdapter{store: st, config: cfg}
	var out strings.Builder
	err = a.runCLIBroadcast(t.Context(), api.CLIRunRequest{Args: []string{"broadcast", "subscriptions", cfg.MessengerBroadcasts[0].Identifier, "--json"}}, func(e api.CLIRunEvent) error { out.WriteString(e.Data); return nil })
	requirements.NoError(err)
	assertions.Contains(out.String(), sub.Fingerprint)
	assertions.NotContains(out.String(), "synthetic-mm-token")
	assertions.Contains(out.String(), "token_fingerprint")
	var rows []map[string]any
	requirements.NoError(json.Unmarshal([]byte(out.String()), &rows))
	requirements.Len(rows, 1)
}
func TestScheduledSubscriptionSyncJobName(t *testing.T) {
	s := scheduler.New(nil)
	p := config.MessengerBroadcastProfile{AppID: "9", PageID: "100", Schedule: "0 * * * *"}
	err := registerScheduledBroadcastJob(s, p, func(context.Context) error { return nil })
	require.NoError(t, err)
	assert.True(t, s.IsJobScheduled("messenger_broadcasts:app:9:page:100"))
}
func TestBroadcastPolicyConversion(t *testing.T) {
	p := config.MessengerBroadcastProfile{AppID: "9", PageID: "100", AdAccountID: "700", Enabled: true, Actions: []string{"send"}, MaxRecipients: 50, MinIntervalHours: 24, ConsentMaxAgeHours: 12}
	got := broadcastPolicy(p)
	assert.Equal(t, meta.PlatformMessenger, got.Source.Platform)
	assert.True(t, got.AllowSend)
	assert.Equal(t, 12*time.Hour, got.PlanPolicy.ConsentMaxAge)
}

func TestRunCLIBroadcastCreatePlanCancelWithoutCredentials(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	cfg, st, sourceID := broadcastWebhookConfig(t)
	cfg.MessengerBroadcasts[0].CampaignDays = 7
	sub, err := st.ApplyConsentWebhookContext(t.Context(), sourceID, store.ConsentWebhook{Token: "synthetic-mm-token-1", PSID: "300", At: time.Now().Add(-time.Minute), Expiry: time.Now().Add(time.Hour), EvidenceRef: "fixture:0"})
	requirements.NoError(err)
	a := &storeAPIAdapter{store: st, config: cfg}
	run := func(args ...string) map[string]any {
		t.Helper()
		var output strings.Builder
		err := a.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: append([]string{"broadcast"}, args...)}, func(e api.CLIRunEvent) error { output.WriteString(e.Data); return nil }, func(context.Context, []string, map[string]string, string, func(string, string) error) error {
			assert.Fail(t, "broadcast must execute inside the daemon")
			return nil
		})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal([]byte(output.String()), &out))
		return out
	}
	b := run("create", cfg.MessengerBroadcasts[0].Identifier, "--name", "Offers", "--message-json", `{"text":"Example offer"}`, "--daily-budget-cents", "500", "--subscription", strconv.FormatInt(sub.ID, 10), "--json")
	id, ok := b["id"].(string)
	requirements.True(ok)
	plan := run("plan", id, "--revision", "1", "--json")
	assertions.NotEmpty(plan["plan_digest"])
	assertions.InDelta(float64(875), plan["worst_daily_cents"], 0.01)
	cancelled := run("cancel", id, "--json")
	assertions.Equal("cancelled", cancelled["state"])
}

func TestBroadcastSuppressionDuringActiveSend(t *testing.T) {
	for _, command := range []string{"cancel", "unsubscribe"} {
		t.Run(command, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			cfg, st, src := broadcastWebhookConfig(t)
			// Local suppression remains available with provider sends disabled.
			cfg.MessengerBroadcasts[0].Enabled = false
			cfg.MessengerBroadcasts[0].AdAccountID = "700"
			sub, err := st.ApplyConsentWebhookContext(t.Context(), src, store.ConsentWebhook{Token: "synthetic-suppress-token", At: time.Now(), EvidenceRef: "suppress-fixture"})
			requirements.NoError(err)
			b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: src, AdAccountID: "700", Name: "Offers", MessageJSON: []byte(`{"text":"Example"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
			requirements.NoError(err)
			gate := api.NewSerialOperationGate()
			release, ok := gate.BeginLabeledWorkContext(t.Context(), "msgvault broadcast send")
			requirements.True(ok)
			defer release()
			adapter := &storeAPIAdapter{store: st, config: cfg}
			srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: testLoggerValue(), OperationGate: gate})
			args := []string{"broadcast", "cancel", b.ID}
			if command == "unsubscribe" {
				args = []string{"broadcast", "unsubscribe", cfg.MessengerBroadcasts[0].Identifier, "--subscription", strconv.FormatInt(sub.ID, 10)}
			}
			body, err := json.Marshal(api.CLIRunRequest{Args: args})
			requirements.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
			req.RemoteAddr = "127.0.0.1:4242"
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			requirements.Equal(http.StatusOK, resp.Code, resp.Body.String())
			assertions.NotContains(resp.Body.String(), `"type":"error"`)
			if command == "cancel" {
				got, _, err := st.GetBroadcastContext(t.Context(), b.ID)
				requirements.NoError(err)
				assertions.Equal("cancelled", got.State)
			} else {
				rows, err := st.ListMarketingSubscriptionsContext(t.Context(), src, store.SubscriptionFilter{SubscriptionIDs: []int64{sub.ID}})
				requirements.NoError(err)
				requirements.Len(rows, 1)
				assertions.Equal("unsubscribed", rows[0].Status)
			}
		})
	}
}
