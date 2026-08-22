//go:build pgvector

package pgvector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
)

const (
	pgDocumentVectorBatchLimit = 1000
	pgDocumentVectorTokenLimit = 1024
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

func (b *DocumentBackend) PutUnpublished(ctx context.Context, generationID document.GenerationID, dimension int, embeddings []document.Embedding) error {
	if err := validatePGDocumentPut(generationID, dimension, embeddings); err != nil {
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
	if err := validatePGDocumentTokens(generationID, tokens); err != nil {
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
	if err := validatePGDocumentSearch(generationID, dimension, query, k); err != nil {
		return nil, err
	}
	stmt := fmt.Sprintf(`
		SELECT token, 1.0 - ((embedding::vector(%[1]d)) <=> $1::vector) AS score
		FROM document_vector_embeddings
		WHERE generation_id = $2 AND dimension = %[1]d
		ORDER BY (embedding::vector(%[1]d)) <=> $1::vector, token ASC
		LIMIT $3`, dimension)
	rows, err := b.db.QueryContext(ctx, stmt, vectorLiteral(query), int64(generationID), k)
	if err != nil {
		return nil, fmt.Errorf("search document vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]document.Hit, 0, k)
	for rows.Next() {
		var hit document.Hit
		if err := rows.Scan(&hit.Token, &hit.Score); err != nil {
			return nil, fmt.Errorf("scan document vector hit: %w", err)
		}
		hit.Rank = len(hits) + 1
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate document vector hits: %w", err)
	}
	return hits, nil
}

func validatePGDocumentPut(generationID document.GenerationID, dimension int, embeddings []document.Embedding) error {
	if generationID <= 0 || dimension <= 0 || len(embeddings) > pgDocumentVectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or batch bound", document.ErrInvalidVector)
	}
	seen := make(map[string]struct{}, len(embeddings))
	for i, embedding := range embeddings {
		if err := validatePGDocumentToken(embedding.Token); err != nil {
			return fmt.Errorf("embedding %d: %w", i, err)
		}
		if _, ok := seen[embedding.Token]; ok {
			return fmt.Errorf("%w: duplicate token %q", document.ErrInvalidVector, embedding.Token)
		}
		seen[embedding.Token] = struct{}{}
		if len(embedding.Vector) != dimension {
			return fmt.Errorf("%w: token %q has %d dimensions, want %d",
				vector.ErrDimensionMismatch, embedding.Token, len(embedding.Vector), dimension)
		}
		if err := validatePGDocumentVector(embedding.Vector); err != nil {
			return fmt.Errorf("token %q: %w", embedding.Token, err)
		}
	}
	return nil
}

func validatePGDocumentTokens(generationID document.GenerationID, tokens []string) error {
	if generationID <= 0 || len(tokens) > pgDocumentVectorBatchLimit {
		return fmt.Errorf("%w: generation or token batch bound", document.ErrInvalidVector)
	}
	for _, token := range tokens {
		if err := validatePGDocumentToken(token); err != nil {
			return err
		}
	}
	return nil
}

func validatePGDocumentSearch(generationID document.GenerationID, dimension int, query []float32, k int) error {
	if generationID <= 0 || dimension <= 0 || k <= 0 || k > pgDocumentVectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or result bound", document.ErrInvalidVector)
	}
	if len(query) != dimension {
		return fmt.Errorf("%w: query has %d dimensions, want %d", vector.ErrDimensionMismatch, len(query), dimension)
	}
	return validatePGDocumentVector(query)
}

func validatePGDocumentToken(token string) error {
	if token == "" || len(token) > pgDocumentVectorTokenLimit ||
		!utf8.ValidString(token) || strings.ContainsRune(token, 0) {
		return fmt.Errorf("%w: token must contain 1..%d bytes", document.ErrInvalidVector, pgDocumentVectorTokenLimit)
	}
	return nil
}

func validatePGDocumentVector(values []float32) error {
	var norm float64
	for _, value := range values {
		f := float64(value)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: non-finite component", document.ErrInvalidVector)
		}
		norm += f * f
	}
	if norm == 0 {
		return fmt.Errorf("%w: zero-norm vector", document.ErrInvalidVector)
	}
	return nil
}
