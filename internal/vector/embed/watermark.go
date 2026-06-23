package embed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/vector"
)

// Watermark reads and writes per-generation forward-scan resume points in
// the embed_watermark table. It lives WITH the generations it watermarks:
// vectors.db on SQLite, the main PostgreSQL database on PG. The worker
// seeds its scan from GetWatermark at run start and advances it after each
// successful batch via SetWatermark.
//
// The watermark is a pure optimization. Losing it (or never seeding it)
// only restarts the next scan from id 0, which is harmless: the scan
// predicate (embed_gen IS NULL OR embed_gen <> gen) plus the idempotent
// embeddings upsert make re-sweeping already-covered rows a no-op. The
// full-scan backstop ignores the watermark entirely.
//
// The upsert SQL (INSERT ... ON CONFLICT ... DO UPDATE SET ... =
// excluded....) is portable across SQLite (3.24+) and PostgreSQL, so
// Watermark needs no dialect probe beyond rebind.
type Watermark struct {
	db     *sql.DB
	rebind func(string) string
}

// NewWatermark returns a Watermark bound to db (the generation-side DB).
// The caller retains ownership of db. rebind translates ?-placeholders to
// the driver's native form; pass nil (or an identity func) for SQLite and
// the PostgreSQL dialect's Rebind for pgx.
func NewWatermark(db *sql.DB, rebind func(string) string) *Watermark {
	if rebind == nil {
		rebind = func(q string) string { return q }
	}
	return &Watermark{db: db, rebind: rebind}
}

// GetWatermark returns the stored watermark for gen, or 0 when no row
// exists yet (which makes the next scan start from the beginning — safe by
// design). A nil db (watermark disabled) returns 0 without error.
func (w *Watermark) GetWatermark(ctx context.Context, gen vector.GenerationID) (int64, error) {
	if w == nil || w.db == nil {
		return 0, nil
	}
	var id int64
	err := w.db.QueryRowContext(ctx,
		w.rebind(`SELECT watermark_id FROM embed_watermark WHERE generation_id = ?`),
		int64(gen)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get watermark: %w", err)
	}
	return id, nil
}

// SetWatermark upserts the watermark for gen to id. A nil db (watermark
// disabled) is a no-op. Advancing the watermark is non-critical — a
// failure here is logged by the worker, not fatal — so callers may treat
// the error as best-effort.
func (w *Watermark) SetWatermark(ctx context.Context, gen vector.GenerationID, id int64) error {
	if w == nil || w.db == nil {
		return nil
	}
	stmt := `INSERT INTO embed_watermark (generation_id, watermark_id) VALUES (?, ?)
	         ON CONFLICT (generation_id) DO UPDATE SET watermark_id = excluded.watermark_id`
	if _, err := w.db.ExecContext(ctx, w.rebind(stmt), int64(gen), id); err != nil {
		return fmt.Errorf("set watermark: %w", err)
	}
	return nil
}
