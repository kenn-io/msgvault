package agentdemo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Each exclusion is wall-clock or random state, never scenario content.
var volatileColumns = map[string][]string{
	"*":                         {"created_at", "updated_at", "last_modified", "content_changed_at", "archived_at"}, // Store mutation timestamps.
	"persons":                   {"vcard_uid", "vcard_projection_revision"},                                         // Random UID and projection bookkeeping.
	"sync_runs":                 {"request_fingerprint", "operation_id", "started_at", "completed_at"},              // Random invocation identity and current sync clock; historical rebase is a later chapter.
	"sources":                   {"last_sync_at"},                                                                   // Sync completion uses the current clock.
	"conversation_participants": {"joined_at"},                                                                      // Roster insertion uses the current clock.
	"message_embedding_runs":    {"finished_at"},                                                                    // Store.Finish supplies the current clock.
	"person_embedding_runs":     {"finished_at"},                                                                    // Store.Finish supplies the current clock.
}
var digestTables = []string{"sources", "participants", "participant_identifiers", "persons", "person_participants", "participant_links", "conversations", "conversation_participants", "messages", "message_recipients", "message_bodies", "message_raw", "labels", "message_labels", "attachments", "meeting_details", "meeting_action_items", "calendar_events", "calendar_event_attendees", "sync_runs", "message_embedding_runs", "person_embedding_runs", "collections", "collection_sources", "saved_views"}

// Keys identify content independently of SQLite allocation order. Composite-only
// tables have no surrogate id and are sorted by their complete canonical row.
var naturalColumns = map[string][]string{
	"sources":                 {"source_type", "identifier"},
	"participants":            {"email_address", "phone_number", "canonical_id", "display_name"},
	"participant_identifiers": {"identifier_type", "identifier_value"},
	"persons":                 {"display_name"},
	"conversations":           {"source_id", "source_conversation_id"},
	"messages":                {"source_id", "source_message_id"},
	"labels":                  {"source_id", "source_label_id"},
	"attachments":             {"message_id", "content_hash", "filename", "source_attachment_id", "source_part_key"},
	"calendar_events":         {"message_id"},
	"meeting_action_items":    {"message_id", "ordinal"},
	"sync_runs":               {"source_id", "sync_type", "cursor_before", "cursor_after", "status"},
	"message_embedding_runs":  {"invocation_key"},
	"person_embedding_runs":   {"invocation_key"},
	"collections":             {"name"},
	"saved_views":             {"name"},
	"communication_services":  {"slug"},
}

type digestRow map[string]any
type digestTable struct {
	columns []string
	rows    []digestRow
	byID    map[string]digestRow
	foreign map[string]string
}
type canonicalizer struct {
	ctx    context.Context
	db     *sql.DB
	tables map[string]*digestTable
}

