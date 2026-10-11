package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestCalendarControlClassifiesDaemonSetupFailures(t *testing.T) {
	for _, tc := range []struct {
		name, token, code         string
		missingToken, closedStore bool
		status                    int
	}{
		{name: "missing write consent", token: gmailCalendarDriveTokenJSON, status: http.StatusForbidden, code: "calendar_denied"},
		{name: "missing calendar consent", token: gmailOnlyTokenJSON, status: http.StatusForbidden, code: "calendar_denied"},
		{name: "missing token", token: gmailCalendarDriveTokenJSON, missingToken: true, status: http.StatusForbidden, code: "calendar_denied"},
		{name: "store failure", token: gmailCalendarDriveTokenJSON, closedStore: true, status: http.StatusInternalServerError, code: "calendar_internal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			tokenPath, restore := seedTokenEnv(t, tc.token)
			defer restore()
			if tc.missingToken {
				requirements.NoError(os.Remove(tokenPath))
			}
			home := filepath.Dir(filepath.Dir(tokenPath))
			cfg := config.NewDefaultConfig()
			cfg.HomeDir, cfg.Data.DataDir = home, home
			cfg.OAuth.ClientSecrets = filepath.Join(home, "client_secret.json")
			cfg.GCal = []config.GCalSource{{Email: scopeEscalationAccount, Enabled: true, WriteCalendars: []string{"team@example.com"}}}
			st := testutil.NewTestStore(t)
			if tc.closedStore {
				requirements.NoError(st.Close())
			}
			adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
			start := gcal.EventDateTime{DateTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
			end := gcal.EventDateTime{DateTime: start.DateTime.Add(time.Hour)}
			title := "Planning"
			control := calcontrol.Request{Action: "create", Account: scopeEscalationAccount, CalendarID: "team@example.com", Event: gcal.EventInput{Summary: &title, Start: &start, End: &end}}
			requirements.NoError(control.Validate())
			body, err := json.Marshal(control)
			requirements.NoError(err)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router().ServeHTTP(response, request)
			assertions.Equal(tc.status, response.Code, response.Body.String())
			assertions.Contains(response.Body.String(), tc.code)
			assertions.NotContains(response.Body.String(), "calendar_failed")
		})
	}
}

