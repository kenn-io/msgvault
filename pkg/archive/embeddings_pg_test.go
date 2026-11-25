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
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/pgvector"
	"go.kenn.io/msgvault/pkg/archive"
)

func testDescriptor() embedmodel.Descriptor {
	return embedmodel.Descriptor{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}}
}

func installTestPgvector(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), "CREATE EXTENSION IF NOT EXISTS vector WITH SCHEMA public")
	require.NoError(t, err)
}

func TestPostgreSQLLibraryEmbeddings(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	installTestPgvector(t, admin)
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
	var failOnce atomic.Bool
	started, resume := make(chan struct{}), make(chan struct{})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() || failOnce.CompareAndSwap(true, false) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if block.Load() {
			close(started)
			<-resume
		}
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			v := []float32{1, 0}
			if strings.Contains(input, "second") {
				v = []float32{0, 1}
			}
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	}))
	t.Cleanup(service.Close)
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}, Retry: embedclient.Retry{MaxAttempts: 1}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: testDescriptor()}}
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
	var state string
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT state FROM index_generations").Scan(&state))
	assert.Equal(t, "building", state)
	fail.Store(false)
	failOnce.Store(true)
	require.NoError(t, runtime.Embed(t.Context()))
	assert.False(t, failOnce.Load())
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT state FROM index_generations").Scan(&state))
	assert.Equal(t, "active", state)
	_, err = owner.Store().DB().ExecContext(t.Context(), "DROP INDEX idx_embeddings_hnsw_d2")
	require.NoError(t, err)
	setupOpts := opts
	setupOpts.URL = dsn
	require.NoError(t, archive.Setup(t.Context(), setupOpts))
	var indexExists bool
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT to_regclass('idx_embeddings_hnsw_d2') IS NOT NULL").Scan(&indexExists))
	assert.True(t, indexExists)
	handler := runtime.NewServer(archive.ServerOptions{}).Handler(nil)
	for _, mode := range []string{"vector", "hybrid"} {
		response := sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, "/api/v1/search?q=second&mode="+mode)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), "observatory")
		assert.NotContains(t, response.Body.String(), `"subject":"second"`)
	}

	// The next pass includes messages written after the first pass.
	conversation, err := owner.Store().EnsureConversation(t.Context(), sourceIDs[0], "channel", "Channel")
	require.NoError(t, err)
	id, err := owner.Store().UpsertMessage(&store.Message{SourceID: sourceIDs[0], ConversationID: conversation, SourceMessageID: "two", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: "new arrival", Valid: true}})
	require.NoError(t, err)
	require.NoError(t, owner.Store().UpsertMessageBody(id, sql.NullString{String: "new arrival", Valid: true}, sql.NullString{}))
	ctx, preempt := jobctx.WithPreemption(t.Context())
	preempt()
	require.ErrorContains(t, runtime.Embed(ctx), "1 messages remaining")
	require.NoError(t, runtime.Embed(t.Context()))
	response := sourceScopeRequest(archive.WithSourceScope(t.Context(), sourceIDs[:1]), t, handler, "/api/v1/search?q=meaning&mode=vector")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), `"subject":"new arrival"`)

	require.NoError(t, owner.Store().UpsertMessageBody(id, sql.NullString{String: "second", Valid: true}, sql.NullString{}))
	require.NoError(t, runtime.Embed(t.Context()))
	var updated string
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT embedding::text FROM embeddings WHERE message_id=$1 AND chunk_index=0`, id).Scan(&updated))
	assert.Equal(t, "[0,1]", updated)

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
	_, err = owner.Store().DB().ExecContext(t.Context(), "UPDATE messages SET source_id=$1, conversation_id=$2, source_message_id='moved' WHERE id=$3", sourceIDs[0], conversation, messageIDs[1])
	close(resume)
	require.NoError(t, err)
	response = <-result
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), `"subject":"second"`)

	var vectors, count int
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings").Scan(&vectors))
	require.Equal(t, 3, vectors)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT message_count FROM index_generations WHERE state='active'").Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, runtime.PurgeChannel(t.Context(), sourceIDs[0], "channel"))
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings").Scan(&vectors))
	assert.Zero(t, vectors)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT message_count FROM index_generations WHERE state='active'").Scan(&count))
	assert.Zero(t, count)
	backend, err := pgvector.Open(t.Context(), pgvector.Options{DB: owner.Store().DB(), SkipMigrate: true, ReadOnly: true})
	require.NoError(t, err)
	active, err := backend.ActiveGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, backend.RetireGeneration(t.Context(), active.ID, true))
	err = runtime.Embed(t.Context())
	require.ErrorIs(t, err, vector.ErrNoActiveGeneration)
	require.ErrorContains(t, err, "run Setup")
	assert.NotContains(t, err.Error(), "differs")
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
	for _, statement := range []string{"CREATE SCHEMA archive_data", "CREATE SCHEMA archive_empty", "CREATE SCHEMA vector_types"} {
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
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	}))
	t.Cleanup(service.Close)
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: u.String(), Embeddings: &archive.Embeddings{Client: client, Descriptor: testDescriptor()}}
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
	_, err = db.ExecContext(t.Context(), `CREATE FUNCTION vector_types.bump_document_index_revision_after_message_type_update() RETURNS integer LANGUAGE SQL AS 'SELECT 1'; CREATE VIEW vector_types.keep_function AS SELECT vector_types.bump_document_index_revision_after_message_type_update(); CREATE TABLE vector_types.pending_embeddings (generation_id bigint, message_id bigint); INSERT INTO vector_types.pending_embeddings VALUES (1, 42); CREATE TABLE vector_types.embeddings (generation_id bigint, message_id bigint); INSERT INTO vector_types.embeddings VALUES (1, 999)`)
	require.NoError(t, err)
	require.NoError(t, archive.Setup(t.Context(), opts))
	var pending int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_types.pending_embeddings`).Scan(&pending))
	assert.Equal(t, 1, pending)
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

	// An empty archive does not prune tables belonging to the extension schema.
	emptyOpts := opts
	emptyOpts.Schema = "archive_empty"
	emptyOpts.Embeddings = nil
	require.NoError(t, archive.Setup(t.Context(), emptyOpts))
	emptyOpts.Embeddings = opts.Embeddings
	empty, err := archive.Open(t.Context(), emptyOpts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, empty.Close()) }()
	unavailable := sourceScopeRequest(t.Context(), t, empty.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=meaning&mode=vector")
	assert.Equal(t, http.StatusServiceUnavailable, unavailable.Code, unavailable.Body.String())
	require.NoError(t, empty.PurgeSource(t.Context(), 999))
	var foreignCount int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM vector_types.embeddings`).Scan(&foreignCount))
	assert.Equal(t, 1, foreignCount)

	params.Del("search_path")
	params.Set("options", "-csearch_path=archive_data,public")
	u.RawQuery = params.Encode()
	opts.URL = u.String()
	require.ErrorContains(t, archive.Setup(t.Context(), opts), "options")
}

func TestPostgreSQLEmbeddingsGenerationIdentity(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	installTestPgvector(t, db)
	cases := []struct {
		name    string
		change  func(*embedmodel.Descriptor)
		changed bool
	}{
		{"model", func(d *embedmodel.Descriptor) { d.Model.Name = "other" }, true},
		{"suffix", func(d *embedmodel.Descriptor) { d.Roles.DocumentSuffix = " end" }, true},
		{"endpoint", func(d *embedmodel.Descriptor) { d.Deployment.BaseURL = "https://other.example" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			schema := "archive_" + strings.ToLower(rand.Text())
			t.Cleanup(func() {
				_, err := db.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
				assert.NoError(t, err)
			})
			d := testDescriptor()
			client, err := embedclient.New(embedclient.Options{Model: d.Model, Roles: d.Roles, Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:1", TrustPrivateNetwork: true}})
			require.NoError(t, err)
			settings := &archive.Embeddings{Client: client, Descriptor: d}
			opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: settings}
			require.NoError(t, archive.Setup(t.Context(), opts))
			archiveA, err := archive.Open(t.Context(), opts)
			require.NoError(t, err)
			require.NoError(t, archiveA.Embed(t.Context()))
			require.NoError(t, archiveA.Close())
			tc.change(&settings.Descriptor)
			if tc.changed {
				require.ErrorContains(t, archive.Setup(t.Context(), opts), "differs from configured")
			} else {
				require.NoError(t, archive.Setup(t.Context(), opts))
			}
		})
	}
}

func assertSearchMessage(t *testing.T, response *httptest.ResponseRecorder, id int64) {
	t.Helper()
	var body struct {
		Results []struct {
			ID int64 `json:"id"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.NotEmpty(t, body.Results)
	assert.Equal(t, id, body.Results[0].ID)
}

