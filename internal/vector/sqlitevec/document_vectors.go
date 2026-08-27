//go:build sqlite_vec

package sqlitevec

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
	documentVectorBatchLimit = 1000
	documentVectorTokenLimit = 1024
)

// DocumentBackend is the independent attachment-document vector store. It
// borrows its parent's connection and does not own or close it.
type DocumentBackend struct {
	db *sql.DB
}

var _ document.Backend = (*DocumentBackend)(nil)

// DocumentBackend returns a non-owning document-vector view of this backend.
func (b *Backend) DocumentBackend() *DocumentBackend {
	return &DocumentBackend{db: b.db}
}

func (b *DocumentBackend) PutUnpublished(ctx context.Context, generationID document.GenerationID, dimension int, embeddings []document.Embedding) error {
	if err := validateDocumentPut(generationID, dimension, embeddings); err != nil {
		return err
	}
	if len(embeddings) == 0 {
		return nil
	}
	if err := EnsureDocumentVectorTable(ctx, b.db, dimension); err != nil {
		return err
	}

	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document vector put: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingDimension int
	err = tx.QueryRowContext(ctx,
		`SELECT dimension FROM document_vector_embeddings WHERE generation_id = ? LIMIT 1`, int64(generationID)).Scan(&existingDimension)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup document generation dimension: %w", err)
	}
	if err == nil && existingDimension != dimension {
		return fmt.Errorf("%w: generation %d already uses dimension %d, got %d",
			document.ErrInvalidVector, generationID, existingDimension, dimension)
	}

	vecTable := DocumentVectorTableName(dimension)
	for _, embedding := range embeddings {
		var rowID int64
		var existingGeneration int64
		err := tx.QueryRowContext(ctx,
			`SELECT document_vector_id, generation_id FROM document_vector_embeddings WHERE token = ?`, embedding.Token).
			Scan(&rowID, &existingGeneration)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = tx.QueryRowContext(ctx, `
				INSERT INTO document_vector_embeddings (token, generation_id, dimension)
				VALUES (?, ?, ?) RETURNING document_vector_id`,
				embedding.Token, int64(generationID), dimension).Scan(&rowID)
			if err != nil {
				return fmt.Errorf("insert document vector metadata: %w", err)
			}
		case err != nil:
			return fmt.Errorf("lookup document vector token: %w", err)
		case existingGeneration != int64(generationID):
			return fmt.Errorf("%w: token %q belongs to generation %d",
				document.ErrInvalidVector, embedding.Token, existingGeneration)
		default:
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM %s WHERE generation_id = ? AND document_vector_id = ?`, vecTable),
				int64(generationID), rowID); err != nil {
				return fmt.Errorf("delete replaced document vector: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s (generation_id, document_vector_id, embedding)
			VALUES (?, ?, ?)`, vecTable), int64(generationID), rowID, float32SliceBlob(embedding.Vector)); err != nil {
			return fmt.Errorf("insert document vector %q: %w", embedding.Token, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit document vector put: %w", err)
	}
	return nil
}

