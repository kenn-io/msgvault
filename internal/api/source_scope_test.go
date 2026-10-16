package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallerScopedReadsShareSnapshotBudget(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	handler := srv.Handler(nil)
	for range readSnapshotConcurrency {
		srv.readSnapshotSlots <- struct{}{}
	}

	ended, cancel := context.WithCancel(sourcescope.WithSourceIDs(t.Context(), []int64{source.ID}))
	cancel()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequestWithContext(ended, http.MethodGet, "/api/v1/messages", nil))
	assertions.Equal(http.StatusServiceUnavailable, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), "source_scope_busy")
	assertions.Zero(st.DB().Stats().InUse)
	assertions.Len(srv.readSnapshotSlots, readSnapshotConcurrency)

	for range readSnapshotConcurrency {
		<-srv.readSnapshotSlots
	}
	w = httptest.NewRecorder()
	scoped := sourcescope.WithSourceIDs(t.Context(), []int64{source.ID})
	handler.ServeHTTP(w, httptest.NewRequestWithContext(scoped, http.MethodGet, "/api/v1/messages", nil))
	assertions.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Empty(srv.readSnapshotSlots, "a finished scoped read returns its slot")
}
