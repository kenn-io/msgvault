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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
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
	model := embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}
	client, err := embedclient.New(embedclient.Options{Model: model, Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:1", TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: embedmodel.Descriptor{Model: model}}}
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

	// Existing vector tables can outlive the binary that created them.
	_, err = a.Store().DB().ExecContext(t.Context(), `CREATE TABLE index_generations (id bigint PRIMARY KEY, message_count bigint NOT NULL);
		CREATE TABLE embeddings (generation_id bigint REFERENCES index_generations(id), message_id bigint, embedding text);
		CREATE TABLE embedding_checkpoints (generation_id bigint REFERENCES index_generations(id), owner text)`)
	require.NoError(t, err)
	_, err = a.Store().DB().ExecContext(t.Context(), `INSERT INTO index_generations VALUES (1, 2)`)
	require.NoError(t, err)
	var sources, messages []int64
	for _, name := range []string{"first", "second"} {
		source, err := a.Store().GetOrCreateSource("slack", name)
		require.NoError(t, err)
		conversation, err := a.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
		require.NoError(t, err)
		id, err := a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}})
		require.NoError(t, err)
		sources, messages = append(sources, source.ID), append(messages, id)
		_, err = a.Store().DB().ExecContext(t.Context(), `INSERT INTO embeddings VALUES (1, $1, '[1,0]')`, id)
		require.NoError(t, err)
	}
	require.NoError(t, a.PurgeSource(t.Context(), sources[0]))
	var count int
	require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=$1`, messages[0]).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT message_count FROM index_generations WHERE id=1`).Scan(&count))
	assert.Equal(t, 1, count)
	opts.Embeddings = &archive.Embeddings{}
	disabled, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, disabled.Close()) }()
	require.NoError(t, disabled.Embed(t.Context()))
	response := sourceScopeRequest(t.Context(), t, disabled.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=example&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "vector_not_enabled")
}
