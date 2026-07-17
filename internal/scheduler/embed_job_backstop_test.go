//go:build sqlite_vec

package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
)

// e2eWorkStore is a minimal embed.WorkStore over the test main DB,
// mirroring store.ScanForEmbedding / store.SetEmbedGen.
type e2eWorkStore struct{ db *sql.DB }

func (s *e2eWorkStore) ScanForEmbedding(ctx context.Context, target, afterID int64, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL
		    AND id > ?
		  ORDER BY id LIMIT ?`, target, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *e2eWorkStore) SetEmbedGen(ctx context.Context, ids []int64, target int64) error {
	if len(ids) == 0 {
		return nil
	}
	ph := make([]string, len(ids))
	args := make([]any, 0, 1+len(ids))
	args = append(args, target)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE messages SET embed_gen = ? WHERE id IN (`+strings.Join(ph, ",")+`)`, args...)
	return err
}

func (s *e2eWorkStore) SetEmbedGenIfUnchanged(ctx context.Context, items []store.EmbedGenStamp, target int64) (missed []int64, err error) {
	for _, it := range items {
		res, err := s.db.ExecContext(ctx,
			`UPDATE messages SET embed_gen = ? WHERE id = ? AND last_modified = ?`,
			target, it.ID, it.LastModified)
		if err != nil {
			return missed, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return missed, err
		}
		if n == 0 {
			missed = append(missed, it.ID)
		}
	}
	return missed, nil
}

// e2eCoverage satisfies EmbedCoverage from the live main DB so the
// EmbedJob's activation gate reflects real coverage.
type e2eCoverage struct{ db *sql.DB }

func (c *e2eCoverage) MissingCount(ctx context.Context, activeGen int64) (int64, error) {
	var missing int64
	if err := c.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, activeGen).Scan(&missing); err != nil {
		return 0, err
	}
	return missing, nil
}

// e2eClient returns one deterministic non-zero vector per input.
type e2eClient struct{ dim int }

func (c *e2eClient) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, c.dim)
		v[0] = float32(len(inputs[i])%c.dim + 1)
		out[i] = v
	}
	return out, nil
}

func countMissingE2E(t *testing.T, db *sql.DB, gen int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM messages
		  WHERE (embed_gen IS NULL OR embed_gen <> ?)
		    AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, gen).Scan(&n))

	return n
}

