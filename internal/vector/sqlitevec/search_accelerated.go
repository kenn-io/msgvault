//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"

	"go.kenn.io/msgvault/internal/vector"
)

// defaultExactFilterVectorThreshold caps the filtered population served by
// the exact-filter accelerator path. Filtered searches at or below it rerank
// the full population exactly; larger populations run on the ANN accelerator.
// Tests lower b.exactFilterVectorThreshold to reach the ANN path without
// building a 32k-vector corpus.
const defaultExactFilterVectorThreshold = 32_768

var _ vector.MetadataSearchingBackend = (*Backend)(nil)

type acceleratedCandidate struct {
	embeddingID int64
	messageID   int64
}

type acceleratedQueryer interface {
	rowQueryer
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// SearchWithMetadata uses a ready Vec1 accelerator when possible and retains
// searchExact as the authoritative fallback. Accelerator failures fail open to
// exact search, except cancellation, which is returned immediately.
func (b *Backend) SearchWithMetadata(
	ctx context.Context,
	gen vector.GenerationID,
	queryVec []float32,
	k int,
	filter vector.Filter,
) ([]vector.Hit, vector.SearchMetadata, error) {
	hits, metadata, err := b.searchAccelerator(ctx, gen, queryVec, k, filter)
	if err != nil || (metadata.Accelerator != "" && metadata.Accelerator != "exact-fallback") {
		return hits, metadata, err
	}
	path := metadata.Accelerator
	if path == "" {
		path = "exact"
	}
	hits, err = b.searchExact(ctx, gen, queryVec, k, filter)
	return hits, exactSearchMetadata(hits, k, path), err
}

// searchAccelerator leaves exact retrieval to the caller when no accelerator
// is eligible or an attempt fails. Fused search can then use its exact SQL path
// without performing a separate exhaustive vector scan first.
func (b *Backend) searchAccelerator(
	ctx context.Context, gen vector.GenerationID, queryVec []float32, k int, filter vector.Filter,
) ([]vector.Hit, vector.SearchMetadata, error) {
	if err := vector.ValidateFilter(filter); err != nil {
		return nil, vector.SearchMetadata{}, err
	}
	if len(queryVec) == 0 {
		return nil, vector.SearchMetadata{}, errors.New("search: empty query vector")
	}
	if k <= 0 {
		return []vector.Hit{}, vector.SearchMetadata{Accelerator: "exact"}, nil
	}
	var dimension int
	err := b.db.QueryRowContext(ctx, `SELECT dimension FROM index_generations WHERE id = ?`, int64(gen)).Scan(&dimension)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, vector.SearchMetadata{}, fmt.Errorf("%w: %d", vector.ErrUnknownGeneration, gen)
	}
	if err != nil {
		return nil, vector.SearchMetadata{}, fmt.Errorf("lookup generation %d: %w", gen, err)
	}
	if len(queryVec) != dimension {
		return nil, vector.SearchMetadata{}, fmt.Errorf("%w: query has %d dims, gen has %d",
			vector.ErrDimensionMismatch, len(queryVec), dimension)
	}
	if b.acceleratorMode == "exact" {
		return nil, vector.SearchMetadata{}, nil
	}
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		if ctx.Err() != nil {
			return nil, vector.SearchMetadata{}, ctx.Err()
		}
		return acceleratorFallback(gen, err)
	}
	defer func() { _ = tx.Rollback() }()
	accelerator, err := b.readyAcceleratorQuery(ctx, tx, gen, dimension, "")
	if err != nil {
		_ = tx.Rollback()
		if ctx.Err() != nil {
			return nil, vector.SearchMetadata{}, ctx.Err()
		}
		return acceleratorFallback(gen, err)
	}
	if accelerator == nil {
		_ = tx.Rollback()
		return nil, vector.SearchMetadata{}, nil
	}
	if !filter.IsEmpty() {
		threshold := b.exactFilterVectorThreshold
		messageIDs, probeErr := b.filteredMessageIDsLimit(ctx, filter, threshold+1)
		if probeErr != nil {
			if ctx.Err() != nil {
				return nil, vector.SearchMetadata{}, ctx.Err()
			}
			_ = tx.Rollback()
			return acceleratorFallback(gen, probeErr)
		}
		fitsExact, fitErr := b.exactFilterPopulationFits(
			ctx, tx, gen, messageIDs, threshold,
		)
		if fitErr != nil {
			if ctx.Err() != nil {
				return nil, vector.SearchMetadata{}, ctx.Err()
			}
			_ = tx.Rollback()
			return acceleratorFallback(gen, fitErr)
		}
		if fitsExact {
			hits, err := b.searchExactFilteredPopulation(
				ctx, tx, *accelerator, queryVec, k, messageIDs,
			)
			if err == nil {
				if err := tx.Commit(); err != nil {
					return nil, vector.SearchMetadata{}, fmt.Errorf("finish exact-filter accelerator read: %w", err)
				}
				return hits, vector.SearchMetadata{
					PoolSaturated: len(hits) >= k, Accelerator: "exact-filter",
				}, nil
			}
			if ctx.Err() != nil {
				return nil, vector.SearchMetadata{}, ctx.Err()
			}
			_ = tx.Rollback()
			return acceleratorFallback(gen, err)
		}
	}
	if k > b.annWorkCeiling {
		return nil, vector.SearchMetadata{}, nil
	}
	hits, metadata, err := b.searchAccelerated(ctx, tx, *accelerator, queryVec, k, filter)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, vector.SearchMetadata{}, fmt.Errorf("finish accelerated read: %w", err)
		}
		return hits, metadata, nil
	}
	if ctx.Err() != nil {
		return nil, vector.SearchMetadata{}, ctx.Err()
	}
	_ = tx.Rollback()
	return acceleratorFallback(gen, err)
}

