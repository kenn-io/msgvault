package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// Materialize the small classification inputs before CSV Prepare closes its
// SQLite transaction. Both scanner and CSV paths use the same pinned reader.
func stageCachePeopleRooms(ctx context.Context, snapshot *cacheSourceSnapshot) error {
	db := snapshot.DuckDB()
	// Regular tables keep these inputs visible to later CSV export connections;
	// the build-local in-memory DuckDB owns their lifetime.
	for _, statement := range []string{
		`CREATE OR REPLACE TABLE cache_room_addresses(email VARCHAR)`,
		`CREATE OR REPLACE TABLE cache_person_contacts(participant_id BIGINT, address_kind VARCHAR, normalized_value VARCHAR)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, item := range []struct {
		table, target, query string
		columns              int
	}{
		{"calendar_event_attendees", "cache_room_addresses", `SELECT lower(trim(a.email)) FROM calendar_event_attendees a WHERE a.role = 'room' AND trim(a.email) <> '' AND EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id AND m.message_type = 'calendar_event' AND m.deleted_at IS NULL) GROUP BY lower(trim(a.email))`, 1},
		{"person_contact_points", "cache_person_contacts", `SELECT pp.participant_id, cp.address_kind, cp.normalized_value FROM person_contact_points cp JOIN person_participants pp ON pp.person_id = cp.person_id WHERE cp.active_until IS NULL AND cp.superseded_at IS NULL AND trim(cp.normalized_value) <> ''`, 3},
	} {
		var exists int
		if err := snapshot.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, item.table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		} // Minimal test archives may omit these optional source tables.
		values, err := cachePeopleRoomInputs(ctx, snapshot, item.query, item.columns)
		if err != nil {
			return fmt.Errorf("read cache room inputs: %w", err)
		}
		if err := insertCacheRows(ctx, db, item.target, values); err != nil {
			return fmt.Errorf("stage %s: %w", item.target, err)
		}
	}
	return nil
}

func cachePeopleRoomInputs(ctx context.Context, snapshot *cacheSourceSnapshot, query string, columns int) (values [][]any, err error) {
	rows, err := snapshot.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if columns == 1 {
			var email string
			if err := rows.Scan(&email); err != nil {
				return nil, err
			}
			values = append(values, []any{email})
		} else {
			var id int64
			var kind, value sql.NullString
			if err := rows.Scan(&id, &kind, &value); err != nil {
				return nil, err
			}
			values = append(values, []any{id, kind.String, value.String})
		}
	}
	return values, rows.Err()
}

func exportCachePeopleRooms(ctx context.Context, db sqlRunner, root string) error {
	for _, dataset := range []string{"room_addresses", "person_contacts"} {
		if err := copyParquet(ctx, db, filepath.Join(root, dataset), dataset+".parquet", `SELECT * FROM cache_`+dataset); err != nil {
			return fmt.Errorf("export %s: %w", dataset, err)
		}
	}
	return nil
}
