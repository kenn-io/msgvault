//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.PersonBackend = (*Backend)(nil)

// EnsurePersonVectorIndex creates the dimension-partial person-owned HNSW
// compatibility index. SearchPeople ranks its capped curated corpus exactly,
// but retaining this lifecycle keeps a future ANN path migration-free.
func EnsurePersonVectorIndex(ctx context.Context, db migrateExecer, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("invalid dimension %d", dim)
	}
	stmt := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s
		ON person_embeddings
		USING hnsw ((embedding::vector(%d)) vector_cosine_ops)
		WHERE dimension = %d AND embedding IS NOT NULL`, PersonVectorIndexName(dim), dim, dim)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person hnsw index tx for dim %d: %w", dim, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL statement_timeout = 0"); err != nil {
		return fmt.Errorf("disable statement_timeout for person hnsw index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("create person hnsw index for dim %d: %w", dim, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit person hnsw index for dim %d: %w", dim, err)
	}
	return nil
}

// PersonVectorIndexName returns the dimension-specific person HNSW index.
func PersonVectorIndexName(dim int) string {
	return fmt.Sprintf("idx_person_embeddings_hnsw_d%d", dim)
}

// UpsertPersons atomically replaces each supplied person's published
// revision and vector without touching message-owned embeddings.
func (b *Backend) UpsertPersons(ctx context.Context, gen vector.GenerationID, persons []vector.PersonEmbedding) error {
	if len(persons) == 0 {
		return nil
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	dim, err := personGeneration(ctx, tx, gen, true)
	if err != nil {
		return err
	}
	seen := make(map[int64]struct{}, len(persons))
	for _, person := range persons {
		if len(person.Vector) != 0 && len(person.Vector) != dim {
			return fmt.Errorf("%w: person %d has %d dims, gen has %d",
				vector.ErrDimensionMismatch, person.PersonID, len(person.Vector), dim)
		}
		if _, duplicate := seen[person.PersonID]; duplicate {
			return fmt.Errorf("duplicate person id %d in upsert batch", person.PersonID)
		}
		seen[person.PersonID] = struct{}{}
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO person_embeddings
			(generation_id, person_id, published_revision, dimension, embedded_at, embedding)
		VALUES ($1, $2, $3, $4, $5, $6::vector)
		ON CONFLICT (generation_id, person_id) DO UPDATE SET
			published_revision = excluded.published_revision,
			dimension = excluded.dimension,
			embedded_at = excluded.embedded_at,
			embedding = excluded.embedding`)
	if err != nil {
		return fmt.Errorf("prepare person upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	now := time.Now().Unix()
	for _, person := range persons {
		var embedding any
		if len(person.Vector) > 0 {
			embedding = vectorLiteral(person.Vector)
		}
		if _, err := stmt.ExecContext(ctx, int64(gen), person.PersonID, person.Revision,
			dim, now, embedding); err != nil {
			return fmt.Errorf("upsert person %d: %w", person.PersonID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit person upsert for generation %d: %w", gen, err)
	}
	return nil
}

// ListPersonRevisions returns the exact published revision owned by every
// current person vector in gen.
func (b *Backend) ListPersonRevisions(ctx context.Context, gen vector.GenerationID) (map[int64]string, error) {
	if _, err := personGeneration(ctx, b.db, gen, false); err != nil {
		return nil, err
	}
	rows, err := b.db.QueryContext(ctx, `
		SELECT person_id, published_revision
		  FROM person_embeddings
		 WHERE generation_id = $1
		 ORDER BY person_id`, int64(gen))
	if err != nil {
		return nil, fmt.Errorf("list person revisions for generation %d: %w", gen, err)
	}
	defer func() { _ = rows.Close() }()
	revisions := make(map[int64]string)
	for rows.Next() {
		var id int64
		var revision string
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, fmt.Errorf("scan person revision: %w", err)
		}
		revisions[id] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person revisions: %w", err)
	}
	return revisions, nil
}

// CountRejectedPersons reports terminal person revisions that have no
// searchable vector for the requested generation.
func (b *Backend) CountRejectedPersons(ctx context.Context, gen vector.GenerationID) (int64, error) {
	if _, err := personGeneration(ctx, b.db, gen, false); err != nil {
		return 0, err
	}
	var count int64
	if err := b.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM person_embeddings
		 WHERE generation_id = $1
		   AND embedding IS NULL`, int64(gen)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count rejected persons for generation %d: %w", gen, err)
	}
	return count, nil
}

// DeletePersonsNotIn reconciles gen to the exact current source person IDs.
func (b *Backend) DeletePersonsNotIn(ctx context.Context, gen vector.GenerationID, currentPersonIDs []int64) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person reconcile tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := personGeneration(ctx, tx, gen, true); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM person_embeddings
		 WHERE generation_id = $1
		   AND person_id <> ALL($2::bigint[])`, int64(gen), int64Array(currentPersonIDs)); err != nil {
		return fmt.Errorf("reconcile person embeddings for generation %d: %w", gen, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit person reconcile for generation %d: %w", gen, err)
	}
	return nil
}

// SearchPeople searches only person-owned vectors and returns their published
// revisions for read-time source revalidation.
func (b *Backend) SearchPeople(ctx context.Context, gen vector.GenerationID, queryVec []float32, k int) ([]vector.PersonHit, error) {
	if len(queryVec) == 0 {
		return nil, errors.New("person search: empty query vector")
	}
	dim, err := personGeneration(ctx, b.db, gen, false)
	if err != nil {
		return nil, err
	}
	if len(queryVec) != dim {
		return nil, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(queryVec), dim)
	}
	if k <= 0 {
		return nil, nil
	}
	// Materialize the requested generation before ranking so the bounded
	// curated corpus is ranked exactly. The person HNSW compatibility index is
	// retained for a future ANN path, but correctness does not rely on it.
	q := fmt.Sprintf(`
		WITH generation_candidates AS MATERIALIZED (
			SELECT person_id, published_revision, embedding
			  FROM person_embeddings
			 WHERE generation_id = $2
			   AND dimension = %[1]d
			   AND embedding IS NOT NULL
		)
		SELECT person_id, published_revision,
		       (embedding::vector(%[1]d)) <=> $1::vector AS distance
		  FROM generation_candidates
		 ORDER BY distance, person_id
		 LIMIT $3`, dim)
	rows, err := b.db.QueryContext(ctx, q, vectorLiteral(queryVec), int64(gen), k)
	if err != nil {
		return nil, fmt.Errorf("search person vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]vector.PersonHit, 0, k)
	for rows.Next() {
		var hit vector.PersonHit
		var distance float64
		if err := rows.Scan(&hit.PersonID, &hit.Revision, &distance); err != nil {
			return nil, fmt.Errorf("scan person hit: %w", err)
		}
		hit.Score = 1 - distance
		hit.Rank = len(hits) + 1
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person hits: %w", err)
	}
	return hits, nil
}

func personGeneration(
	ctx context.Context, q rowQueryer, gen vector.GenerationID, lock bool,
) (int, error) {
	query := `SELECT dimension, state FROM index_generations WHERE id = $1`
	if lock {
		query += ` FOR UPDATE`
	}
	var dim int
	var state vector.GenerationState
	err := q.QueryRowContext(ctx, query, int64(gen)).Scan(&dim, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return 0, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if state == vector.GenerationRetired {
		return 0, fmt.Errorf("%w: %d", vector.ErrGenerationRetired, gen)
	}
	return dim, nil
}
