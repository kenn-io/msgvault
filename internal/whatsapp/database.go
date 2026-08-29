package whatsapp

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

type databaseKind int

const (
	databaseKindUnknown databaseKind = iota
	databaseKindAndroid
	databaseKindApple
)

func openReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat database: %w", err)
	}

	dsn := (&url.URL{
		Scheme:   "file",
		OmitHost: true,
		Path:     path,
		RawQuery: "_busy_timeout=5000&_query_only=1",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func detectDatabaseKind(db *sql.DB) (databaseKind, error) {
	rows, err := db.Query(`
		SELECT name
		FROM sqlite_master
		WHERE type = 'table'
		  AND name IN ('message', 'jid', 'chat', 'ZWAMESSAGE', 'ZWACHATSESSION')
	`)
	if err != nil {
		return databaseKindUnknown, fmt.Errorf("inspect whatsapp database schema: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return databaseKindUnknown, fmt.Errorf("scan whatsapp database table: %w", err)
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return databaseKindUnknown, fmt.Errorf("iterate whatsapp database tables: %w", err)
	}

	switch {
	case tables["message"] && tables["jid"] && tables["chat"]:
		return databaseKindAndroid, nil
	case tables["ZWAMESSAGE"] && tables["ZWACHATSESSION"]:
		return databaseKindApple, nil
	default:
		return databaseKindUnknown, errors.New(
			"not a valid WhatsApp database: expected Android message/jid/chat tables " +
				"or Apple ZWAMESSAGE/ZWACHATSESSION tables",
		)
	}
}
