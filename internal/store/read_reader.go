package store

import (
	"context"
	"database/sql"
)

// rowReader is the context-aware read contract shared by calendar and meeting reads.
type rowReader = SQLReader

type reboundReader struct {
	reader SQLReader
	rebind func(string) string
}

var _ rowReader = reboundReader{}
var _ rowReader = (*sql.DB)(nil)
var _ rowReader = (*sql.Tx)(nil)

func (r reboundReader) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.reader.QueryContext(ctx, r.rebind(query), args...)
}
func (r reboundReader) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.reader.QueryRowContext(ctx, r.rebind(query), args...)
}

// snapshotReader uses the caller's active snapshot and binds placeholders once.
// Public reads first establish a snapshot with BeginReadSnapshotContext.
func (s *Store) snapshotReader(ctx context.Context) rowReader {
	return reboundReader{reader: ReadDBContext(ctx, s.db.DB), rebind: s.dialect.Rebind}
}
