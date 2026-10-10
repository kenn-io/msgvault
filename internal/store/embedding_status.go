package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/vector"
)

func (s *Store) SaveEmbeddingDiagnostics(ctx context.Context, d vector.EmbeddingDiagnostics) error {
	data, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("encode embedding diagnostics: %w", err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open embedding diagnostic writer: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if sqlite, ok := s.dialect.(*SQLiteDialect); ok {
		restore, err := sqlite.narrowBusyTimeout(ctx, conn, 250*time.Millisecond, "embedding diagnostics write")
		if err != nil {
			return err
		}
		defer restore()
	}
	_, err = conn.ExecContext(ctx, s.dialect.Rebind(`INSERT INTO embedding_diagnostics (generation_id, snapshot)
		VALUES (?, ?) ON CONFLICT(generation_id) DO UPDATE
		SET snapshot = excluded.snapshot`), int64(d.GenerationID), string(data))
	if err != nil {
		return fmt.Errorf("save embedding diagnostics: %w", err)
	}
	return nil
}

func (s *Store) ReadEmbeddingDiagnostics(ctx context.Context, gen int64) (*vector.EmbeddingDiagnostics, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT snapshot FROM embedding_diagnostics WHERE generation_id = ?`, gen).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // A generation without a diagnostic pass has no snapshot.
	}
	if err != nil {
		return nil, fmt.Errorf("read embedding diagnostics: %w", err)
	}
	var d vector.EmbeddingDiagnostics
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return nil, fmt.Errorf("decode embedding diagnostics: %w", err)
	}
	return &d, nil
}
