package vector

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// EmbeddingCheckpoints saves provider responses before another request starts.
// Owners identify messages or contextual documents; revisions identify their
// exact input. They are separate from published, searchable vectors.
type EmbeddingCheckpoints struct {
	DB             *sql.DB
	Rebind         func(string) string
	LockGeneration bool
}

// Load discards obsolete input revisions and returns this owner's saved output.
func (s EmbeddingCheckpoints) Load(ctx context.Context, gen GenerationID, owner, revision string) (map[string][][]float32, error) {
	saved, err := s.LoadMany(ctx, gen, map[string]string{owner: revision})
	return saved[owner], err
}

// LoadMany reads and invalidates a bounded page of owners in two statements.
func (s EmbeddingCheckpoints) LoadMany(ctx context.Context, gen GenerationID, revisions map[string]string) (map[string]map[string][][]float32, error) {
	owners := make([]string, 0, len(revisions))
	for owner := range revisions {
		owners = append(owners, owner)
	}
	out := make(map[string]map[string][][]float32, len(owners))
	for batch := range slices.Chunk(owners, 300) {
		stale := make([]string, len(batch))
		args := []any{int64(gen)}
		for i, owner := range batch {
			stale[i] = "(owner = ? AND revision <> ?)"
			args = append(args, owner, revisions[owner])
		}
		if _, err := s.DB.ExecContext(ctx, s.Rebind(`DELETE FROM embedding_checkpoints WHERE generation_id = ? AND (`+strings.Join(stale, " OR ")+`)`), args...); err != nil {
			return nil, fmt.Errorf("invalidate embedding checkpoints: %w", err)
		}
		args = []any{int64(gen)}
		for _, owner := range batch {
			args = append(args, owner)
		}
		err := func() error {
			rows, err := s.DB.QueryContext(ctx, s.Rebind(`SELECT owner, revision, input_key, vectors FROM embedding_checkpoints WHERE generation_id = ? AND owner IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`), args...)
			if err != nil {
				return fmt.Errorf("read embedding checkpoints: %w", err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var owner, revision, key, data string
				if err := rows.Scan(&owner, &revision, &key, &data); err != nil {
					return err
				}
				if revision != revisions[owner] {
					continue
				}
				var vectors [][]float32
				if err := json.Unmarshal([]byte(data), &vectors); err != nil {
					return fmt.Errorf("decode embedding checkpoint: %w", err)
				}
				if out[owner] == nil {
					out[owner] = make(map[string][][]float32)
				}
				out[owner][key] = vectors
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// EmbeddingCheckpoint is one owner's output from a completed provider response.
type EmbeddingCheckpoint struct {
	Owner, Revision, Key string
	Vectors              [][]float32
}

// Save commits a completed provider response. The generation predicate prevents
// work completing after retirement from restoring retired checkpoints.
// A returned response gets a bounded save even if its indexing pass has ended.
func (s EmbeddingCheckpoints) Save(ctx context.Context, gen GenerationID, entries []EmbeddingCheckpoint) (retErr error) {
	if len(entries) == 0 {
		return nil
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, ctx.Err())
		}
	}()
	tx, err := s.DB.BeginTx(saveCtx, nil)
	if err != nil {
		return fmt.Errorf("begin embedding response save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	lock := ""
	if s.LockGeneration {
		lock = " FOR SHARE"
	}
	for _, entry := range entries {
		data, err := json.Marshal(entry.Vectors)
		if err != nil {
			return fmt.Errorf("encode embedding checkpoint: %w", err)
		}
		res, err := tx.ExecContext(saveCtx, s.Rebind(`INSERT INTO embedding_checkpoints (generation_id, owner, revision, input_key, vectors)
		SELECT id, ?, ?, ?, ? FROM index_generations WHERE id = ? AND state <> 'retired'`+lock+`
		ON CONFLICT (generation_id, owner, input_key) DO UPDATE SET revision = excluded.revision, vectors = excluded.vectors`), entry.Owner, entry.Revision, entry.Key, string(data), int64(gen))
		if err != nil {
			return fmt.Errorf("save embedding checkpoint: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrGenerationRetired
		}
	}
	return tx.Commit()
}

// Owners lists a bounded page of checkpoint owners in lexical order.
func (s EmbeddingCheckpoints) Owners(ctx context.Context, gen GenerationID, prefix, after string, limit int) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, s.Rebind(`SELECT DISTINCT owner FROM embedding_checkpoints WHERE generation_id = ? AND owner LIKE ? AND owner > ? ORDER BY owner LIMIT ?`), int64(gen), prefix+"%", after, limit)
	if err != nil {
		return nil, fmt.Errorf("list embedding checkpoint owners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		owners = append(owners, owner)
	}
	return owners, rows.Err()
}

// Delete releases checkpoints once their owner has published.
func (s EmbeddingCheckpoints) Delete(ctx context.Context, gen GenerationID, owners []string) error {
	for batch := range slices.Chunk(owners, 300) {
		args := []any{int64(gen)}
		for _, owner := range batch {
			args = append(args, owner)
		}
		if _, err := s.DB.ExecContext(ctx, s.Rebind(`DELETE FROM embedding_checkpoints WHERE generation_id = ? AND owner IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`), args...); err != nil {
			return fmt.Errorf("delete published embedding checkpoints: %w", err)
		}
	}
	return nil
}
