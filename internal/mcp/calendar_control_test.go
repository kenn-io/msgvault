package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/query/querytest"
)

type calendarServiceBackend func(context.Context, calcontrol.Request) (*calcontrol.Result, error)

func (f calendarServiceBackend) ControlCalendar(ctx context.Context, request calcontrol.Request) (*calcontrol.Result, error) {
	return f(ctx, request)
}

func TestWriteOnlyCalendarConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name, action, intent string
		arguments            map[string]any
		future, changeSeries bool
	}{
		{name: "add attendee", action: "update", intent: `"add_attendees":["added@example.com"]`, arguments: map[string]any{"add_attendees": []string{"added@example.com"}}},
		{name: "RSVP", action: "respond", intent: `"response":"declined"`, arguments: map[string]any{"response": "declined"}},
		{name: "unchanged future series", action: "update", intent: `"summary":"New title"`, future: true},
		{name: "changed hidden recurrence", action: "update", intent: `"summary":"New title"`, future: true, changeSeries: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var mu sync.Mutex
			count := 4
			var writes []gcal.EventInput
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				var body string
				switch r.Method + " " + r.URL.Path {
				case "GET /users/me/calendarList":
					body = `{"items":[{"id":"team@example.com","accessRole":"owner","timeZone":"UTC"}]}`
				case "GET /calendars/team@example.com/events/event":
					recurrence := ""
					if tc.future {
						recurrence = fmt.Sprintf(`,"recurrence":["RRULE:FREQ=DAILY;COUNT=%d"]`, count)
					}
					body = fmt.Sprintf(`{"id":"event","summary":"Provider title","description":"Provider agenda","location":"Provider room","start":{"dateTime":"2026-10-02T09:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2026-10-02T10:00:00Z","timeZone":"UTC"},"attendees":[{"email":"person@example.com","self":true,"responseStatus":"needsAction"},{"email":"hidden@example.com"}]%s}`, recurrence)
				case "GET /calendars/team@example.com/events":
					body = `{"items":[]}`
				case "GET /calendars/team@example.com/events/event/instances":
					body = `{"items":[{"id":"instance","recurringEventId":"event","originalStartTime":{"dateTime":"2026-10-03T09:00:00Z"},"start":{"dateTime":"2026-10-03T09:00:00Z","timeZone":"UTC"},"end":{"dateTime":"2026-10-03T10:00:00Z","timeZone":"UTC"}}]}`
				case "PATCH /calendars/team@example.com/events/event", "POST /calendars/team@example.com/events":
					var input gcal.EventInput
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &input)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					writes = append(writes, input)
					body = `{"id":"written","status":"confirmed"}`
				default:
					assert.Fail(t, "unexpected provider request", "%s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			t.Cleanup(provider.Close)
			providerClient := gcal.NewClient(nil, gcal.WithBaseURL(provider.URL), gcal.WithHTTPClient(provider.Client()), gcal.WithRateLimiter(gmail.NewRateLimiterWithCapacity(100, 100)))
			t.Cleanup(func() { assert.NoError(t, providerClient.Close()) })
			var archived atomic.Int64
			backend := calendarServiceBackend(func(ctx context.Context, request calcontrol.Request) (*calcontrol.Result, error) {
				// The daemon constructs a fresh service for each operation.
				service := calcontrol.Service{
					Source:  config.GCalSource{Email: "person@example.com", Enabled: true, WriteCalendars: []string{"team@example.com"}, InviteCalendars: []string{"team@example.com"}},
					Client:  providerClient,
					Persist: func(context.Context, gcal.Calendar, gcal.Event) (int64, error) { return archived.Add(1), nil },
				}
				return service.Execute(ctx, request, &agentgrant.Grant{
					Permissions: []agentgrant.Permission{agentgrant.PermissionCalendarWrite, agentgrant.PermissionCalendarInvite},
					Sources:     []agentgrant.SourceRef{{Type: "gcal", Identifier: "person@example.com/team@example.com"}},
				})
			})
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Calendar: backend, CalendarOnly: true, AllowCalendarWrites: true}, true).Connect(t.Context(), serverTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, server.Close()) })
			var message string
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "write-only-calendar-test", Version: "1"}, &sdkmcp.ClientOptions{
				ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					message = request.Params.Message
					if tc.changeSeries {
						mu.Lock()
						count = 6
						mu.Unlock()
					}
					return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"approved": true}}, nil
				},
			})
			session, err := client.Connect(t.Context(), clientTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, session.Close()) })
			args := map[string]any{"account": "person@example.com", "calendar_id": "team@example.com", "event_id": "event"}
			maps.Copy(args, tc.arguments)
			if tc.future {
				args["scope"], args["original_start"] = "future", "2026-10-03T09:00:00Z"
				args["event"] = map[string]any{"summary": "New title"}
			}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "calendar_" + tc.action, Arguments: args})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.Contains(message, tc.intent)
			for _, hidden := range []string{"Provider title", "Provider agenda", "Provider room", "hidden@example.com"} {
				assertions.NotContains(message, hidden)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.changeSeries {
				assertions.True(result.IsError)
				assertions.Empty(writes)
				assertions.Zero(archived.Load())
				return
			}
			assertions.False(result.IsError)
			if tc.future {
				assertions.Contains(message, `"original_start":"2026-10-03T09:00:00Z"`)
				requirements.Len(writes, 2)
				assertions.Equal(new("New title"), writes[1].Summary)
				assertions.Equal(new([]string{"RRULE:FREQ=DAILY;COUNT=3"}), writes[1].Recurrence)
			} else {
				requirements.Len(writes, 1)
				requirements.NotNil(writes[0].Attendees)
				if tc.action == "respond" {
					assertions.Equal([]gcal.Attendee{{Email: "person@example.com", ResponseStatus: "declined"}}, *writes[0].Attendees)
				} else {
					assertions.Contains(*writes[0].Attendees, gcal.Attendee{Email: "added@example.com"})
				}
			}
			assertions.Equal(int64(len(writes)), archived.Load())
		})
	}
}

