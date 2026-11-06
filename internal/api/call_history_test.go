package api

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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

func TestCallHistoryHTTPRoundtrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	peer, err := archive.EnsureParticipantByPhone("+12025550101", "Example", "signal")
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
	var evidence CallEvidenceResponse
	requirements.NoError(json.Unmarshal(out.Body.Bytes(), &evidence))
	requirements.Len(evidence.Items, 1)
	assertions.NotContains(out.Body.String(), `"raw"`)
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
		{"/api/v1/import/calls", "{", 400},
		{"/api/v1/import/calls", `{}`, 422},
		{"/api/v1/import/calls", strings.Repeat(" ", callhistory.MaxRequestBytes+1), 413},
	} {
		assertions.Equal(tc.status, do("POST", tc.path, tc.body).Code)
	}
	assertions.Equal(400, do("GET", "/api/v1/calls/summary?after=invalid", "").Code)
	t.Run("normalized nanosecond timestamps", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		body := fmt.Sprintf(`{"format":"normalized","account":"example","record":{"version":1,"provider":"signal","account":"example","record_id":"nanoseconds","kind":"call","direction":"incoming","outcome":"answered","owner_participation":"joined","occurred_at":"2026-10-01T09:00:00.123456789Z","ended_at":"2026-10-01T09:01:00.987654321Z","precision":"instant","participants":[{"participant_id":%d,"participation":"joined"}],"raw":{"original_timestamp":"2026-10-01T09:00:00.123456789Z"}}}`, peer)
		out := do("POST", "/api/v1/import/calls", body)
		require.Equal(200, out.Code, out.Body.String())
		var imported CallImportResponse
		require.NoError(json.Unmarshal(out.Body.Bytes(), &imported))
		const window = "?after=2026-10-01T09:00:00.123456Z&before=2026-10-01T09:00:00.123457Z"
		out = do("GET", "/api/v1/calls"+window, "")
		require.Equal(200, out.Code, out.Body.String())
		var evidence CallEvidenceResponse
		require.NoError(json.Unmarshal(out.Body.Bytes(), &evidence))
		require.Len(evidence.Items, 1)
		assert.Equal(imported.MessageID, evidence.Items[0].MessageID)
		assert.Equal(time.Date(2026, 10, 1, 9, 0, 0, 123456000, time.UTC), evidence.Items[0].OccurredAt)
		require.NotNil(evidence.Items[0].EndedAt)
		assert.Equal(time.Date(2026, 10, 1, 9, 1, 0, 987654000, time.UTC), *evidence.Items[0].EndedAt)
		out = do("GET", "/api/v1/calls/summary"+window, "")
		require.Equal(200, out.Code, out.Body.String())
		var summary CallSummaryResponse
		require.NoError(json.Unmarshal(out.Body.Bytes(), &summary))
		require.Len(summary.Items, 1)
		assert.Equal(person.ID, summary.Items[0].PersonID)
		assert.Equal(1, summary.Items[0].Answered)
		assert.Equal(imported.MessageID, summary.Items[0].ExampleMessageID)
		raw, err := archive.GetMessageRawContext(t.Context(), imported.MessageID)
		require.NoError(err)
		assert.JSONEq(`{"original_timestamp":"2026-10-01T09:00:00.123456789Z"}`, string(raw))
	})
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
			`{"format":"signal_desktop","account":"example","record":{"callId":"x","timestamp":0}}`))
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

func TestCallHistoryNormalizedReplacement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := testutil.NewTestStore(t)
	st := &callHTTPStore{mockStore: &mockStore{stats: &StoreStats{}}, CallHistoryStore: archive}
	server := NewServer(&config.Config{Server: config.ServerConfig{APIKey: meetingImportTestAPIKey}}, st, nil, testLogger())
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/import/calls", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", meetingImportTestAPIKey)
		out := httptest.NewRecorder()
		server.Router().ServeHTTP(out, req)
		return out
	}
	const supplied = `{"format":"signal_desktop","account":"example","record":{"callId":"e1","timestamp":1790841600000,"mode":"Direct","direction":"Incoming","status":"Accepted"}}`
	out := post(supplied)
	requirements.Equal(200, out.Code, out.Body.String())
	out = post(`{"format":"normalized","account":"example","record":{"version":1,"provider":"signal","account":"example","record_id":"e1","kind":"call","direction":"unknown","outcome":"cancelled","owner_participation":"unknown","occurred_at":"2026-10-01T00:00:00Z","precision":"day","raw":{"cancelled":true}}}`)
	requirements.Equal(200, out.Code, out.Body.String())
	items, err := archive.ListCallObservationsContext(t.Context(), store.CallQuery{})
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal("e1", items[0].RecordID)
	assertions.Equal("cancelled", items[0].Outcome)
	assertions.Equal("unknown", items[0].Direction)
	assertions.Equal("day", items[0].Precision)
	raw, err := archive.GetMessageRawContext(t.Context(), items[0].MessageID)
	requirements.NoError(err)
	assertions.JSONEq(`{"cancelled":true}`, string(raw))
}