func acceleratorFallback(gen vector.GenerationID, cause error) ([]vector.Hit, vector.SearchMetadata, error) {
	slog.Warn("SQLite accelerator failed; using exact search", "generation", gen, "error", cause)
	return nil, vector.SearchMetadata{Accelerator: "exact-fallback"}, nil
}

func exactSearchMetadata(hits []vector.Hit, k int, accelerator string) vector.SearchMetadata {
	return vector.SearchMetadata{PoolSaturated: k > 0 && len(hits) >= k, Accelerator: accelerator}
}

func (b *Backend) searchAccelerated(
	ctx context.Context,
	queryer acceleratedQueryer,
	accelerator AcceleratorStatus,
	queryVec []float32,
	k int,
	filter vector.Filter,
) ([]vector.Hit, vector.SearchMetadata, error) {
	ceiling := min(b.annWorkCeiling, int(accelerator.IndexedCount))
	if ceiling <= 0 {
		return []vector.Hit{}, vector.SearchMetadata{Accelerator: acceleratorKind}, nil
	}
	pool := max(k*b.annOversample, 32)
	pool = min(pool, ceiling)
	maxNProbe := acceleratorNBuckets(accelerator.ModelConfig, b.annNProbe)
	nprobe := min(b.annNProbe, maxNProbe)
	for {
		rowIDs, err := b.acceleratedRowIDs(ctx, queryer, accelerator.TableName, queryVec, pool, nprobe)
		if err != nil {
			return nil, vector.SearchMetadata{}, err
		}
		hits, err := b.rerankAcceleratedCandidates(ctx, queryer, accelerator, queryVec, k, filter, rowIDs)
		if err != nil {
			return nil, vector.SearchMetadata{}, err
		}
		metadata := vector.SearchMetadata{
			Accelerator: acceleratorKind,
		}
		if len(hits) >= k {
			metadata.PoolSaturated = true
			return hits, metadata, nil
		}
		if len(rowIDs) < pool && nprobe >= maxNProbe {
			return hits, metadata, nil
		}
		if pool >= ceiling && nprobe >= maxNProbe {
			metadata.PoolSaturated = ceiling < int(accelerator.IndexedCount) && len(rowIDs) >= pool
			return hits, metadata, nil
		}
		pool = min(pool*2, ceiling)
		nprobe = min(nprobe*2, maxNProbe)
	}
}

