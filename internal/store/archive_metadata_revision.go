package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

func readArchiveMetadataRevisionContext(
	ctx context.Context,
	q contextRowQuerier,
	key, name string,
) (int64, error) {
	var value string
	err := q.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`, key,
	).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read %s revision: %w", name, err)
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s revision %q: %w", name, value, err)
	}
	return revision, nil
}
