//go:build pgvector

package archive_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
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
	descriptor := testDescriptor()
	client, err := embedclient.New(embedclient.Options{Model: descriptor.Model, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}, Retry: embedclient.Retry{MaxAttempts: 1}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: descriptor}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, a.Close()) })
	return a
}

func regressionMessage(t *testing.T, a *archive.Archive, sourceName, channel, body string) {
	t.Helper()
	source, err := a.Store().GetOrCreateSource("slack", sourceName)
	require.NoError(t, err)
	conversation, err := a.Store().EnsureConversation(t.Context(), source.ID, channel, channel)
	require.NoError(t, err)
	_, err = a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: body, MessageType: "slack", Subject: sql.NullString{String: body, Valid: true}})
	require.NoError(t, err)
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
	regressionMessage(t, a, "source", "channel", "accepted")
	regressionMessage(t, a, "source", "channel", "rejected")
	require.ErrorContains(t, a.Embed(t.Context()), "embedding failed for 1 messages")
}
