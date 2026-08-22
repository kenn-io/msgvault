//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// TestPersonMigration_UpgradesAndIsIdempotent catches an upgrade that only
// creates person storage for fresh databases, or a migration that fails when
// startup applies it a second time.
func TestPersonMigration_UpgradesAndIsIdempotent(t *testing.T) {
	db := openPGTestDB(t)
	ctx := context.Background()
	seedLegacyPersonlessVectorSchema(t, db)

	var before sql.NullString
	require.NoError(t, db.QueryRow(`SELECT to_regclass('person_embeddings')::text`).Scan(&before))
	require.False(t, before.Valid, "legacy schema must predate person vector storage")

	require.NoError(t, Migrate(ctx, db, 4, false))
	require.NoError(t, Migrate(ctx, db, 4, false))

	var tableName string
	require.NoError(t, db.QueryRow(`SELECT to_regclass('person_embeddings')::text`).Scan(&tableName))
	assert.Equal(t, "person_embeddings", tableName)
	var deadDimensionIndex sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass('idx_person_embeddings_dim')::text`).Scan(&deadDimensionIndex))
	assert.False(t, deadDimensionIndex.Valid, "exact person ranking must not retain a dead dimension index")
	var personHNSWIndex string
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass($1)::text`, PersonVectorIndexName(4)).Scan(&personHNSWIndex))
	assert.Equal(t, PersonVectorIndexName(4), personHNSWIndex,
		"migration must retain the approved per-dimension person HNSW compatibility object")
	var personHNSWPredicate string
	require.NoError(t, db.QueryRow(`
		SELECT pg_get_expr(i.indpred, i.indrelid)
		  FROM pg_index AS i
		  JOIN pg_class AS c ON c.oid = i.indexrelid
		 WHERE c.relname = $1`, PersonVectorIndexName(4)).Scan(&personHNSWPredicate))
	assert.Contains(t, personHNSWPredicate, "dimension = 4")
	assert.Contains(t, personHNSWPredicate, "embedding IS NOT NULL")
	var legacyRows int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM embeddings WHERE message_id = 1`).Scan(&legacyRows))
	assert.Equal(t, 1, legacyRows, "upgrade must preserve legacy message vectors")
}

// TestPersonBackend_CreateGenerationSupportsNonDefaultDimension catches an
// exact person-ranking path accidentally tied to the configured default.
func TestPersonBackend_CreateGenerationSupportsNonDefaultDimension(t *testing.T) {
	b, ctx, db := newPersonBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 7, "")
	require.NoError(t, err)
	var personHNSWIndex string
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass($1)::text`, PersonVectorIndexName(7)).Scan(&personHNSWIndex))
	assert.Equal(t, PersonVectorIndexName(7), personHNSWIndex,
		"new dimensions must lazily receive their person HNSW compatibility object")
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 70, Revision: "rev-7", Vector: unitVec(7, 6),
	}}))
	hits, err := b.SearchPeople(ctx, gen, unitVec(7, 6), 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(70), hits[0].PersonID)
}

// TestPersonBackend_PublishSearchReplaceReconcile catches wrong cosine ordering,
// stale revision/vector rows after replacement, and reconciliation that keeps
// people absent from the exact current-ID set.
func TestPersonBackend_PublishSearchReplaceReconcile(t *testing.T) {
	b, ctx, _ := newPersonBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err)

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{
		{PersonID: 10, Revision: "rev-a", Vector: unitVec(4, 0)},
		{PersonID: 20, Revision: "rev-b", Vector: unitVec(4, 1)},
	}))
	revisions, err := b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{10: "rev-a", 20: "rev-b"}, revisions)

	hits, err := b.SearchPeople(ctx, gen, unitVec(4, 0), 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(10), hits[0].PersonID)
	assert.Equal(t, "rev-a", hits[0].Revision)
	assert.Equal(t, 1, hits[0].Rank)
	assert.Equal(t, int64(20), hits[1].PersonID)
	assert.Equal(t, 2, hits[1].Rank)

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{
		{PersonID: 10, Revision: "rev-c", Vector: unitVec(4, 2)},
	}))
	hits, err = b.SearchPeople(ctx, gen, unitVec(4, 2), 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(10), hits[0].PersonID)
	assert.Equal(t, "rev-c", hits[0].Revision)

	require.NoError(t, b.DeletePersonsNotIn(ctx, gen, []int64{20}))
	revisions, err = b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{20: "rev-b"}, revisions)
	hits, err = b.SearchPeople(ctx, gen, unitVec(4, 2), 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(20), hits[0].PersonID)

	require.NoError(t, b.DeletePersonsNotIn(ctx, gen, nil))
	revisions, err = b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Empty(t, revisions)
}

