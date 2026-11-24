//go:build pgvector

package archive_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/pkg/archive"
)

func regressionArchive(t *testing.T, handler http.HandlerFunc) *archive.Archive {
	t.Helper()
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	installTestPgvector(t, admin)
	schema := "archive_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	service := httptest.NewServer(handler)
	t.Cleanup(service.Close)
	descriptor := testDescriptor("test")
	client, err := embedclient.New(embedclient.Options{Model: descriptor.Model, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}, Retry: embedclient.Retry{MaxAttempts: 1}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: descriptor}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, a.Close()) })
	return a
}

func regressionMessage(t *testing.T, a *archive.Archive, sourceName, channel, body string) (int64, int64) {
	t.Helper()
	source, err := a.Store().GetOrCreateSource("slack", sourceName)
	require.NoError(t, err)
	conversation, err := a.Store().EnsureConversation(t.Context(), source.ID, channel, channel)
	require.NoError(t, err)
	id, err := a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: body, MessageType: "slack", Subject: sql.NullString{String: body, Valid: true}})
	require.NoError(t, err)
	return source.ID, id
}

func TestPostgreSQLEmbedReportsRejectedMessage(t *testing.T) {
	a := regressionArchive(t, func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			if strings.Contains(input, "rejected") {
				http.Error(w, "input too long", http.StatusBadRequest)
				return
			}
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	})
	_, accepted := regressionMessage(t, a, "source", "channel", "accepted")
	_, rejected := regressionMessage(t, a, "source", "channel", "rejected")
	err := a.Embed(t.Context())
	if assert.Error(t, err, "a rejected message must be reported") {
		assert.Contains(t, err.Error(), "1")
	}
	for _, tc := range []struct {
		id   int64
		want int
	}{{accepted, 1}, {rejected, 0}} {
		var count int
		require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings WHERE message_id=$1", tc.id).Scan(&count))
		assert.Equal(t, tc.want, count)
	}
}

func TestPostgreSQLPurgeEmbeddingCheckpoints(t *testing.T) {
	for _, kind := range []string{"source", "channel"} {
		t.Run(kind, func(t *testing.T) {
			a := regressionArchive(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
			source, deleted := regressionMessage(t, a, "source", "deleted", "deleted")
			otherSource := "source"
			if kind == "source" {
				otherSource = "other"
			}
			_, retained := regressionMessage(t, a, otherSource, "retained", "retained")
			var gen int64
			require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), "SELECT id FROM index_generations WHERE state='building'").Scan(&gen))
			backend, err := pgvector.Open(t.Context(), pgvector.Options{DB: a.Store().DB(), SkipMigrate: true, ReadOnly: true})
			require.NoError(t, err)
			checkpoints := backend.EmbeddingCheckpoints()
			entry := func(id int64) vector.EmbeddingCheckpoint {
				return vector.EmbeddingCheckpoint{Owner: fmt.Sprintf("message:%d", id), Revision: "revision", Key: "input", Vectors: [][]float32{{1, 0}}}
			}
			require.NoError(t, checkpoints.Save(t.Context(), vector.GenerationID(gen), []vector.EmbeddingCheckpoint{entry(deleted), entry(retained)}))
			if kind == "source" {
				require.NoError(t, a.PurgeSource(t.Context(), source))
			} else {
				require.NoError(t, a.PurgeChannel(t.Context(), source, "deleted"))
			}
			count := func(id int64) int {
				var n int
				require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embedding_checkpoints WHERE owner=$1", entry(id).Owner).Scan(&n))
				return n
			}
			assert.Zero(t, count(deleted), "purge removes a saved response")
			assert.Equal(t, 1, count(retained), "purge keeps another message's response")
			// Isolate the late save even when the purge assertion above fails.
			require.NoError(t, checkpoints.Delete(t.Context(), vector.GenerationID(gen), []string{entry(deleted).Owner}))
			_ = checkpoints.Save(t.Context(), vector.GenerationID(gen), []vector.EmbeddingCheckpoint{entry(deleted)})
			assert.Zero(t, count(deleted), "a response saved after purge must not restore the checkpoint")
		})
	}
}
