package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const cacheInsertBatchSize = 100

type cacheContextExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertCacheRows(ctx context.Context, db cacheContextExecer, table string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	columnCount := len(rows[0])
	if columnCount == 0 {
		return fmt.Errorf("insert cache rows into %s: rows have no columns", table)
	}
	rowPlaceholders := "(" + strings.TrimSuffix(strings.Repeat("?,", columnCount), ",") + ")"
	for start := 0; start < len(rows); start += cacheInsertBatchSize {
		end := min(start+cacheInsertBatchSize, len(rows))
		placeholders := make([]string, end-start)
		args := make([]any, 0, (end-start)*columnCount)
		for i, row := range rows[start:end] {
			if len(row) != columnCount {
				return fmt.Errorf("insert cache rows into %s: row %d has %d columns, want %d", table, start+i, len(row), columnCount)
			}
			placeholders[i] = rowPlaceholders
			args = append(args, row...)
		}
		statement := "INSERT INTO " + table + " VALUES " + strings.Join(placeholders, ",")
		if _, err := db.ExecContext(ctx, statement, args...); err != nil {
			return fmt.Errorf("insert cache rows into %s: %w", table, err)
		}
	}
	return nil
}