// TestPersonBackend_SearchCompletenessIsGenerationBounded catches exact
// ranking leaking rows from a concurrent generation or underfilling the
// requested generation's bounded corpus.
func TestPersonBackend_SearchCompletenessIsGenerationBounded(t *testing.T) {
	b, ctx, db := newPersonBackendForTest(t)
	db.SetMaxOpenConns(1)

	serving, err := b.CreateGeneration(ctx, "m", 4, "serving")
	require.NoError(t, err)
	servingPeople := make([]vector.PersonEmbedding, 5)
	for i := range servingPeople {
		servingPeople[i] = vector.PersonEmbedding{
			PersonID: int64(101 + i), Revision: "serving", Vector: unitVec(4, 1),
		}
	}
	require.NoError(t, b.UpsertPersons(ctx, serving, servingPeople))
	require.NoError(t, b.ActivateGeneration(ctx, serving, true))

	building, err := b.CreateGeneration(ctx, "m", 4, "building")
	require.NoError(t, err)
	buildingPeople := make([]vector.PersonEmbedding, 256)
	for i := range buildingPeople {
		buildingPeople[i] = vector.PersonEmbedding{
			PersonID: int64(1000 + i), Revision: "building", Vector: unitVec(4, 0),
		}
	}
	require.NoError(t, b.UpsertPersons(ctx, building, buildingPeople))

	hits, err := b.SearchPeople(ctx, serving, unitVec(4, 0), 5)
	require.NoError(t, err)
	require.Len(t, hits, 5)
	assert.Equal(t, []int64{101, 102, 103, 104, 105}, []int64{
		hits[0].PersonID, hits[1].PersonID, hits[2].PersonID, hits[3].PersonID, hits[4].PersonID,
	})
}

func TestPersonBackend_TerminalRevisionIsCoveredButNotSearchable(t *testing.T) {
	b, ctx, _ := newPersonBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{
		{PersonID: 1, Revision: "searchable", Vector: unitVec(4, 0)},
		{PersonID: 2, Revision: "terminal"},
	}))
	revisions, err := b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{1: "searchable", 2: "terminal"}, revisions)
	rejected, err := b.CountRejectedPersons(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rejected)
	hits, err := b.SearchPeople(ctx, gen, unitVec(4, 0), 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(1), hits[0].PersonID)
	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{
		{PersonID: 1, Revision: "now-terminal"},
	}))
	revisions, err = b.ListPersonRevisions(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, map[int64]string{1: "now-terminal", 2: "terminal"}, revisions)
	rejected, err = b.CountRejectedPersons(ctx, gen)
	require.NoError(t, err)
	assert.Equal(t, int64(2), rejected)
	hits, err = b.SearchPeople(ctx, gen, unitVec(4, 0), 10)
	require.NoError(t, err)
	assert.Empty(t, hits, "terminal replacement must remove the old searchable vector")
}

// TestPersonBackend_ErrorsAndRetirement catches writes/searches against an
// unknown or retired generation and accepts neither vectors nor queries whose
// dimensions disagree with the generation.
func TestPersonBackend_ErrorsAndRetirement(t *testing.T) {
	b, ctx, _ := newPersonBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err)

	err = b.UpsertPersons(ctx, vector.GenerationID(999), []vector.PersonEmbedding{{
		PersonID: 1, Revision: "rev", Vector: unitVec(4, 0),
	}})
	require.ErrorIs(t, err, vector.ErrUnknownGeneration)
	err = b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 1, Revision: "rev", Vector: unitVec(3, 0),
	}})
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)
	_, err = b.SearchPeople(ctx, gen, unitVec(3, 0), 1)
	require.ErrorIs(t, err, vector.ErrDimensionMismatch)

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 1, Revision: "rev", Vector: unitVec(4, 0),
	}}))
	require.NoError(t, b.RetireGeneration(ctx, gen, false))
	err = b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 2, Revision: "rev", Vector: unitVec(4, 0),
	}})
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
	_, err = b.SearchPeople(ctx, gen, unitVec(4, 0), 1)
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
	_, err = b.ListPersonRevisions(ctx, gen)
	require.ErrorIs(t, err, vector.ErrGenerationRetired)

	var count int
	require.NoError(t, b.db.QueryRow(`SELECT COUNT(*) FROM person_embeddings WHERE generation_id = $1`, int64(gen)).Scan(&count))
	assert.Zero(t, count, "pgvector retirement should remove rows from its shared ANN graph")
}