func (b *DocumentBackend) DeleteTokens(ctx context.Context, generationID document.GenerationID, tokens []string) error {
	if err := validateDocumentTokens(generationID, tokens); err != nil {
		return err
	}
	if len(tokens) == 0 {
		return nil
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin document vector delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		var rowID int64
		var dimension int
		err := tx.QueryRowContext(ctx, `
			SELECT document_vector_id, dimension FROM document_vector_embeddings
			WHERE generation_id = ? AND token = ?`, int64(generationID), token).Scan(&rowID, &dimension)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("lookup document vector for delete: %w", err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			DELETE FROM %s WHERE generation_id = ? AND document_vector_id = ?`, DocumentVectorTableName(dimension)),
			int64(generationID), rowID); err != nil {
			return fmt.Errorf("delete document vector %q: %w", token, err)
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM document_vector_embeddings
			WHERE generation_id = ? AND token = ?`, int64(generationID), token); err != nil {
			return fmt.Errorf("delete document vector metadata %q: %w", token, err)
		}
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
	if err := validateDocumentSearch(generationID, dimension, query, k); err != nil {
		return document.HitPage{}, err
	}
	afterDistance, afterToken, afterRank, err := document.DecodePageCursor(cursor)
	if err != nil {
		return document.HitPage{}, err
	}
	var exists int
	err = b.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, DocumentVectorTableName(dimension)).Scan(&exists)
	if err != nil {
		return document.HitPage{}, fmt.Errorf("check document vector table: %w", err)
	}
	if exists == 0 {
		return document.HitPage{Exhausted: true}, nil
	}
	pagePredicate := ""
	args := []any{float32SliceBlob(query), int64(generationID), int64(generationID), dimension}
	if cursor != "" {
		pagePredicate = "WHERE distance > ? OR (distance = ? AND token > ?)"
		args = append(args, afterDistance, afterDistance, afterToken)
	}
	args = append(args, k+1)
	q := fmt.Sprintf(`
		WITH exact AS (
			SELECT m.token, vec_distance_cosine(v.embedding, ?) AS distance
			FROM %s v
			JOIN document_vector_embeddings m
			  ON m.document_vector_id = v.document_vector_id
			WHERE v.generation_id = ? AND m.generation_id = ? AND m.dimension = ?
		)
		SELECT token, 1.0 - distance AS score, distance
		FROM exact
		%s
		ORDER BY distance ASC, token ASC
		LIMIT ?`, DocumentVectorTableName(dimension), pagePredicate)
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return document.HitPage{}, fmt.Errorf("search document vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]document.Hit, 0, k+1)
	distances := make([]float64, 0, k+1)
	for rows.Next() {
		var hit document.Hit
		var distance float64
		if err := rows.Scan(&hit.Token, &hit.Score, &distance); err != nil {
			return document.HitPage{}, fmt.Errorf("scan document vector hit: %w", err)
		}
		hit.Rank = afterRank + len(hits) + 1
		hits = append(hits, hit)
		distances = append(distances, distance)
	}
	if err := rows.Err(); err != nil {
		return document.HitPage{}, fmt.Errorf("iterate document vector hits: %w", err)
	}
	page := document.HitPage{Exhausted: len(hits) <= k}
	if len(hits) > k {
		page.Hits = hits[:k]
	} else {
		page.Hits = hits
	}
	if !page.Exhausted {
		page.NextCursor, err = document.EncodePageCursor(distances[k-1], hits[k-1].Token, hits[k-1].Rank)
		if err != nil {
			return document.HitPage{}, err
		}
	}
	return page, nil
}

func validateDocumentPut(generationID document.GenerationID, dimension int, embeddings []document.Embedding) error {
	if generationID <= 0 || dimension <= 0 || len(embeddings) > documentVectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or batch bound", document.ErrInvalidVector)
	}
	seen := make(map[string]struct{}, len(embeddings))
	for i, embedding := range embeddings {
		if err := validateDocumentToken(embedding.Token); err != nil {
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
		if err := validateDocumentVector(embedding.Vector); err != nil {
			return fmt.Errorf("token %q: %w", embedding.Token, err)
		}
	}
	return nil
}

func validateDocumentTokens(generationID document.GenerationID, tokens []string) error {
	if generationID <= 0 || len(tokens) > documentVectorBatchLimit {
		return fmt.Errorf("%w: generation or token batch bound", document.ErrInvalidVector)
	}
	for _, token := range tokens {
		if err := validateDocumentToken(token); err != nil {
			return err
		}
	}
	return nil
}

func validateDocumentSearch(generationID document.GenerationID, dimension int, query []float32, k int) error {
	if generationID <= 0 || dimension <= 0 || k <= 0 || k > documentVectorBatchLimit {
		return fmt.Errorf("%w: generation, dimension, or result bound", document.ErrInvalidVector)
	}
	if len(query) != dimension {
		return fmt.Errorf("%w: query has %d dimensions, want %d", vector.ErrDimensionMismatch, len(query), dimension)
	}
	return validateDocumentVector(query)
}

func validateDocumentToken(token string) error {
	if token == "" || len(token) > documentVectorTokenLimit ||
		!utf8.ValidString(token) || strings.ContainsRune(token, 0) {
		return fmt.Errorf("%w: token must contain 1..%d bytes", document.ErrInvalidVector, documentVectorTokenLimit)
	}
	return nil
}

func validateDocumentVector(values []float32) error {
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
