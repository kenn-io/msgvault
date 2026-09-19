//go:build fts5 && sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// evalVectorConfig builds a config whose vector section is valid apart from
// the embeddings fields the caller overrides, pointed at a scratch data dir.
func evalVectorConfig(t *testing.T, format vector.EmbeddingAPIFormat, model string) *config.Config {
	t.Helper()
	c := &config.Config{}
	c.Data.DataDir = t.TempDir()
	c.Vector.Enabled = true
	c.Vector.Embeddings.Endpoint = "http://127.0.0.1:1/v1"
	c.Vector.Embeddings.Model = model
	c.Vector.Embeddings.APIFormat = format
	c.Vector.Embeddings.Dimension = 1024
	c.Vector.ApplyDefaults()
	return c
}

// TestAttachVector_RejectsUnusableEmbeddingConfig pins the eval command's
// fail-fast contract on the resolved vector config. An api_format this binary
// cannot build a client for, and a contextual format paired with a model the
// contextual endpoint does not serve, must both stop the run with an error
// naming the offending value — before any index is opened, so a bad config can
// never be scored as poor retrieval.
func TestAttachVector_RejectsUnusableEmbeddingConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, _ := setupScopeFixture(t)

	c := evalVectorConfig(t, "voyage", "voyage-context-4")
	withTestConfig(t, c)

	ev := &evaluator{ctx: context.Background()}
	cleanup, err := ev.attachVector(context.Background(), f.Store)
	require.Error(err, "an unsupported api_format must not fall back to the OpenAI-compatible client")
	assert.Nil(cleanup)
	assert.Contains(err.Error(), "api_format")
	assert.Contains(err.Error(), `"voyage"`)

	_, statErr := os.Stat(filepath.Join(c.Data.DataDir, "vectors.db"))
	assert.True(os.IsNotExist(statErr), "the config check must run before the index is opened")
}

// seedActiveGeneration activates an empty generation carrying the config's
// own fingerprint, so attachVector gets past the active-generation check and
// on to the part under test.
func seedActiveGeneration(t *testing.T, dataDir, mainPath string, mainDB *sql.DB, vecCfg vector.Config) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dataDir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: vecCfg.Embeddings.Dimension,
		MainDB:    mainDB,
	})
	require.NoError(t, err, "open vectors.db")
	defer func() { require.NoError(t, b.Close(), "close vectors.db") }()

	gen, err := b.CreateGeneration(ctx,
		vecCfg.Embeddings.Model, vecCfg.Embeddings.Dimension, vecCfg.GenerationFingerprint())
	require.NoError(t, err, "CreateGeneration")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "ActivateGeneration")
}

// TestAttachVector_EmbedsQueriesThroughConfiguredAPIFormat is the end-to-end
// binding: run the eval command's own vector setup against a config that says
// api_format = "voyage-contextual", then embed a query through the engine it
// wired and watch what goes over the wire. Before this fix the request landed
// on /v1/embeddings as a flat OpenAI-compatible body, so a contextual index was
// scored with query vectors from a different endpoint and a different role.
func TestAttachVector_EmbedsQueriesThroughConfiguredAPIFormat(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	s := seedRankingDivergenceArchiveIn(t, dataDir)
	rec, endpoint := embedTestServer(t, `{"data":[{"index":0,"data":[{"index":0,"embedding":[0.25,0.5,0.75]}]}]}`)

	c := evalVectorConfig(t, vector.APIFormatVoyageContextual, "voyage-context-4")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Endpoint = endpoint
	c.Vector.Embeddings.Dimension = 3
	withTestConfig(t, c)
	seedActiveGeneration(t, dataDir, c.DatabaseDSN(), s.DB(), c.Vector)

	ev := &evaluator{ctx: ctx, diag: &runDiagnostics{}}
	cleanup, err := ev.attachVector(ctx, s)
	require.NoError(err, "attachVector")
	defer cleanup()

	vec, err := ev.heng.EmbedQuery(ctx, "lease renewal")
	require.NoError(err, "EmbedQuery")
	assert.Equal([]float32{0.25, 0.5, 0.75}, vec)

	path, body := rec.seen()
	assert.Equal("/v1/contextualizedembeddings", path, "queries must go to the contextual endpoint")
	assert.Equal("query", body["input_type"], "queries must carry the query role, not the document role")
	assert.Equal("voyage-contextual", ev.prov.APIFormat, "the run reports the format that produced its scores")
}

// TestAttachVector_RejectsContextualModelMismatch covers the other resolved
// config the eval tool must refuse: api_format = "voyage-contextual" with a
// non-contextual model.
func TestAttachVector_RejectsContextualModelMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, _, _ := setupScopeFixture(t)

	withTestConfig(t, evalVectorConfig(t, vector.APIFormatVoyageContextual, "voyage-large-4"))

	ev := &evaluator{ctx: context.Background()}
	_, err := ev.attachVector(context.Background(), f.Store)
	require.Error(err)
	assert.Contains(err.Error(), "voyage-large-4")
}
