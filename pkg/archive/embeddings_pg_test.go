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
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestPostgreSQLLibraryEmbeddings(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	schema := "archive_" + strings.ToLower(rand.Text())
	role := "runtime_" + strings.ToLower(rand.Text())
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	t.Cleanup(func() {
		_, e := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+quote(schema)+" CASCADE")
		assert.NoError(t, e)
		_, e = admin.ExecContext(context.WithoutCancel(t.Context()), "DROP ROLE IF EXISTS "+quote(role))
		assert.NoError(t, e)
	})
	var block, fail atomic.Bool
	started, resume := make(chan struct{}), make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		if block.Load() {
			close(started)
			<-resume
		}
		var request struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			v := []float32{1, 0}
			if strings.Contains(input, "second") {
				v = []float32{0, 1}
			}
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	}))
	t.Cleanup(service.Close)
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Model: "test", Dimension: 2}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	owner, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, owner.Close()) }()
	var sourceIDs, messageIDs []int64
	for i, body := range []string{"observatory", "second"} {
		source, e := owner.Store().GetOrCreateSource("slack", fmt.Sprintf("source-%d", i))
		require.NoError(t, e)
		sourceIDs = append(sourceIDs, source.ID)
		conversation, e := owner.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
		require.NoError(t, e)
		id, e := owner.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: body, Valid: true}})
		require.NoError(t, e)
		messageIDs = append(messageIDs, id)
		require.NoError(t, owner.Store().UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		require.NoError(t, owner.Store().UpsertFTS(id, body, body, "", "", ""))
	}
	_, err = admin.ExecContext(t.Context(), "CREATE ROLE "+quote(role)+" LOGIN PASSWORD 'archive_test'")
	require.NoError(t, err)
	for _, grant := range []string{"GRANT USAGE ON SCHEMA " + quote(schema) + " TO " + quote(role), "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA " + quote(schema) + " TO " + quote(role), "GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA " + quote(schema) + " TO " + quote(role)} {
		_, err = admin.ExecContext(t.Context(), grant)
		require.NoError(t, err)
	}
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.User = url.UserPassword(role, "archive_test")
	opts.URL = u.String()
	runtime, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, runtime.Close()) }()
	fail.Store(true)
	require.Error(t, runtime.Embed(t.Context()))
	fail.Store(false)
	require.NoError(t, runtime.Embed(t.Context()))
	handler := runtime.NewServer(archive.ServerOptions{}).Handler(nil)
	for _, mode := range []string{"vector", "hybrid"} {
		response := sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, "/api/v1/search?q=second&mode="+mode)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "observatory")
		assert.NotContains(t, response.Body.String(), `"subject":"second"`)
		response = sourceScopeRequest(archive.WithSourceScope(t.Context(), nil), t, handler, "/api/v1/search?q=second&mode="+mode)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.NotContains(t, response.Body.String(), "observatory")
		assert.NotContains(t, response.Body.String(), `"subject":"second"`)
	}
	response := sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, fmt.Sprintf("/api/v1/search?q=second&mode=vector&source_id=%d", sourceIDs[1]))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), `"subject":"second"`)
	assert.NotContains(t, response.Body.String(), `"subject":"observatory"`)

	// The next pass includes messages written after the first pass.
	conversation, err := owner.Store().EnsureConversation(t.Context(), sourceIDs[0], "channel", "Channel")
	require.NoError(t, err)
	id, err := owner.Store().UpsertMessage(&store.Message{SourceID: sourceIDs[0], ConversationID: conversation, SourceMessageID: "two", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: "new arrival", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, owner.Store().UpsertMessageBody(id, sql.NullString{String: "new arrival", Valid: true}, sql.NullString{}))
	require.NoError(t, runtime.Embed(t.Context()))
	response = sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, "/api/v1/search?q=meaning&mode=vector")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"subject":"new arrival"`)

	// The read snapshot must cover ranking as well as hydration when a source moves.
	block.Store(true)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, "/api/v1/search?q=second&mode=vector")
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		close(resume)
		require.FailNow(t, "embedding request did not start")
	}
	_, err = owner.Store().DB().ExecContext(t.Context(), "UPDATE messages SET source_id=$1, source_message_id='moved' WHERE id=$2", sourceIDs[0], messageIDs[1])
	close(resume)
	require.NoError(t, err)
	response = <-result
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), `"subject":"second"`)

	// Purges remove vectors and update generation counts.
	require.NoError(t, runtime.PurgeChannel(t.Context(), sourceIDs[0], "channel"))
	var vectors, count int
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings WHERE message_id IN ($1,$2)", messageIDs[0], id).Scan(&vectors))
	assert.Zero(t, vectors)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT message_count FROM index_generations WHERE state='active'").Scan(&count))
	assert.Equal(t, 1, count)
	unpreparedOpts := opts
	unpreparedOpts.Embeddings = &archive.Embeddings{Client: client, Model: "unprepared", Dimension: 2}
	unprepared, err := archive.Open(t.Context(), unpreparedOpts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, unprepared.Close()) }()
	require.Error(t, unprepared.Embed(t.Context()))
	unavailable := sourceScopeRequest(t.Context(), t, unprepared.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=meaning&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.Code, unavailable.Body.String())
	require.NoError(t, unprepared.PurgeSource(t.Context(), sourceIDs[1]))
	require.NoError(t, unprepared.PurgeSource(t.Context(), sourceIDs[1]))
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings").Scan(&vectors))
	assert.Zero(t, vectors)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT message_count FROM index_generations WHERE state='active'").Scan(&count))
	assert.Zero(t, count)

}

func TestPostgreSQLEmbeddingsPreservesSearchPath(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	database := "archive_" + strings.ToLower(rand.Text())
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	_, err = admin.ExecContext(t.Context(), "CREATE DATABASE "+quote(database))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP DATABASE "+quote(database))
		assert.NoError(t, err)
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + database
	db, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{"CREATE SCHEMA archive_data", "CREATE SCHEMA archive_other", "CREATE SCHEMA vector_types"} {
		_, err = db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
	params := u.Query()
	params.Set("search_path", "archive_data,public")
	u.RawQuery = params.Encode()
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	}))
	t.Cleanup(service.Close)
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: u.String(), Embeddings: &archive.Embeddings{Client: client, Model: "test", Dimension: 2}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	var installed bool
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname='vector')").Scan(&installed))
	assert.False(t, installed)
	missing, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	response := sourceScopeRequest(t.Context(), t, missing.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=meaning&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "pgvector")
	require.NoError(t, missing.Close())
	_, err = db.ExecContext(t.Context(), "CREATE EXTENSION vector WITH SCHEMA vector_types")
	require.NoError(t, err)
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	firstSource, err := a.Store().GetOrCreateSource("slack", "first-source")
	require.NoError(t, err)
	firstConversation, err := a.Store().EnsureConversation(t.Context(), firstSource.ID, "channel", "Channel")
	require.NoError(t, err)
	firstID, err := a.Store().UpsertMessage(&store.Message{SourceID: firstSource.ID, ConversationID: firstConversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: "first archive", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, a.Store().UpsertMessageBody(firstID, sql.NullString{String: "first archive", Valid: true}, sql.NullString{}))
	require.NoError(t, a.Store().UpsertFTS(firstID, "first archive", "first archive", "", "", ""))
	require.NoError(t, a.Embed(t.Context()))
	var current, path, vector string
	require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT current_schema(), current_setting('search_path'), '[1,2]'::vector::text`).Scan(&current, &path, &vector))
	assert.Equal(t, "archive_data", current)
	assert.Contains(t, path, "archive_data,public")
	assert.Contains(t, path, `"vector_types"`)
	assert.Equal(t, "[1,2]", vector)

	// A second archive keeps its vector columns after the first is removed.
	other := opts
	other.Schema = "archive_other"
	require.NoError(t, archive.Setup(t.Context(), other))
	second, err := archive.Open(t.Context(), other)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, second.Close()) })
	source, err := second.Store().GetOrCreateSource("slack", "other-source")
	require.NoError(t, err)
	conversation, err := second.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
	require.NoError(t, err)
	id, err := second.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: "observatory", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, second.Store().UpsertMessageBody(id, sql.NullString{String: "observatory", Valid: true}, sql.NullString{}))
	require.NoError(t, second.Store().UpsertFTS(id, "observatory", "observatory", "", "", ""))
	require.NoError(t, second.Embed(t.Context()))
	require.NoError(t, a.Close())
	_, err = db.ExecContext(t.Context(), "DROP SCHEMA archive_data CASCADE")
	require.NoError(t, err)
	require.NoError(t, second.Store().DB().QueryRowContext(t.Context(), `SELECT '[1,2]'::vector::text`).Scan(&vector))
	assert.Equal(t, "[1,2]", vector)
	response = sourceScopeRequest(t.Context(), t, second.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=meaning&mode=vector")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "observatory")

	params.Del("search_path")
	params.Set("options", "-csearch_path=archive_data,public")
	u.RawQuery = params.Encode()
	opts.URL = u.String()
	assert.ErrorContains(t, archive.Setup(t.Context(), opts), "options")
}

