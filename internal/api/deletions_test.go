package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	saved       []*deletion.Manifest
	manifests   map[deletion.Status][]*deletion.Manifest
	getStatus   deletion.Status
	getManifest *deletion.Manifest
	getErr      error
	cancelled   []string
	cancelErr   error
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
	if s.getManifest != nil {
		return s.getManifest, s.getStatus, nil
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

func TestStageDeletionLegacyFilterIsDryRunOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &deletionMockStore{}
	var gotFilter query.MessageFilter
	engine := &querytest.MockEngine{
		GetGmailIDsByFilterFunc: func(_ context.Context, f query.MessageFilter) ([]string, error) {
			gotFilter = f
			return []string{"gm-1", "gm-2"}, nil
		},
		GmailAccounts: []string{"user@example.com"},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{
		"filter": {"sender": "alice@example.com", "after": "2019-01-01", "before": "2020-01-01"},
		"description": "old alice mail", "dry_run": true
	}`)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	assert.Equal("alice@example.com", gotFilter.Sender)
	require.NotNil(gotFilter.After, "after parsed")
	assert.Equal(time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), gotFilter.After.UTC())
	require.NotNil(gotFilter.Before, "before parsed")

	var resp StageDeletionResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.True(resp.DryRun)
	assert.Equal(2, resp.MessageCount)
	assert.Equal("user@example.com", resp.Account, "resolved account reported")
	assert.Equal([]string{"gm-1", "gm-2"}, resp.SampleGmailIDs)
	assert.Empty(resp.ID)
	assert.Empty(st.saved, "legacy filter preview must not persist a manifest")

	mutation := postDeletions(t, srv, `{"filter":{"sender":"alice@example.com"}}`)
	assert.Equal(http.StatusPreconditionRequired, mutation.Code, mutation.Body.String())
	assert.Contains(mutation.Body.String(), "preflight_required")
}

func TestStageDeletionLegacyExplicitMessageIDsRemainCompatible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &deletionMockStore{}
	engine := &querytest.MockEngine{
		GetGmailIDsByMessageIDsFunc: func(_ context.Context, ids []int64) ([]string, error) {
			assert.Equal([]int64{7, 8}, ids)
			return []string{"gm-2", "gm-3"}, nil
		},
		GmailAccounts: []string{"user@example.com"},
	}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"message_ids": [7, 8]}`)

	require.Equal(http.StatusCreated, w.Code, "status (body: %s)", w.Body.String())
	var response StageDeletionResponse
	require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(2, response.MessageCount)
	assert.Equal("user@example.com", response.Account)
	require.Len(st.saved, 1)
	assert.Equal([]string{"gm-2", "gm-3"}, st.saved[0].GmailIDs)
}

func TestStageDeletionLegacyFilterCannotPiggybackOnExplicitMessageIDs(t *testing.T) {
	st := &deletionMockStore{}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := postDeletions(t, srv, `{"filter":{"sender":"alice@example.com"},"message_ids":[7,8]}`)

	assert.Equal(t, http.StatusPreconditionRequired, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "preflight_required")
	assert.Empty(t, st.saved)
}

func TestStageDeletionRejectsMultiAccountSelection(t *testing.T) {
	assert := assert.New(t)

	st := &deletionMockStore{}
	var gotGmailIDs []string
	engine := &querytest.MockEngine{
		GmailIDs: []string{"gm-1", "gm-2"},
		GetAccountsByGmailIDsFunc: func(_ context.Context, gmailIDs []string) ([]string, error) {
			gotGmailIDs = gmailIDs
			return []string{"a@example.com", "b@example.com"}, nil
		},
	}
	srv := newDeletionTestServer(t, st, engine)

	for _, body := range []string{
		`{"filter": {"domain": "example.com"}, "dry_run": true}`,
	} {
		w := postDeletions(t, srv, body)
		assert.Equal(http.StatusBadRequest, w.Code, "body %s -> status", body)
		assert.Contains(w.Body.String(), "multi_account_selection", "body %s -> error code", body)
		assert.Contains(w.Body.String(), "a@example.com, b@example.com", "body %s -> accounts listed", body)
	}
	assert.Equal([]string{"gm-1", "gm-2"}, gotGmailIDs, "resolution queried with staged IDs")
	assert.Empty(st.saved, "nothing staged across accounts")
}

func TestStageDeletionDryRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &deletionMockStore{}
	ids := make([]string, 25)
	for i := range ids {
		ids[i] = "gm-" + string(rune('a'+i))
	}
	engine := &querytest.MockEngine{GmailIDs: ids, GmailAccounts: []string{"user@example.com"}}
	srv := newDeletionTestServer(t, st, engine)

	w := postDeletions(t, srv, `{"filter": {"domain": "example.com"}, "dry_run": true}`)

	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp StageDeletionResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.True(resp.DryRun)
	assert.Equal(25, resp.MessageCount)
	assert.Equal("user@example.com", resp.Account, "dry run reports the account")
	assert.Len(resp.SampleGmailIDs, 10, "sample capped at 10")
	assert.Empty(resp.ID, "dry run stages nothing")
	assert.Empty(st.saved, "dry run writes nothing")
}