// TestEmbedJob_Backstop_RecoversSubWatermarkStraggler is the end-to-end
// backstop test: a real EmbedJob (real Worker + sqlitevec backend) whose
// backstop interval has elapsed runs a backstop that re-embeds a
// below-watermark straggler WITHOUT re-embedding already-stamped messages;
// and when the interval has NOT elapsed it only runs RunOnce (which misses
// the sub-watermark straggler).
func TestEmbedJob_Backstop_RecoversSubWatermarkStraggler(t *testing.T) {
	assert := assert.New(
		t,
	)
	require := require.New(t)

	ctx := context.Background()
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "main.db")
	require.NoError(
		sqlitevec.RegisterExtension(), "RegisterExtension")

	mainDB, err := sql.Open(sqlitevec.DriverName(), mainPath)
	require.NoError(
		err, "open main")

	t.Cleanup(func() { _ = mainDB.Close() })

	_, err = mainDB.Exec(`
CREATE TABLE messages (
    id INTEGER PRIMARY KEY, subject TEXT,
    deleted_at DATETIME, deleted_from_source_at DATETIME, embed_gen INTEGER,
    last_modified DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE message_bodies (
    message_id INTEGER PRIMARY KEY, body_text TEXT, body_html TEXT);
CREATE TABLE applied_migrations (name TEXT PRIMARY KEY, applied_at DATETIME);
CREATE TRIGGER trg_messages_last_modified
AFTER UPDATE ON messages FOR EACH ROW
WHEN OLD.last_modified = NEW.last_modified
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_upd
AFTER UPDATE ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;
CREATE TRIGGER trg_message_bodies_last_modified_ins
AFTER INSERT ON message_bodies FOR EACH ROW
BEGIN
    UPDATE messages SET last_modified = CURRENT_TIMESTAMP WHERE id = NEW.message_id;
END;`)
	require.NoError(
		err, "schema")

	const n = 4
	for i := 1; i <= n; i++ {
		_, err = mainDB.Exec(`INSERT INTO messages (id, subject) VALUES (?, ?)`, i, fmt.Sprintf("msg %d", i))
		require.NoError(
			err, "insert message")

		_, err = mainDB.Exec(`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`, i, fmt.Sprintf("body %d", i))
		require.NoError(
			err, "insert body")
	}

	vecPath := filepath.Join(dir, "vectors.db")
	backend, err := sqlitevec.Open(ctx, sqlitevec.Options{
		Path: vecPath, MainPath: mainPath, Dimension: 4, MainDB: mainDB,
	})
	require.NoError(
		err, "sqlitevec.Open")

	t.Cleanup(func() { _ = backend.Close() })

	gen, err := backend.CreateGeneration(ctx, "fake", 4, "")
	require.NoError(
		err, "CreateGeneration")

	vecDB, err := sql.Open(sqlitevec.DriverName(), vecPath)
	require.NoError(
		err, "open vectors handle")

	t.Cleanup(func() { _ = vecDB.Close() })

	ws := &e2eWorkStore{db: mainDB}
	worker := embed.NewWorker(embed.WorkerDeps{
		Backend: backend, VectorsDB: vecDB, MainDB: mainDB,
		Store: ws, Client: &e2eClient{dim: 4}, BatchSize: 8,
		LastModifiedExpr: "CAST(m.last_modified AS TEXT)",
	})

	// Drain the corpus fully via the worker so every message is embedded +
	// stamped and the per-gen watermark advances to the max id.
	_, err = worker.RunOnce(ctx, gen)
	require.NoError(
		err, "initial drain")

	require.NoError(
		backend.ActivateGeneration(ctx, gen, false), "activate (coverage complete)")

	require.Equal(0, countMissingE2E(t, mainDB, int64(gen)), "all embedded after drain")

	// Create a sub-watermark straggler: un-stamp message 2 (its id is below
	// the watermark, so a plain RunOnce will skip it). This models a
	// repair-encoding NULL reset.
	_, err = mainDB.ExecContext(ctx, `UPDATE messages SET embed_gen = NULL WHERE id = 2`)
	require.NoError(
		err, "un-stamp straggler")

	require.Equal(1, countMissingE2E(t, mainDB, int64(gen)), "straggler now missing")

	now := time.Now()
	clock := &now
	job := &EmbedJob{
		Worker:           worker,
		Backend:          backend,
		Store:            &e2eCoverage{db: mainDB},
		Fingerprint:      "fake:4",
		BackstopInterval: 24 * time.Hour,
		Now:              func() time.Time { return *clock },
		// Seed this generation's last backstop to "just now" so the first Run
		// is WITHIN the interval and exercises the RunOnce-only path before we
		// elapse it.
		lastBackstop: map[vector.GenerationID]time.Time{gen: now},
	}

	// Tick A (within interval): RunOnce only. The sub-watermark straggler is
	// NOT recovered because RunOnce resumes from the watermark.
	job.Run(ctx)
	assert.Equal(1, countMissingE2E(t, mainDB, int64(gen)),
		"within interval: RunOnce alone misses the sub-watermark straggler")

	// Tick B (interval elapsed): the backstop runs and recovers the
	// straggler without re-embedding the already-stamped messages.
	*clock = now.Add(25 * time.Hour)
	job.Run(ctx)
	assert.Equal(0, countMissingE2E(t, mainDB, int64(gen)),
		"after interval: backstop recovers the sub-watermark straggler")
}
