//go:build fts5 && sqlite_vec

package cmd

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// seedLifecycleShapedArchive builds an archive with the row shapes a long-lived
// install accumulates and a fresh benchmark corpus never has:
//
//	thread-1  m1 live, m2 live
//	thread-2  m3 live, m4 deleted from its source account
//	thread-3  m5 dedup-hidden (deleted_at set — a losing duplicate)
//	thread-4  no messages left at all
//
// Three messages are retrievable, across two threads. The tables hold five and
// four.
func seedLifecycleShapedArchive(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "msgvault.db"))
	require.NoError(t, err, "open store")
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchema(), "init schema")

	_, err = s.DB().Exec(`
INSERT INTO sources (id, source_type, identifier) VALUES (1, 'gmail', 'me@example.com');
INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type) VALUES
	(1, 1, 'thread-1', 'email_thread'),
	(2, 1, 'thread-2', 'email_thread'),
	(3, 1, 'thread-3', 'email_thread'),
	(4, 1, 'thread-4', 'email_thread');
INSERT INTO messages
	(id, conversation_id, source_id, source_message_id, message_type, subject, sent_at,
	 size_estimate, deleted_at, deleted_from_source_at) VALUES
	(1, 1, 1, '<m1@example.com>', 'email', 'Lease renewal terms',  '2020-01-01T00:00:00Z', 100, NULL, NULL),
	(2, 1, 1, '<m2@example.com>', 'email', 'Re: Lease renewal',    '2020-01-02T00:00:00Z', 100, NULL, NULL),
	(3, 2, 1, '<m3@example.com>', 'email', 'Insurance certificate','2020-02-01T00:00:00Z', 100, NULL, NULL),
	(4, 2, 1, '<m4@example.com>', 'email', 'Deleted upstream',     '2020-02-02T00:00:00Z', 100, NULL, '2023-06-01T00:00:00Z'),
	(5, 3, 1, '<m5@example.com>', 'email', 'Duplicate copy',       '2020-03-01T00:00:00Z', 100, '2023-01-01T00:00:00Z', NULL);
`)
	require.NoError(t, err, "seed archive")
	return s
}

// TestCollectCorpusStats_CountsOnlyTheSearchablePopulation is the regression for
// provenance that described the tables instead of the haystack.
//
// COUNT(*) over messages and conversations includes dedup-hidden duplicates,
// messages deleted from their source, and conversations with nothing left in
// them — none of which any search this command runs can return. Reporting them
// as the corpus size inflates the denominator a reader mentally divides recall
// by, and does it invisibly: on the flat TREC corpus this branch is normally
// exercised against, every count coincides.
func TestCollectCorpusStats_CountsOnlyTheSearchablePopulation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := seedLifecycleShapedArchive(t)

	// What the raw tables hold, so a change to the fixture cannot quietly
	// make the assertions below trivially true.
	var rawMessages, rawConversations int64
	require.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&rawMessages))
	require.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rawConversations))
	require.EqualValues(5, rawMessages)
	require.EqualValues(4, rawConversations)

	ev, _ := newTestEvaluator(t, s, "message")
	ev.collectCorpusStats(s.DB())

	assert.EqualValues(3, ev.prov.Messages,
		"the dedup-hidden and source-deleted rows are not part of the searched corpus")
	assert.EqualValues(2, ev.prov.Conversations,
		"and neither is a thread with no live message left in it")
}

// TestCollectCorpusStats_MatchesWhatSearchCanReturn ties the count to the
// retrieval path rather than to a hand-written expectation: whatever the
// production FTS search is willing to return over the whole archive is the
// population the provenance block must be describing.
func TestCollectCorpusStats_MatchesWhatSearchCanReturn(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := seedLifecycleShapedArchive(t)

	// An empty query with no text terms filters on nothing but the deletion
	// scope, so the result set is exactly the live population.
	found, _, err := s.SearchMessagesQueryContext(t.Context(), &search.Query{}, 0, 100)
	require.NoError(err, "production store search")

	threads := map[int64]struct{}{}
	for _, m := range found {
		threads[m.ConversationID] = struct{}{}
	}

	ev, _ := newTestEvaluator(t, s, "message")
	ev.collectCorpusStats(s.DB())

	assert.EqualValues(len(found), ev.prov.Messages,
		"the reported corpus must be the one search draws from")
	assert.EqualValues(len(threads), ev.prov.Conversations)
}

// seedTwoGenerations writes an index that has been rebuilt once: an older
// generation carrying three vectors, retired but retained (sqlitevec keeps a
// retired generation's rows — vec0 partition-key isolation means retiring does
// not delete them), and the active generation carrying one. Returns the total
// row count and the active generation's.
func seedTwoGenerations(
	t *testing.T, dataDir, mainPath string, mainDB *sql.DB, vecCfg vector.Config,
) (total, active int64) {
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

	dim := vecCfg.Embeddings.Dimension
	chunk := func(msgID int64) vector.Chunk {
		v := make([]float32, dim)
		for i := range v {
			v[i] = float32(msgID) / 10
		}
		return vector.Chunk{MessageID: msgID, Vector: v, SourceCharLen: 32}
	}
	newGen := func(msgIDs ...int64) {
		gen, err := b.CreateGeneration(ctx,
			vecCfg.Embeddings.Model, dim, vecCfg.GenerationFingerprint())
		require.NoError(t, err, "CreateGeneration")
		chunks := make([]vector.Chunk, 0, len(msgIDs))
		for _, id := range msgIDs {
			chunks = append(chunks, chunk(id))
		}
		require.NoError(t, b.Upsert(ctx, gen, chunks), "Upsert")
		// force: the coverage gate is about messages.embed_gen, which this
		// test never stamps; the generation lifecycle is what is under test.
		require.NoError(t, b.ActivateGeneration(ctx, gen, true), "ActivateGeneration")
	}
	newGen(1, 2, 3) // superseded by the next activation, rows retained
	newGen(1)       // active

	require.NoError(t, b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings`).Scan(&total), "count all embeddings")
	require.NoError(t, b.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM embeddings WHERE generation_id =
			(SELECT id FROM index_generations WHERE state = 'active')`).Scan(&active),
		"count active embeddings")
	return total, active
}

// TestAttachVector_CountsOnlyTheActiveGenerationsVectors is the regression for
// the index-size half of the same problem. Search reads exactly one generation
// — the active one attachVector resolves — but the reported vector count was a
// COUNT(*) over the whole embeddings table, which also holds every retired
// generation's rows and any rebuild in progress. On a rebuilt archive that
// reports an index several times the size of the one the scores came from.
func TestAttachVector_CountsOnlyTheActiveGenerationsVectors(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()

	dataDir := t.TempDir()
	s := seedRankingDivergenceArchiveIn(t, dataDir)

	c := evalVectorConfig(t, vector.APIFormatOpenAI, "test-model")
	c.Data.DataDir = dataDir
	c.Vector.Embeddings.Dimension = 3
	withTestConfig(t, c)

	total, active := seedTwoGenerations(t, dataDir, c.DatabaseDSN(), s.DB(), c.Vector)
	require.EqualValues(4, total, "the retired generation's rows are retained")
	require.EqualValues(1, active, "and the active generation is the smaller one")

	ev := &evaluator{ctx: ctx, diag: &runDiagnostics{}}
	cleanup, err := ev.attachVector(ctx, s)
	require.NoError(err, "attachVector")
	defer cleanup()

	assert.Equal(active, ev.prov.IndexedVectors,
		"the run must report the index it searched, not every generation on disk")
	assert.Positive(ev.prov.IndexSizeBytes,
		"the whole-file measure stays available separately")
}
