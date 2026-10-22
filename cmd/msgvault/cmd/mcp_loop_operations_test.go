package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/loops"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func loopOperationMCPSession(t *testing.T, backend *daemonMCPOperations, families []mcpserver.OperationFamily, clientOptions *sdkmcp.ClientOptions) *sdkmcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	directory := filepath.Join(t.TempDir(), "mcp")
	opts := daemonMCPServeOptions(ctx, backend.client, nil)
	opts.Operations = backend
	opts.OperationCapabilities = backend.capabilities()
	opts.OperationWriteFamilies = families
	opts.AllowProfileWrites = slices.Contains(families, mcpserver.OperationFamilyRecords)
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeHTTPWithOptions(ctx, opts, mcpserver.HTTPOptions{Addr: "127.0.0.1:0", DiscoveryDirectory: directory, AllowWrites: true})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(10 * time.Second):
			assert.Fail(t, "MCP listener did not stop")
		}
	})
	var endpoints []mcpdiscovery.Endpoint
	require.Eventually(t, func() bool {
		var err error
		endpoints, err = mcpdiscovery.List(directory)
		return err == nil && len(endpoints) == 1
	}, 10*time.Second, 10*time.Millisecond)
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-client", Version: "1"}, clientOptions)
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{Endpoint: endpoints[0].URL}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func loopOperationFixture(t *testing.T) (*storetest.Fixture, *daemonMCPOperations, int64) {
	t.Helper()
	f := storetest.New(t)
	now := time.Now().UTC()
	f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: storetest.OwnerAddress, To: []string{"alex@example.test"}, Body: "Hello", SentAt: now.Add(-10 * 24 * time.Hour)})
	anchor := f.PersistEmail(storetest.EmailSpec{Thread: "owed", From: "alex@example.test", To: []string{storetest.OwnerAddress}, Body: "Can you send the contract?", SentAt: now.Add(-5 * 24 * time.Hour)})
	f.PersistEmail(storetest.EmailSpec{Thread: "asked", From: storetest.OwnerAddress, To: []string{"sam@example.test"}, Body: "Are you free?", SentAt: now.Add(-20 * 24 * time.Hour)})
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}))
	return f, backend, anchor
}

func loopToolOutput[T any](t *testing.T, result *sdkmcp.CallToolResult) T {
	t.Helper()
	require.False(t, result.IsError, "%+v", result)
	data, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var value T
	require.NoError(t, json.Unmarshal(data, &value))
	return value
}

func assertLoopRefusal(t *testing.T, result *mcpserver.OperationResult, code string, uncertain bool) {
	t.Helper()
	require.True(t, result.IsError)
	data, err := json.Marshal(result.Output)
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprintf(`{"error":%q,"operation_may_have_completed":%t}`, code, uncertain), string(data))
}

