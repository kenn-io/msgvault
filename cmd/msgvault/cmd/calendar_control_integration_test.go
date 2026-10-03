package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
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
	var creates atomic.Int64
	var reader atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic-calendar-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /users/me/calendarList":
			role := "owner"
			if reader.Load() {
				role = "reader"
			}
			assertions.NoError(json.MarshalWrite(w, gcal.CalendarListPage{Items: []gcal.Calendar{{ID: "team@example.com", AccessRole: role}, {ID: "other@example.com", AccessRole: "writer"}}}))
		case "POST /calendars/team@example.com/events":
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
		assert.True(t, write)
		return gcal.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-calendar-token"}), gcal.WithBaseURL(provider.URL)), nil
	}}
	daemon := httptest.NewServer(api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router())
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
	issued, err := owner.IssueAgentToken(t.Context(), "calendar writer", []string{"calendar.write"}, []int64{source.ID}, nil)
	requirements.NoError(err)
	delegated, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AgentToken: issued.Secret, AllowInsecure: true, HTTPClient: daemon.Client()})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, delegated.Close()) })
	request.DryRun = true
	result, err = delegated.ControlCalendar(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal(fingerprint, result.PlanFingerprint)
	assertions.True(result.DryRun)
	assertions.Equal(int64(1), creates.Load())
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
	reader.Store(true)
	_, err = owner.ControlCalendar(t.Context(), request)
	requirements.Error(err)
	assertions.Contains(err.Error(), "owner or writer")
	assertions.Equal(int64(1), creates.Load())
}