func TestPostgreSQLEmbeddingsRebuild(t *testing.T) {
	require.ErrorContains(t, archive.RebuildEmbeddings(t.Context(), archive.PostgreSQL{}), "requires an embedding client")
	require.ErrorContains(t, archive.RebuildEmbeddings(t.Context(), archive.PostgreSQL{Embeddings: &archive.Embeddings{}}), "requires an embedding client")
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	installTestPgvector(t, admin)
	schema := "archive_" + strings.ToLower(rand.Text())
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	t.Cleanup(func() {
		_, e := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+quote(schema)+" CASCADE")
		assert.NoError(t, e)
	})
	var failOther atomic.Bool
	var otherSuccesses atomic.Int64
	var requests atomic.Int64
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		if request.Model == "other" && failOther.Load() && otherSuccesses.Load() > 0 {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		if request.Model == "other" {
			otherSuccesses.Add(1)
		}
		requests.Add(1)
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			v := []float32{0, 1}
			if strings.Contains(input, "observatory") {
				v = []float32{1, 0}
			}
			if request.Model == "other" {
				v = append(v, 0)
			}
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": request.Model}))
	}))
	t.Cleanup(service.Close)
	settings := func(t *testing.T, d embedmodel.Descriptor) *archive.Embeddings {
		t.Helper()
		client, err := embedclient.New(embedclient.Options{Model: d.Model, Roles: d.Roles, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}})
		require.NoError(t, err)
		return &archive.Embeddings{Client: client, Descriptor: d}
	}
	desc := testDescriptor()
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: settings(t, desc)}
	require.NoError(t, archive.Setup(t.Context(), opts))
	owner, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, owner.Close()) }()
	source, err := owner.Store().GetOrCreateSource("slack", "source")
	require.NoError(t, err)
	conversation, err := owner.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
	require.NoError(t, err)
	var observatoryID int64
	for i := range 33 {
		body := fmt.Sprintf("message %d", i)
		if i == 0 {
			body = "observatory"
		}
		id, err := owner.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: body, MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: body, Valid: true}})
		require.NoError(t, err)
		if i == 0 {
			observatoryID = id
		}
		require.NoError(t, owner.Store().UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		require.NoError(t, owner.Store().UpsertFTS(id, body, body, "", "", ""))
	}
	require.NoError(t, owner.Embed(t.Context()))
	var oldID int64
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT id FROM index_generations WHERE state='active'`).Scan(&oldID))
	original := desc
	require.True(t, t.Run("unsupported dimension", func(t *testing.T) {
		unsupported := desc
		unsupported.Model.Dimensions = 3072
		opts.Embeddings = settings(t, unsupported)
		require.Error(t, archive.RebuildEmbeddings(t.Context(), opts))
	}))
	var buildingID int64
	var built int
	require.True(t, t.Run("failed build and revert", func(t *testing.T) {
		desc.Model.Name, desc.Model.Dimensions = "other", 3
		opts.Embeddings = settings(t, desc)
		require.ErrorContains(t, archive.Setup(t.Context(), opts), "differs from configured")
		failOther.Store(true)
		require.ErrorContains(t, archive.RebuildEmbeddings(t.Context(), opts), "unavailable")
		failOther.Store(false)
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT id FROM index_generations WHERE state='building'`).Scan(&buildingID))
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE generation_id=$1`, buildingID).Scan(&built))
		require.Positive(t, built)
		require.Less(t, built, 33)
		invalid := opts
		invalid.Embeddings = settings(t, original)
		invalid.MaxOpenConnections = 1
		require.ErrorContains(t, archive.RebuildEmbeddings(t.Context(), invalid), "MaxOpenConnections")
		var stillBuilding int64
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT id FROM index_generations WHERE state='building'`).Scan(&stillBuilding))
		assert.Equal(t, buildingID, stillBuilding)
		var remaining int
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE generation_id=$1`, buildingID).Scan(&remaining))
		assert.Equal(t, built, remaining)
		var count int
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE generation_id=$1`, oldID).Scan(&count))
		assert.Equal(t, 33, count)
		response := sourceScopeRequest(t.Context(), t, owner.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=observatory&mode=vector")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assertSearchMessage(t, response, observatoryID)
		require.ErrorContains(t, archive.Setup(t.Context(), opts), "RebuildEmbeddings")
		opts.Embeddings = settings(t, original)
		before := requests.Load()
		require.NoError(t, archive.RebuildEmbeddings(t.Context(), opts))
		assert.Greater(t, requests.Load(), before)
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM index_generations WHERE state='building'`).Scan(&count))
		assert.Zero(t, count)
		before = requests.Load()
		require.NoError(t, owner.Embed(t.Context()))
		assert.Equal(t, before, requests.Load())
	}))
	require.True(t, t.Run("resume build", func(t *testing.T) {
		opts.Embeddings = settings(t, desc)
		otherSuccesses.Store(0)
		failOther.Store(true)
		require.ErrorContains(t, archive.RebuildEmbeddings(t.Context(), opts), "unavailable")
		failOther.Store(false)
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT id FROM index_generations WHERE state='building'`).Scan(&buildingID))
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE generation_id=$1`, buildingID).Scan(&built))
		require.Positive(t, built)
		require.Less(t, built, 33)
		require.NoError(t, archive.RebuildEmbeddings(t.Context(), opts))
		var activeID int64
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT id FROM index_generations WHERE state='active'`).Scan(&activeID))
		assert.Equal(t, buildingID, activeID)
		runtime, err := archive.Open(t.Context(), opts)
		require.NoError(t, err)
		defer func() { assert.NoError(t, runtime.Close()) }()
		handler := runtime.NewServer(archive.ServerOptions{}).Handler(nil)
		response := sourceScopeRequest(t.Context(), t, handler, "/api/v1/search?q=observatory&mode=vector")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assertSearchMessage(t, response, observatoryID)
		var count, dimension int
		var model string
		require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), `SELECT count(*), max(dimension), max(model) FROM index_generations WHERE state='active'`).Scan(&count, &dimension, &model))
		assert.Equal(t, 1, count)
		assert.Equal(t, 3, dimension)
		assert.Equal(t, "other", model)
	}))
	require.True(t, t.Run("same dimension", func(t *testing.T) {
		suffix := desc
		suffix.Roles.DocumentSuffix = " end"
		opts.Embeddings = settings(t, suffix)
		require.ErrorContains(t, archive.Setup(t.Context(), opts), "RebuildEmbeddings")
		require.NoError(t, archive.RebuildEmbeddings(t.Context(), opts))
		reopened, err := archive.Open(t.Context(), opts)
		require.NoError(t, err)
		defer func() { assert.NoError(t, reopened.Close()) }()
		response := sourceScopeRequest(t.Context(), t, reopened.NewServer(archive.ServerOptions{}).Handler(nil), "/api/v1/search?q=observatory&mode=vector")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assertSearchMessage(t, response, observatoryID)
	}))
}

