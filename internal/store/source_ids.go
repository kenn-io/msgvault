package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const sourceIDHighWaterKey = "source_id_high_water"

// reserveSourceIDWriterContext must precede every counter/live-max read. The
// first write serializes independent SQLite connections, including first use
// on an archive that predates the retained counter.
func reserveSourceIDWriterContext(ctx context.Context, tx contextStatementQuerier) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value)
		VALUES(?,'0') ON CONFLICT(key) DO UPDATE
		SET value=archive_metadata.value`, sourceIDHighWaterKey)
	if err != nil {
		return fmt.Errorf("reserve source ID writer: %w", err)
	}
	return nil
}

func sourceIDHighWaterContext(ctx context.Context, tx contextStatementQuerier) (int64, error) {
	var text string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key=?`, sourceIDHighWaterKey).Scan(&text); err != nil {
		return 0, fmt.Errorf("read source ID counter: %w", err)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || value < 0 || strconv.FormatInt(value, 10) != text {
		return 0, errors.New("invalid retained source ID counter")
	}
	var liveMax int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM sources`).Scan(&liveMax); err != nil {
		return 0, fmt.Errorf("read live source ID maximum: %w", err)
	}
	return max(value, liveMax), nil
}

func writeSourceIDHighWaterContext(ctx context.Context, tx contextStatementQuerier, value int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value=? WHERE key=?`, strconv.FormatInt(value, 10), sourceIDHighWaterKey)
	if err != nil {
		return fmt.Errorf("retain source ID counter: %w", err)
	}
	return nil
}

// retainSourceIDHighWaterContext runs inside the caller's removal transaction
// before DELETE, so a legacy archive's highest ID is retained before it vanishes.
// PostgreSQL already retains its native noncycling identity sequence.
func (s *Store) retainSourceIDHighWaterContext(ctx context.Context, tx contextStatementQuerier) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return retainSQLiteSourceIDHighWaterContext(ctx, tx)
}

func retainSQLiteSourceIDHighWaterContext(ctx context.Context, tx contextStatementQuerier) error {
	if err := reserveSourceIDWriterContext(ctx, tx); err != nil {
		return err
	}
	value, err := sourceIDHighWaterContext(ctx, tx)
	if err != nil {
		return err
	}
	return writeSourceIDHighWaterContext(ctx, tx, value)
}

// allocateSQLiteSourceIDContext reserves and advances the retained ID inside
// the source creation transaction. Call after the transaction's sync fence so
// first use of the counter serializes independent SQLite connections.
func allocateSQLiteSourceIDContext(ctx context.Context, tx contextStatementQuerier) (int64, error) {
	if err := reserveSourceIDWriterContext(ctx, tx); err != nil {
		return 0, err
	}
	value, err := sourceIDHighWaterContext(ctx, tx)
	if err != nil {
		return 0, err
	}
	if value == math.MaxInt64 {
		return 0, errors.New("source ID supply exhausted")
	}
	next := value + 1
	if err := writeSourceIDHighWaterContext(ctx, tx, next); err != nil {
		return 0, err
	}
	return next, nil
}
