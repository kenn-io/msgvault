package whatsapp

import (
	"database/sql"
	"errors"
	"fmt"
)

type databaseKind int

const (
	databaseKindUnknown databaseKind = iota
	databaseKindAndroid
	databaseKindApple
)

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