func TestPostgreSQLEmbeddingsForeignBuild(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	installTestPgvector(t, db)
	schema := "archive_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, err := db.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	desc := testDescriptor()
	client, err := embedclient.New(embedclient.Options{Model: desc.Model, Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:1", TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: desc}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	q := u.Query()
	q.Set("search_path", schema+",public")
	u.RawQuery = q.Encode()
	check, err := sql.Open("pgx", u.String())
	require.NoError(t, err)
	defer func() { _ = check.Close() }()
	var matchingFingerprint string
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT fingerprint FROM index_generations WHERE state='building'`).Scan(&matchingFingerprint))
	_, err = check.ExecContext(t.Context(), `CREATE TABLE pending_embeddings (generation_id bigint, message_id bigint); INSERT INTO pending_embeddings VALUES (1, 999)`)
	require.NoError(t, err)
	_, err = check.ExecContext(t.Context(), `UPDATE index_generations SET fingerprint='unknown-future-policy' WHERE state='building'`)
	require.NoError(t, err)
	_, err = check.ExecContext(t.Context(), `INSERT INTO embeddings (generation_id,message_id,embedded_at,source_char_len,dimension,embedding) SELECT id,999,0,0,2,'[1,0]'::vector FROM index_generations WHERE state='building'`)
	require.NoError(t, err)
	require.ErrorContains(t, archive.Setup(t.Context(), opts), "unknown-future-policy")
	var pendingCount int
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_embeddings`).Scan(&pendingCount))
	assert.Equal(t, 1, pendingCount)
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.Close()) }()
	require.ErrorContains(t, a.Embed(t.Context()), "unknown-future-policy")
	var count int
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=999`).Scan(&count))
	assert.Equal(t, 1, count)

	// An unknown active generation must remain untouched, even if a matching
	// generation is building and could otherwise be activated over it.
	_, err = check.ExecContext(t.Context(), `UPDATE index_generations SET state='active', fingerprint='future-format:v2' WHERE state='building'`)
	require.NoError(t, err)
	require.ErrorContains(t, archive.Setup(t.Context(), opts), "future-format:v2")
	require.ErrorContains(t, a.Embed(t.Context()), "future-format:v2")
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT count(*) FROM pending_embeddings`).Scan(&pendingCount))
	assert.Equal(t, 1, pendingCount)
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=999`).Scan(&count))
	assert.Equal(t, 1, count)

	// A matching build cannot replace an unknown active generation without a rebuild.
	_, err = check.ExecContext(t.Context(), `INSERT INTO index_generations (model, dimension, fingerprint, started_at, state) VALUES ('test', 2, $1, 0, 'building')`, matchingFingerprint)
	require.NoError(t, err)
	require.ErrorContains(t, archive.Setup(t.Context(), opts), "future-format:v2")
	require.ErrorContains(t, a.Embed(t.Context()), "future-format:v2")
	require.NoError(t, check.QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings WHERE message_id=999`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestPostgreSQLEmbeddingsPurgeRetry(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires PostgreSQL")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	installTestPgvector(t, admin)
	schema := "archive_" + strings.ToLower(rand.Text())
	role := "runtime_" + strings.ToLower(rand.Text())
	quote := func(s string) string { return pgx.Identifier{s}.Sanitize() }
	t.Cleanup(func() {
		_, e := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+quote(schema)+" CASCADE")
		assert.NoError(t, e)
		_, e = admin.ExecContext(context.WithoutCancel(t.Context()), "DROP ROLE IF EXISTS "+quote(role))
		assert.NoError(t, e)
	})
	desc := testDescriptor()
	client, err := embedclient.New(embedclient.Options{Model: desc.Model, Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:1", TrustPrivateNetwork: true}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: desc}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	owner, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, owner.Close()) }()
	require.NoError(t, owner.Embed(t.Context()))
	source, err := owner.Store().GetOrCreateSource("slack", "source")
	require.NoError(t, err)
	conversation, err := owner.Store().EnsureConversation(t.Context(), source.ID, "channel", "Channel")
	require.NoError(t, err)
	id, err := owner.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "one", MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}})
	require.NoError(t, err)
	_, err = owner.Store().DB().ExecContext(t.Context(), `INSERT INTO embeddings (generation_id, message_id, embedded_at, source_char_len, dimension, embedding) SELECT id, $1, 0, 0, 2, '[1,0]'::vector FROM index_generations WHERE state='active'`, id)
	require.NoError(t, err)
	_, err = owner.Store().DB().ExecContext(t.Context(), `UPDATE index_generations SET message_count=1 WHERE state='active'`)
	require.NoError(t, err)
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
	_, err = admin.ExecContext(t.Context(), "REVOKE DELETE ON "+quote(schema)+".embeddings FROM "+quote(role))
	require.NoError(t, err)
	require.ErrorContains(t, runtime.PurgeSource(t.Context(), source.ID), "delete orphan embeddings")
	var vectors, count int
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sources WHERE id=$1", source.ID).Scan(&count))
	assert.Zero(t, count)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings").Scan(&vectors))
	assert.Equal(t, 1, vectors)
	_, err = admin.ExecContext(t.Context(), "GRANT DELETE ON "+quote(schema)+".embeddings TO "+quote(role))
	require.NoError(t, err)
	require.NoError(t, runtime.PurgeSource(t.Context(), source.ID))
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT count(*) FROM embeddings").Scan(&vectors))
	assert.Zero(t, vectors)
	require.NoError(t, owner.Store().DB().QueryRowContext(t.Context(), "SELECT message_count FROM index_generations WHERE state='active'").Scan(&count))
	assert.Zero(t, count)
}

