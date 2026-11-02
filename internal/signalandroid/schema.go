package signalandroid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MinDatabaseVersion is the oldest Signal Android database version the
// importer reads. Version 185 moved message authorship into
// message.from_recipient_id and message.to_recipient_id and added the
// edit-revision columns; older databases need a separate reader.
const MinDatabaseVersion = 185

// ErrUnsupportedDatabase reports a SQLite file that is not a Signal Android
// database this importer can read.
var ErrUnsupportedDatabase = errors.New("unsupported Signal database")

// schema records the table and column names that differ between the Signal
// Android database versions the importer supports.
type schema struct {
	// recipientACI is "aci" from version 200 on and "uuid" before it.
	recipientACI string
	// recipientE164 is "e164" from version 201 on and "phone" before it.
	recipientE164 string
	// recipientName is a SQL expression for the best available display name.
	recipientName string

	// threadRecipient is the thread column naming the chat's recipient.
	threadRecipient string

	// attachmentTable is "attachment" from version 215 on and "part" before
	// it; attachmentMessage and attachmentType are its renamed columns.
	attachmentTable   string
	attachmentMessage string
	attachmentType    string
	attachmentColumns map[string]bool

	hasGroups          bool
	groupsMembers      bool
	hasGroupMembership bool
	hasReactions       bool
	hasMentions        bool

	messageColumns map[string]bool
}

// requiredMessageColumns are present in every supported database version.
var requiredMessageColumns = []string{
	"_id", "date_sent", "date_received", "thread_id", "from_recipient_id",
	"to_recipient_id", "type", "body", "quote_id", "quote_author",
	"original_message_id", "latest_revision_id",
}

func openReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat database: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory, not a Signal database file", path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	uriPath := filepath.ToSlash(absPath)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&_busy_timeout=5000&_query_only=1"}).String()
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

// detectSchema inspects the database and returns the column layout to read,
// or ErrUnsupportedDatabase with the reason.
func detectSchema(ctx context.Context, db *sql.DB) (*schema, error) {
	tables, err := tableNames(ctx, db)
	if err != nil {
		return nil, err
	}
	if !tables["recipient"] || !tables["thread"] {
		return nil, fmt.Errorf("%w: the recipient and thread tables are missing; "+
			"pass the database.sqlite file written by a Signal Android backup", ErrUnsupportedDatabase)
	}
	if !tables["message"] {
		if tables["mms"] || tables["sms"] {
			return nil, fmt.Errorf("%w: this database predates Signal Android database version %d; "+
				"open the backup in a current Signal Android release and create a new backup",
				ErrUnsupportedDatabase, MinDatabaseVersion)
		}
		return nil, fmt.Errorf("%w: the message table is missing", ErrUnsupportedDatabase)
	}

	s := &schema{}
	if s.messageColumns, err = tableColumns(ctx, db, "message"); err != nil {
		return nil, err
	}
	for _, column := range requiredMessageColumns {
		if !s.messageColumns[column] {
			return nil, fmt.Errorf("%w: message.%s is missing; msgvault reads Signal Android "+
				"database version %d and newer", ErrUnsupportedDatabase, column, MinDatabaseVersion)
		}
	}

	recipient, err := tableColumns(ctx, db, "recipient")
	if err != nil {
		return nil, err
	}
	if s.recipientACI = firstColumn(recipient, "aci", "uuid"); s.recipientACI == "" {
		return nil, fmt.Errorf("%w: recipient.aci is missing", ErrUnsupportedDatabase)
	}
	if s.recipientE164 = firstColumn(recipient, "e164", "phone"); s.recipientE164 == "" {
		return nil, fmt.Errorf("%w: recipient.e164 is missing", ErrUnsupportedDatabase)
	}
	if !recipient["_id"] || !recipient["group_id"] {
		return nil, fmt.Errorf("%w: recipient._id or recipient.group_id is missing", ErrUnsupportedDatabase)
	}
	s.recipientName = recipientNameExpr(recipient)

	thread, err := tableColumns(ctx, db, "thread")
	if err != nil {
		return nil, err
	}
	if s.threadRecipient = firstColumn(thread, "recipient_id", "thread_recipient_id"); s.threadRecipient == "" {
		return nil, fmt.Errorf("%w: thread.recipient_id is missing", ErrUnsupportedDatabase)
	}

	switch {
	case tables["attachment"]:
		s.attachmentTable = "attachment"
	case tables["part"]:
		s.attachmentTable = "part"
	}
	if s.attachmentTable != "" {
		if s.attachmentColumns, err = tableColumns(ctx, db, s.attachmentTable); err != nil {
			return nil, err
		}
		s.attachmentMessage = firstColumn(s.attachmentColumns, "message_id", "mid")
		s.attachmentType = firstColumn(s.attachmentColumns, "content_type", "ct")
		if s.attachmentMessage == "" || s.attachmentType == "" {
			return nil, fmt.Errorf("%w: the %s table lacks its message or content-type column",
				ErrUnsupportedDatabase, s.attachmentTable)
		}
	}

	if tables["groups"] {
		groups, err := tableColumns(ctx, db, "groups")
		if err != nil {
			return nil, err
		}
		s.hasGroups = groups["group_id"] && groups["title"]
		s.groupsMembers = groups["members"]
	}
	if tables["group_membership"] {
		membership, err := tableColumns(ctx, db, "group_membership")
		if err != nil {
			return nil, err
		}
		s.hasGroupMembership = membership["group_id"] && membership["recipient_id"]
	}
	if tables["reaction"] {
		reaction, err := tableColumns(ctx, db, "reaction")
		if err != nil {
			return nil, err
		}
		s.hasReactions = reaction["message_id"] && reaction["author_id"] && reaction["emoji"]
	}
	if tables["mention"] {
		mention, err := tableColumns(ctx, db, "mention")
		if err != nil {
			return nil, err
		}
		s.hasMentions = mention["message_id"] && mention["recipient_id"] && mention["range_start"]
	}
	return s, nil
}

// messageColumn returns the column of message row m if the table has it,
// or a constant fallback so queries stay valid across versions.
func (s *schema) messageColumn(column, fallback string) string {
	if s.messageColumns[column] {
		return "m." + column
	}
	return fallback
}

// attachmentColumn returns the attachment column if present, or fallback.
func (s *schema) attachmentColumn(column, fallback string) string {
	if s.attachmentColumns[column] {
		return "a." + column
	}
	return fallback
}

func recipientNameExpr(columns map[string]bool) string {
	var parts []string
	for _, column := range []string{
		"nickname_joined_name", "system_joined_name", "system_display_name",
		"profile_joined_name", "profile_given_name", "signal_profile_name",
	} {
		if columns[column] {
			parts = append(parts, "NULLIF(TRIM(r."+column+"), '')")
		}
	}
	if len(parts) == 0 {
		return "''"
	}
	return "COALESCE(" + strings.Join(parts, ", ") + ", '')"
}

func firstColumn(columns map[string]bool, names ...string) string {
	for _, name := range names {
		if columns[name] {
			return name
		}
	}
	return ""
}

func tableNames(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return nil, fmt.Errorf("inspect signal database: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tables := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan signal table name: %w", err)
		}
		tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list signal tables: %w", err)
	}
	return tables, nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	// The table name comes from a fixed list in this package, never from input.
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect signal table %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan signal column of %s: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list signal columns of %s: %w", table, err)
	}
	return columns, nil
}
