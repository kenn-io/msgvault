package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type tierHTTPFixture struct {
	srv      *api.Server
	st       *store.Store
	cfg      *config.Config
	personID int64
	now      time.Time
	gate     *api.SerialOperationGate
}

func newTierHTTPFixture(t *testing.T) tierHTTPFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	participantID, err := st.EnsureParticipant("tiers-http@example.test", "HTTP Tier Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participantID)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.People.Tiers.MinimumTier = 2
	gate := api.NewSerialOperationGate()
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, OperationGate: gate, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	api.SetPersonTierHTTPClockForTest(srv, func() time.Time { return now })
	return tierHTTPFixture{srv: srv, st: st, cfg: cfg, personID: person.ID, now: now, gate: gate}
}
func (f tierHTTPFixture) request(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.RemoteAddr = "127.0.0.1:1234"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	f.srv.Router().ServeHTTP(response, r)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	return response
}
func tierHTTPBody[T any](t *testing.T, r *httptest.ResponseRecorder) T {
	t.Helper()
	var body T
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &body), r.Body.String())
	return body
}
func tierHTTPCount(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n))
	return n
}

func TestPersonTiersHTTPPreviewExplainAndHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	preview := f.request(t, http.MethodGet, "/api/v1/people-tiers/preview?limit=1&tier=2", "")
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	page := tierHTTPBody[store.PersonTierPage](t, preview)
	require.Len(t, page.People, 1)
	assert.Equal(t, f.personID, page.People[0].PersonID)
	assert.Equal(t, time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), page.AsOf)
	assert.Zero(t, tierHTTPCount(t, f.st, "person_tier_state"))
	assert.Zero(t, tierHTTPCount(t, f.st, "person_tier_events"))
	filtered := f.request(t, http.MethodGet, "/api/v1/people-tiers/preview?tier=1", "")
	require.Equal(t, http.StatusOK, filtered.Code)
	assert.Empty(t, tierHTTPBody[store.PersonTierPage](t, filtered).People)
	explanation := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/people/%d/tier", f.personID), "")
	require.Equal(t, http.StatusOK, explanation.Code, explanation.Body.String())
	assert.Equal(t, f.personID, tierHTTPBody[store.PersonTierExplanation](t, explanation).PersonID)
	history := f.request(t, http.MethodGet, "/api/v1/people-tiers/history?person_id="+strconv.FormatInt(f.personID, 10), "")
	assert.Equal(t, http.StatusOK, history.Code, history.Body.String())
	candidates := f.request(t, http.MethodGet, "/api/v1/people-tiers/candidates?limit=1", "")
	assert.Equal(t, http.StatusOK, candidates.Code, candidates.Body.String())
	missing := f.request(t, http.MethodGet, "/api/v1/people/999999/tier", "")
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestPersonTiersHTTPApplyFrozenContinuationAndSupersession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	for i := 0; i < 2; i++ {
		id, err := f.st.EnsureParticipant(fmt.Sprintf("http-page-%d@example.test", i), "HTTP Page Person", "example.test")
		require.NoError(t, err)
		_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), id)
		require.NoError(t, err)
	}
	dry := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", `{"dry_run":true,"limit":1}`)
	require.Equal(t, http.StatusOK, dry.Code, dry.Body.String())
	assert.Zero(t, tierHTTPCount(t, f.st, "person_tier_state"))
	first := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", `{"limit":1}`)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	page := tierHTTPBody[store.PersonTierPage](t, first)
	require.NotEmpty(t, page.RunID)
	require.NotEmpty(t, page.NextCursor)
	api.SetPersonTierHTTPClockForTest(f.srv, func() time.Time { return f.now.Add(24 * time.Hour) })
	nextBody, err := json.Marshal(map[string]any{"limit": 1, "run_id": page.RunID, "cursor": page.NextCursor})
	require.NoError(t, err)
	next := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", string(nextBody))
	require.Equal(t, http.StatusOK, next.Code, next.Body.String())
	continued := tierHTTPBody[store.PersonTierPage](t, next)
	assert.Equal(t, page.RunID, continued.RunID)
	assert.Equal(t, page.AsOf, continued.AsOf)
	replacement := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", `{"limit":1}`)
	require.Equal(t, http.StatusOK, replacement.Code, replacement.Body.String())
	stale := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", string(nextBody))
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	var actor string
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), `SELECT actor FROM person_tier_events ORDER BY id LIMIT 1`).Scan(&actor))
	assert.Equal(t, "api", actor)
}