// A library host's embedding pass and meaning search follow room policy like
// the daemon's: a Cold room is neither embedded nor counted as pending, and
// its messages stay out of vector and hybrid results.
func TestPostgreSQLLibraryEmbeddingsHideColdRooms(t *testing.T) {
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
		_, e := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, e)
	})
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i, input := range request.Input {
			v := []float32{1, 0}
			if strings.Contains(input, "second") {
				v = []float32{0, 1}
			}
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "test"}))
	}))
	t.Cleanup(service.Close)
	client, err := embedclient.New(embedclient.Options{Model: embedconfig.Model{Name: "test", Dimensions: 2, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone}, Deployment: embedconfig.Deployment{BaseURL: service.URL, TrustPrivateNetwork: true}, Retry: embedclient.Retry{MaxAttempts: 1}})
	require.NoError(t, err)
	opts := archive.PostgreSQL{URL: dsn, Schema: schema, Embeddings: &archive.Embeddings{Client: client, Descriptor: testDescriptor()}}
	require.NoError(t, archive.Setup(t.Context(), opts))
	a, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	defer func() { assert.NoError(t, a.Close()) }()
	source, err := a.Store().GetOrCreateSource("slack", "workspace")
	require.NoError(t, err)
	for _, body := range []string{"observatory", "second"} {
		conversation, e := a.Store().EnsureConversation(t.Context(), source.ID, body, body)
		require.NoError(t, e)
		id, e := a.Store().UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: body, MessageType: "slack", SentAt: sql.NullTime{Time: time.Now(), Valid: true}, Subject: sql.NullString{String: body, Valid: true}})
		require.NoError(t, e)
		require.NoError(t, a.Store().UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		require.NoError(t, a.Store().UpsertFTS(id, body, body, "", "", ""))
	}
	require.NoError(t, a.Store().SetConversationPolicyOverride(t.Context(), source.ID, "second", conversationpolicy.Cold))
	require.NoError(t, a.Embed(t.Context()), "a Cold room's messages are not left pending")
	var coldVectors int
	require.NoError(t, a.Store().DB().QueryRowContext(t.Context(), `SELECT count(*) FROM embeddings e JOIN messages m ON m.id = e.message_id WHERE m.source_message_id = 'second'`).Scan(&coldVectors))
	assert.Zero(t, coldVectors)
	handler := a.NewServer(archive.ServerOptions{}).Handler(nil)
	for _, mode := range []string{"vector", "hybrid"} {
		response := sourceScopeRequest(t.Context(), t, handler, "/api/v1/search?q=second&mode="+mode)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Contains(t, response.Body.String(), `"subject":"observatory"`, mode)
		assert.NotContains(t, response.Body.String(), `"subject":"second"`, mode)
	}
}