// TestPersonBackend_AutomaticRetirementDeletesPersonRows catches the distinct
// activation path forgetting to remove the demoted generation's person rows
// from pgvector's dimension-shared HNSW graph.
func TestPersonBackend_AutomaticRetirementDeletesPersonRows(t *testing.T) {
	b, ctx, db := newPersonBackendForTest(t)
	serving, err := b.CreateGeneration(ctx, "m", 4, "serving")
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, serving, []vector.PersonEmbedding{{
		PersonID: 10, Revision: "serving-rev", Vector: unitVec(4, 0),
	}}))
	require.NoError(t, b.ActivateGeneration(ctx, serving, true))

	replacement, err := b.CreateGeneration(ctx, "m", 4, "replacement")
	require.NoError(t, err)
	require.NoError(t, b.UpsertPersons(ctx, replacement, []vector.PersonEmbedding{{
		PersonID: 20, Revision: "replacement-rev", Vector: unitVec(4, 1),
	}}))
	require.NoError(t, b.ActivateGeneration(ctx, replacement, true))

	var retiredCount, replacementCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM person_embeddings WHERE generation_id = $1`, int64(serving)).Scan(&retiredCount))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM person_embeddings WHERE generation_id = $1`, int64(replacement)).Scan(&replacementCount))
	assert.Zero(t, retiredCount, "automatic retirement must clean the demoted person corpus")
	assert.Equal(t, 1, replacementCount, "activation must preserve the promoted person corpus")
}

// TestPersonBackend_MessageIDCollisionIsIsolated catches any implementation
// that stores a person ID in message-owned embeddings or deletes one corpus
// while replacing/reconciling the other.
func TestPersonBackend_MessageIDCollisionIsIsolated(t *testing.T) {
	b, ctx, db := newPersonBackendForTest(t)
	gen := seedAndEmbed(t, b, db, map[int64][]float32{1: unitVec(4, 0)})

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 1, Revision: "person-rev", Vector: unitVec(4, 1),
	}}))
	messageHits, err := b.Search(ctx, gen, unitVec(4, 0), 1, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, messageHits, 1)
	assert.Equal(t, int64(1), messageHits[0].MessageID)
	personHits, err := b.SearchPeople(ctx, gen, unitVec(4, 1), 1)
	require.NoError(t, err)
	require.Len(t, personHits, 1)
	assert.Equal(t, int64(1), personHits[0].PersonID)

	require.NoError(t, b.DeletePersonsNotIn(ctx, gen, nil))
	messageHits, err = b.Search(ctx, gen, unitVec(4, 0), 1, vector.Filter{})
	require.NoError(t, err)
	require.Len(t, messageHits, 1)
	assert.Equal(t, int64(1), messageHits[0].MessageID)

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 1, Revision: "person-rev-2", Vector: unitVec(4, 2),
	}}))
	require.NoError(t, b.Delete(ctx, gen, []int64{1}))
	personHits, err = b.SearchPeople(ctx, gen, unitVec(4, 2), 1)
	require.NoError(t, err)
	require.Len(t, personHits, 1)
	assert.Equal(t, "person-rev-2", personHits[0].Revision)
}

func newPersonBackendForTest(t *testing.T) (*Backend, context.Context, *sql.DB) {
	t.Helper()
	return newBackendForTest(t)
}

// seedLegacyPersonlessVectorSchema installs the last vector schema shape that
// existed before person_embeddings. Migrate then has to upgrade real existing
// generation/message-vector tables rather than initialize an empty schema.
func seedLegacyPersonlessVectorSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE TABLE index_generations (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			model TEXT NOT NULL,
			dimension INTEGER NOT NULL,
			fingerprint TEXT NOT NULL,
			started_at BIGINT NOT NULL,
			seeded_at BIGINT,
			completed_at BIGINT,
			activated_at BIGINT,
			state TEXT NOT NULL,
			message_count BIGINT NOT NULL DEFAULT 0
		);
		CREATE TABLE embeddings (
			generation_id BIGINT NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
			message_id BIGINT NOT NULL,
			chunk_index INTEGER NOT NULL DEFAULT 0,
			embedded_at BIGINT NOT NULL,
			source_char_len INTEGER NOT NULL,
			chunk_char_start INTEGER NOT NULL DEFAULT 0,
			chunk_char_end INTEGER NOT NULL DEFAULT 0,
			source_basis SMALLINT NOT NULL DEFAULT 0,
			truncated BOOLEAN NOT NULL DEFAULT FALSE,
			dimension INTEGER NOT NULL,
			embedding vector NOT NULL,
			PRIMARY KEY (generation_id, message_id, chunk_index)
		);
		INSERT INTO index_generations
			(model, dimension, fingerprint, started_at, seeded_at, state, message_count)
		VALUES ('legacy-model', 4, 'legacy:4', 1, 1, 'active', 1);
		INSERT INTO embeddings
			(generation_id, message_id, chunk_index, embedded_at, source_char_len,
			 chunk_char_start, chunk_char_end, source_basis, truncated, dimension, embedding)
		VALUES (1, 1, 0, 1, 1, 0, 1, 0, FALSE, 4, '[1,0,0,0]');
	`)
	require.NoError(t, err, "seed pre-person vector schema")
}