type calendarMCPFake struct {
	request                  calcontrol.Request
	requests                 []calcontrol.Request
	calls                    int
	changePlanBeforeMutation bool
	omitFingerprint          bool
}

func (f *calendarMCPFake) ControlCalendar(_ context.Context, r calcontrol.Request) (*calcontrol.Result, error) {
	f.request = r
	f.requests = append(f.requests, r)
	f.calls++
	plan := []calcontrol.PlannedWrite{}
	if !calcontrol.IsRead(r.Action) {
		plan = append(plan, calcontrol.PlannedWrite{Action: r.Action, CalendarID: r.CalendarID, Destination: r.Destination, Event: r.Event})
	}
	fingerprint := "original-plan"
	if f.changePlanBeforeMutation && !r.DryRun {
		updatedSummary := "Changed planning"
		plan[0].Event.Summary = &updatedSummary
		fingerprint = "changed-plan"
	}
	if r.ExpectedPlanFingerprint != "" && r.ExpectedPlanFingerprint != fingerprint {
		return nil, calcontrol.ErrPlanChanged
	}
	if f.omitFingerprint {
		fingerprint = ""
	}
	return &calcontrol.Result{CalendarID: r.CalendarID, SendUpdates: "none", DryRun: r.DryRun, Plan: plan, PlanFingerprint: fingerprint, Writes: []calcontrol.WriteReceipt{}}, nil
}
func TestCalendarToolsThroughOfficialSDK(t *testing.T) {
	for _, allowWrites := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only", true: "writes"}[allowWrites], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			backend := &calendarMCPFake{}
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Engine: &querytest.MockEngine{}, Calendar: backend, AllowCalendarWrites: allowWrites}, allowWrites).Connect(t.Context(), serverTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, server.Close()) })
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-test", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, session.Close()) })
			listed, err := session.ListTools(t.Context(), nil)
			requirements.NoError(err)
			names := map[string]bool{}
			for _, tool := range listed.Tools {
				names[tool.Name] = true
			}
			assertions.Contains(names, "calendar_freebusy")
			assertions.Contains(names, "calendar_conflicts")
			if !allowWrites {
				assertions.NotContains(names, "calendar_create")
				return
			}
			requirements.Contains(names, "calendar_create")
			args := map[string]any{"account": "person@example.com", "calendar_id": "team@example.com", "dry_run": true, "event": map[string]any{"summary": "Planning", "start": map[string]any{"dateTime": "2026-10-02T09:00:00Z"}, "end": map[string]any{"dateTime": "2026-10-02T10:00:00Z"}}}
			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "calendar_create", Arguments: args})
			requirements.NoError(err)
			assertions.False(result.IsError)
			assertions.Equal("create", backend.request.Action)
			assertions.Equal("none", backend.request.SendUpdates)
			assertions.True(backend.request.DryRun)
			calls := backend.calls
			args["action"] = "delete"
			result, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "calendar_create", Arguments: args})
			assertions.True(err != nil || (result != nil && result.IsError))
			assertions.Equal(calls, backend.calls)
		})
	}
}

func TestCalendarMutationRequiresOutOfBandConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		supported       bool
		response        *sdkmcp.ElicitResult
		wantCalls       int
		wantToolError   bool
		wantCallError   bool
		wantWrite       bool
		changePlan      bool
		omitFingerprint bool
	}{
		{name: "missing plan fingerprint", omitFingerprint: true, wantCalls: 1, wantToolError: true},
		{name: "client without elicitation support fails closed", wantCalls: 1, wantCallError: true},
		{name: "declined", supported: true, response: &sdkmcp.ElicitResult{Action: "decline"}, wantCalls: 1, wantToolError: true},
		{name: "cancelled", supported: true, response: &sdkmcp.ElicitResult{Action: "cancel"}, wantCalls: 1, wantToolError: true},
		{name: "accept without explicit approval fails closed", supported: true, response: &sdkmcp.ElicitResult{Action: "accept"}, wantCalls: 1, wantToolError: true},
		{name: "accepted", supported: true, response: &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"approved": true}}, wantCalls: 2, wantWrite: true},
		{name: "changed plan rejects approved mutation", supported: true, response: &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"approved": true}}, wantCalls: 2, wantToolError: true, changePlan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			backend := &calendarMCPFake{changePlanBeforeMutation: tc.changePlan, omitFingerprint: tc.omitFingerprint}
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Calendar: backend, CalendarOnly: true, AllowCalendarWrites: true}, true).Connect(t.Context(), serverTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, server.Close()) })
			clientOptions := &sdkmcp.ClientOptions{}
			elicitationCalls := 0
			elicitationMessage := ""
			if tc.supported {
				clientOptions.ElicitationHandler = func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
					elicitationCalls++
					elicitationMessage = request.Params.Message
					return tc.response, nil
				}
			}
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-confirmation-test", Version: "1"}, clientOptions)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, session.Close()) })

			result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
				Name: "calendar_create",
				Arguments: map[string]any{
					"account": "person@example.com", "calendar_id": "team@example.com",
					"event": map[string]any{
						"summary": "Planning",
						"start":   map[string]any{"dateTime": "2026-10-02T09:00:00Z"},
						"end":     map[string]any{"dateTime": "2026-10-02T10:00:00Z"},
					},
				},
			})

			if tc.wantCallError {
				requirements.Error(err)
				assertions.Nil(result)
			} else {
				requirements.NoError(err)
				requirements.NotNil(result)
				assertions.Equal(tc.wantToolError, result.IsError)
			}
			assertions.Len(backend.requests, tc.wantCalls)
			for i, request := range backend.requests {
				wantMutation := (tc.wantWrite || tc.changePlan) && i == tc.wantCalls-1
				wantDryRun := !wantMutation
				assertions.Equal(wantDryRun, request.DryRun, "only an explicitly approved follow-up may write")
				if wantMutation {
					assertions.Equal("original-plan", request.ExpectedPlanFingerprint)
				} else {
					assertions.Empty(request.ExpectedPlanFingerprint)
				}
			}
			if tc.supported {
				assertions.Equal(1, elicitationCalls)
				assertions.Contains(elicitationMessage, "person@example.com")
				assertions.Contains(elicitationMessage, "Planning")
			}
		})
	}
}

