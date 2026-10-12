package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const sourceIDHighWaterKey = "source_id_high_water"

type sourceIDTransaction interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// reserveSourceIDWriterContext must precede every counter/live-max read. The
// first write serializes independent SQLite connections, including first use
// on an archive that predates the retained counter.
func reserveSourceIDWriterContext(ctx context.Context, tx sourceIDTransaction) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value)
		VALUES(?,'0') ON CONFLICT(key) DO UPDATE
		SET value=archive_metadata.value`, sourceIDHighWaterKey)
	if err != nil {
		return fmt.Errorf("reserve source ID writer: %w", err)
	}
	return nil
}

func sourceIDHighWaterContext(ctx context.Context, tx sourceIDTransaction) (int64, error) {
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

func writeSourceIDHighWaterContext(ctx context.Context, tx sourceIDTransaction, value int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE archive_metadata SET value=? WHERE key=?`, strconv.FormatInt(value, 10), sourceIDHighWaterKey)
	if err != nil {
		return fmt.Errorf("retain source ID counter: %w", err)
	}
	return nil
}

// retainSourceIDHighWaterContext runs inside the caller's removal transaction
// before DELETE, so a legacy archive's highest ID is retained before it vanishes.
// PostgreSQL already retains its native noncycling identity sequence.
func (s *Store) retainSourceIDHighWaterContext(ctx context.Context, tx sourceIDTransaction) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return retainSQLiteSourceIDHighWaterContext(ctx, tx)
}

func retainSQLiteSourceIDHighWaterContext(ctx context.Context, tx sourceIDTransaction) error {
	if err := reserveSourceIDWriterContext(ctx, tx); err != nil {
		return err
	}
	value, err := sourceIDHighWaterContext(ctx, tx)
	if err != nil {
		return err
	}
	return writeSourceIDHighWaterContext(ctx, tx, value)
}

func (s *Store) getOrCreateSQLiteSourceContext(ctx context.Context, sourceType, identifier string) (*Source, error) {
	var source *Source
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		// withTx applies any current sync-generation fence before this writer
		// reservation; do not acquire a new identity/sync lock after it.
		if err := reserveSourceIDWriterContext(ctx, tx); err != nil {
			return err
		}
		var err error
		source, err = scanSource(tx.QueryRowContext(ctx, `SELECT id, source_type,
			identifier, display_name, google_user_id, last_sync_at, sync_cursor,
			sync_config, oauth_app, created_at, updated_at
			FROM sources WHERE source_type=? AND identifier=?`, sourceType, identifier))
		if err == nil {
			return nil // An existing tuple needs no new ID, even at exhaustion.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		value, err := sourceIDHighWaterContext(ctx, tx)
		if err != nil {
			return err
		}
		if value == math.MaxInt64 {
			return errors.New("source ID supply exhausted")
		}
		if err := writeSourceIDHighWaterContext(ctx, tx, value+1); err != nil {
			return err
		}
		now := s.dialect.Now()
		source, err = scanSource(tx.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO sources
			(id,source_type,identifier,created_at,updated_at) VALUES(?,?,?,%s,%s)
			ON CONFLICT(source_type,identifier) DO UPDATE SET identifier=sources.identifier
			RETURNING id,source_type,identifier,display_name,google_user_id,
			last_sync_at,sync_cursor,sync_config,oauth_app,created_at,updated_at`, now, now), value+1, sourceType, identifier))
		return err
	})
	return source, err
}
