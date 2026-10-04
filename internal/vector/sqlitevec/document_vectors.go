//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector/document"
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
	if err := document.ValidatePut(generationID, dimension, embeddings); err != nil {
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
	if err := document.ValidateTokens(generationID, tokens); err != nil {
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
	if err := document.ValidateSearch(generationID, dimension, query, k); err != nil {
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
	//nolint:rowserrcheck // ReadHitPage owns rows and checks Err.
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return document.HitPage{}, fmt.Errorf("search document vectors: %w", err)
	}
	return document.ReadHitPage(rows, k, afterRank)
}
