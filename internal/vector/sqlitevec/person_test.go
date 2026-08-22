//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vector"
)

// TestPersonMigration_UpgradesAndIsIdempotent catches an upgrade that only
// creates person storage for fresh databases, or a migration that fails when
// startup applies it a second time.
func TestPersonMigration_UpgradesAndIsIdempotent(t *testing.T) {
	require.NoError(t, RegisterExtension())
	db, err := sql.Open(DriverName(), filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = db.Exec(`CREATE TABLE index_generations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model TEXT NOT NULL,
		dimension INTEGER NOT NULL,
		fingerprint TEXT NOT NULL,
		started_at INTEGER NOT NULL,
		completed_at INTEGER,
		activated_at INTEGER,
		state TEXT NOT NULL,
		message_count INTEGER NOT NULL DEFAULT 0
	)`)
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, Migrate(ctx, db, 4))
	require.NoError(t, Migrate(ctx, db, 4))

	for _, table := range []string{"person_embeddings", PersonVectorTableName(4)} {
		exists, err := tableExists(ctx, db, table)
		require.NoError(t, err)
		assert.Truef(t, exists, "table %s should exist after upgrade", table)
	}
}

func TestPersonMigration_RebuildsLegacyL2TableForCosine(t *testing.T) {
	require.NoError(t, RegisterExtension())
	db, err := sql.Open(DriverName(), filepath.Join(t.TempDir(), "vectors.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	require.NoError(t, Migrate(ctx, db, 4))

	_, err = db.ExecContext(ctx, `
		INSERT INTO index_generations
			(id, model, dimension, fingerprint, started_at, state)
		VALUES (1, 'm', 4, 'legacy-l2', 1, 'active')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE person_vectors_vec_d4`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE VIRTUAL TABLE person_vectors_vec_d4 USING vec0(
		generation_id INTEGER PARTITION KEY,
		embedding_id INTEGER PRIMARY KEY,
		embedding FLOAT[4]
	)`)
	require.NoError(t, err)
	var embeddingID int64
	err = db.QueryRowContext(ctx, `
		INSERT INTO person_embeddings
			(generation_id, person_id, published_revision, dimension, embedded_at)
		VALUES (1, 10, 'legacy', 4, 1)
		RETURNING embedding_id`).Scan(&embeddingID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO person_vectors_vec_d4 (generation_id, embedding_id, embedding)
		VALUES (?, ?, ?)`, 1, embeddingID, float32SliceBlob([]float32{2, 0, 0, 0}))
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, db, 7))
	var ddl string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE name = ?`, PersonVectorTableName(4)).Scan(&ddl))
	assert.Contains(t, ddl, "distance_metric=cosine")
	var metadataRows int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_embeddings WHERE dimension = 4`).Scan(&metadataRows))
	assert.Zero(t, metadataRows, "discarded L2 vectors must be marked for re-embedding")
	require.NoError(t, Migrate(ctx, db, 7), "cosine migration must be idempotent")
}

// TestPersonBackend_CreateGenerationCreatesNonDefaultDimensionANN catches a
// generation path that creates only the configured default-dimension person
// vec0 table and leaves a later generation's distinct dimension unavailable.
func TestPersonBackend_CreateGenerationCreatesNonDefaultDimensionANN(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)

	exists, err := tableExists(ctx, b.db, "person_vectors_vec_d7")
	require.NoError(t, err)
	require.False(t, exists, "dimension 7 table must be created lazily")

	gen, err := b.CreateGeneration(ctx, "m", 7, "")
	require.NoError(t, err)
	exists, err = tableExists(ctx, b.db, "person_vectors_vec_d7")
	require.NoError(t, err)
	assert.True(t, exists, "generation dimension must own a person vec0 table")

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 70, Revision: "rev-7", Vector: unitVec(7, 6),
	}}))
	hits, err := b.SearchPeople(ctx, gen, unitVec(7, 6), 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, int64(70), hits[0].PersonID)
}

// TestPersonBackend_PublishSearchReplaceReconcile catches wrong ANN ordering,
// stale revision/vector rows after replacement, and reconciliation that keeps
// people absent from the exact current-ID set.
func TestPersonBackend_PublishSearchReplaceReconcile(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
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

func TestPersonBackend_SearchUsesCosineScores(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
	gen, err := b.CreateGeneration(ctx, "m", 4, "")
	require.NoError(t, err)

	require.NoError(t, b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{
		{PersonID: 10, Revision: "scaled", Vector: []float32{2, 0, 0, 0}},
		{PersonID: 20, Revision: "orthogonal", Vector: []float32{0, 1, 0, 0}},
	}))
	hits, err := b.SearchPeople(ctx, gen, []float32{1, 0, 0, 0}, 2)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, int64(10), hits[0].PersonID)
	assert.InDelta(t, 1, hits[0].Score, 1e-6)
	assert.Equal(t, int64(20), hits[1].PersonID)
	assert.InDelta(t, 0, hits[1].Score, 1e-6)
}

func TestPersonBackend_TerminalRevisionIsCoveredButNotSearchable(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
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

func TestPersonBackend_CoverageAndReconcileRestoreMissingDimensionTable(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Backend, vector.GenerationID) error
	}{
		{name: "coverage", run: func(ctx context.Context, backend *Backend, gen vector.GenerationID) error {
			count, err := backend.CountRejectedPersons(ctx, gen)
			assert.Zero(t, count)
			return err
		}},
		{name: "reconcile", run: func(ctx context.Context, backend *Backend, gen vector.GenerationID) error {
			return backend.DeletePersonsNotIn(ctx, gen, nil)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, ctx := newPersonBackendForTest(t)
			gen, err := backend.CreateGeneration(ctx, "m", 7, test.name)
			require.NoError(t, err)
			_, err = backend.db.ExecContext(ctx, `DROP TABLE `+PersonVectorTableName(7))
			require.NoError(t, err)

			require.NoError(t, test.run(ctx, backend, gen))
			exists, err := tableExists(ctx, backend.db, PersonVectorTableName(7))
			require.NoError(t, err)
			assert.True(t, exists)
		})
	}
}

func TestPersonBackend_SearchCompletenessIsGenerationBounded(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
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

// TestPersonBackend_ErrorsAndRetirement catches writes/searches against an
// unknown or retired generation and accepts neither vectors nor queries whose
// dimensions disagree with the generation.
func TestPersonBackend_ErrorsAndRetirement(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
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
	var retained int
	require.NoError(t, b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&retained))
	assert.Equal(t, 1, retained, "retired SQLite generations retain partitioned person rows")
	err = b.UpsertPersons(ctx, gen, []vector.PersonEmbedding{{
		PersonID: 2, Revision: "rev", Vector: unitVec(4, 0),
	}})
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
	_, err = b.SearchPeople(ctx, gen, unitVec(4, 0), 1)
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
	_, err = b.ListPersonRevisions(ctx, gen)
	require.ErrorIs(t, err, vector.ErrGenerationRetired)
}

// TestPersonBackend_MessageIDCollisionIsIsolated catches any implementation
// that stores a person ID in message-owned embeddings or deletes one corpus
// while replacing/reconciling the other.
func TestPersonBackend_MessageIDCollisionIsIsolated(t *testing.T) {
	b, ctx := newPersonBackendForTest(t)
	gen := seedAndEmbed(t, b, map[int64][]float32{1: unitVec(4, 0)})

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

func newPersonBackendForTest(t *testing.T) (*Backend, context.Context) {
	t.Helper()
	ctx := context.Background()
	mainDB := openMainDBWithOneMessage(t)
	b, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 4,
		MainDB:    mainDB,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	return b, ctx
}