func TestMCPLoopsSDKDecisionsAndList(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, backend, anchor := loopOperationFixture(t)
	approved := true
	var disclosures []string
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyLoops}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosures = append(disclosures, request.Params.Message)
		action := "accept"
		if !approved {
			action = "decline"
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": approved}}, nil
	}})
	call := func(name string, args map[string]any) *sdkmcp.CallToolResult {
		t.Helper()
		if args == nil {
			args = map[string]any{}
		}
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
		requirements.NoError(err)
		return result
	}
	list := loopToolOutput[generated.LoopListResponse](t, call("list_open_loops", nil))
	requirements.Len(list.Items, 2)
	kinds := []string{}
	for _, item := range list.Items {
		kinds = append(kinds, string(item.Kind))
	}
	assertions.ElementsMatch([]string{"reply_owed", "awaiting_reply"}, kinds)
	truncated := loopToolOutput[generated.LoopListResponse](t, call("list_open_loops", map[string]any{"limit": 1}))
	assertions.True(truncated.Truncated)
	assertions.Len(truncated.Items, 1)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("alex@example.test", "", "example.test"))
	requirements.NoError(err)
	filtered := loopToolOutput[generated.LoopListResponse](t, call("list_open_loops", map[string]any{"person_id": person.ID}))
	requirements.Len(filtered.Items, 1)
	assertions.Equal(anchor, filtered.Items[0].AnchorMessageID)
	args := map[string]any{"message_id": anchor}
	approved = false
	assertions.True(call("close_open_loop", args).IsError)
	var decisions int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM loop_decisions").Scan(&decisions))
	assertions.Zero(decisions)
	approved = true
	done := loopToolOutput[generated.LoopDecisionResponse](t, call("close_open_loop", args))
	assertions.Equal("done", string(done.State))
	assertions.Equal(anchor, done.AnchorMessageID)
	assertions.Contains(disclosures[len(disclosures)-1], fmt.Sprintf(`"message_id":%d`, anchor))
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM loop_decisions").Scan(&decisions))
	assertions.Equal(1, decisions)
	loopToolOutput[generated.LoopDecisionResponse](t, call("reopen_open_loop", args))
	deadline := time.Now().Add(48 * time.Hour)
	until := deadline.Format(time.RFC3339Nano)
	snoozed := loopToolOutput[generated.LoopDecisionResponse](t, call("snooze_open_loop", map[string]any{"message_id": anchor, "until": until}))
	assertions.Equal("snoozed", string(snoozed.State))
	requirements.NotNil(snoozed.SnoozedUntil)
	assertions.True(deadline.Equal(*snoozed.SnoozedUntil))
	assertions.Contains(disclosures[len(disclosures)-1], until)
	loopToolOutput[generated.LoopDecisionResponse](t, call("reopen_open_loop", args))
	date := time.Now().AddDate(0, 0, 3).Format(time.DateOnly)
	midnight, err := time.ParseInLocation(time.DateOnly, date, time.Local)
	requirements.NoError(err)
	dated := loopToolOutput[generated.LoopDecisionResponse](t, call("snooze_open_loop", map[string]any{"message_id": anchor, "until": date}))
	requirements.NotNil(dated.SnoozedUntil)
	assertions.True(midnight.Equal(*dated.SnoozedUntil))
	assertions.Contains(disclosures[len(disclosures)-1], midnight.Format(time.RFC3339Nano))
	loopToolOutput[generated.LoopDecisionResponse](t, call("reopen_open_loop", args))
	assertions.Len(loopToolOutput[generated.LoopListResponse](t, call("list_open_loops", nil)).Items, 2)
}

func TestMCPLoopArgumentValidationAndDateResolution(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	_, backend, anchor := loopOperationFixture(t)
	backend.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("injected", -7*3600)) }
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"list_open_loops", map[string]any{"limit": 0}}, {"list_open_loops", map[string]any{"limit": 201}}, {"list_open_loops", map[string]any{"limit": 1.5}},
		{"list_open_loops", map[string]any{"person_id": 0}}, {"list_open_loops", map[string]any{"person_id": int64(1 << 53)}}, {"list_open_loops", map[string]any{"kind": "reply_owed"}},
		{"close_open_loop", map[string]any{"message_id": 0}}, {"close_open_loop", map[string]any{"message_id": int64(1 << 53)}}, {"close_open_loop", map[string]any{"id": anchor}}, {"close_open_loop", map[string]any{"message_id": anchor, "note": "x"}},
		{"snooze_open_loop", map[string]any{"message_id": anchor, "until": "24h"}}, {"snooze_open_loop", map[string]any{"message_id": anchor, "until": "garbage"}}, {"snooze_open_loop", map[string]any{"message_id": anchor, "until": "2020-01-01"}},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		requirements.NoError(err)
		assertLoopRefusal(t, result, "invalid_arguments", false)
	}
	date, err := backend.loopDeadline("2027-01-02")
	requirements.NoError(err)
	assertions.Equal(time.Local, date.Location())
	assertions.Equal(0, date.Hour())
	disclosure, err := backend.OperationDisclosure(t.Context(), "snooze_open_loop", map[string]any{"message_id": anchor, "until": "2027-01-02"})
	requirements.NoError(err)
	assertions.Contains(disclosure, date.Format(time.RFC3339Nano))
	again, err := backend.OperationDisclosure(t.Context(), "snooze_open_loop", map[string]any{"message_id": anchor, "until": "2027-01-02"})
	requirements.NoError(err)
	assertions.Equal(disclosure, again)
	instant, err := backend.loopDeadline("2027-01-02T03:04:05.123456789+05:30")
	requirements.NoError(err)
	assertions.Equal("2027-01-02T03:04:05.123456789+05:30", instant.Format(time.RFC3339Nano))
}

