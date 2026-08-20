//go:build pgvector

package pgvector

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const visualDimension = 1024

type VisualBackend struct{ backend *Backend }

var _ visual.Backend = (*VisualBackend)(nil)

func (b *Backend) Visual() *VisualBackend { return &VisualBackend{backend: b} }

func (b *VisualBackend) PutUnpublished(ctx context.Context, token visual.VectorToken, vector []float32) error {
	if strings.TrimSpace(string(token)) == "" || len(vector) != visualDimension || !finiteVisualVector(vector) {
		return errors.New("invalid unpublished visual vector")
	}
	_, err := b.backend.db.ExecContext(ctx, `
		INSERT INTO visual_vectors (vector_token, dimension, embedding, created_at)
		VALUES ($1, $2, $3::vector, $4)
		ON CONFLICT (vector_token) DO UPDATE SET
			dimension = excluded.dimension,
			embedding = excluded.embedding,
			created_at = excluded.created_at`,
		string(token), visualDimension, vectorLiteral(vector), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("store unpublished visual vector: %w", err)
	}
	return nil
}

func (b *VisualBackend) DeleteTokens(ctx context.Context, tokens []visual.VectorToken) error {
	values := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token != "" {
			values = append(values, string(token))
		}
	}
	if len(values) == 0 {
		return nil
	}
	if _, err := b.backend.db.ExecContext(ctx,
		`DELETE FROM visual_vectors WHERE vector_token = ANY($1::text[])`, values); err != nil {
		return fmt.Errorf("delete visual vectors: %w", err)
	}
	return nil
}

func (b *VisualBackend) Search(ctx context.Context, request visual.SearchRequest) ([]visual.Hit, error) {
	if request.GenerationID <= 0 || request.Limit < 1 || request.Limit > 1000 {
		return nil, errors.New("invalid visual search request")
	}
	if len(request.Vector) != visualDimension || !finiteVisualVector(request.Vector) {
		return nil, errors.New("invalid visual query vector")
	}
	query := `
		SELECT vv.vector_token,
		       1 - (vv.embedding::vector(1024) <=> $2::vector(1024)) AS score
		FROM visual_vectors vv
		JOIN visual_publications vp ON vp.current_vector_token = vv.vector_token
		JOIN visual_generations vg ON vg.id = vp.generation_id
		JOIN messages m ON m.id = vp.message_id
		JOIN attachments a ON a.id = vp.representative_attachment_id
		WHERE vp.generation_id = $1 AND vg.state = 'active'
		  AND vp.state = 'current' AND ` + store.LiveMessagesWhere("m", true) + `
		  AND a.message_id = vp.message_id AND a.attachment_role = 'standalone'
		  AND vv.dimension = 1024`
	args := []any{int64(request.GenerationID), vectorLiteral(request.Vector), request.Limit}
	if request.SenderPersonID > 0 {
		query += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id = $%d AND pp.participant_id = m.sender_id)`, len(args)+1)
		args = append(args, request.SenderPersonID)
	}
	if request.SourceID > 0 {
		query += fmt.Sprintf(` AND m.source_id = $%d`, len(args)+1)
		args = append(args, request.SourceID)
	}
	if request.MessageID > 0 {
		query += fmt.Sprintf(` AND m.id = $%d`, len(args)+1)
		args = append(args, request.MessageID)
	}
	if request.Filename != "" {
		query += fmt.Sprintf(` AND LOWER(COALESCE(a.filename, '')) LIKE '%%' || LOWER($%d) || '%%'`, len(args)+1)
		args = append(args, request.Filename)
	}
	if request.MIMEPrefix != "" {
		query += fmt.Sprintf(` AND LOWER(COALESCE(a.mime_type, '')) LIKE LOWER($%d) || '%%'`, len(args)+1)
		args = append(args, request.MIMEPrefix)
	}
	if request.After != nil {
		query += fmt.Sprintf(` AND COALESCE(m.sent_at, m.received_at, m.internal_date) >= $%d`, len(args)+1)
		args = append(args, *request.After)
	}
	if request.Before != nil {
		query += fmt.Sprintf(` AND COALESCE(m.sent_at, m.received_at, m.internal_date) < $%d`, len(args)+1)
		args = append(args, *request.Before)
	}
	if request.AfterScore != nil {
		scoreArg := len(args) + 1
		tokenArg := len(args) + 2
		query += fmt.Sprintf(` AND ((1 - (vv.embedding::vector(1024) <=> $2::vector(1024))) < $%d
			OR ((1 - (vv.embedding::vector(1024) <=> $2::vector(1024))) = $%d AND vv.vector_token > $%d))`,
			scoreArg, scoreArg, tokenArg)
		args = append(args, *request.AfterScore, string(request.AfterToken))
	}
	query += `
		ORDER BY vv.embedding::vector(1024) <=> $2::vector(1024), vv.vector_token
		LIMIT $3`
	rows, err := b.backend.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search visual vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]visual.Hit, 0, request.Limit)
	for rows.Next() {
		var hit visual.Hit
		if err := rows.Scan(&hit.Token, &hit.Score); err != nil {
			return nil, fmt.Errorf("scan visual hit: %w", err)
		}
		hit.Rank = len(hits) + 1
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func (b *VisualBackend) LoadOwnerVector(ctx context.Context, generationID visual.GenerationID, owner visual.Owner) ([]float32, error) {
	var raw string
	err := b.backend.db.QueryRowContext(ctx, `
		SELECT vv.embedding::text
		FROM visual_publications vp
		JOIN visual_vectors vv ON vv.vector_token = vp.current_vector_token
		WHERE vp.generation_id = $1 AND vp.message_id = $2 AND vp.blob_hash = $3
		  AND vp.media_input_key = $4 AND vp.state = 'current'`,
		int64(generationID), owner.MessageID, owner.BlobHash, owner.MediaInputKey).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("load visual owner vector: %w", err)
	}
	return parseVisualVectorLiteral(raw, visualDimension)
}

func parseVisualVectorLiteral(raw string, dimension int) ([]float32, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || raw[0] != '[' || raw[len(raw)-1] != ']' {
		return nil, errors.New("invalid pgvector literal")
	}
	parts := strings.Split(raw[1:len(raw)-1], ",")
	if len(parts) != dimension {
		return nil, fmt.Errorf("pgvector dimension: got %d, want %d", len(parts), dimension)
	}
	result := make([]float32, dimension)
	for index, part := range parts {
		var value float64
		if _, err := fmt.Sscan(strings.TrimSpace(part), &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, errors.New("invalid pgvector value")
		}
		result[index] = float32(value)
	}
	return result, nil
}

func finiteVisualVector(vector []float32) bool {
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}
