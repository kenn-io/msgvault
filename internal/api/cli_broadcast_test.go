package api

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseCLIRunBroadcastTable(t *testing.T) {
	valid := [][]string{
		{"subscriptions", "example", "--participant", "300", "--status", "active"}, {"sync-subscriptions", "example", "--max-requests", "2"},
		{"create", "example", "--name", "Offers", "--message-json", `{"text":"Hello"}`, "--daily-budget-cents", "500", "--participant", "300"},
		{"plan", "broadcast-abc", "--revision", "1"}, {"show", "broadcast-abc"}, {"list", "example"}, {"preview", "broadcast-abc"},
		{"send", "broadcast-abc", "--revision", "1", "--plan-digest", "abc", "--limit", "5"}, {"adopt-campaign", "broadcast-abc", "--campaign-id", "55"},
		{"cancel", "broadcast-abc"}, {"unsubscribe", "example", "--subscription", "1"},
	}
	for _, args := range valid {
		t.Run(args[0], func(t *testing.T) {
			got, err := ParseCLIRunBroadcast(CLIRunRequest{Args: append([]string{"broadcast"}, args...)})
			require.NoError(t, err)
			assert.Equal(t, args[0], got.Sub)
		})
	}
	invalid := [][]string{{}, {"unknown", "x"}, {"request-optin", "example"}, {"list"}, {"send", "x", "--revision", "1"}, {"plan", "x"}, {"show", "x", "extra"}, {"list", "x", "--message-json", "{}"}, {"unsubscribe", "x"}, {"unsubscribe", "x", "--participant", "1", "--subscription", "2"}, {"subscriptions", "x", "--participant", "-1"}, {"subscriptions", "x", "--status", "bogus"}}
	for _, msg := range []string{`[]`, `null`, `{bad}`, `{"text":"` + strings.Repeat("a", 32768) + `"}`} {
		invalid = append(invalid, []string{"create", "x", "--name", "offers", "--message-json", msg, "--daily-budget-cents", "1", "--participant", "1"})
	}
	tooMany := []string{"create", "x", "--name", "offers", "--message-json", "{}", "--daily-budget-cents", "1"}
	for range 1001 {
		tooMany = append(tooMany, "--subscription", "1")
	}
	invalid = append(invalid, tooMany)
	for _, args := range invalid {
		_, err := ParseCLIRunBroadcast(CLIRunRequest{Args: append([]string{"broadcast"}, args...)})
		require.Error(t, err, "%v", args[:min(len(args), 5)])
	}
	for _, req := range []CLIRunRequest{{Env: map[string]string{"EXAMPLE": "1"}}, {Cwd: "/tmp"}, {GrantDecided: true}} {
		req.Args = []string{"broadcast", "list", "example"}
		_, err := ParseCLIRunBroadcast(req)
		require.Error(t, err)
	}
}
func TestCLIRunAllowsOnlyParsedBroadcast(t *testing.T) {
	assert.True(t, cliRunCommandAllowed([]string{"broadcast", "list", "example"}))
	assert.False(t, cliRunCommandAllowed([]string{"broadcast", "send", "id"}))
	assert.False(t, cliRunCommandAllowed([]string{"broadcast", "unknown"}))
}
func TestScheduledSubscriptionSyncNotSourceJob(t *testing.T) {
	assertions := assert.New(t)

	assertions.True(isSourceSyncJobName("messenger_broadcasts:app:9:page:100"))
	_, ok := SchedulerJobNameForSource("messenger_broadcasts", "app:9:page:100")
	assertions.False(ok)
	name, ok := SchedulerJobNameForSource("facebook_messages", "app:9:page:100")
	assertions.True(ok)
	assertions.Equal("facebook_messages:app:9:page:100", name)
}

func TestBroadcastSuppressionLeaseRetainsGate(t *testing.T) {
	requirements := require.New(t)
	gate := NewSerialOperationGate()
	releaseSend, ok := gate.BeginLabeledWorkContext(t.Context(), "msgvault broadcast send")
	requirements.True(ok)
	releaseSuppression, ok := gate.beginConcurrentBroadcastSendWorkContext(t.Context())
	requirements.True(ok)
	releaseSend()
	_, _, held := gate.Holder()
	assert.True(t, held, "suppression must finish before a backup can take the gate")
	_, ok = gate.beginConcurrentBroadcastSendWorkContext(t.Context())
	assert.False(t, ok, "no additional shared work after the send finished")
	gate.StartDrain()
	wait, cancel := context.WithCancel(t.Context())
	cancel()
	requirements.ErrorIs(gate.Wait(wait), context.Canceled)
	releaseSuppression()
	requirements.NoError(gate.Wait(t.Context()))
}

func TestBroadcastSuppressionDoesNotShareBackupLease(t *testing.T) {
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "backup freeze")
	require.True(t, ok)
	defer release()
	_, ok = gate.beginConcurrentBroadcastSendWorkContext(t.Context())
	assert.False(t, ok)
}

func TestBroadcastSuppressionAdmissionRequiresValidRequest(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, req := range []CLIRunRequest{
		{Args: []string{"broadcast", "cancel"}},
		{Args: []string{"broadcast", "cancel", "broadcast-example", "--message-json", `{}`}},
		{Args: []string{"broadcast", "unsubscribe", "example", "--subscription", "-1"}},
		{Args: []string{"broadcast", "cancel", "broadcast-example"}, Cwd: "/tmp"},
		{Args: []string{"broadcast", "cancel", "broadcast-example"}, Env: map[string]string{"EXAMPLE": "1"}},
	} {
		body, err := json.Marshal(req)
		requirements.NoError(err)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", bytes.NewReader(body))
		label, skip, err := cliRunGateDecision(r, requestAuthentication{Mode: AuthModeLoopback})
		requirements.NoError(err)
		assertions.False(skip)
		assertions.NotEqual("msgvault broadcast cancel", label)
		assertions.NotEqual("msgvault broadcast unsubscribe", label)
	}
}
