//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
)

const visualDimension = 1024

// VisualBackend stores opaque multimodal vectors in vectors.db and resolves
// their reachability through the authoritative archive database.
type VisualBackend struct{ backend *Backend }

var _ visual.Backend = (*VisualBackend)(nil)

func (b *Backend) Visual() *VisualBackend { return &VisualBackend{backend: b} }

func (b *VisualBackend) PutUnpublished(ctx context.Context, token visual.VectorToken, vector []float32) error {
	if strings.TrimSpace(string(token)) == "" {
		return errors.New("visual vector token is required")
	}
	if len(vector) != visualDimension {
		return fmt.Errorf("visual vector dimension: got %d, want %d", len(vector), visualDimension)
	}
	if !finiteVisualVector(vector) {
		return errors.New("visual vector contains non-finite values")
	}
	tx, err := b.backend.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var vectorID int64
	err = tx.QueryRowContext(ctx, `SELECT vector_id FROM visual_vectors WHERE vector_token = ?`, string(token)).Scan(&vectorID)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO visual_vectors (vector_token, dimension, created_at) VALUES (?, ?, ?)`,
			string(token), visualDimension, time.Now().Unix())
		if insertErr != nil {
			return fmt.Errorf("store visual vector metadata: %w", insertErr)
		}
		vectorID, err = result.LastInsertId()
	}
	if err != nil {
		return fmt.Errorf("resolve visual vector row: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM visual_vectors_vec WHERE rowid = ?`, vectorID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO visual_vectors_vec(rowid, embedding) VALUES (?, ?)`, vectorID, float32SliceBlob(vector)); err != nil {
		return fmt.Errorf("store visual vector index: %w", err)
	}
	return tx.Commit()
}

func (b *VisualBackend) DeleteTokens(ctx context.Context, tokens []visual.VectorToken) error {
	// Bounded batches: retiring a large generation hands every token to one
	// call, and a single IN list would exceed SQLite's host-parameter limit
	// and leave cleanup permanently stuck.
	const deleteTokenBatch = 500
	filtered := make([]visual.VectorToken, 0, len(tokens))
	for _, token := range tokens {
		if token != "" {
			filtered = append(filtered, token)
		}
	}
	for start := 0; start < len(filtered); start += deleteTokenBatch {
		batch := filtered[start:min(start+deleteTokenBatch, len(filtered))]
		if err := b.deleteTokenBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (b *VisualBackend) deleteTokenBatch(ctx context.Context, tokens []visual.VectorToken) error {
	placeholders := make([]string, 0, len(tokens))
	args := make([]any, 0, len(tokens))
	for _, token := range tokens {
		placeholders = append(placeholders, "?")
		args = append(args, string(token))
	}
	rows, err := b.backend.db.QueryContext(ctx, `SELECT vector_id FROM visual_vectors WHERE vector_token IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate visual vector metadata: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close visual vector metadata: %w", err)
	}
	tx, err := b.backend.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM visual_vectors_vec WHERE rowid = ?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM visual_vectors WHERE vector_token IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		return fmt.Errorf("delete visual vectors: %w", err)
	}
	return tx.Commit()
}