func quoteIdentifier(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func (c *canonicalizer) load(name string) (result *digestTable, err error) {
	if table, ok := c.tables[name]; ok {
		return table, nil
	}
	rows, err := c.db.QueryContext(c.ctx, "SELECT * FROM "+quoteIdentifier(name))
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
			delete(c.tables, name)
		}
	}(rows)
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	table := &digestTable{columns: columns, byID: map[string]digestRow{}, foreign: map[string]string{}}
	for rows.Next() {
		values := make([]any, len(columns))
		ptrs := make([]any, len(columns))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err = rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := digestRow{}
		for i, col := range columns {
			v := values[i]
			if data, ok := v.([]byte); ok {
				v = bytes.Clone(data)
			}
			row[col] = v
		}
		table.rows = append(table.rows, row)
		if id, ok := row["id"]; ok {
			table.byID[fmt.Sprint(id)] = row
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = c.db.QueryContext(c.ctx, "PRAGMA foreign_key_list("+quoteIdentifier(name)+")")
	if err != nil {
		return nil, err
	}
	defer func(rows *sql.Rows) {
		err = errors.Join(err, rows.Close())
		if err != nil {
			result = nil
			delete(c.tables, name)
		}
	}(rows)
	for rows.Next() {
		var id, seq int
		var target, from, to, onUpdate, onDelete, match string
		if err = rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, err
		}
		switch to {
		case "id":
			table.foreign[from] = target
		case "message_id":
			table.foreign[from] = "messages"
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	c.tables[name] = table
	return table, nil
}
func (c *canonicalizer) value(table *digestTable, col string, v any) (any, error) {
	// SQL NULL is a valid canonical value, including in foreign-key columns.
	if target, ok := table.foreign[col]; ok && v != nil {
		t, err := c.load(target)
		if err != nil {
			return nil, err
		}
		row, ok := t.byID[fmt.Sprint(v)]
		if !ok {
			return nil, fmt.Errorf("missing %s natural key for %v", target, v)
		}
		return c.key(target, t, row)
	}
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return v, nil
}
func (c *canonicalizer) key(name string, table *digestTable, row digestRow) (string, error) {
	if name == "participants" {
		identifiers, err := c.load("participant_identifiers")
		if err != nil {
			return "", err
		}
		var keys []string
		for _, identifier := range identifiers.rows {
			if fmt.Sprint(identifier["participant_id"]) == fmt.Sprint(row["id"]) {
				data, err := json.Marshal([]any{identifier["identifier_type"], identifier["identifier_value"]}, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
				if err != nil {
					return "", err
				}
				keys = append(keys, string(data))
			}
		}
		if len(keys) > 0 {
			slices.Sort(keys)
			data, err := json.Marshal(keys, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
			return name + ":" + string(data), err
		}
	}
	if name == "persons" {
		bindings, err := c.load("person_participants")
		if err != nil {
			return "", err
		}
		participants, err := c.load("participants")
		if err != nil {
			return "", err
		}
		var keys []string
		for _, binding := range bindings.rows {
			if fmt.Sprint(binding["person_id"]) == fmt.Sprint(row["id"]) {
				participant, ok := participants.byID[fmt.Sprint(binding["participant_id"])]
				if !ok {
					return "", errors.New("missing person participant")
				}
				key, err := c.key("participants", participants, participant)
				if err != nil {
					return "", err
				}
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		data, err := json.Marshal([]any{row["display_name"], keys}, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
		return name + ":" + string(data), err
	}
	columns, ok := naturalColumns[name]
	if !ok {
		return "", fmt.Errorf("no natural key for referenced table %s", name)
	}
	values := make([]any, 0, len(columns))
	for _, col := range columns {
		v, err := c.value(table, col, row[col])
		if err != nil {
			return "", err
		}
		values = append(values, v)
	}
	data, err := json.Marshal(values, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
	return name + ":" + string(data), err
}

// Digest hashes semantic archive rows, normalizing foreign keys and raw MIME.
func Digest(ctx context.Context, db *sql.DB) (string, error) {
	c := &canonicalizer{ctx: ctx, db: db, tables: map[string]*digestTable{}}
	hash := sha256.New()
	for _, name := range digestTables {
		table, err := c.load(name)
		if err != nil {
			return "", fmt.Errorf("digest %s: %w", name, err)
		}
		lines := make([]string, 0, len(table.rows))
		for _, row := range table.rows {
			canonical := map[string]any{}
			for _, col := range table.columns {
				if col == "id" || slices.Contains(volatileColumns["*"], col) || slices.Contains(volatileColumns[name], col) {
					continue
				}
				value := row[col]
				if name == "message_raw" && col == "raw_data" && row["compression"] == "zlib" {
					data, ok := value.([]byte)
					if !ok {
						return "", errors.New("digest: raw_data is not bytes")
					}
					decoded, err := store.DecodeMessageRaw(data, "zlib")
					if err != nil {
						return "", err
					}
					value = decoded
				}
				value, err = c.value(table, col, value)
				if err != nil {
					return "", fmt.Errorf("digest %s.%s: %w", name, col, err)
				}
				canonical[col] = value
			}
			if name == "participant_links" {
				a, aok := canonical["participant_a"].(string)
				b, bok := canonical["participant_b"].(string)
				if aok && bok && a > b {
					canonical["participant_a"], canonical["participant_b"] = b, a
				}
			}
			data, err := json.Marshal(canonical, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
			if err != nil {
				return "", err
			}
			key := ""
			if _, ok := naturalColumns[name]; ok {
				key, err = c.key(name, table, row)
				if err != nil {
					return "", err
				}
			}
			lines = append(lines, key+"\t"+string(data))
		}
		slices.Sort(lines)
		for _, line := range lines {
			if _, err := fmt.Fprintf(hash, "%s\t%s\n", name, line); err != nil {
				return "", fmt.Errorf("write digest: %w", err)
			}
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
