//go:build !pgvector

package archive_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestPostgreSQLEmbeddingsWithoutPgvector(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	schema := "archive_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, e := db.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, e)
	})
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Model: "test", Dimension: 2}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.Close()) }()
	handler := a.NewServer(archive.ServerOptions{}).Handler(nil)
	fts := sourceScopeRequest(t.Context(), t, handler, "/api/v1/search?q=example")
	assert.Equal(t, http.StatusOK, fts.Code, fts.Body.String())
	semantic := sourceScopeRequest(t.Context(), t, handler, "/api/v1/search?q=example&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, semantic.Code, semantic.Body.String())
	assert.Contains(t, semantic.Body.String(), "pgvector")
}
