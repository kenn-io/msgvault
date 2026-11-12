package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/introops"
	"go.kenn.io/msgvault/internal/store"
)

func TestIntroductionReadsWithoutCapability(t *testing.T) {
	srv, _ := newTestServerWithAgentGrants(t)
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/unknown"} {
		response := doRequest(t, srv.Router(), http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer owner-key"})
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Contains(t, response.Body.String(), "feature_unavailable")
	}
}

type introductionTestStore struct {
	stubSourceStore

	err error
}

func (s *introductionTestStore) ListIntroductionsContext(context.Context, *agentgrant.Grant, store.PersonIntroductionQuery) (introops.Page, error) {
	return introops.Page{}, s.err
}
func (s *introductionTestStore) GetIntroductionContext(context.Context, *agentgrant.Grant, string) (introops.Record, error) {
	return introops.Record{}, s.err
}

func TestIntroductionReadErrorsAndSnapshotCapacity(t *testing.T) {
	st := &introductionTestStore{}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}, st, nil, testLogger())
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account"}}, time.Time{})
	require.NoError(t, err)
	for _, query := range []string{"source_id=1%ZZ", "after_id=%ZZ", "limit=1;limit=2", "limit=1&limit=%ZZ", "limit=0", "limit=101", "source_id=0", "limit=1&limit=2"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/intros", nil)
		req.URL.RawQuery = query
		req.Header.Set("X-Api-Key", "owner-key")
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assert.Equal(t, http.StatusBadRequest, response.Code, query)
		assert.Contains(t, response.Body.String(), "invalid_args")
	}
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/id"} {
		for _, tc := range []struct {
			name   string
			err    error
			status int
			code   string
		}{
			{"success", nil, http.StatusOK, ""},
			{"forbidden", introops.ErrForbidden, http.StatusForbidden, "not_permitted"},
			{"storage failure", errors.New("storage failure"), http.StatusServiceUnavailable, "local_store_failed"},
		} {
			if !t.Run(path+"/"+tc.name, func(t *testing.T) {
				st.err = tc.err
				// A separate occupied slot exposes duplicate cleanup.
				srv.readSnapshotSlots <- struct{}{}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
				req.Header.Set("X-Msgvault-Agent-Token", token)
				response := &introductionBoundaryRecorder{ResponseRecorder: httptest.NewRecorder(), slots: srv.readSnapshotSlots}
				done := make(chan struct{})
				go func() {
					srv.Router().ServeHTTP(response, req)
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					assert.Fail(t, "response cleanup blocked")
					cancel()
					// Unblock duplicate receives without letting recovery hang.
					for i := 0; i < 3; i++ {
						select {
						case <-done:
							return
						case srv.readSnapshotSlots <- struct{}{}:
						case <-time.After(5 * time.Second):
							return
						}
					}
					return
				}
				assert.True(t, response.observed)
				assert.Equal(t, 1, response.firstSlotCount, "only the unrelated slot should remain at the response boundary")
				assert.Equal(t, tc.status, response.Code)
				if tc.code != "" {
					assert.Contains(t, response.Body.String(), tc.code)
				}
				require.Len(t, srv.readSnapshotSlots, 1, "deferred cleanup must not release another read's slot")
				<-srv.readSnapshotSlots
			}) {
				return
			}
		}
	}
	st.err = store.ErrIntroductionNotFound
	response := doRequest(t, srv.Router(), http.MethodGet, "/api/v1/intros/id", nil, map[string]string{"X-Api-Key": "owner-key"})
	assert.Equal(t, http.StatusNotFound, response.Code)
	assert.Contains(t, response.Body.String(), "intro_not_found")
	assert.Empty(t, srv.readSnapshotSlots)
	st.err = nil
	_, unauthorized, _, err := srv.agentGrants.Issue("searcher", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "beeper", Identifier: "account"}}, time.Time{})
	require.NoError(t, err)
	for range readSnapshotConcurrency {
		srv.readSnapshotSlots <- struct{}{}
	}
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/id"} {
		response := doRequest(t, srv.Router(), http.MethodGet, path, nil, map[string]string{"X-Msgvault-Agent-Token": unauthorized})
		assert.Equal(t, http.StatusForbidden, response.Code)
		assert.Len(t, srv.readSnapshotSlots, readSnapshotConcurrency)
	}
	for _, path := range []string{"/api/v1/intros", "/api/v1/intros/id"} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		req.Header.Set("X-Msgvault-Agent-Token", token)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
		assert.Contains(t, response.Body.String(), "agent_read_busy")
		assert.Len(t, srv.readSnapshotSlots, readSnapshotConcurrency)
	}
}

// introductionBoundaryRecorder observes capacity before the underlying writer sends headers.
type introductionBoundaryRecorder struct {
	*httptest.ResponseRecorder

	slots          chan struct{}
	firstSlotCount int
	observed       bool
}

func (w *introductionBoundaryRecorder) WriteHeader(status int) {
	if !w.observed {
		w.firstSlotCount = len(w.slots)
		w.observed = true
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *introductionBoundaryRecorder) Write(body []byte) (int, error) {
	if !w.observed {
		w.firstSlotCount = len(w.slots)
		w.observed = true
	}
	return w.ResponseRecorder.Write(body)
}