func acceleratorNBuckets(modelConfig string, fallback int) int {
	var config acceleratorModelConfig
	if err := json.Unmarshal([]byte(modelConfig), &config); err != nil || config.NBuckets <= 0 {
		return fallback
	}
	return config.NBuckets
}

func (b *Backend) acceleratedRowIDs(
	ctx context.Context,
	queryer acceleratedQueryer,
	tableName string,
	queryVec []float32,
	pool int,
	nprobe int,
) ([]int64, error) {
	parameters, err := json.Marshal(map[string]any{"K": pool, "nprobe": nprobe})
	if err != nil {
		return nil, fmt.Errorf("encode Vec1 query parameters: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT rowid FROM `+tableName+`
		WHERE cmd = ? AND arg = ? ORDER BY distance`, float32SliceBlob(queryVec), string(parameters))
	if err != nil {
		return nil, fmt.Errorf("query Vec1 accelerator: %w", err)
	}
	defer func() { _ = rows.Close() }()
	rowIDs := make([]int64, 0, pool)
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			return nil, fmt.Errorf("scan Vec1 candidate: %w", err)
		}
		rowIDs = append(rowIDs, rowID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Vec1 candidates: %w", err)
	}
	return rowIDs, nil
}

func (b *Backend) exactFilterPopulationFits(
	ctx context.Context,
	queryer rowQueryer,
	generationID vector.GenerationID,
	messageIDs []int64,
	limit int,
) (bool, error) {
	if len(messageIDs) > limit {
		return false, nil
	}
	if len(messageIDs) == 0 {
		return true, nil
	}
	encoded, err := json.Marshal(messageIDs)
	if err != nil {
		return false, fmt.Errorf("encode exact-filter population: %w", err)
	}
	var count int
	err = queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT 1 FROM embeddings
		 WHERE generation_id = ?
		   AND message_id IN (SELECT value FROM json_each(?))
		 LIMIT ?
	)`, int64(generationID), string(encoded), limit+1).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("count exact-filter population: %w", err)
	}
	return count <= limit, nil
}