func TestMCPLoopDeadlineCrossesApproval(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(strconv.FormatBool(resumed), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f, backend, anchor := loopOperationFixture(t)
			now := time.Now()
			deadline := now.Add(time.Hour)
			var clock atomic.Pointer[time.Time]
			clock.Store(&now)
			backend.now = func() time.Time { return *clock.Load() }
			options := &sdkmcp.ClientOptions{}
			if resumed {
				options.MultiRoundTrip = &sdkmcp.MultiRoundTripOptions{Disabled: true}
			} else {
				options.ElicitationHandler = func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					clock.Store(&deadline)
					return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
				}
			}
			session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyLoops}, options)
			params := &sdkmcp.CallToolParams{Name: "snooze_open_loop", Arguments: map[string]any{"message_id": anchor, "until": deadline.Format(time.RFC3339Nano)}}
			result, err := session.CallTool(t.Context(), params)
			requirements.NoError(err)
			if resumed {
				requirements.NotEmpty(result.RequestState)
				clock.Store(&deadline)
				params.RequestState = result.RequestState
				params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
				result, err = session.CallTool(t.Context(), params)
				requirements.NoError(err)
			}
			assertions.True(result.IsError)
			var count int
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM loop_decisions").Scan(&count))
			assertions.Zero(count)
		})
	}
}

func TestMCPLoopResponseClassifierIsScoped(t *testing.T) {
	for _, code := range []string{"loop_not_found", "loop_decision_not_found", "loop_scan_incomplete", "loops_unavailable", "invalid_request", "server_busy"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, err := fmt.Fprintf(w, `{"error":%q}`, code)
				assert.NoError(t, err)
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			result, err := readMCPLoopJSON[generated.LoopDecisionResponse](t.Context(), client, http.MethodPost, "/api/v1/loops/1/done", nil)
			require.NoError(t, err)
			assertLoopRefusal(t, result, code, false)
			result, err = readMCPOperationJSON[generated.Person](t.Context(), client, http.MethodPost, "/api/v1/people", nil, http.StatusOK)
			require.NoError(t, err)
			assertLoopRefusal(t, result, "operation_refused", true)
		})
	}
}

func TestMCPLoopRealRefusals(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, backend, anchor := loopOperationFixture(t)
	for _, tc := range []struct {
		name string
		args map[string]any
		code string
	}{
		{"close_open_loop", map[string]any{"message_id": anchor - 1}, "loop_not_found"},
		{"reopen_open_loop", map[string]any{"message_id": anchor}, "loop_decision_not_found"},
	} {
		result, err := backend.ExecuteOperation(t.Context(), tc.name, tc.args)
		requirements.NoError(err)
		assertLoopRefusal(t, result, tc.code, false)
	}
	backend.now = func() time.Time { return time.Now().Add(-48 * time.Hour) }
	result, err := backend.ExecuteOperation(t.Context(), "snooze_open_loop", map[string]any{"message_id": anchor, "until": time.Now().Add(-time.Hour).Format(time.RFC3339)})
	requirements.NoError(err)
	assertLoopRefusal(t, result, "invalid_request", false)
	unavailable := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Logger: slog.New(slog.DiscardHandler)}))
	result, err = unavailable.ExecuteOperation(t.Context(), "list_open_loops", nil)
	requirements.NoError(err)
	assertLoopRefusal(t, result, "loops_unavailable", false)
	gate := api.NewSerialOperationGate()
	gate.StartDrain()
	busy := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: f.Store}, OperationGate: gate, Logger: slog.New(slog.DiscardHandler)}))
	result, err = busy.ExecuteOperation(t.Context(), "close_open_loop", map[string]any{"message_id": anchor})
	requirements.NoError(err)
	assertLoopRefusal(t, result, "server_busy", false)
	var count int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM loop_decisions").Scan(&count))
	assertions.Zero(count)
}

