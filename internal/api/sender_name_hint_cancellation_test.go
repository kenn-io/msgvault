//go:build cgo

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func TestCLISenderNameHintCancellation(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	db, observer := sqlobserve.OpenFile(t, path, nil)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: st, Engine: query.NewSQLiteEngine(db), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.WithoutCancel(t.Context()))) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observer.BeforeQuery = func(_ context.Context, statement string) {
		if strings.Contains(statement, "WITH candidates AS") {
			cancel()
		}
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/cli/search?q=from:alex", nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assertions.Equal(http.StatusServiceUnavailable, w.Code)
	assertions.Contains(w.Body.String(), `"error":"query_canceled"`)
}