func TestPostgreSQLEmbeddingsReplacesBuildingGeneration(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	schema := "archive_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "a"}))
	}))
	t.Cleanup(service.Close)
	newClient := func(model, documentPrefix, queryPrefix string) *embedclient.Client {
		client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: model, Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Roles: embedconfig.Roles{DocumentPrefix: documentPrefix, QueryPrefix: queryPrefix}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
		require.NoError(t, err)
		return client
	}
	settings := &archive.Embeddings{Client: newClient("a", "", ""), Model: "a", Dimension: 2}
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: settings}
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	require.NoError(t, a.Embed(t.Context()))
	require.NoError(t, a.Close())
	settings.Model = "b"
	settings.Client = newClient("b", "", "")
	require.NoError(t, archive.Setup(t.Context(), opts))
	other := opts
	other.Embeddings = &archive.Embeddings{Client: newClient("a", "", ""), Model: "a", Dimension: 2}
	stale, err := archive.Open(t.Context(), other)
	require.NoError(t, err)
	require.Error(t, stale.Embed(t.Context()))
	unavailable := sourceScopeRequest(t.Context(), t, stale.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=meaning&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.Code, unavailable.Body.String())
	require.NoError(t, stale.Close())
	settings.Model = "c"
	settings.Client = newClient("c", "", "")
	require.NoError(t, archive.Setup(t.Context(), opts))
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	params := u.Query()
	params.Set("search_path", schema)
	u.RawQuery = params.Encode()
	check, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = check.Close() })
	var model string
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT model FROM index_generations WHERE state='building'").Scan(&model))
	assert.Equal(t, "c", model)
	settings.Model = "a"
	settings.Client = newClient("a", "", "")
	require.NoError(t, archive.Setup(t.Context(), opts))
	var building int
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT count(*) FROM index_generations WHERE state='building'").Scan(&building))
	assert.Zero(t, building)
	settings.Model = "b"
	settings.Client = newClient("b", "", "")
	require.NoError(t, archive.Setup(t.Context(), opts))
	b, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	source, err := b.Store().GetOrCreateSource("slack", "new-source")
	require.NoError(t, err)
	conversation, err := b.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
	require.NoError(t, err)
	id, err := b.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: "New message", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, b.Store().UpsertMessageBody(id, sql.NullString{String: "New message", Valid: true}, sql.NullString{}))
	require.NoError(t, b.Store().UpsertFTS(id, "New message", "New message", "", "", ""))
	require.NoError(t, b.Close())
	settings.Model = "a"
	settings.Client = newClient("a", "", "")
	require.NoError(t, archive.Setup(t.Context(), opts))
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT count(*) FROM index_generations WHERE state='building'").Scan(&building))
	assert.Zero(t, building)
	a, err = archive.Open(t.Context(), opts)
	require.NoError(t, err)
	require.NoError(t, a.Embed(t.Context()))
	var count int
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings e JOIN index_generations g ON g.id=e.generation_id WHERE e.message_id=$1 AND g.state='active'", id).Scan(&count))
	assert.Equal(t, 1, count)
	require.NoError(t, a.Close())
	settings.DocumentPrefix = "doc: "
	settings.QueryPrefix = "query: "
	settings.Client = newClient("a", settings.DocumentPrefix, settings.QueryPrefix)
	require.NoError(t, archive.Setup(t.Context(), opts))
	var fingerprint string
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT fingerprint FROM index_generations WHERE state='building'").Scan(&fingerprint))
	assert.NotEmpty(t, fingerprint)
	var activeFingerprint string
	require.NoError(t, check.QueryRowContext(t.Context(), "SELECT fingerprint FROM index_generations WHERE state='active'").Scan(&activeFingerprint))
	assert.NotEqual(t, activeFingerprint, fingerprint)
}

