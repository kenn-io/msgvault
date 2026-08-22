//go:build fts5 && sqlite_vec

package cmd

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// seedEmbeddedGeneration embeds exactly msgIDs, activates the result, and
// stamps embed_gen on those rows in the main DB the way a real embed run
// would — needed because EmbeddedMessageCount requires the stamp, not just a
// vectors.db row, to count a message as embedded.
func seedEmbeddedGeneration(
	t *testing.T, dataDir, mainPath string, mainDB *store.Store, vecCfg vector.Config, msgIDs ...int64,
) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, sqlitevec.RegisterExtension(), "RegisterExtension")
	b, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path:      filepath.Join(dataDir, "vectors.db"),
		MainPath:  mainPath,
		Dimension: vecCfg.Embeddings.Dimension,
		MainDB:    mainDB.DB(),
	})
	require.NoError(t, err, "open vectors.db")
	defer func() { require.NoError(t, b.Close(), "close vectors.db") }()

	gen, err := b.CreateGeneration(ctx,
		vecCfg.Embeddings.Model, vecCfg.Embeddings.Dimension, vecCfg.GenerationFingerprint())
	require.NoError(t, err, "CreateGeneration")

	dim := vecCfg.Embeddings.Dimension
	chunks := make([]vector.Chunk, 0, len(msgIDs))
	for _, id := range msgIDs {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(id) / 10
		}
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: v, SourceCharLen: 32})
	}
	require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")
	require.NoError(t, b.ActivateGeneration(ctx, gen, true), "ActivateGeneration")

	for _, id := range msgIDs {
		_, err = mainDB.DB().Exec(`UPDATE messages SET embed_gen = ? WHERE id = ?`, int64(gen), id)
		require.NoError(t, err, "stamp embed_gen on message %d", id)
	}
}

// TestAttachVector_ReportsScopedCorpusSeparatelyFromTheArchive is the
// regression for provenance that described the archive when a run's vector
// generation only ever searches part of it. An account-scoped
// [vector.embed.scope] means retrieval can return source 1's two messages
// and never source 2's, so a report that prints the whole three-message,
// two-conversation archive as "the corpus" overstates what vector mode's
// recall is actually measured against.
func TestAttachVector_ReportsScopedCorpusSeparatelyFromTheArchive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	s := seedTwoSourceArchiveIn(t, dataDir, false)

	c := evalVectorConfig(t, vector.APIFormatOpenAI, "test-model")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Dimension = 3
	c.Vector.Embed.Scope.SourceIDs = []int64{1}
	withTestConfig(t, c)

	seedEmbeddedGeneration(t, dataDir, c.DatabaseDSN(), s, c.Vector, 1, 2)

	ev := &evaluator{ctx: ctx, diag: &runDiagnostics{}}
	cleanup, err := ev.attachVector(ctx, s)
	require.NoError(err, "attachVector")
	defer cleanup()
	ev.collectCorpusStats(s.DB())

	assert.EqualValues(3, ev.prov.Messages, "the archive holds three live messages across both sources")
	assert.EqualValues(2, ev.prov.Conversations, "and two conversations")
	assert.EqualValues(2, ev.prov.VectorMessages,
		"only source 1's two messages are embedded in the active generation")
	assert.EqualValues(1, ev.prov.VectorConversations,
		"and only source 1's one conversation")
}

// TestAttachVector_CorpusScopeFilterExcludesOutOfScopeStampedMessages pins
// the scope-filter clause in collectVectorCorpusStats specifically, as
// opposed to the embed_gen stamp doing all the work by coincidence. Message
// 3 (source 2) is embedded and stamped into the very same generation as
// messages 1 and 2 — as if a stray embed run, or a since-narrowed scope,
// left an out-of-scope message carrying this generation's stamp — while
// [vector.embed.scope] declares only source 1. Without the source_id filter
// this would count all three messages and both conversations; with it, the
// out-of-scope stamp must not move the reported corpus at all.
func TestAttachVector_CorpusScopeFilterExcludesOutOfScopeStampedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	s := seedTwoSourceArchiveIn(t, dataDir, false)

	c := evalVectorConfig(t, vector.APIFormatOpenAI, "test-model")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Dimension = 3
	c.Vector.Embed.Scope.SourceIDs = []int64{1}
	withTestConfig(t, c)

	seedEmbeddedGeneration(t, dataDir, c.DatabaseDSN(), s, c.Vector, 1, 2, 3)

	ev := &evaluator{ctx: ctx, diag: &runDiagnostics{}}
	cleanup, err := ev.attachVector(ctx, s)
	require.NoError(err, "attachVector")
	defer cleanup()

	assert.EqualValues(2, ev.prov.VectorMessages,
		"source 2's stamped-but-out-of-scope message must not count")
	assert.EqualValues(1, ev.prov.VectorConversations,
		"nor must its conversation")
}

// TestAttachVector_MatchesArchiveCorpusWhenScopeCoversIt pins the other
// half: a generation with no [vector.embed.scope] restriction embeds every
// live message in the archive, so VectorMessages/VectorConversations equal
// Messages/Conversations exactly and the table print (eval.go's table
// method) has nothing narrower to add.
func TestAttachVector_MatchesArchiveCorpusWhenScopeCoversIt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	s := seedRankingDivergenceArchiveIn(t, dataDir)

	c := evalVectorConfig(t, vector.APIFormatOpenAI, "test-model")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Dimension = 3
	withTestConfig(t, c)

	// seedRankingDivergenceArchiveIn's two live messages (1 and 2) sit in
	// separate conversations; message 3 is deleted from its source and
	// excluded from both the archive count and the embedded set.
	seedEmbeddedGeneration(t, dataDir, c.DatabaseDSN(), s, c.Vector, 1, 2)

	ev := &evaluator{ctx: ctx, diag: &runDiagnostics{}}
	cleanup, err := ev.attachVector(ctx, s)
	require.NoError(err, "attachVector")
	defer cleanup()
	ev.collectCorpusStats(s.DB())

	require.EqualValues(2, ev.prov.Messages)
	require.EqualValues(2, ev.prov.Conversations)
	assert.Equal(ev.prov.Messages, ev.prov.VectorMessages,
		"an unscoped generation embeds the whole archive")
	assert.Equal(ev.prov.Conversations, ev.prov.VectorConversations)
}
