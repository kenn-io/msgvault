package macoscalls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/msgvault/internal/callhistory"
)

// Read streams normalized observations from a private snapshot of a supplied
// Core Data call store. The source is read-only; emit errors stop immediately.
func Read(ctx context.Context, path, account string, emit func(callhistory.Observation) error) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve macOS call database: %w", err)
	}
	if _, err := os.Stat(absolute); err != nil {
		return sourceError(err)
	}
	dir, err := os.MkdirTemp("", "msgvault-macos-calls-")
	if err != nil {
		return fmt.Errorf("create macOS call snapshot: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	snapshot := filepath.Join(dir, "history.sqlite")
	if err := copySnapshot(ctx, absolute, snapshot); err != nil {
		return sourceError(err)
	}
	db, err := sql.Open("sqlite3", fileDSN(snapshot, "ro"))
	if err != nil {
		return fmt.Errorf("open call snapshot: %w", err)
	}
	defer func() { _ = db.Close() }()
	available, err := readSchema(ctx, db)
	if err != nil {
		return err
	}
	for _, name := range []string{"Z_PK", "ZUNIQUE_ID", "ZDATE"} {
		if !available[name] {
			return fmt.Errorf("unsupported macOS call schema: missing %s", name)
		}
	}
	fields := make([]string, len(columns))
	for i, c := range columns {
		fields[i] = "NULL"
		if available[c.name] {
			// Unary + preserves the storage value but removes declared-type
			// metadata, preventing driver TIMESTAMP/BOOLEAN conversion.
			fields[i] = "+" + c.name
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT "+strings.Join(fields, ",")+" FROM ZCALLRECORD ORDER BY Z_PK")
	if err != nil {
		return fmt.Errorf("query macOS calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for ordinal := 1; rows.Next(); ordinal++ {
		values := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("read macOS call row %d: unsupported field types", ordinal)
		}
		r := make(record, len(columns))
		for i, c := range columns {
			r[c.name] = values[i]
		}
		o, err := r.observation(account)
		if err != nil {
			return fmt.Errorf("macOS call row %d: %w", ordinal, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(o); err != nil {
			return err
		}
	}
	return rows.Err()
}

func readSchema(ctx context.Context, db *sql.DB) (available map[string]bool, retErr error) {
	schema, err := db.QueryContext(ctx, `PRAGMA table_info(ZCALLRECORD)`)
	if err != nil {
		return nil, fmt.Errorf("read macOS call schema: %w", err)
	}
	defer func() {
		if err := schema.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close macOS call schema: %w", err))
		}
	}()
	available = map[string]bool{}
	var scanErr error
	for schema.Next() {
		var cid, notnull, pk int
		var name, kind string
		var def any
		if err := schema.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
			scanErr = errors.New("unsupported macOS call schema")
			break
		}
		available[strings.ToUpper(name)] = true
	}
	err = errors.Join(scanErr, schema.Err())
	if err != nil {
		return nil, fmt.Errorf("read macOS call schema: %w", err)
	}
	return available, nil
}

func sourceError(err error) error {
	var se sqlite3.Error
	if errors.Is(err, os.ErrPermission) || (errors.As(err, &se) && (se.Code == sqlite3.ErrCantOpen || se.Code == sqlite3.ErrPerm || se.Code == sqlite3.ErrAuth)) {
		return fmt.Errorf("read macOS call database: if macOS denies access, grant Full Disk Access to the terminal or application in System Settings, or supply an accessible copy with --db: %w", err)
	}
	return fmt.Errorf("read macOS call database: %w", err)
}