func TestPostgreSQLEmbeddingsPurgeDuringRequest(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	for _, channel := range []bool{false, true} {
		t.Run(fmt.Sprintf("channel=%t", channel), func(t *testing.T) {
			db, err := sql.Open("pgx", dsn)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			schema := "archive_" + strings.ToLower(rand.Text())
			t.Cleanup(func() {
				_, err := db.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
				assert.NoError(t, err)
			})
			var pause atomic.Bool
			started, resume := make(chan struct{}), make(chan struct{})
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if pause.Load() {
					close(started)
					<-resume
				}
				var req struct {
					Input []string `json:"input"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				data := make([]map[string]any, len(req.Input))
				for i := range req.Input {
					data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
				}
				require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
			}))
			t.Cleanup(service.Close)
			client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
			require.NoError(t, err)
			opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Model: "test", Dimension: 2}}
			require.NoError(t, archive.Setup(t.Context(), opts))
			a, err := archive.Open(t.Context(), opts)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, a.Close()) })
			seed := func(name string) (int64, int64) {
				source, err := a.Store().GetOrCreateSource("slack", name)
				require.NoError(t, err)
				conv, err := a.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
				require.NoError(t, err)
				id, err := a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: name, Valid: true}})
				require.NoError(t, err)
				require.NoError(t, a.Store().UpsertMessageBody(id, sql.NullString{String: name, Valid: true}, sql.NullString{}))
				return source.ID, id
			}
			_, retained := seed("retained")
			require.NoError(t, a.Embed(t.Context()))
			target, removed := seed("removed")
			pause.Store(true)
			done := make(chan error, 1)
			go func() { done <- a.Embed(t.Context()) }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				close(resume)
				require.FailNow(t, "embedding request did not start")
			}
			if channel {
				err = a.PurgeChannel(t.Context(), target, "channel")
			} else {
				err = a.PurgeSource(t.Context(), target)
			}
			close(resume)
			require.NoError(t, err)
			require.NoError(t, <-done)
			var count int
			require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=$1`, removed).Scan(&count))
			assert.Zero(t, count)
			require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=$1`, retained).Scan(&count))
			assert.Equal(t, 1, count)
			require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT message_count FROM index_generations WHERE state='active'`).Scan(&count))
			assert.Equal(t, 1, count)
		})
	}
}
