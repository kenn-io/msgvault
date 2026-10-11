package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
)

type controlTestStore struct {
	*mockStore

	calls   int
	grant   *agentgrant.Grant
	request calcontrol.Request
	result  *calcontrol.Result
	err     error
}

func (s *controlTestStore) ControlCalendar(_ context.Context, r calcontrol.Request, g *agentgrant.Grant, _ func(context.Context) (func(), error)) (*calcontrol.Result, error) {
	s.calls++
	s.grant = g
	s.request = r
	if s.result != nil {
		return s.result, s.err
	}
	return &calcontrol.Result{CalendarID: r.CalendarID, SendUpdates: "none"}, s.err
}

const controlTestBody = `{"action":"create","account":"person@example.com","calendar_id":"team@example.com","event":{"summary":"Planning","start":{"dateTime":"2026-10-02T09:00:00Z"},"end":{"dateTime":"2026-10-02T10:00:00Z"}}}`

func TestCalendarControlDelegationAndStrictJSON(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := &controlTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true}}, backend, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, issued, err := srv.agentGrants.Issue("calendar", []agentgrant.Permission{agentgrant.PermissionCalendarWrite}, []agentgrant.SourceRef{{ID: 1, Type: "gcal", Identifier: "person@example.com/team@example.com"}}, time.Time{})
	requirements.NoError(err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(apiprotocol.AgentTokenHeader, secret)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NotNil(backend.grant)
	assertions.Equal(issued.ID, backend.grant.ID)
	assertions.Equal("team@example.com", backend.request.CalendarID)
	for _, body := range []string{strings.Replace(controlTestBody, `"action":"create"`, `"action":"create","unknown":true`, 1), strings.Replace(controlTestBody, `"summary":"Planning"`, `"summary":null`, 1), controlTestBody + controlTestBody, strings.Replace(controlTestBody, `"summary":"Planning"`, `"summary":"Planning","Summary":"Other"`, 1)} {
		before := backend.calls
		request = httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "synthetic-owner-key")
		response = httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		assertions.Equal(http.StatusBadRequest, response.Code, response.Body.String())
		assertions.Equal(before, backend.calls)
	}
	backend.err = calcontrol.ErrDenied
	request = httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "synthetic-owner-key")
	response = httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assertions.Equal(http.StatusForbidden, response.Code)
}
func TestCalendarControlRequiresAuthentication(t *testing.T) {
	backend := &controlTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(t, http.StatusUnauthorized, response.Code)
	assert.Zero(t, backend.calls)
}

func TestCalendarControlUnknownWriteOutcomeIsNotQueryTimeout(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			assertions := assert.New(t)
			backend := &controlTestStore{mockStore: &mockStore{stats: &StoreStats{}}, err: fmt.Errorf("%w; inspect the event before retrying: %w", gcal.ErrOutcomeUnknown, cause)}
			srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
			request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", "synthetic-owner-key")
			response := httptest.NewRecorder()
			srv.Router().ServeHTTP(response, request)
			assertions.Equal(http.StatusBadGateway, response.Code)
			assertions.Contains(response.Body.String(), "calendar_outcome_unknown")
			assertions.Contains(response.Body.String(), "reconcile the calendar state")
			assertions.NotContains(response.Body.String(), "retry")
			assertions.NotContains(response.Body.String(), "inspect the event")
			assertions.NotContains(response.Body.String(), "query_timeout")
			assertions.NotContains(response.Body.String(), "narrow the query")
		})
	}
}

func TestCalendarControlPartialUnknownOutcomeIsExplicitAndKeepsReceipts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	var result calcontrol.Result
	requirements.NoError(json.Unmarshal([]byte(`{"account":"person@example.com","calendar_id":"team@example.com","send_updates":"none","plan":[],"writes":[{"action":"update","calendar_id":"team@example.com","event":{"id":"series","status":"confirmed"},"message_id":42,"archived":true}],"error":"create outcome unknown after completed writes; reconcile the calendar and receipts before taking further action","outcome_unknown":true,"outcome_code":"calendar_outcome_unknown"}`), &result))
	backend := &controlTestStore{mockStore: &mockStore{stats: &StoreStats{}}, result: &result}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "synthetic-owner-key")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var body struct {
		OutcomeUnknown bool                      `json:"outcome_unknown"`
		OutcomeCode    string                    `json:"outcome_code"`
		Writes         []calcontrol.WriteReceipt `json:"writes"`
	}
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &body))
	assertions.True(body.OutcomeUnknown)
	assertions.Equal("calendar_outcome_unknown", body.OutcomeCode)
	requirements.Len(body.Writes, 1)
	assertions.Equal(int64(42), body.Writes[0].MessageID)
	assertions.Equal("series", body.Writes[0].Event.ID)
}

func TestCalendarControlPlanChangeUses409(t *testing.T) {
	backend := &controlTestStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		err:       calcontrol.ErrPlanChanged,
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "synthetic-owner-key")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "calendar_plan_changed")
}

func TestCalendarControlPreconditionConflictUses409(t *testing.T) {
	backend := &controlTestStore{
		mockStore: &mockStore{stats: &StoreStats{}},
		err:       &gcal.PreconditionFailedError{},
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, backend, nil, testLogger())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", strings.NewReader(controlTestBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "synthetic-owner-key")
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)

	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "calendar_event_conflict")
	assert.Contains(t, response.Body.String(), "calendar event changed")
}

func TestCalendarControlReadsBodyBeforeTakingGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		gate := NewSerialOperationGate()
		backend := &controlTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
		srv := NewServerWithOptions(ServerOptions{
			Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}},
			Store:  backend, Logger: testLogger(), OperationGate: gate,
		})
		defer func() { assertions.NoError(srv.Shutdown(context.Background())) }()
		reader, writer := io.Pipe()
		defer func() { assertions.NoError(reader.Close()) }()
		defer func() { assertions.NoError(writer.Close()) }()
		request := httptest.NewRequest(http.MethodPost, "/api/v1/calendar/control", reader)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "synthetic-owner-key")
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.Router().ServeHTTP(response, request)
		}()
		synctest.Wait()
		_, _, held := gate.Holder()
		assertions.False(held, "incomplete calendar upload must not hold the archive gate")
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		release, acquired := gate.BeginWorkContext(ctx)
		cancel()
		assertions.True(acquired, "unrelated archive work remains available during the upload")
		if acquired {
			release()
		}
		_, err := io.WriteString(writer, controlTestBody)
		requirements.NoError(err)
		requirements.NoError(writer.Close())
		<-done
		assertions.Equal(http.StatusOK, response.Code, response.Body.String())
	})
}
