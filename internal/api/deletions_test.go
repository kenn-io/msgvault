package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// deletionMockStore layers manifest-capability fakes over mockStore.
type deletionMockStore struct {
	mockStore

	saved     []*deletion.Manifest
	manifests map[deletion.Status][]*deletion.Manifest
	getStatus deletion.Status
	getErr    error
	cancelled []string
	cancelErr error
}

func (s *deletionMockStore) SaveCLIDeletionManifest(_ context.Context, m *deletion.Manifest) error {
	s.saved = append(s.saved, m)
	return nil
}

func (s *deletionMockStore) ListDeletionManifests(_ context.Context, status deletion.Status) ([]*deletion.Manifest, error) {
	if status != "" {
		return s.manifests[status], nil
	}
	var all []*deletion.Manifest
	for _, st := range deletion.PersistedStatuses() {
		all = append(all, s.manifests[st]...)
	}
	return all, nil
}

func (s *deletionMockStore) GetDeletionManifest(_ context.Context, id string) (*deletion.Manifest, deletion.Status, error) {
	if s.getErr != nil {
		return nil, "", s.getErr
	}
	return &deletion.Manifest{ID: id, Status: s.getStatus}, s.getStatus, nil
}

func (s *deletionMockStore) CancelDeletionManifest(_ context.Context, id string) error {
	if s.cancelErr != nil {
		return s.cancelErr
	}
	s.cancelled = append(s.cancelled, id)
	return nil
}

func newDeletionTestServer(t *testing.T, st MessageStore, engine query.Engine) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{APIPort: 8080}},
		Store:  st,
		Engine: engine,
		Logger: testLogger(),
	})
}

func postDeletions(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deletions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	return w
}

func TestStageDeletionByFilter(t *testing.T) {
	st := &deletionMockStore{}
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
			gotFilter = f
			return []string{"gm-1", "gm-2"}, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{
		"filter": {"sender": "alice@example.com", "after": "2019-01-01", "before": "2020-01-01"},
		"description": "old alice mail"
	}`)

	require.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal(t, "alice@example.com", gotFilter.Sender)
	require.NotNil(t, gotFilter.After, "after parsed")
	assert.Equal(t, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), gotFilter.After.UTC())
	require.NotNil(t, gotFilter.Before, "before parsed")

	var resp StageDeletionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.False(t, resp.DryRun)
	assert.Equal(t, 2, resp.MessageCount)
	assert.NotEmpty(t, resp.ID)
	assert.Equal(t, "pending", resp.Status)
	assert.Empty(t, resp.SampleGmailIDs, "create response has no sample")

	require.Len(t, st.saved, 1, "manifest saved")
	m := st.saved[0]
	assert.Equal(t, "api", m.CreatedBy)
	assert.Equal(t, []string{"gm-1", "gm-2"}, m.GmailIDs)
	assert.Equal(t, []string{"alice@example.com"}, m.Filters.Senders)
	assert.Equal(t, "2019-01-01", m.Filters.After)
	assert.NotEmpty(t, m.RawFilter, "raw provenance recorded")
}

func TestStageDeletionByMessageIDsUnionsAndDedupes(t *testing.T) {
	st := &deletionMockStore{}
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, _ query.MessageFilter) ([]string, error) {
			return []string{"gm-1", "gm-2"}, nil
		},
		GetGmailIDsByMessageIDsFunc: func(_ context.Context, ids []int64) ([]string, error) {
			assert.Equal(t, []int64{7, 8}, ids)
			return []string{"gm-2", "gm-3"}, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"sender": "alice@example.com"}, "message_ids": [7, 8]}`)

	require.Equal(t, http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())
	require.Len(t, st.saved, 1)
	assert.Equal(t, []string{"gm-1", "gm-2", "gm-3"}, st.saved[0].GmailIDs, "union, deduped, order-preserving")
}

func TestStageDeletionDryRun(t *testing.T) {
	st := &deletionMockStore{}
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = "gm-" + string(rune('a'+i))
	}
	engine := &querytest.MockEngine{GmailIDs: ids}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"domain": "example.com"}, "dry_run": true}`)

	require.Equal(t, http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp StageDeletionResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.True(t, resp.DryRun)
	assert.Equal(t, 25, resp.MessageCount)
	assert.Len(t, resp.SampleGmailIDs, 10, "sample capped at 10")
	assert.Empty(t, resp.ID, "dry run stages nothing")
	assert.Empty(t, st.saved, "dry run writes nothing")
}

func TestStageDeletionRejectsEmptyFilter(t *testing.T) {
	st := &deletionMockStore{}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	for _, body := range []string{`{}`, `{"filter": {}}`, `{"filter": {"sender": ""}, "message_ids": []}`} {
		w := postDeletions(t, srv, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "body %s -> status", body)
		assert.Contains(t, w.Body.String(), "empty_filter", "body %s -> error code", body)
	}
	assert.Empty(t, st.saved)
}

func TestStageDeletionNoMatches(t *testing.T) {
	st := &deletionMockStore{}
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, _ query.MessageFilter) ([]string, error) {
			return nil, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"sender": "nobody@example.com"}}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no_messages_matched")
	assert.Empty(t, st.saved)
}

func TestStageDeletionInvalidDate(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, &querytest.MockEngine{})
	w := postDeletions(t, srv, `{"filter": {"after": "not-a-date"}}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_date")
}

func TestStageDeletionEngineUnavailable(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, nil)
	w := postDeletions(t, srv, `{"filter": {"sender": "alice@example.com"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "engine_unavailable")
}