func TestMCPLoopIncompleteScanNeedsFreshApproval(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	storetest.PersistLoopOriginalReadFixture(f, loops.MaxListEvidenceComputations+1, time.Now().UTC())
	var anchor, conversation int64
	requirements.NoError(f.Store.DB().QueryRow("SELECT id, conversation_id FROM messages WHERE subject = 'Can you send the contract?'").Scan(&anchor, &conversation))
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET conversation_id = ?"), conversation)
	requirements.NoError(err)
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: f.Store}, Logger: slog.New(slog.DiscardHandler)}))
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyLoops}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	call := func(state string) *sdkmcp.CallToolResult {
		t.Helper()
		params := &sdkmcp.CallToolParams{Name: "close_open_loop", Arguments: map[string]any{"message_id": anchor}, RequestState: state}
		if state != "" {
			params.InputResponses = sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
		}
		result, err := session.CallTool(t.Context(), params)
		requirements.NoError(err)
		return result
	}
	pending := call("")
	requirements.NotEmpty(pending.RequestState)
	refused := call(pending.RequestState)
	assertions.True(refused.IsError)
	data, err := json.Marshal(refused.StructuredContent)
	requirements.NoError(err)
	assertions.Contains(string(data), "loop_scan_incomplete")
	var count int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM loop_decisions").Scan(&count))
	assertions.Zero(count)
	assertions.True(call(pending.RequestState).IsError)
	fresh := call("")
	requirements.NotEmpty(fresh.RequestState)
	receipt := loopToolOutput[generated.LoopDecisionResponse](t, call(fresh.RequestState))
	assertions.Equal("done", string(receipt.State))
}

func TestMCPLoopDiscoveryRequiresEveryDescriptorDetail(t *testing.T) {
	_, backend, _ := loopOperationFixture(t)
	for _, tc := range []struct{ name, id string }{{"list_open_loops", "listLoops"}, {"close_open_loop", "markLoopDone"}, {"snooze_open_loop", "snoozeLoop"}, {"reopen_open_loop", "reopenLoop"}} {
		for _, mutation := range []string{"missing", "method", "path", "query-person", "query-limit", "body"} {
			t.Run(tc.id+"/"+mutation, func(t *testing.T) {
				if (mutation == "query-person" || mutation == "query-limit") && tc.id != "listLoops" || mutation == "body" && tc.id != "snoozeLoop" {
					return
				}
				capabilities, err := backend.client.MCPCapabilities(t.Context())
				require.NoError(t, err)
				for i := range capabilities.Routes {
					route := &capabilities.Routes[i]
					if route.OperationID != tc.id {
						continue
					}
					switch mutation {
					case "missing":
						route.OperationID = "unknown"
					case "method":
						route.Method = http.MethodDelete
					case "path":
						route.Path = "/wrong"
					case "query-person":
						route.QueryParameters = []string{"limit"}
					case "query-limit":
						route.QueryParameters = []string{"person_id"}
					case "body":
						route.RequestProperties = nil
					}
				}
				got := newDaemonMCPOperations(backend.client, capabilities).capabilities()
				assert.NotContains(t, got, tc.name)
				for _, other := range []string{"list_open_loops", "close_open_loop", "snooze_open_loop", "reopen_open_loop"} {
					if other != tc.name {
						assert.Contains(t, got, other)
					}
				}
			})
		}
	}
}

func TestMCPLoopReaderPreservesSuccessfulPartialReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	// Error-shaped fields inside a successful result belong to the owning operation.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, err := fmt.Fprint(w, `{"completed":1,"failure":{"error":"server_busy"}}`)
		assertions.NoError(err)
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	result, err := readMCPLoopJSON[map[string]any](t.Context(), client, http.MethodPost, "/api/v1/loops/1/done", nil)
	requirements.NoError(err)
	output := operationOutput[map[string]any](t, result)
	assertions.InDelta(1, output["completed"], 0)
	assertions.Equal(map[string]any{"error": "server_busy"}, output["failure"])
}