func TestCalendarControlDaemonClientArchiveAndDelegatedGrants(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	gate := api.NewSerialOperationGate()
	var creates atomic.Int64
	var reader atomic.Bool
	var writeClient atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-calendar-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /users/me/calendarList":
			role := "owner"
			if reader.Load() {
				role = "reader"
			}
			assertions.NoError(json.MarshalWrite(w, gcal.CalendarListPage{Items: []gcal.Calendar{{ID: "team@example.com", AccessRole: role, Primary: true}, {ID: "other@example.com", AccessRole: "writer"}}}))
		case "POST /freeBusy":
			assertions.NoError(json.MarshalWrite(w, gcal.FreeBusyResponse{Calendars: map[string]gcal.CalendarBusy{"team@example.com": {Busy: []gcal.BusyPeriod{}}}}))
		case "POST /calendars/team@example.com/events":
			_, _, held := gate.Holder()
			assertions.True(held, "calendar writes must hold the archive operation gate")
			creates.Add(1)
			assert.Equal(t, "none", r.URL.Query().Get("sendUpdates"))
			var input gcal.EventInput
			if !assertions.NoError(json.UnmarshalRead(r.Body, &input)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assert.Nil(t, input.Attendees)
			if !assertions.NotNil(input.Summary) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assertions.NoError(json.MarshalWrite(w, gcal.Event{ID: "created", Status: "confirmed", Summary: *input.Summary, Start: *input.Start, End: *input.End, Organizer: gcal.Person{Email: "team@example.com"}}))
		default:
			assert.Fail(t, "unexpected Google request", r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(provider.Close)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}, GCal: []config.GCalSource{{Email: "Person@Example.COM", Enabled: true, WriteCalendars: []string{"team@example.com", "other@example.com"}}}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler), calendarClientFactory: func(_ context.Context, source config.GCalSource, write bool) (gcal.ControlAPI, error) {
		assert.Equal(t, "person@example.com", source.Email)
		writeClient.Store(write)
		return gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-calendar-token"}), gcal.WithBaseURL(provider.URL)), nil
	}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler), OperationGate: gate})
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, owner.Close()) })
	title := "Planning"
	start := gcal.EventDateTime{DateTime: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)}
	end := gcal.EventDateTime{DateTime: start.DateTime.Add(time.Hour)}
	request := calcontrol.Request{Action: "create", Account: "person@example.com", CalendarID: "team@example.com", Event: gcal.EventInput{Summary: &title, Start: &start, End: &end}}
	result, err := owner.ControlCalendar(t.Context(), request)
	requirements.NoError(err)
	assertions.NotEmpty(result.PlanFingerprint)
	fingerprint := result.PlanFingerprint
	requirements.Len(result.Writes, 1)
	assertions.True(result.Writes[0].Archived)
	requirements.Positive(result.Writes[0].MessageID)
	source, err := st.GetSourceByIdentifier("person@example.com/team@example.com")
	requirements.NoError(err)
	meta, err := st.GetMessageMetadata(result.Writes[0].MessageID)
	requirements.NoError(err)
	var archivedMetadata struct {
		OrganizerEmail string `json:"organizer_email"`
		AccountEmail   string `json:"account_email"`
	}
	requirements.NoError(json.Unmarshal([]byte(meta.String), &archivedMetadata))
	assertions.Equal("team@example.com", archivedMetadata.OrganizerEmail)
	assertions.Equal("person@example.com", archivedMetadata.AccountEmail)
	issued, err := owner.IssueAgentToken(t.Context(), "calendar writer", []string{"calendar.write"}, []int64{source.ID}, nil, time.Time{})
	requirements.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AgentToken: issued.Secret, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, delegated.Close()) })
	releaseGate, ok := gate.BeginLabeledWorkContext(t.Context(), "scheduled source sync")
	requirements.True(ok)
	releaseGate = sync.OnceFunc(releaseGate)
	defer releaseGate()
	for _, action := range []string{"freebusy", "conflicts"} {
		availability, err := owner.ControlCalendar(t.Context(), calcontrol.Request{
			Action: action, Account: request.Account, CalendarID: request.CalendarID,
			TimeMin: start.DateTime, TimeMax: end.DateTime,
		})
		requirements.NoError(err)
		assertions.NotNil(availability.FreeBusy)
		assertions.False(writeClient.Load())
	}
	request.DryRun = true
	result, err = delegated.ControlCalendar(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(fingerprint, result.PlanFingerprint)
	assertions.True(result.DryRun)
	assertions.True(writeClient.Load())
	assertions.Equal(int64(1), creates.Load())
	request.CalendarID = "primary"
	_, err = delegated.ControlCalendar(t.Context(), request)
	requirements.NoError(err, "primary is authorized against its live canonical ID")
	otherSource, err := st.GetOrCreateSource(sourceTypeCalendar, "person@example.com/other@example.com")
	requirements.NoError(err)
	otherGrant, err := owner.IssueAgentToken(t.Context(), "other calendar", []string{"calendar.write"}, []int64{otherSource.ID}, nil, time.Time{})
	requirements.NoError(err)
	primaryBody, err := json.Marshal(request)
	requirements.NoError(err)
	primaryRequest := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", bytes.NewReader(primaryBody))
	primaryRequest.Header.Set("Content-Type", "application/json")
	primaryRequest.Header.Set(apiprotocol.AgentTokenHeader, otherGrant.Secret)
	primaryResponse := httptest.NewRecorder()
	server.Router().ServeHTTP(primaryResponse, primaryRequest)
	assertions.Equal(http.StatusForbidden, primaryResponse.Code)
	var primaryError api.ErrorResponse
	requirements.NoError(json.Unmarshal(primaryResponse.Body.Bytes(), &primaryError))
	assertions.Equal(calcontrol.ErrDenied.Error(), primaryError.Message)
	request.CalendarID = "other@example.com"
	_, err = delegated.ControlCalendar(t.Context(), request)
	requirements.Error(err)
	assertions.Equal(int64(1), creates.Load())
	request.CalendarID = "team@example.com"
	request.Event.Attendees = &[]gcal.Attendee{{Email: "guest@example.com"}}
	_, err = delegated.ControlCalendar(t.Context(), request)
	requirements.Error(err)
	assertions.Equal(int64(1), creates.Load())
	request.Event.Attendees = nil
	request.DryRun = false
	body, err := json.Marshal(request)
	requirements.NoError(err)
	waitCtx, cancel := context.WithCancel(t.Context())
	blocked := httptest.NewRequestWithContext(waitCtx, http.MethodPost, "/api/v1/calendar/control", bytes.NewReader(body))
	blocked.Header.Set("Content-Type", "application/json")
	blocked.Header.Set("X-Api-Key", cfg.Server.APIKey)
	response := httptest.NewRecorder()
	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		server.Router().ServeHTTP(response, blocked)
	}()
	assertions.Eventually(gate.HasRequestWaiters, time.Second, time.Millisecond, "validated mutation queues behind the held gate")
	cancel()
	<-blockedDone
	assertions.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "operation_in_progress")
	assertions.Equal(int64(1), creates.Load(), "blocked mutation never reaches the provider")
	releaseGate()
	reader.Store(true)
	_, err = owner.ControlCalendar(t.Context(), request)
	requirements.Error(err)
	assertions.Contains(err.Error(), "owner or writer")
	assertions.Equal(int64(1), creates.Load())
}

