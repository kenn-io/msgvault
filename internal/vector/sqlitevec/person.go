//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

var _ vector.PersonBackend = (*Backend)(nil)

const personEmbeddingsSchema = `
CREATE TABLE IF NOT EXISTS person_embeddings (
	embedding_id       INTEGER PRIMARY KEY AUTOINCREMENT,
	generation_id      INTEGER NOT NULL REFERENCES index_generations(id) ON DELETE CASCADE,
	person_id          INTEGER NOT NULL,
	published_revision TEXT NOT NULL,
	dimension          INTEGER NOT NULL,
	embedded_at        INTEGER NOT NULL,
	UNIQUE (generation_id, person_id)
);
`

// migratePersonEmbeddings is the explicit upgrade path for vector databases
// created before the person corpus existed. Fresh databases receive the same
// definition from schema.sql.
func migratePersonEmbeddings(ctx context.Context, db *sql.DB) error {
	exists, err := tableExists(ctx, db, "index_generations")
	if err != nil || !exists {
		return err
	}
	if _, err := db.ExecContext(ctx, personEmbeddingsSchema); err != nil {
		return fmt.Errorf("create person embeddings metadata: %w", err)
	}
	return migratePersonVectorMetrics(ctx, db)
}

// migratePersonVectorMetrics replaces person vec0 tables created before the
// corpus standardized on cosine distance. Their metadata is discarded with
// the incompatible vectors so the next worker pass re-embeds those people.
func migratePersonVectorMetrics(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `
		SELECT name, sql
		  FROM sqlite_master
		 WHERE type = 'table'
		   AND name LIKE 'person_vectors_vec_d%'
		   AND sql LIKE '%USING vec0%'`)
	if err != nil {
		return fmt.Errorf("list person vector tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type legacyTable struct {
		name      string
		dimension int
	}
	legacy := make([]legacyTable, 0)
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			return fmt.Errorf("scan person vector table: %w", err)
		}
		normalizedDDL := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(strings.ToLower(ddl))
		if strings.Contains(normalizedDDL, "distance_metric=cosine") {
			continue
		}
		dimension, err := strconv.Atoi(strings.TrimPrefix(name, "person_vectors_vec_d"))
		if err != nil || dimension <= 0 || PersonVectorTableName(dimension) != name {
			return fmt.Errorf("invalid person vector table name %q", name)
		}
		legacy = append(legacy, legacyTable{name: name, dimension: dimension})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate person vector tables: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close person vector tables: %w", err)
	}
	if len(legacy) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person vector metric migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range legacy {
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+table.name); err != nil {
			return fmt.Errorf("drop legacy person vector table %s: %w", table.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM person_embeddings WHERE dimension = ?`, table.dimension); err != nil {
			return fmt.Errorf("clear legacy person vector metadata for dimension %d: %w", table.dimension, err)
		}
		if _, err := tx.ExecContext(ctx, personVectorTableDDL(table.dimension)); err != nil {
			return fmt.Errorf("recreate cosine person vector table %s: %w", table.name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit person vector metric migration: %w", err)
	}
	return nil
}

// EnsurePersonVectorTable creates the person-owned vec0 table for dim. Its
// synthetic embedding_id key permits the same person to coexist in multiple
// generations without colliding in vec0's global rowid namespace.
func EnsurePersonVectorTable(ctx context.Context, db *sql.DB, dim int) error {
	if dim <= 0 {
		return fmt.Errorf("invalid dimension %d", dim)
	}
	name := PersonVectorTableName(dim)
	if _, err := db.ExecContext(ctx, personVectorTableDDL(dim)); err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	return nil
}

func personVectorTableDDL(dim int) string {
	return fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS %s USING vec0(
		generation_id INTEGER PARTITION KEY,
		embedding_id  INTEGER PRIMARY KEY,
		embedding     FLOAT[%d] distance_metric=cosine
	)`, PersonVectorTableName(dim), dim)
}

// PersonVectorTableName returns the dimension-specific person vec0 table.
func PersonVectorTableName(dim int) string {
	return fmt.Sprintf("person_vectors_vec_d%d", dim)
}