func (b *VisualBackend) Search(ctx context.Context, request visual.SearchRequest) ([]visual.Hit, error) {
	if request.GenerationID <= 0 || request.Limit < 1 || request.Limit > 1000 {
		return nil, errors.New("invalid visual search request")
	}
	if len(request.Vector) != visualDimension || !finiteVisualVector(request.Vector) {
		return nil, fmt.Errorf("visual query vector dimension: got %d, want %d", len(request.Vector), visualDimension)
	}
	if b.backend.mainDB == nil {
		return nil, errors.New("visual search requires the authoritative archive database")
	}
	live, err := sqliteLiveVisualTokens(ctx, b.backend.mainDB, request)
	if err != nil || len(live) == 0 {
		return nil, err
	}
	var count int
	if err := b.backend.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM visual_vectors WHERE dimension = ?`, visualDimension).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	// The vec0 store also holds obsolete and retired vectors awaiting
	// cleanup, so eligible results are filtered against the live token set.
	// k grows progressively instead of streaming the whole store: most
	// searches finish in the first bounded batch, and only a query landing
	// in a cluster of dead vectors pays for a deeper pass.
	hits := make([]visual.Hit, 0, min(request.Limit, len(live)))
	for k := max(request.Limit*4, 64); ; k *= 4 {
		k = min(k, count)
		hits = hits[:0]
		if err := b.collectLiveHits(ctx, request, live, k, &hits); err != nil {
			return nil, err
		}
		if len(hits) >= request.Limit || k == count {
			break
		}
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].Token < hits[j].Token
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > request.Limit {
		hits = hits[:request.Limit]
	}
	for i := range hits {
		hits[i].Rank = i + 1
	}
	return hits, nil
}

// collectLiveHits appends the eligible hits among the k nearest stored
// vectors, filtering obsolete tokens and applying score-cursor pagination.
func (b *VisualBackend) collectLiveHits(
	ctx context.Context,
	request visual.SearchRequest,
	live map[string]struct{},
	k int,
	hits *[]visual.Hit,
) error {
	rows, err := b.backend.db.QueryContext(ctx, `
		SELECT vv.vector_token, v.distance
		FROM visual_vectors_vec v JOIN visual_vectors vv ON vv.vector_id = v.rowid
		WHERE v.embedding MATCH ? AND k = ?
		ORDER BY v.distance, vv.vector_token`, float32SliceBlob(request.Vector), k)
	if err != nil {
		return fmt.Errorf("scan visual vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var token string
		var distance float64
		if err := rows.Scan(&token, &distance); err != nil {
			return fmt.Errorf("scan visual vector: %w", err)
		}
		if _, ok := live[token]; !ok {
			continue
		}
		score := 1 - distance
		if request.AfterScore != nil && (score > *request.AfterScore ||
			(score == *request.AfterScore && visual.VectorToken(token) <= request.AfterToken)) {
			continue
		}
		*hits = append(*hits, visual.Hit{Token: visual.VectorToken(token), Score: score})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate visual vectors: %w", err)
	}
	return nil
}

func (b *VisualBackend) LoadOwnerVector(ctx context.Context, generationID visual.GenerationID, owner visual.Owner) ([]float32, error) {
	if b.backend.mainDB == nil {
		return nil, errors.New("visual owner lookup requires the authoritative archive database")
	}
	var token string
	err := b.backend.mainDB.QueryRowContext(ctx, `
		SELECT current_vector_token FROM visual_publications
		WHERE generation_id = ? AND message_id = ? AND blob_hash = ?
		  AND media_input_key = ? AND state = 'current'
		  AND current_vector_token IS NOT NULL`,
		int64(generationID), owner.MessageID, owner.BlobHash, owner.MediaInputKey).Scan(&token)
	if err != nil {
		return nil, fmt.Errorf("resolve visual owner vector: %w", err)
	}
	var raw []byte
	if err := b.backend.db.QueryRowContext(ctx,
		`SELECT v.embedding FROM visual_vectors vv JOIN visual_vectors_vec v ON v.rowid = vv.vector_id WHERE vv.vector_token = ? AND vv.dimension = ?`,
		token, visualDimension).Scan(&raw); err != nil {
		return nil, fmt.Errorf("load visual owner vector: %w", err)
	}
	return blobToFloat32(raw, visualDimension)
}

func sqliteLiveVisualTokens(ctx context.Context, db *sql.DB, request visual.SearchRequest) (map[string]struct{}, error) {
	query := `
		SELECT vp.current_vector_token
		FROM visual_publications vp
		JOIN visual_generations vg ON vg.id = vp.generation_id
		JOIN messages m ON m.id = vp.message_id
		JOIN attachments a ON a.id = vp.representative_attachment_id
		WHERE vp.generation_id = ? AND vg.state = 'active'
		  AND vp.state = 'current' AND vp.current_vector_token IS NOT NULL
		  AND ` + store.LiveMessagesWhere("m", true) + `
		  AND a.message_id = vp.message_id
		  AND a.attachment_role = 'standalone'`
	args := []any{int64(request.GenerationID)}
	if request.SenderPersonID > 0 {
		// Legacy imports leave sender_id NULL and record the sender as a
		// 'from' recipient row; fall back the same way the message list
		// queries do so those results are not silently excluded.
		query += ` AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id = ? AND pp.participant_id = COALESCE(m.sender_id, (
			SELECT mr.participant_id FROM message_recipients mr
			WHERE mr.message_id = m.id AND mr.recipient_type = 'from'
			ORDER BY mr.id LIMIT 1)))`
		args = append(args, request.SenderPersonID)
	}
	if request.SourceID > 0 {
		query += ` AND m.source_id = ?`
		args = append(args, request.SourceID)
	}
	if request.MessageID > 0 {
		query += ` AND m.id = ?`
		args = append(args, request.MessageID)
	}
	if request.Filename != "" {
		query += ` AND LOWER(COALESCE(a.filename, '')) LIKE '%' || LOWER(?) || '%'`
		args = append(args, request.Filename)
	}
	if request.MIMEPrefix != "" {
		query += ` AND LOWER(COALESCE(a.mime_type, '')) LIKE LOWER(?) || '%'`
		args = append(args, request.MIMEPrefix)
	}
	if request.After != nil {
		query += ` AND COALESCE(m.sent_at, m.received_at, m.internal_date) >= ?`
		args = append(args, *request.After)
	}
	if request.Before != nil {
		query += ` AND COALESCE(m.sent_at, m.received_at, m.internal_date) < ?`
		args = append(args, *request.Before)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolve live visual vectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tokens := make(map[string]struct{})
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		tokens[token] = struct{}{}
	}
	return tokens, rows.Err()
}

func finiteVisualVector(vector []float32) bool {
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return false
		}
	}
	return true
}