func TestStageDeletionSelectionRequiresAndConsumesExactPreflightAuthority(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	engine := newExploreDuckDBFixture(t)
	st := &deletionMockStore{}
	srv := newDeletionTestServer(t, st, engine)

	explore := postExploreJSON(t, srv, "/api/v1/explore", `{"filters":[{"dimension":"source","values":["1"]}]}`)
	requirements.Equal(http.StatusOK, explore.Code, explore.Body.String())
	var explored struct {
		CacheRevision string `json:"cache_revision"`
	}
	requirements.NoError(json.Unmarshal(explore.Body.Bytes(), &explored))
	selection := fmt.Sprintf(`{"mode":"all_matching","predicate":{"filters":[{"dimension":"source","values":["1"]}]},"exclusions":["source:1:message:m1"],"cache_revision":%q}`, explored.CacheRevision)

	withoutPreflight := postDeletions(t, srv, `{"selection":`+selection+`}`)
	assertions.Equal(http.StatusPreconditionRequired, withoutPreflight.Code, withoutPreflight.Body.String())
	assertions.Contains(withoutPreflight.Body.String(), "preflight_required")

	preflight := postExploreJSON(t, srv, "/api/v1/explore/preflight", `{"selection":`+selection+`}`)
	requirements.Equal(http.StatusOK, preflight.Code, preflight.Body.String())
	var reviewed ExplorePreflightResponse
	requirements.NoError(json.Unmarshal(preflight.Body.Bytes(), &reviewed))
	requirements.NotEmpty(reviewed.OperationToken)

	dryRun := postDeletions(t, srv, fmt.Sprintf(`{"selection":%s,"operation_token":%q,"dry_run":true}`, selection, reviewed.OperationToken))
	requirements.Equal(http.StatusOK, dryRun.Code, dryRun.Body.String())
	var preview StageDeletionResponse
	requirements.NoError(json.Unmarshal(dryRun.Body.Bytes(), &preview))
	assertions.True(preview.DryRun)
	assertions.Equal(1, preview.MessageCount)
	assertions.Equal([]string{"m2"}, preview.SampleGmailIDs)
	assertions.Empty(st.saved, "dry-run must not persist a manifest")

	stage := postDeletions(t, srv, fmt.Sprintf(`{"selection":%s,"operation_token":%q,"description":"reviewed batch"}`, selection, reviewed.OperationToken))
	requirements.Equal(http.StatusCreated, stage.Code, stage.Body.String())
	requirements.Len(st.saved, 1)
	assertions.Equal([]string{"m2"}, st.saved[0].GmailIDs)
	assertions.Equal("archive-a@example.com", st.saved[0].Filters.Account)

	reused := postDeletions(t, srv, fmt.Sprintf(`{"selection":%s,"operation_token":%q}`, selection, reviewed.OperationToken))
	assertions.Equal(http.StatusConflict, reused.Code, reused.Body.String())
	assertions.Contains(reused.Body.String(), "operation_token_invalid")
	assertions.Len(st.saved, 1, "one-shot authority must not create another manifest")
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

	w := postDeletions(t, srv, `{"filter": {"sender": "nobody@example.com"}, "dry_run": true}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "no_messages_matched")
	assert.Empty(t, st.saved)
}

func TestStageDeletionInvalidDate(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, &querytest.MockEngine{})
	w := postDeletions(t, srv, `{"filter": {"after": "not-a-date"}, "dry_run": true}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_date")
}

func TestStageDeletionRejectsUnknownFields(t *testing.T) {
	st := &deletionMockStore{}
	engine := &querytest.MockEngine{GmailIDs: []string{"gm-1"}}
	srv := newDeletionTestServer(t, st, engine)

	// A typo'd narrowing key must fail the request, not silently widen it.
	for _, body := range []string{
		`{"filter": {"snder": "alice@example.com", "domain": "example.com"}}`,
		`{"filter": {"sender": "alice@example.com"}, "dry_rn": true}`,
	} {
		w := postDeletions(t, srv, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "body %s -> status", body)
		assert.Contains(t, w.Body.String(), "unknown field", "body %s -> error detail", body)
	}
	assert.Empty(t, st.saved, "nothing staged from rejected requests")
}

func TestStageDeletionEngineUnavailable(t *testing.T) {
	srv := newDeletionTestServer(t, &deletionMockStore{}, nil)
	w := postDeletions(t, srv, `{"filter": {"sender": "alice@example.com"}}`)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), "engine_unavailable")
}