// UpsertPersons atomically replaces each supplied person's published
// revision and vector without touching message-owned embeddings.
func (b *Backend) UpsertPersons(ctx context.Context, gen vector.GenerationID, persons []vector.PersonEmbedding) error {
	if len(persons) == 0 {
		return nil
	}
	dim, err := personGeneration(ctx, b.db, gen)
	if err != nil {
		return err
	}
	if err := EnsurePersonVectorTable(ctx, b.db, dim); err != nil {
		return err
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	dim, err = personGeneration(ctx, tx, gen)
	if err != nil {
		return err
	}

	ids := make([]int64, 0, len(persons))
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
		ids = append(ids, person.PersonID)
	}

	if err := deleteSQLitePersons(ctx, tx, PersonVectorTableName(dim), gen, ids, false); err != nil {
		return err
	}
	metadata, err := tx.PrepareContext(ctx, `
		INSERT INTO person_embeddings
			(generation_id, person_id, published_revision, dimension, embedded_at)
		VALUES (?, ?, ?, ?, ?)
		RETURNING embedding_id`)
	if err != nil {
		return fmt.Errorf("prepare person metadata insert: %w", err)
	}
	defer func() { _ = metadata.Close() }()
	vecTable := PersonVectorTableName(dim)
	vecInsert, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (generation_id, embedding_id, embedding) VALUES (?, ?, ?)`, vecTable))
	if err != nil {
		return fmt.Errorf("prepare person vector insert: %w", err)
	}
	defer func() { _ = vecInsert.Close() }()

	now := time.Now().Unix()
	for _, person := range persons {
		var embeddingID int64
		if err := metadata.QueryRowContext(ctx, int64(gen), person.PersonID, person.Revision, dim, now).Scan(&embeddingID); err != nil {
			return fmt.Errorf("insert person %d metadata: %w", person.PersonID, err)
		}
		if len(person.Vector) == 0 {
			continue
		}
		if _, err := vecInsert.ExecContext(ctx, int64(gen), embeddingID, float32SliceBlob(person.Vector)); err != nil {
			return fmt.Errorf("insert person %d vector: %w", person.PersonID, err)
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
	if _, err := personGeneration(ctx, b.db, gen); err != nil {
		return nil, err
	}
	rows, err := b.db.QueryContext(ctx, `
		SELECT person_id, published_revision
		  FROM person_embeddings
		 WHERE generation_id = ?
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

// CountRejectedPersons reports terminal person revisions that have metadata
// but no searchable vector for the requested generation.
func (b *Backend) CountRejectedPersons(ctx context.Context, gen vector.GenerationID) (int64, error) {
	dim, err := personGeneration(ctx, b.db, gen)
	if err != nil {
		return 0, err
	}
	if err := EnsurePersonVectorTable(ctx, b.db, dim); err != nil {
		return 0, err
	}
	query := fmt.Sprintf(`
		SELECT COUNT(*)
		  FROM person_embeddings AS p
		  LEFT JOIN %s AS v
		    ON v.generation_id = p.generation_id
		   AND v.embedding_id = p.embedding_id
		 WHERE p.generation_id = ?
		   AND v.embedding_id IS NULL`, PersonVectorTableName(dim))
	var count int64
	if err := b.db.QueryRowContext(ctx, query, int64(gen)).Scan(&count); err != nil {
		return 0, fmt.Errorf("count rejected persons for generation %d: %w", gen, err)
	}
	return count, nil
}

// DeletePersonsNotIn reconciles gen to the exact current source person IDs.
func (b *Backend) DeletePersonsNotIn(ctx context.Context, gen vector.GenerationID, currentPersonIDs []int64) error {
	dim, err := personGeneration(ctx, b.db, gen)
	if err != nil {
		return err
	}
	if err := EnsurePersonVectorTable(ctx, b.db, dim); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin person reconcile tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := personGeneration(ctx, tx, gen); err != nil {
		return err
	}
	if err := deleteSQLitePersons(ctx, tx, PersonVectorTableName(dim), gen, currentPersonIDs, true); err != nil {
		return err
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
	dim, err := personGeneration(ctx, b.db, gen)
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
	var count int
	if err := b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM person_embeddings WHERE generation_id = ?`, int64(gen)).Scan(&count); err != nil {
		return nil, fmt.Errorf("count person embeddings: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(`
		SELECT p.person_id, p.published_revision, v.distance
		  FROM %s v
		  JOIN person_embeddings p ON p.embedding_id = v.embedding_id
		 WHERE v.generation_id = ?
		   AND v.embedding MATCH ?
		   AND k = ?
		 ORDER BY v.distance, p.person_id`, PersonVectorTableName(dim))
	rows, err := b.db.QueryContext(ctx, q, int64(gen), float32SliceBlob(queryVec), min(k, count))
	if err != nil {
		return nil, fmt.Errorf("search person vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]vector.PersonHit, 0, min(k, count))
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

type personGenerationQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func personGeneration(ctx context.Context, q personGenerationQueryer, gen vector.GenerationID) (int, error) {
	var dim int
	var state vector.GenerationState
	err := q.QueryRowContext(ctx,
		`SELECT dimension, state FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dim, &state)
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

func deleteSQLitePersons(ctx context.Context, tx *sql.Tx, vecTable string, gen vector.GenerationID, ids []int64, invert bool) error {
	if ids == nil {
		ids = []int64{}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("encode person ids: %w", err)
	}
	comparison := "IN"
	if invert {
		comparison = "NOT IN"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM %s
		 WHERE generation_id = ?
		   AND embedding_id IN (
			SELECT embedding_id FROM person_embeddings
			 WHERE generation_id = ?
			   AND person_id %s (SELECT value FROM json_each(?))
		   )`, vecTable, comparison), int64(gen), int64(gen), string(encoded)); err != nil {
		return fmt.Errorf("delete person vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM person_embeddings
		 WHERE generation_id = ?
		   AND person_id %s (SELECT value FROM json_each(?))`, comparison),
		int64(gen), string(encoded)); err != nil {
		return fmt.Errorf("delete person metadata: %w", err)
	}
	return nil
}
