//go:build cgo

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
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
	db, observer := sqlobserve.Open(t, nil)
	schema, err := os.ReadFile("../store/schema.sql")
	requirements.NoError(err)
	_, err = db.Exec(string(schema))
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: testutil.NewTestStore(t), Engine: query.NewSQLiteEngine(db), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
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