func TestListDeletions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	st := &deletionMockStore{manifests: map[deletion.Status][]*deletion.Manifest{
		deletion.StatusPending: {{
			ID: "batch-1", Status: deletion.StatusPending, CreatedAt: now,
			CreatedBy: "api", Description: "old mail", GmailIDs: []string{"gm-1", "gm-2"},
		}},
		deletion.StatusCancelled: {{
			ID: "batch-2", Status: deletion.StatusCancelled, CreatedAt: now.Add(time.Hour),
			CreatedBy: "tui", Description: "cancelled batch", GmailIDs: []string{"gm-3"},
		}},
	}}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	// All statuses, newest first.
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions", nil))
	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp ListDeletionsResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	require.Len(resp.Manifests, 2)
	assert.Equal("batch-2", resp.Manifests[0].ID, "newest first")
	assert.Equal(2, resp.Manifests[1].MessageCount)

	// Filtered by status.
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions?status=pending", nil))
	require.Equal(http.StatusOK, w.Code)
	resp = ListDeletionsResponse{}
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode filtered")
	require.Len(resp.Manifests, 1)
	assert.Equal("batch-1", resp.Manifests[0].ID)

	// Invalid status.
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions?status=bogus", nil))
	assert.Equal(http.StatusBadRequest, w.Code)
	assert.Contains(w.Body.String(), "invalid_status")
}

func TestGetDeletionReturnsManifestLifecycleDetail(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	completedAt := time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)
	manifest := &deletion.Manifest{
		ID: "batch-1", Status: deletion.StatusFailed, CreatedAt: completedAt.Add(-time.Hour),
		CreatedBy: "api", Description: "reviewed batch", GmailIDs: []string{"gm-1", "gm-2"},
		Filters: deletion.Filters{Account: "user@example.com"},
		Execution: &deletion.Execution{StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt,
			Method: deletion.MethodTrash, Succeeded: 1, Failed: 1, FailedIDs: []string{"gm-2"}},
	}
	st := &deletionMockStore{getStatus: deletion.StatusFailed}
	st.getManifest = manifest
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/deletions/batch-1", nil))

	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var detail DeletionManifestDetail
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &detail))
	assertions.Equal("batch-1", detail.ID)
	assertions.Equal("failed", detail.Status)
	assertions.Equal("user@example.com", detail.Account)
	assertions.Equal(2, detail.MessageCount)
	requirements.NotNil(detail.Execution)
	assertions.Equal(1, detail.Execution.Failed)
	assertions.Equal([]string{"gm-2"}, detail.Execution.FailedIDs)
}

func deleteDeletion(srv *Server, id string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/v1/deletions/"+id, nil))
	return w
}

func TestCancelDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := &deletionMockStore{getStatus: deletion.StatusPending}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := deleteDeletion(srv, "batch-1")
	require.Equal(http.StatusOK, w.Code, "status (body: %s)", w.Body.String())
	var resp CancelDeletionResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&resp), "decode")
	assert.Equal("batch-1", resp.ID)
	assert.Equal("cancelled", resp.Status)
	assert.Equal([]string{"batch-1"}, st.cancelled)
}

func TestCancelDeletionNotFound(t *testing.T) {
	st := &deletionMockStore{getErr: fmt.Errorf("manifest batch-x: %w", deletion.ErrManifestNotFound)}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	w := deleteDeletion(srv, "batch-x")
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "not_found")
	assert.Empty(t, st.cancelled)
}

func TestCancelDeletionNotCancellable(t *testing.T) {
	for _, status := range []deletion.Status{deletion.StatusCompleted, deletion.StatusFailed, deletion.StatusCancelled} {
		st := &deletionMockStore{getStatus: status}
		srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

		w := deleteDeletion(srv, "batch-1")
		assert.Equal(t, http.StatusConflict, w.Code, "status %s", status)
		assert.Contains(t, w.Body.String(), "not_cancellable", "status %s", status)
		assert.Empty(t, st.cancelled, "status %s", status)
	}
}

func TestCancelDeletionRejectsTraversalID(t *testing.T) {
	st := &deletionMockStore{getStatus: deletion.StatusPending}
	srv := newDeletionTestServer(t, st, &querytest.MockEngine{})

	// Traversal-shaped ID — must be rejected by ValidateManifestID before
	// it ever reaches the store. A "../"-style ID would be normalized away
	// by Go's ServeMux before routing, so this uses an ID that reaches the
	// handler (dots aren't path separators) but still fails validation
	// (dots aren't in the allowed alphabet either).
	w := deleteDeletion(srv, "bad..id")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_manifest_id")
	assert.Empty(t, st.cancelled)
}