func TestCalendarApprovalCannotAuthorizeChangedArguments(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &calendarMCPFake{}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{Calendar: backend, CalendarOnly: true, AllowCalendarWrites: true}, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-changed-arguments-test", Version: "1"}, &sdkmcp.ClientOptions{
		MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true},
	})
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	args := map[string]any{
		"account": "person@example.com", "calendar_id": "team@example.com",
		"event": map[string]any{
			"summary": "Planning",
			"start":   map[string]any{"dateTime": "2026-10-02T09:00:00Z"},
			"end":     map[string]any{"dateTime": "2026-10-02T10:00:00Z"},
		},
	}
	first, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "calendar_create", Arguments: args})
	requirements.NoError(err)
	requirements.NotNil(first)
	assertions.True(first.NeedsInput())
	requirements.Len(backend.requests, 1)
	assertions.True(backend.requests[0].DryRun)

	changedArgs := map[string]any{
		"account": "person@example.com", "calendar_id": "team@example.com",
		"event": map[string]any{
			"summary": "Different planning",
			"start":   map[string]any{"dateTime": "2026-10-02T09:00:00Z"},
			"end":     map[string]any{"dateTime": "2026-10-02T10:00:00Z"},
		},
	}
	second, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "calendar_create", Arguments: changedArgs, RequestState: first.RequestState,
		InputResponses: sdkmcp.InputResponseMap{
			calendarMutationApprovalRequest: &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"approved": true}},
		},
	})
	requirements.NoError(err)
	requirements.NotNil(second)
	assertions.True(second.IsError)
	assertions.Len(backend.requests, 1, "changed tool arguments must not reach the backend after confirmation")
}

func TestCalendarToolsDelegatedBridgeExcludesArchiveAccess(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{Calendar: &calendarMCPFake{}, CalendarOnly: true, AllowCalendarWrites: true}, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "delegated-calendar-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	listed, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assertions.ElementsMatch([]string{"calendar_create", "calendar_update", "calendar_delete", "calendar_move", "calendar_respond", "calendar_freebusy", "calendar_conflicts"}, names)
}

func TestCalendarMutationToolsRequireExplicitOptIn(t *testing.T) {
	for _, tc := range []struct {
		name                string
		allowWrites         bool
		allowCalendarWrites bool
		wantCalendarWrites  bool
	}{
		{name: "calendar opt-in required", allowWrites: true},
		{name: "general write policy still applies", allowCalendarWrites: true},
		{name: "both policies enabled", allowWrites: true, allowCalendarWrites: true, wantCalendarWrites: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
			server, err := newMCPServer(ServeOptions{Calendar: &calendarMCPFake{}, AllowCalendarWrites: tc.allowCalendarWrites}, tc.allowWrites).Connect(t.Context(), serverTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, server.Close()) })
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-opt-in-test", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, session.Close()) })
			listed, err := session.ListTools(t.Context(), nil)
			requirements.NoError(err)
			names := map[string]*sdkmcp.Tool{}
			for _, tool := range listed.Tools {
				names[tool.Name] = tool
			}
			if tc.wantCalendarWrites {
				requirements.Contains(names, "calendar_create")
				assertions.Contains(names["calendar_create"].Description, "untrusted input")
			} else {
				assertions.NotContains(names, "calendar_create")
			}
		})
	}
}

func TestCalendarAvailabilityAcceptsExplicitCalendarIDsWithoutCalendarID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &calendarMCPFake{}
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{Calendar: backend, CalendarOnly: true}, false).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-availability-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "calendar_freebusy",
		Arguments: map[string]any{
			"account":      "person@example.com",
			"calendar_ids": []string{"team@example.com"},
			"time_min":     "2026-10-02T09:00:00Z",
			"time_max":     "2026-10-02T10:00:00Z",
		},
	})
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.False(result.IsError)
	assertions.Equal(1, backend.calls)
	assertions.Empty(backend.request.CalendarID)
	assertions.Equal([]string{"team@example.com"}, backend.request.CalendarIDs)
}

func TestCalendarRespondScopeSchemaOmitsUnsupportedFutureScope(t *testing.T) {
	requirements := require.New(t)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	server, err := newMCPServer(ServeOptions{Calendar: &calendarMCPFake{}, CalendarOnly: true, AllowCalendarWrites: true}, true).Connect(t.Context(), serverTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "calendar-scope-test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, session.Close()) })
	listed, err := session.ListTools(t.Context(), nil)
	requirements.NoError(err)
	var inputSchema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	for _, tool := range listed.Tools {
		if tool.Name == "calendar_respond" {
			encoded, err := json.Marshal(tool.InputSchema)
			requirements.NoError(err)
			requirements.NoError(json.Unmarshal(encoded, &inputSchema))
			break
		}
	}
	requirements.NotNil(inputSchema.Properties["scope"].Enum)
	assert.NotContains(t, inputSchema.Properties["scope"].Enum, "future")
	assert.Contains(t, inputSchema.Properties["scope"].Enum, "all")
}
