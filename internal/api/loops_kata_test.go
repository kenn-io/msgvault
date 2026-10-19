package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

func TestLoopHTTPTrackRealKataAndDaemonClient(t *testing.T) {
	f := newLoopHTTPFixture(t)
	row, err := f.store.GetOpenLoopContext(t.Context(), f.loopID)
	require.NoError(t, err)
	require.NotNil(t, row.AnchorMessageID)
	require.NoError(t, f.store.UpsertMessageBodyContext(t.Context(), *row.AnchorMessageID, sql.NullString{String: "Could you review the synthetic draft?", Valid: true}, sql.NullString{}))
	native := katatest.New(t)
	kataServer := httptest.NewServer(native.Service.Handler())
	t.Cleanup(kataServer.Close)
	f.config.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	ops := api.NewKataIssueOperations(f.config, f.store, nil)
	require.NotNil(t, ops)
	service, err := loops.NewService(f.store, f.config, func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{ComputedAt: f.projection}, nil
	}, loops.WithKata(ops))
	require.NoError(t, err)
	server := api.NewServerWithOptions(api.ServerOptions{Config: f.config, Store: f.store, Logger: slog.New(slog.DiscardHandler), LoopService: service, KataIssueOperations: ops})
	path := fmt.Sprintf("/api/v1/loops/%d/kata", f.loopID)
	created := loopHTTPRequest(t, server, http.MethodPost, path, `{}`, "")
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	assert.Contains(t, created.Body.String(), `"replayed":false`)
	replay := loopHTTPRequest(t, server, http.MethodPost, path, `{}`, "")
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	assert.Contains(t, replay.Body.String(), `"replayed":true`)
	invalid := loopHTTPRequest(t, server, http.MethodPost, path, `{"issue_ref":"example#abcd","title":"Ignored"}`, "")
	assert.Equal(t, http.StatusUnprocessableEntity, invalid.Code, invalid.Body.String())
	assert.Contains(t, invalid.Body.String(), "invalid_request")
	httpServer := httptest.NewServer(server.Router())
	t.Cleanup(httpServer.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: httpServer.URL, APIKey: f.config.Server.APIKey, AllowInsecure: true, Context: t.Context()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.TrackOpenLoop(t.Context(), f.loopID, loops.TrackRequest{})
	require.NoError(t, err)
	assert.True(t, result.Replayed)
	require.NotNil(t, result.Loop.Kata)
	assert.Equal(t, result.Issue.QualifiedRef, result.Loop.Kata.QualifiedRef)
	require.NoError(t, client.UntrackOpenLoop(t.Context(), f.loopID))
	response := loopHTTPRequest(t, server, http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", f.loopID), "", "")
	assert.Equal(t, http.StatusOK, response.Code)
	assert.Contains(t, response.Body.String(), `"kata":null`)
	issue, err := ops.Get(t.Context(), result.Issue.UID)
	require.NoError(t, err)
	assert.Equal(t, "open", issue.Status, "untracking leaves Kata untouched")
}

func TestLoopHTTPDetailRefreshSkipsArchiveWriteWhileOperationGateIsHeld(t *testing.T) {
	gate := api.NewSerialOperationGate()
	f := newLoopHTTPFixtureWithOperationGate(t, gate)
	row, err := f.store.GetOpenLoopContext(t.Context(), f.loopID)
	require.NoError(t, err)
	require.NotNil(t, row.AnchorMessageID)
	require.NoError(t, f.store.UpsertMessageBodyContext(t.Context(), *row.AnchorMessageID, sql.NullString{String: "Could you review the synthetic draft?", Valid: true}, sql.NullString{}))

	native := katatest.New(t)
	kataServer := httptest.NewServer(native.Service.Handler())
	t.Cleanup(kataServer.Close)
	f.config.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: katatest.Project}
	ops := api.NewKataIssueOperations(f.config, f.store, nil)
	require.NotNil(t, ops)
	service, err := loops.NewService(f.store, f.config, func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{ComputedAt: f.projection}, nil
	}, loops.WithKata(ops))
	require.NoError(t, err)
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: f.config, Store: f.store, Logger: slog.New(slog.DiscardHandler),
		LoopService: service, OperationGate: gate,
	})
	path := fmt.Sprintf("/api/v1/loops/%d/kata", f.loopID)
	tracked := loopHTTPRequest(t, server, http.MethodPost, path, `{}`, "")
	require.Equal(t, http.StatusCreated, tracked.Code, tracked.Body.String())

	link, err := f.store.GetLoopKataLinkContext(t.Context(), f.loopID)
	require.NoError(t, err)
	oldChecked := time.Now().UTC().Add(-time.Hour)
	link.CheckedAt = &oldChecked
	require.NoError(t, f.store.UpdateLoopKataLinkContext(t.Context(), link))
	link, err = f.store.GetLoopKataLinkContext(t.Context(), f.loopID)
	require.NoError(t, err)
	require.NotNil(t, link.CheckedAt)
	oldChecked = *link.CheckedAt

	release, ok := gate.BeginWork()
	require.True(t, ok)
	defer release()
	detail := loopHTTPRequest(t, server, http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", f.loopID), "", "")
	require.Equal(t, http.StatusOK, detail.Code, detail.Body.String())
	assert.Contains(t, detail.Body.String(), `"live":false`)
	got, err := f.store.GetLoopKataLinkContext(t.Context(), f.loopID)
	require.NoError(t, err)
	require.NotNil(t, got.CheckedAt)
	assert.True(t, oldChecked.Equal(*got.CheckedAt), "a detail refresh must leave the archive cache unchanged while a backup operation owns the gate")

	release()
	refreshed := loopHTTPRequest(t, server, http.MethodGet, fmt.Sprintf("/api/v1/loops/%d", f.loopID), "", "")
	require.Equal(t, http.StatusOK, refreshed.Code, refreshed.Body.String())
	assert.Contains(t, refreshed.Body.String(), `"live":true`)
	got, err = f.store.GetLoopKataLinkContext(t.Context(), f.loopID)
	require.NoError(t, err)
	require.NotNil(t, got.CheckedAt)
	assert.True(t, got.CheckedAt.After(oldChecked))
}