func (b *Backend) searchExactFilteredPopulation(
	ctx context.Context,
	queryer acceleratedQueryer,
	accelerator AcceleratorStatus,
	queryVec []float32,
	k int,
	messageIDs []int64,
) ([]vector.Hit, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	blob, err := json.Marshal(messageIDs)
	if err != nil {
		return nil, fmt.Errorf("encode exact-filter message IDs: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT e.message_id,
		MIN(vec1_l2_distance(?, a.embedding)) AS distance
		FROM embeddings e
		JOIN `+accelerator.TableName+` a ON a.rowid = e.embedding_id
		WHERE e.generation_id = ?
		  AND e.message_id IN (SELECT value FROM json_each(?))
		GROUP BY e.message_id
		ORDER BY distance ASC, e.message_id ASC
		LIMIT ?`, float32SliceBlob(queryVec), int64(accelerator.GenerationID), string(blob), k)
	if err != nil {
		return nil, fmt.Errorf("query exact-filter population: %w", err)
	}
	defer func() { _ = rows.Close() }()
	hits := make([]vector.Hit, 0, min(k, len(messageIDs)))
	for rows.Next() {
		var hit vector.Hit
		var distance float64
		if err := rows.Scan(&hit.MessageID, &distance); err != nil {
			return nil, fmt.Errorf("scan exact-filter hit: %w", err)
		}
		hit.Score = 1 - math.Sqrt(distance)
		hit.Rank = len(hits) + 1
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exact-filter hits: %w", err)
	}
	return hits, nil
}

func (b *Backend) rerankAcceleratedCandidates(
	ctx context.Context,
	queryer acceleratedQueryer,
	accelerator AcceleratorStatus,
	queryVec []float32,
	k int,
	filter vector.Filter,
	rowIDs []int64,
) ([]vector.Hit, error) {
	if len(rowIDs) == 0 {
		return []vector.Hit{}, nil
	}
	encoded, err := json.Marshal(rowIDs)
	if err != nil {
		return nil, fmt.Errorf("encode accelerator candidate IDs: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT embedding_id, message_id FROM embeddings
		WHERE generation_id = ? AND embedding_id IN (SELECT value FROM json_each(?))`,
		int64(accelerator.GenerationID), string(encoded))
	if err != nil {
		return nil, fmt.Errorf("map accelerator candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]acceleratedCandidate, 0, len(rowIDs))
	messageIDs := make([]int64, 0, len(rowIDs))
	seenMessages := make(map[int64]struct{}, len(rowIDs))
	for rows.Next() {
		var candidate acceleratedCandidate
		if err := rows.Scan(&candidate.embeddingID, &candidate.messageID); err != nil {
			return nil, fmt.Errorf("scan accelerator candidate mapping: %w", err)
		}
		candidates = append(candidates, candidate)
		if _, seen := seenMessages[candidate.messageID]; !seen {
			seenMessages[candidate.messageID] = struct{}{}
			messageIDs = append(messageIDs, candidate.messageID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate accelerator candidate mappings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close accelerator candidate mappings: %w", err)
	}
	boundedFilter := filter
	boundedFilter.MessageIDs = intersectCandidateMessageIDs(messageIDs, filter.MessageIDs)
	if len(boundedFilter.MessageIDs) == 0 {
		return []vector.Hit{}, nil
	}
	liveIDs, err := b.filteredMessageIDs(ctx, boundedFilter)
	if err != nil {
		return nil, err
	}
	live := make(map[int64]struct{}, len(liveIDs))
	for _, messageID := range liveIDs {
		live[messageID] = struct{}{}
	}
	queryBlob := float32SliceBlob(queryVec)
	stmt, err := queryer.PrepareContext(ctx, `SELECT vec1_l2_distance(?, embedding)
		FROM `+accelerator.TableName+` WHERE rowid = ?`)
	if err != nil {
		return nil, fmt.Errorf("prepare accelerator exact rerank: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	best := make(map[int64]float64, len(live))
	for _, candidate := range candidates {
		if _, ok := live[candidate.messageID]; !ok {
			continue
		}
		var distance float64
		if err := stmt.QueryRowContext(ctx, queryBlob, candidate.embeddingID).Scan(&distance); err != nil {
			return nil, fmt.Errorf("rerank accelerator row %d: %w", candidate.embeddingID, err)
		}
		if previous, exists := best[candidate.messageID]; !exists || distance < previous {
			best[candidate.messageID] = distance
		}
	}
	hits := make([]vector.Hit, 0, len(best))
	for messageID, distance := range best {
		hits = append(hits, vector.Hit{MessageID: messageID, Score: 1 - math.Sqrt(distance)})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].MessageID < hits[j].MessageID
		}
		return hits[i].Score > hits[j].Score
	})
	if len(hits) > k {
		hits = hits[:k]
	}
	for i := range hits {
		hits[i].Rank = i + 1
	}
	return hits, nil
}

func intersectCandidateMessageIDs(candidates, requested []int64) []int64 {
	if len(requested) == 0 {
		return candidates
	}
	wanted := make(map[int64]struct{}, len(requested))
	for _, messageID := range requested {
		wanted[messageID] = struct{}{}
	}
	result := make([]int64, 0, min(len(candidates), len(requested)))
	for _, messageID := range candidates {
		if _, ok := wanted[messageID]; ok {
			result = append(result, messageID)
		}
	}
	return result
}