func TestPersonTiersHTTPPreviewCursorAndChangesFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	id, err := f.st.EnsureParticipant("http-preview-other@example.test", "HTTP Preview Other", "example.test")
	require.NoError(t, err)
	_, _, err = f.st.CreatePersonFromParticipantContext(t.Context(), id)
	require.NoError(t, err)
	response := f.request(t, http.MethodGet, "/api/v1/people-tiers/preview?limit=1&changes_only=true", "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	first := tierHTTPBody[store.PersonTierPage](t, response)
	require.NotEmpty(t, first.NextCursor)
	response = f.request(t, http.MethodGet, "/api/v1/people-tiers/preview?limit=1&cursor="+url.QueryEscape(first.NextCursor), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	next := tierHTTPBody[store.PersonTierPage](t, response)
	require.Len(t, first.People, 1)
	require.Len(t, next.People, 1)
	assert.NotEqual(t, first.People[0].PersonID, next.People[0].PersonID)
	assert.Equal(t, first.AsOf, next.AsOf)
}

func TestPersonTiersHTTPResetOptOutAndManualActor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	apply := f.request(t, http.MethodPost, "/api/v1/people-tiers/apply", `{}`)
	require.Equal(t, http.StatusOK, apply.Code, apply.Body.String())
	togglePath := fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", f.personID)
	response := f.request(t, http.MethodPut, togglePath, `{"lane":"enrichment","opted_out":true,"reason":"owner choice"}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	outs, err := f.st.ListPersonTierOptOutsContext(t.Context(), f.personID)
	require.NoError(t, err)
	require.Len(t, outs, 1)
	assert.Equal(t, "api", outs[0].Actor)
	assert.Equal(t, "owner choice", outs[0].Reason)
	response = f.request(t, http.MethodPut, togglePath, `{"lane":"enrichment","opted_out":false}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	outs, err = f.st.ListPersonTierOptOutsContext(t.Context(), f.personID)
	require.NoError(t, err)
	assert.Empty(t, outs)
	response = f.request(t, http.MethodPut, togglePath, `{"lane":"enrichment","opted_out":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	response = f.request(t, http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tracking", f.personID), `{"tracked":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var tracking map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &tracking))
	assert.Equal(t, "manual", tracking["provenance"])
	assert.Equal(t, "api", tracking["actor"])
	response = f.request(t, http.MethodPut, fmt.Sprintf("/api/v1/people/%d/brief-enrollment", f.personID), `{"enrolled":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var brief map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &brief))
	assert.Equal(t, "manual", brief["provenance"])
	assert.Equal(t, "api", brief["actor"])
	response = f.request(t, http.MethodPost, "/api/v1/people-tiers/reset", `{"confirm":true}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Zero(t, tierHTTPCount(t, f.st, "person_tier_state"))
	state, err := f.st.GetPersonTrackingContext(t.Context(), f.personID)
	require.NoError(t, err)
	assert.True(t, state.Tracked)
	outs, err = f.st.ListPersonTierOptOutsContext(t.Context(), f.personID)
	require.NoError(t, err)
	assert.Len(t, outs, 1)
}

func TestPersonTiersHTTPRejectsInvalidRequests(t *testing.T) {
	f := newTierHTTPFixture(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/people-tiers/preview?limit=501", ""}, {http.MethodGet, "/api/v1/people-tiers/preview?limit=0", ""}, {http.MethodGet, "/api/v1/people-tiers/preview?tier=4", ""}, {http.MethodGet, "/api/v1/people-tiers/preview?changes_only=maybe", ""}, {http.MethodGet, "/api/v1/people-tiers/preview?cursor=bad", ""},
		{http.MethodPost, "/api/v1/people-tiers/apply", `{"limit":501}`}, {http.MethodPost, "/api/v1/people-tiers/apply", `{"limit":0}`}, {http.MethodPost, "/api/v1/people-tiers/apply", `{"dry_run":null}`}, {http.MethodPost, "/api/v1/people-tiers/reset", `{}`}, {http.MethodPost, "/api/v1/people-tiers/reset", `{"confirm":false}`}, {http.MethodPost, "/api/v1/people-tiers/reset", `{"confirm":null}`},
		{http.MethodGet, "/api/v1/people-tiers/history?person_id=-1", ""}, {http.MethodGet, "/api/v1/people-tiers/history?offset=-1", ""}, {http.MethodGet, "/api/v1/people-tiers/history?limit=201", ""}, {http.MethodGet, "/api/v1/people-tiers/candidates?limit=501", ""}, {http.MethodGet, "/api/v1/people-tiers/candidates?cursor=0", ""}, {http.MethodGet, "/api/v1/people-tiers/candidates?cursor=bad", ""},
		{http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", f.personID), `{"lane":"wrong","opted_out":true}`}, {http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", f.personID), `{"lane":"enrichment"}`}, {http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", f.personID), `{"lane":"enrichment","opted_out":null}`},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			response := f.request(t, tc.method, tc.path, tc.body)
			assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
	missing := f.request(t, http.MethodPut, "/api/v1/people/999999/tier/opt-outs", `{"lane":"enrichment","opted_out":true}`)
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
}

func TestPersonTiersHTTPApplyWaitsForOperationGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	release, ok := f.gate.BeginWork()
	require.True(t, ok)
	defer release()
	preview := f.request(t, http.MethodGet, "/api/v1/people-tiers/preview", "")
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/people-tiers/apply", bytes.NewBufferString(`{}`))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.srv.Router().ServeHTTP(w, r)
		response <- w
	}()
	require.Eventually(t, f.gate.HasRequestWaiters, 30*time.Second, 10*time.Millisecond, "apply did not wait for the daemon operation gate")
	assert.Zero(t, tierHTTPCount(t, f.st, "person_tier_state"))
	release()
	got := <-response
	assert.Equal(t, http.StatusOK, got.Code, got.Body.String())
	assert.Equal(t, "no-store", got.Header().Get("Cache-Control"))
}

func TestPersonTiersHTTPContractAndCLIRunnerRefusal(t *testing.T) {
	assert := assert.New(t)
	paths := api.OpenAPIDocument().Paths
	for path, method := range map[string]string{"/api/v1/people-tiers/preview": "get", "/api/v1/people-tiers/apply": "post", "/api/v1/people-tiers/reset": "post", "/api/v1/people-tiers/history": "get", "/api/v1/people-tiers/candidates": "get", "/api/v1/people/{id}/tier": "get", "/api/v1/people/{id}/tier/opt-outs": "put"} {
		require.Contains(t, paths, path)
		item := paths[path]
		switch method {
		case "get":
			assert.NotNil(t, item.Get)
		case "post":
			assert.NotNil(t, item.Post)
		case "put":
			assert.NotNil(t, item.Put)
		}
	}
	for _, sub := range []string{"preview", "explain", "apply", "reset", "history", "candidates", "opt-out", "opt-in"} {
		assert.False(t, api.PersonTiersCLIRunAllowedForTest([]string{"person", "tiers", sub, "1"}))
		assert.False(t, api.PersonTiersCLIRunAllowedForTest([]string{"person", "--json", "tiers", sub, "1"}))
	}
}

func TestPersonTiersHTTPManualLaneConflictAndLegacyActor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierHTTPFixture(t)
	_, err := f.st.SetPersonTrackingContext(t.Context(), f.personID, true)
	require.NoError(t, err)
	_, err = f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE person_tracking SET actor='' WHERE person_id=?`), f.personID)
	require.NoError(t, err)
	response := f.request(t, http.MethodGet, fmt.Sprintf("/api/v1/people/%d/tracking", f.personID), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var legacy map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &legacy))
	assert.Equal(t, "manual", legacy["provenance"])
	assert.Equal(t, "", legacy["actor"])
	response = f.request(t, http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", f.personID), `{"lane":"tracking","opted_out":true}`)
	assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
	outs, err := f.st.ListPersonTierOptOutsContext(t.Context(), f.personID)
	require.NoError(t, err)
	assert.Empty(t, outs)
}