type loopKataFailureOperations struct {
	api.KataIssueOperations
	prepareErr error
	linkErr    error
}

func (f loopKataFailureOperations) Connect(context.Context) (loops.KataIssues, error) {
	return f, nil
}

func (f loopKataFailureOperations) Prepare(context.Context, []kataevidence.Selector) ([]kataevidence.Evidence, error) {
	return nil, f.prepareErr
}

func (f loopKataFailureOperations) Link(context.Context, string, []kataevidence.Reference) (taskclient.KataTask, error) {
	return taskclient.KataTask{}, f.linkErr
}

// The status route owns archive-state failures; Kata failures must pass through
// the real tracking route and its configured Kata boundary.
func TestLoopHTTPStatusStoreErrorClassification(t *testing.T) {
	f := newLoopHTTPFixture(t)
	service, err := loops.NewService(f.store, f.config, func(context.Context) (freshness.ProjectionState, error) {
		return freshness.ProjectionState{ComputedAt: f.projection}, nil
	})
	require.NoError(t, err)
	var logs bytes.Buffer
	wrapped := fmt.Errorf("read loop derivation state: %w", sql.ErrConnDone)
	server := api.NewServerWithOptions(api.ServerOptions{
		Config: f.config, Store: f.store, LoopService: service,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		LoopState: func(context.Context) (store.LoopDerivationState, error) {
			return store.LoopDerivationState{}, wrapped
		},
	})
	response := loopHTTPRequest(t, server, http.MethodGet, "/api/v1/loops/status", "", "")
	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())
	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.Equal(t, "internal_error", body.Error)
	assert.NotContains(t, response.Body.String(), wrapped.Error())
	assert.Contains(t, logs.String(), "open-loop request failed")
	assert.Contains(t, logs.String(), sql.ErrConnDone.Error())
}

func TestLoopHTTPTrackErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		prepareErr error
		linkErr    error
		status     int
		code       string
	}{
		{"invalid request", `{"issue_ref":"example#ab12","title":"ignored"}`, nil, nil, http.StatusUnprocessableEntity, "invalid_request"},
		{"evidence changed", `{}`, kataevidence.ErrChanged, nil, http.StatusConflict, "evidence_changed"},
		{"invalid ref", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrInvalidRef, http.StatusBadRequest, "invalid_ref"},
		{"remote unavailable", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrUnreachable, http.StatusServiceUnavailable, "kata_unavailable"},
		{"remote invalid response", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrInvalidResponse, http.StatusServiceUnavailable, "kata_unavailable"},
		{"wrong project", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrWrongProject, http.StatusServiceUnavailable, "wrong_project"},
		{"remote authentication", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrAuthenticationRequired, http.StatusUnauthorized, "authentication_required"},
		{"missing issue", `{"issue_ref":"example#ab12"}`, nil, taskclient.ErrNotFound, http.StatusNotFound, "kata_issue_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLoopHTTPFixture(t)
			ops := loopKataFailureOperations{prepareErr: tc.prepareErr, linkErr: tc.linkErr}
			service, err := loops.NewService(f.store, f.config, func(context.Context) (freshness.ProjectionState, error) {
				return freshness.ProjectionState{ComputedAt: f.projection}, nil
			}, loops.WithKata(ops))
			require.NoError(t, err)
			server := api.NewServerWithOptions(api.ServerOptions{Config: f.config, Store: f.store, Logger: slog.New(slog.DiscardHandler), LoopService: service, KataIssueOperations: ops})
			path := fmt.Sprintf("/api/v1/loops/%d/kata", f.loopID)
			response := loopHTTPRequest(t, server, http.MethodPost, path, tc.body, "")
			assert.Equal(t, tc.status, response.Code, response.Body.String())
			var body struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, tc.code, body.Error)
		})
	}
}
