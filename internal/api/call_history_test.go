package api

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type callHTTPStore struct {
	*mockStore
	CallHistoryStore
}

func TestCallQueryIntegerParameterErrors(t *testing.T) {
	for _, name := range []string{"person_id", "after_id", "limit", "offset"} {
		t.Run(name, func(t *testing.T) {
			_, err := callQueryFromRequest(httptest.NewRequest(http.MethodGet, "/api/v1/calls?"+name+"=invalid", nil))
			var paramErr *paramError
			require.ErrorAs(t, err, &paramErr)
			assert.Equal(t, name, paramErr.param)
		})
	}
}

func TestCallQueryDateParameters(t *testing.T) {
	for _, name := range []string{"after", "before"} {
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			for _, tc := range []struct {
				value string
				want  *time.Time
			}{
				{"", nil},
				{"2026-10-01", new(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))},
				{"2026-10-01T09:00:00.123456789-04:00", new(time.Date(2026, 10, 1, 13, 0, 0, 123456789, time.UTC))},
			} {
				q, err := callQueryFromRequest(httptest.NewRequest(http.MethodGet, "/api/v1/calls?"+name+"="+url.QueryEscape(tc.value), nil))
				requirements.NoError(err)
				got := q.After
				if name == "before" {
					got = q.Before
				}
				assertions.Equal(tc.want, got)
			}
			_, err := callQueryFromRequest(httptest.NewRequest(http.MethodGet, "/api/v1/calls?"+name+"=invalid", nil))
			var paramErr *paramError
			requirements.ErrorAs(err, &paramErr)
			assertions.Equal(name, paramErr.param)
		})
	}
}

func TestCallHistoryHTTPRoundtrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	peer, err := archive.EnsureParticipantByPhone(t.Context(), "+12025550101", "Example", "signal")
	requirements.NoError(err)
	person, _, err := archive.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	st := &callHTTPStore{mockStore: &mockStore{stats: &StoreStats{}}, CallHistoryStore: archive}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, st, nil, testLogger())
	request := callhistory.ImportRequest{
		Format: "signal_desktop", Account: "example",
		Record:   []byte(`{"callId":"one","peerId":"peer","mode":"Direct","status":"Accepted","timestamp":1790841600000}`),
		Bindings: map[string]int64{"peer": peer},
	}
	body, err := json.Marshal(request)
	requirements.NoError(err)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, req)
		return out
	}
	out := do("POST", "/api/v1/import/calls", string(body))
	requirements.Equal(200, out.Code, out.Body.String())
	out = do("GET", "/api/v1/calls/summary", "")
	requirements.Equal(200, out.Code, out.Body.String())
	var summary CallSummaryResponse
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &summary))
	requirements.Len(summary.Items, 1)
	assertions.Equal(person.ID, summary.Items[0].PersonID)
	out = do("GET", "/api/v1/calls", "")
	requirements.Equal(200, out.Code)
	var evidence generated.CallEvidenceResponse
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &evidence))
	requirements.Len(evidence.Items, 1)
	assertions.NotContains(out.Body.String(), `"raw"`)
	assertions.Contains(out.Body.String(), `"duration_seconds":null`)
	assertions.Equal(400, do("GET", "/api/v1/calls?offset=1", "").Code)
	for _, path := range []string{"/api/v1/calls?after_id=9223372036854775807", "/api/v1/calls/summary?offset=1"} {
		page := do("GET", path, "")
		requirements.Equal(200, page.Code)
		assertions.JSONEq(`{"items":[]}`, page.Body.String())
	}
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/api/v1/import/calls", `{"unexpected_field":true}`, 400},
		{"/api/v1/import/calls", `{}`, 422},
		{"/api/v1/import/calls", strings.Repeat(" ", callhistory.MaxRequestBytes+1), 413},
	} {
		out := do("POST", tc.path, tc.body)
		assertions.Equal(tc.status, out.Code)
		if tc.status == 400 {
			assertions.Contains(out.Body.String(), "unexpected_field")
		}
	}
	for _, mediaType := range []string{"", "text/plain", "application/json; invalid"} {
		t.Run("unsupported media type "+mediaType, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, callImportPath, strings.NewReader(string(body)))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Content-Type", mediaType)
			req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
			out := httptest.NewRecorder()
			server.Router().ServeHTTP(out, req)
			assert.Equal(t, http.StatusUnsupportedMediaType, out.Code, out.Body.String())
		})
	}
}

func TestCallHistoryImportUsesMutationGate(t *testing.T) {
	archive := testutil.NewTestStore(t)
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st := &callHTTPStore{mockStore: &mockStore{stats: &StoreStats{}}, CallHistoryStore: archive}
		gate := NewSerialOperationGate()
		server := NewServerWithOptions(ServerOptions{
			Config: &config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}},
			Store:  st, Logger: testLogger(), OperationGate: gate,
		})
		defer func() { requirements.NoError(server.Shutdown(context.Background())) }()
		release, ok := gate.BeginWork()
		requirements.True(ok)
		releaseOnce := sync.OnceFunc(release)
		defer releaseOnce()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/import/calls", strings.NewReader(
			`{"format":"signal_desktop","account":"example","record":{"callId":"x","timestamp":1790841600000}}`))
		req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { server.Router().ServeHTTP(out, req); close(done) }()
		synctest.Wait()
		requirements.True(gate.HasRequestWaiters())
		select {
		case <-done:
			requirements.FailNow("import finished while mutation gate was held")
		default:
		}
		releaseOnce()
		<-done
		assertions.Equal(200, out.Code, out.Body.String())
	})
}
