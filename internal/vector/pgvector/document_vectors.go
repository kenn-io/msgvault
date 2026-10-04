//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector/document"
)

// DocumentBackend is the independent attachment-document vector store. It
// borrows its parent's pool and does not own or close it.
type DocumentBackend struct {
	db *sql.DB
}

var _ document.Backend = (*DocumentBackend)(nil)

// DocumentBackend returns a non-owning document-vector view of this backend.
func (b *Backend) DocumentBackend() *DocumentBackend {
	return &DocumentBackend{db: b.db}
}

// DocumentBackendForDB returns a non-owning document-vector backend for an
// already initialized PostgreSQL database. It is used by cleanup operations
// that must not initialize an embedding provider or rerun backend migrations.
func DocumentBackendForDB(db *sql.DB) (*DocumentBackend, error) {
	if db == nil {
		return nil, errors.New("pgvector document backend database is required")
	}
	return &DocumentBackend{db: db}, nil
}

func (b *DocumentBackend) PutUnpublished(ctx context.Context, generationID document.GenerationID, dimension int, embeddings []document.Embedding) error {
	if err := document.ValidatePut(generationID, dimension, embeddings); err != nil {
		return err
	}
	if len(embeddings) == 0 {
		return nil
	}
	if err := EnsureDocumentVectorIndex(ctx, b.db, dimension); err != nil {
		return err
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document vector put: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Establish a lockable generation row before inspecting its dimension.
	// For the first concurrent writers, PostgreSQL's unique-index conflict
	// serializes the INSERTs: the loser waits for the winner, then locks and
	// observes the committed authority row. The authority insert and all token
	// writes share this transaction, so a later batch failure rolls everything
	// back and lets the next writer establish the generation cleanly.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO document_vector_backend_generations (generation_id, dimension)
		VALUES ($1, $2)
		ON CONFLICT (generation_id) DO NOTHING`, int64(generationID), dimension); err != nil {
		return fmt.Errorf("establish document vector generation: %w", err)
	}
	var authoritativeDimension int
	if err := tx.QueryRowContext(ctx, `
		SELECT dimension FROM document_vector_backend_generations
		WHERE generation_id = $1 FOR UPDATE`, int64(generationID)).Scan(&authoritativeDimension); err != nil {
		return fmt.Errorf("lock document vector generation: %w", err)
	}
	if authoritativeDimension != dimension {
		return fmt.Errorf("%w: generation %d already uses dimension %d, got %d",
			document.ErrInvalidVector, generationID, authoritativeDimension, dimension)
	}

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO document_vector_embeddings (token, generation_id, dimension, embedding)
		VALUES ($1, $2, $3, $4::vector)
		ON CONFLICT (token) DO UPDATE SET
			dimension = excluded.dimension,
			embedding = excluded.embedding
		WHERE document_vector_embeddings.generation_id = excluded.generation_id
		RETURNING generation_id`)
	if err != nil {
		return fmt.Errorf("prepare document vector put: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, embedding := range embeddings {
		var storedGeneration int64
		err := stmt.QueryRowContext(ctx, embedding.Token, int64(generationID), dimension,
			vectorLiteral(embedding.Vector)).Scan(&storedGeneration)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: token %q belongs to another generation",
				document.ErrInvalidVector, embedding.Token)
		}
		if err != nil {
			return fmt.Errorf("put document vector %q: %w", embedding.Token, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit document vector put: %w", err)
	}
	return nil
}

func (b *DocumentBackend) DeleteTokens(ctx context.Context, generationID document.GenerationID, tokens []string) error {
	if err := document.ValidateTokens(generationID, tokens); err != nil {
		return err
	}
	if len(tokens) == 0 {
		return nil
	}
	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document vector delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM document_vector_embeddings
		WHERE generation_id = $1 AND token = ANY($2::text[])`, int64(generationID), textArray(unique)); err != nil {
		return fmt.Errorf("delete document vectors: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit document vector delete: %w", err)
	}
	return nil
}

func (b *DocumentBackend) Search(ctx context.Context, generationID document.GenerationID, dimension int, query []float32, k int) ([]document.Hit, error) {
	page, err := b.SearchPage(ctx, generationID, dimension, query, "", k)
	return page.Hits, err
}

func (b *DocumentBackend) SearchPage(ctx context.Context, generationID document.GenerationID, dimension int, query []float32, cursor string, k int) (document.HitPage, error) {
	if err := document.ValidateSearch(generationID, dimension, query, k); err != nil {
		return document.HitPage{}, err
	}
	afterDistance, afterToken, afterRank, err := document.DecodePageCursor(cursor)
	if err != nil {
		return document.HitPage{}, err
	}
	// Scope is resolved after this backend page, so Exhausted must mean the
	// complete generation was consumed. Materialize every generation distance
	// before sorting so PostgreSQL cannot substitute the approximate HNSW order.
	pagePredicate := ""
	limitPlaceholder := "$3"
	args := []any{vectorLiteral(query), int64(generationID), k + 1}
	if cursor != "" {
		pagePredicate = "WHERE distance > $3 OR (distance = $3 AND token > $4)"
		limitPlaceholder = "$5"
		args = []any{vectorLiteral(query), int64(generationID), afterDistance, afterToken, k + 1}
	}
	stmt := fmt.Sprintf(`
		WITH exact AS MATERIALIZED (
			SELECT token, (embedding::vector(%[1]d)) <=> $1::vector AS distance
			FROM document_vector_embeddings
			WHERE generation_id = $2 AND dimension = %[1]d
		)
		SELECT token, 1.0 - distance AS score, distance
		FROM exact
		%[2]s
		ORDER BY distance ASC, token ASC
		LIMIT %[3]s`, dimension, pagePredicate, limitPlaceholder)
	//nolint:rowserrcheck // ReadHitPage owns rows and checks Err.
	rows, err := b.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return document.HitPage{}, fmt.Errorf("search document vectors: %w", err)
	}
	return document.ReadHitPage(rows, k, afterRank)
}