func TestCalendarControlDelegatedSetupHidesConfiguration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	team, err := st.GetOrCreateSource(sourceTypeCalendar, "person@example.com/team@example.com")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource(sourceTypeCalendar, "person@example.com/other@example.com")
	requirements.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	const credentialPath = "/configured/calendar-credentials.json"
	var clientCalls atomic.Int64
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: logger, calendarClientFactory: func(context.Context, config.GCalSource, bool) (gcal.ControlAPI, error) {
		clientCalls.Add(1)
		return nil, fmt.Errorf("read OAuth credentials %s: unavailable", credentialPath)
	}}
	server := api.NewServer(cfg, adapter, nil, logger)
	daemon := httptest.NewServer(server.Router())
	t.Cleanup(daemon.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, owner.Close()) })
	configured := []config.GCalSource{{Name: "work", Email: "Person@Example.COM", Enabled: true,
		WriteCalendars: []string{"team@example.com"}, CalendarAliases: map[string]string{"team": "team@example.com"}}}
	const body = `{"action":"create","account":"work","calendar_id":"team","event":{"summary":"Planning","start":{"dateTime":"2026-10-02T09:00:00Z"},"end":{"dateTime":"2026-10-02T10:00:00Z"}}}`
	for _, tc := range []struct {
		name, permission, calendar string
		sourceID                   int64
		configured                 bool
		clientCalls                int64
	}{
		{"missing capability and configuration", "calendar.read", "team", team.ID, false, 0},
		{"missing capability with configuration", "calendar.read", "team", team.ID, true, 0},
		{"unrelated calendar grant", "calendar.write", "team", other.ID, true, 0},
		{"authorized setup failure", "calendar.write", "team", team.ID, true, 1},
		{"primary setup failure", "calendar.write", "primary", other.ID, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			if tc.configured {
				cfg.GCal = configured
			} else {
				cfg.GCal = nil
			}
			issued, err := owner.IssueAgentToken(t.Context(), tc.name, []string{tc.permission}, []int64{tc.sourceID}, nil, time.Time{})
			requirements.NoError(err)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(strings.Replace(body, `"calendar_id":"team"`, `"calendar_id":"`+tc.calendar+`"`, 1)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(apiprotocol.AgentTokenHeader, issued.Secret)
			before := clientCalls.Load()
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
			var result api.ErrorResponse
			requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
			assertions.Equal(calcontrol.ErrDenied.Error(), result.Message)
			assertions.NotContains(response.Body.String(), credentialPath)
			assertions.Equal(before+tc.clientCalls, clientCalls.Load())
		})
	}
	assertions.Contains(logs.String(), credentialPath, "operator log retains delegated setup diagnostics")
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", cfg.Server.APIKey)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	assertions.Equal(http.StatusInternalServerError, response.Code)
	assertions.Contains(response.Body.String(), credentialPath, "owner retains setup diagnostics")
}
