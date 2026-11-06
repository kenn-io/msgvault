package signalandroid

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/sqliteutil"
)

// MinDatabaseVersion is the oldest Signal Android database version the
// importer reads. Version 185 moved message authorship into
// message.from_recipient_id and message.to_recipient_id and added the
// edit-revision columns; older databases need a separate reader.
const MinDatabaseVersion = 185

// MaxDatabaseVersion is the newest layout covered by importer fixtures.
const MaxDatabaseVersion = 330

// ErrUnsupportedDatabase reports a SQLite file that is not a Signal Android
// database this importer can read.
var ErrUnsupportedDatabase = errors.New("unsupported Signal database")

// schema records the table and column names that differ between the Signal
// Android database versions the importer supports.
type schema struct {
	// recipientACI is "aci" from version 200 on and "uuid" before it.
	recipientACI string
	// recipientPNI is optional in older backups.
	recipientPNI string
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

	hasGroups bool

	messageColumns map[string]bool
}

// requiredMessageColumns are present in every supported database version.
var requiredMessageColumns = []string{
	"_id", "date_sent", "date_received", "thread_id", "from_recipient_id",
	"to_recipient_id", "type", "body", "quote_id", "quote_author",
	"original_message_id", "latest_revision_id",
}

// detectSchema inspects the database and returns the column layout to read,
// or ErrUnsupportedDatabase with the reason.
func detectSchema(ctx context.Context, db *sql.DB, dbPath string) (*schema, error) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(dbPath), "DatabaseVersion.sbf"))
	if err != nil {
		return nil, fmt.Errorf("%w: read DatabaseVersion.sbf beside database.sqlite: %w; export without --onlydb", ErrUnsupportedDatabase, err)
	}
	value, found := strings.CutPrefix(strings.TrimSpace(string(data)), "VERSION:uint32:")
	version, err := strconv.ParseUint(value, 10, 32)
	if !found || err != nil {
		return nil, fmt.Errorf("%w: invalid DatabaseVersion.sbf", ErrUnsupportedDatabase)
	}
	if version > MaxDatabaseVersion {
		return nil, fmt.Errorf("%w: database version %d is newer than the newest tested version %d", ErrUnsupportedDatabase, version, MaxDatabaseVersion)
	}
	recipient, err := sqliteutil.TableColumns(ctx, db, "recipient")
	if err != nil {
		return nil, err
	}
	thread, err := sqliteutil.TableColumns(ctx, db, "thread")
	if err != nil {
		return nil, err
	}
	if len(recipient) == 0 || len(thread) == 0 {
		return nil, fmt.Errorf("%w: chat details are missing; use database.sqlite from a Signal Android backup", ErrUnsupportedDatabase)
	}
	s := &schema{}
	if s.messageColumns, err = sqliteutil.TableColumns(ctx, db, "message"); err != nil {
		return nil, err
	}
	if len(s.messageColumns) == 0 {
		mms, err := sqliteutil.TableColumns(ctx, db, "mms")
		if err != nil {
			return nil, err
		}
		sms, err := sqliteutil.TableColumns(ctx, db, "sms")
		if err != nil {
			return nil, err
		}
		if len(mms) > 0 || len(sms) > 0 {
			return nil, fmt.Errorf("%w: this database predates Signal Android database version %d; "+
				"open the backup in a current Signal Android release and create a new backup",
				ErrUnsupportedDatabase, MinDatabaseVersion)
		}
		return nil, fmt.Errorf("%w: the message table is missing", ErrUnsupportedDatabase)
	}

	for _, column := range requiredMessageColumns {
		if !s.messageColumns[column] {
			return nil, fmt.Errorf("%w: message details are missing; msgvault reads Signal Android database versions %d through %d", ErrUnsupportedDatabase, MinDatabaseVersion, MaxDatabaseVersion)
		}
	}

	if version < MinDatabaseVersion {
		return nil, fmt.Errorf("%w: database version %d predates Signal Android database version %d", ErrUnsupportedDatabase, version, MinDatabaseVersion)
	}

	if s.recipientACI = firstColumn(recipient, "aci", "uuid"); s.recipientACI == "" {
		return nil, fmt.Errorf("%w: account IDs are missing", ErrUnsupportedDatabase)
	}
	if s.recipientE164 = firstColumn(recipient, "e164", "phone"); s.recipientE164 == "" {
		return nil, fmt.Errorf("%w: phone numbers are missing", ErrUnsupportedDatabase)
	}
	if !recipient["_id"] || !recipient["group_id"] {
		return nil, fmt.Errorf("%w: contact details are missing", ErrUnsupportedDatabase)
	}
	s.recipientName = recipientNameExpr(recipient)
	s.recipientPNI = firstColumn(recipient, "pni")

	if s.threadRecipient = firstColumn(thread, "recipient_id"); s.threadRecipient == "" {
		return nil, fmt.Errorf("%w: chat contacts are missing", ErrUnsupportedDatabase)
	}

	for _, table := range []string{"attachment", "part"} {
		if s.attachmentColumns, err = sqliteutil.TableColumns(ctx, db, table); err != nil {
			return nil, err
		}
		if len(s.attachmentColumns) > 0 {
			s.attachmentTable = table
			break
		}
	}
	s.attachmentMessage = firstColumn(s.attachmentColumns, "message_id", "mid")
	s.attachmentType = firstColumn(s.attachmentColumns, "content_type", "ct")
	if s.attachmentMessage == "" || s.attachmentType == "" {
		return nil, fmt.Errorf("%w: attachment details are missing", ErrUnsupportedDatabase)
	}

	groups, err := sqliteutil.TableColumns(ctx, db, "groups")
	if err != nil {
		return nil, err
	}
	s.hasGroups = groups["group_id"] && groups["title"]
	membership, err := sqliteutil.TableColumns(ctx, db, "group_membership")
	if err != nil {
		return nil, err
	}
	if !membership["group_id"] || !membership["recipient_id"] {
		return nil, fmt.Errorf("%w: group membership details are missing", ErrUnsupportedDatabase)
	}
	reaction, err := sqliteutil.TableColumns(ctx, db, "reaction")
	if err != nil {
		return nil, err
	}
	if !reaction["message_id"] || !reaction["author_id"] || !reaction["emoji"] || !reaction["date_sent"] {
		return nil, fmt.Errorf("%w: reaction details are missing", ErrUnsupportedDatabase)
	}
	mention, err := sqliteutil.TableColumns(ctx, db, "mention")
	if err != nil {
		return nil, err
	}
	if !mention["message_id"] || !mention["recipient_id"] || !mention["range_start"] {
		return nil, fmt.Errorf("%w: mention details are missing", ErrUnsupportedDatabase)
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
	if columns["shared_given_name"] && columns["shared_family_name"] {
		parts = append(parts, "NULLIF(TRIM(COALESCE(r.shared_given_name, '') || ' ' || COALESCE(r.shared_family_name, '')), '')")
	}
	return "COALESCE(" + strings.Join(append(parts, "''"), ", ") + ", '')"
}

func firstColumn(columns map[string]bool, names ...string) string {
	for _, name := range names {
		if columns[name] {
			return name
		}
	}
	return ""
}
