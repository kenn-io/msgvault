package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// CopyResult holds the summary of a subset copy operation.
type CopyResult struct {
	Messages      int64
	Conversations int64
	Participants  int64
	Labels        int64
	Sources       int64
	DBSize        int64
	Elapsed       time.Duration
}

// CopySubsetOptions controls optional sensitive metadata included in a subset.
type CopySubsetOptions struct {
	IncludeIdentity   bool
	IncludeAttributes bool
}

// CopySubset copies rowCount most recent messages (and all referenced
// data) from srcDBPath into a new database in dstDir. The destination
// schema is initialized using the embedded store schema.
//
// Identity policy: subsets are documented for sharing, so by default the
// participant boundary is message-derived — no participant, identifier,
// link edge, or person binding is copied for identities without selected
// messages. Link edges between included participants are preserved, and a
// durable person is copied only when every one of its bindings falls
// inside the subset (a partial profile under its original revision would
// misrepresent curated data). includeIdentity opts in to the full identity
// closure instead: participants are expanded through participant_links and
// shared person bindings until every included cluster and person profile
// is complete, which exposes identifiers of linked identities that have no
// messages in the subset. Person attribute definitions and values are not copied;
// callers sharing attributes must explicitly use CopySubsetWithOptions with
// IncludeAttributes. When attributes are included, person-valued references
// follow the same boundary: references to excluded people are omitted by
// default, while IncludeIdentity follows references from included people and
// copies each target's complete identity profile.
//
// Security: validates srcDBPath for control characters and canonicalizes
// it before use in SQL. Callers must validate path containment.
func CopySubset(
	srcDBPath, dstDir string, rowCount int, includeIdentity bool,
) (*CopyResult, error) {
	return CopySubsetWithOptions(srcDBPath, dstDir, rowCount, CopySubsetOptions{
		IncludeIdentity: includeIdentity,
	})
}

// CopySubsetWithOptions copies a subset with explicitly selected sensitive
// metadata. IncludeAttributes copies current and historical attribute values,
// including their value content, provenance references, and actor metadata.
func CopySubsetWithOptions(
	srcDBPath, dstDir string, rowCount int, options CopySubsetOptions,
) (*CopyResult, error) {
	if rowCount <= 0 {
		return nil, fmt.Errorf("rowCount must be positive, got %d", rowCount)
	}

	start := time.Now()

	dstDBPath := filepath.Join(dstDir, "msgvault.db")
	if _, err := os.Stat(dstDBPath); err == nil {
		return nil, fmt.Errorf(
			"destination database already exists: %s", dstDBPath,
		)
	}

	// Track whether we created the dir so cleanup only removes
	// what we made.
	createdDir := false
	if _, err := os.Stat(dstDir); os.IsNotExist(err) {
		createdDir = true
	}

	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return nil, fmt.Errorf("create destination directory: %w", err)
	}

	cleanup := func() {
		if createdDir {
			_ = os.RemoveAll(dstDir)
		} else {
			_ = os.Remove(dstDBPath)
			_ = os.Remove(dstDBPath + "-wal")
			_ = os.Remove(dstDBPath + "-shm")
		}
	}

	// Phase 1: create destination DB with schema
	st, err := Open(dstDBPath)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create destination database: %w", err)
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		cleanup()
		return nil, fmt.Errorf("initialize schema: %w", err)
	}
	if err := st.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close schema database: %w", err)
	}

	// Validate source path before opening destination DB, so
	// ATTACH doesn't silently create an empty file for a bad path.
	srcDBPath, err = filepath.Abs(filepath.Clean(srcDBPath))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("canonicalize source path: %w", err)
	}
	for _, r := range srcDBPath {
		if r < 0x20 || r == 0x7F {
			cleanup()
			return nil, fmt.Errorf(
				"source database path contains control character (0x%02X)", r,
			)
		}
	}
	if _, err := os.Stat(srcDBPath); err != nil {
		cleanup()
		return nil, fmt.Errorf("source database not found: %w", err)
	}

	// Phase 2: re-open with foreign keys OFF for bulk copy
	dsn := dstDBPath +
		"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=OFF"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("reopen database: %w", err)
	}

	// closeAndCleanup closes db before cleanup to ensure WAL/SHM
	// files are released before removal.
	closeAndCleanup := func() {
		_ = db.Close()
		cleanup()
	}

	escapedSrcPath := strings.ReplaceAll(srcDBPath, "'", "''")
	attachSQL := fmt.Sprintf(
		"ATTACH DATABASE '%s' AS src", escapedSrcPath,
	)
	if _, err := db.Exec(attachSQL); err != nil {
		closeAndCleanup()
		return nil, fmt.Errorf("attach source database: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		closeAndCleanup()
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	result, err := copyData(tx, rowCount, options)
	if err != nil {
		_ = tx.Rollback()
		_, _ = db.Exec("DETACH DATABASE src")
		closeAndCleanup()
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		_, _ = db.Exec("DETACH DATABASE src")
		closeAndCleanup()
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	// Detach source before post-copy operations so PRAGMA
	// foreign_key_check only scans the destination database.
	if _, err := db.Exec("DETACH DATABASE src"); err != nil {
		closeAndCleanup()
		return nil, fmt.Errorf("detach source database: %w", err)
	}

	if err := verifyForeignKeys(db); err != nil {
		closeAndCleanup()
		return nil, err
	}

	if err := updateConversationCounts(db); err != nil {
		closeAndCleanup()
		return nil, fmt.Errorf("update conversation counts: %w", err)
	}

	if ftsErr := populateFTS(db); ftsErr != nil {
		errMsg := ftsErr.Error()
		ftsUnavailable :=
			strings.HasSuffix(errMsg, "no such table: messages_fts") ||
				strings.HasSuffix(errMsg, "no such module: fts5")
		if !ftsUnavailable {
			fmt.Fprintf(
				os.Stderr,
				"warning: FTS index population failed: %v\n",
				ftsErr,
			)
		}
	}

	_ = db.Close()

	if info, err := os.Stat(dstDBPath); err == nil {
		result.DBSize = info.Size()
	}

	result.Elapsed = time.Since(start)
	return result, nil
}

// verifyForeignKeys runs PRAGMA foreign_key_check and returns an error
// if any violations are found.
func verifyForeignKeys(db *sql.DB) error {
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var violations []string
	for rows.Next() {
		var table, rowid, parent, fkid string
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			violations = append(violations,
				fmt.Sprintf("scan error: %v", err))
		} else {
			violations = append(violations,
				fmt.Sprintf("%s(rowid=%s) -> %s", table, rowid, parent))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate foreign key check: %w", err)
	}

	if len(violations) > 0 {
		return fmt.Errorf(
			"foreign key violations: %s",
			strings.Join(violations, "; "),
		)
	}
	return nil
}

// copyData executes INSERT INTO ... SELECT in dependency order.
func copyData(tx *sql.Tx, rowCount int, options CopySubsetOptions) (*CopyResult, error) {
	result := &CopyResult{}

	if _, err := tx.Exec(fmt.Sprintf(`
		CREATE TEMP TABLE selected_messages AS
		SELECT id FROM src.messages
		WHERE %s
		ORDER BY COALESCE(sent_at, received_at, internal_date)
			DESC, id DESC LIMIT ?`, LiveMessagesWhere("", true)), rowCount); err != nil {
		return nil, fmt.Errorf("select messages: %w", err)
	}

	// Try copying with oauth_app column first; fall back to NULL
	// for source databases created before this column existed.
	res, err := tx.Exec(`
		INSERT INTO sources
			(id, source_type, identifier, display_name, google_user_id,
			 last_sync_at, sync_cursor, sync_config, oauth_app,
			 created_at, updated_at)
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM src.sources
		WHERE id IN (
			SELECT DISTINCT source_id FROM src.messages
			WHERE id IN (SELECT id FROM selected_messages)
		)`)
	if err != nil && isSQLiteError(err, "no such column") {
		res, err = tx.Exec(`
			INSERT INTO sources
				(id, source_type, identifier, display_name, google_user_id,
				 last_sync_at, sync_cursor, sync_config, oauth_app,
				 created_at, updated_at)
			SELECT id, source_type, identifier, display_name, google_user_id,
			       last_sync_at, sync_cursor, sync_config, NULL,
			       created_at, updated_at
			FROM src.sources
			WHERE id IN (
				SELECT DISTINCT source_id FROM src.messages
				WHERE id IN (SELECT id FROM selected_messages)
			)`)
	}
	if err != nil {
		return nil, fmt.Errorf("copy sources: %w", err)
	}
	if result.Sources, err = res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("sources rows affected: %w", err)
	}

	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM selected_messages",
	).Scan(&result.Messages); err != nil {
		return nil, fmt.Errorf("count selected messages: %w", err)
	}

	res, err = tx.Exec(`
		INSERT INTO conversations SELECT * FROM src.conversations
		WHERE id IN (
			SELECT DISTINCT conversation_id FROM src.messages
			WHERE id IN (SELECT id FROM selected_messages)
		)`)
	if err != nil {
		return nil, fmt.Errorf("copy conversations: %w", err)
	}
	if result.Conversations, err = res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("conversations rows affected: %w", err)
	}

	res, err = tx.Exec(`
		INSERT INTO participants SELECT * FROM src.participants
		WHERE id IN (
			SELECT sender_id FROM src.messages
			WHERE id IN (SELECT id FROM selected_messages)
			UNION
			SELECT participant_id FROM src.message_recipients
			WHERE message_id IN (SELECT id FROM selected_messages)
			UNION
			SELECT participant_id FROM src.reactions
			WHERE message_id IN (SELECT id FROM selected_messages)
		)`)
	if err != nil {
		return nil, fmt.Errorf("copy participants: %w", err)
	}
	if result.Participants, err = res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("participants rows affected: %w", err)
	}

	// Identity policy (see CopySubset): by default the boundary stays
	// message-derived. With includeIdentity, expand the participant set
	// through the closure of link edges and shared person bindings so
	// every included identity cluster and person profile is complete —
	// components can pass through participants with no copied messages.
	if options.IncludeIdentity {
		res, err = tx.Exec(`
			INSERT INTO participants SELECT * FROM src.participants
			WHERE id IN (
				WITH RECURSIVE symmetric_edge(a, b) AS (
					SELECT participant_a, participant_b FROM src.participant_links
					UNION ALL
					SELECT pp1.participant_id, pp2.participant_id
					FROM src.person_participants pp1
					JOIN src.person_participants pp2
					  ON pp2.person_id = pp1.person_id
					 AND pp2.participant_id != pp1.participant_id
				), reference_edge(a, b) AS (
					SELECT owner_pp.participant_id, target_pp.participant_id
					FROM src.person_attribute_values value
					JOIN src.person_participants owner_pp
					  ON owner_pp.person_id = value.person_id
					JOIN src.person_participants target_pp
					  ON target_pp.person_id = value.value_record_id
					WHERE value.value_record_type = 'person'
					  AND ?
				), identity(id) AS (
					SELECT id FROM participants
					UNION
					SELECT CASE WHEN symmetric_edge.a = identity.id
					            THEN symmetric_edge.b ELSE symmetric_edge.a END
					FROM symmetric_edge
					JOIN identity
					  ON identity.id IN (symmetric_edge.a, symmetric_edge.b)
					UNION
					SELECT reference_edge.b
					FROM reference_edge
					JOIN identity ON identity.id = reference_edge.a
				)
				SELECT id FROM identity
			)
			  AND id NOT IN (SELECT id FROM participants)`, options.IncludeAttributes)
		if err != nil {
			return nil, fmt.Errorf("copy identity-closure participants: %w", err)
		}
		identityMates, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("identity-closure participants rows affected: %w", err)
		}
		result.Participants += identityMates
	}

	if _, err := tx.Exec(`
		INSERT INTO participant_links SELECT * FROM src.participant_links
		WHERE participant_a IN (SELECT id FROM participants)
		  AND participant_b IN (SELECT id FROM participants)`); err != nil {
		return nil, fmt.Errorf("copy participant_links: %w", err)
	}

	// Only complete profiles are copied: a person with any binding outside
	// the subset is skipped, because a partial binding set under the
	// original revision would misrepresent the curated profile. With
	// includeIdentity, the closure above already pulled every bound
	// participant in, so no touched person is skipped.
	if _, err := tx.Exec(`
		INSERT INTO persons
			(id, vcard_uid, display_name, revision, created_at, updated_at)
		SELECT p.id, p.vcard_uid, p.display_name, p.revision, p.created_at, p.updated_at
		FROM src.persons p
		WHERE EXISTS (
			SELECT 1 FROM src.person_participants pp
			WHERE pp.person_id = p.id
			  AND pp.participant_id IN (SELECT id FROM participants)
		)
		  AND NOT EXISTS (
			SELECT 1 FROM src.person_participants pp
			WHERE pp.person_id = p.id
			  AND pp.participant_id NOT IN (SELECT id FROM participants)
		)`); err != nil {
		return nil, fmt.Errorf("copy persons: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO person_participants (person_id, participant_id)
		SELECT person_id, participant_id
		FROM src.person_participants
		WHERE person_id IN (SELECT id FROM persons)
		  AND participant_id IN (SELECT id FROM participants)`); err != nil {
		return nil, fmt.Errorf("copy person_participants: %w", err)
	}

	if options.IncludeAttributes {
		// Definitions are portable by universal_id, not their database-local
		// numeric key. Reconcile every person definition into the destination,
		// then map copied values through universal_id below.
		if _, err := tx.Exec(`
		INSERT INTO attribute_definitions (
		    universal_id, object_type, slug, label, description,
		    value_type, field_type, record_target, cardinality, display_order,
		    is_required, ownership, ui_creatable, ui_editable, api_mutable,
		    is_searchable, is_audited, is_deletable, history_exempt,
		    derived_source, options, vcard_property, is_active, revision,
		    created_at, updated_at
		)
		SELECT
		    universal_id, object_type, slug, label, description,
		    value_type, field_type, record_target, cardinality, display_order,
		    is_required, ownership, ui_creatable, ui_editable, api_mutable,
		    is_searchable, is_audited, is_deletable, history_exempt,
		    derived_source, options, vcard_property, is_active, revision,
		    created_at, updated_at
		FROM src.attribute_definitions
		WHERE object_type = 'person'
		ON CONFLICT(universal_id) DO UPDATE SET
		    object_type = excluded.object_type,
		    slug = excluded.slug,
		    label = excluded.label,
		    description = excluded.description,
		    value_type = excluded.value_type,
		    field_type = excluded.field_type,
		    record_target = excluded.record_target,
		    cardinality = excluded.cardinality,
		    display_order = excluded.display_order,
		    is_required = excluded.is_required,
		    ownership = excluded.ownership,
		    ui_creatable = excluded.ui_creatable,
		    ui_editable = excluded.ui_editable,
		    api_mutable = excluded.api_mutable,
		    is_searchable = excluded.is_searchable,
		    is_audited = excluded.is_audited,
		    is_deletable = excluded.is_deletable,
		    history_exempt = excluded.history_exempt,
		    derived_source = excluded.derived_source,
		    options = excluded.options,
		    vcard_property = excluded.vcard_property,
		    is_active = excluded.is_active,
		    revision = excluded.revision,
		    created_at = excluded.created_at,
		    updated_at = excluded.updated_at`); err != nil {
			return nil, fmt.Errorf("copy person attribute definitions: %w", err)
		}

		// Preserve complete value history for copied people. Record references
		// only survive when their target person crossed the selected identity
		// boundary, preventing a subset from containing a dangling private ID.
		if _, err := tx.Exec(`
		INSERT INTO person_attribute_values (
		    id, person_id, definition_id, ordinal,
		    value_text, value_integer, value_real, value_boolean,
		    value_date, value_timestamp, value_json,
		    value_record_type, value_record_id,
		    active_from, active_until, created_at, superseded_at,
		    source, source_ref, confidence, actor
		)
		SELECT
		    value.id, value.person_id, destination_definition.id, value.ordinal,
		    value.value_text, value.value_integer, value.value_real,
		    value.value_boolean, value.value_date, value.value_timestamp,
		    value.value_json, value.value_record_type, value.value_record_id,
		    value.active_from, value.active_until, value.created_at,
		    value.superseded_at, value.source, value.source_ref,
		    value.confidence, value.actor
		FROM src.person_attribute_values value
		JOIN src.attribute_definitions source_definition
		  ON source_definition.id = value.definition_id
		JOIN attribute_definitions destination_definition
		  ON destination_definition.universal_id = source_definition.universal_id
		WHERE value.person_id IN (SELECT id FROM persons)
		  AND (
		    value.value_record_type IS NULL
		    OR (
		      value.value_record_type = 'person'
		      AND value.value_record_id IN (SELECT id FROM persons)
		    )
		  )`); err != nil {
			return nil, fmt.Errorf("copy person attribute values: %w", err)
		}
	}

	if _, err := tx.Exec(`
		INSERT INTO participant_identifiers
		SELECT * FROM src.participant_identifiers
		WHERE participant_id IN (SELECT id FROM participants)`); err != nil {
		return nil, fmt.Errorf("copy participant_identifiers: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO conversation_participants
		SELECT * FROM src.conversation_participants
		WHERE conversation_id IN (SELECT id FROM conversations)
		  AND participant_id IN (SELECT id FROM participants)`); err != nil {
		return nil, fmt.Errorf("copy conversation_participants: %w", err)
	}

	if err := copyMessages(tx); err != nil {
		return nil, err
	}

	// The copy names content_changed_at whenever the source has it, which
	// supplies the value explicitly and so bypasses the column's DEFAULT, and on
	// a database created from schema.sql there is no AFTER INSERT trigger behind
	// that default (the default is the whole INSERT-time writer there — see
	// EnsureTriggers). So a NULL watermark in the source lands in the subset as
	// a NULL watermark and nothing ever stamps it: the change feed's range
	// predicate excludes NULL, and InitSchema's `WHERE content_changed_at IS
	// NULL` backfill already ran on this database while it was empty and is
	// recorded as applied, so it will not run again. The row would be invisible
	// to the feed for the life of the archive.
	//
	// Normally this updates nothing — every write path stamps the column, and
	// the source's own migration filled it. It is the copy that has to be
	// closed, not the writers: this statement is the only thing standing
	// between a single NULL anywhere upstream and a permanently unreportable
	// message.
	//
	// It names only content_changed_at, so no trigger on `messages` fires here:
	// the content-change trigger is UPDATE OF the content columns, and the
	// last_modified trigger is UPDATE OF every column except this one (see
	// lastModifiedUpdateOfColumns).
	//
	// It is not, however, what decides the watermarks of a message that has a
	// body. The `INSERT INTO message_bodies` further down fires two triggers
	// that write the parent row directly rather than reacting to an UPDATE of
	// it: trg_message_bodies_content_changed_ins, and schema.sql's pre-existing
	// trg_message_bodies_last_modified_ins. So a copied message WITH a body
	// leaves this function with both content_changed_at and last_modified set
	// to the time of the copy; only a bodyless one keeps the source's values.
	// (Measured: a source row stamped 2001-02-03 04:05:06 arrives copy-stamped
	// when it has a body and unchanged when it does not.)
	//
	// That is left alone rather than worked around. last_modified has behaved
	// this way for as long as those body triggers have existed, and
	// content_changed_at now simply matches it. Neither column promises to
	// survive a copy: a subset is a new archive, its feed consumers start from
	// an empty cursor, and all they require of the watermark is that it is
	// non-NULL and in the single textual shape the lexical cursor can order —
	// which a copy-time stamp and a copied source value both satisfy.
	// TestCopySubset_BodyTriggersRestampWatermarks pins the behaviour.
	if _, err := tx.Exec(fmt.Sprintf(
		`UPDATE messages SET content_changed_at = %s WHERE content_changed_at IS NULL`,
		(&SQLiteDialect{}).ContentChangedNow())); err != nil {
		return nil, fmt.Errorf("stamp missing content_changed_at watermarks: %w", err)
	}

	// Null out reply_to_message_id when the parent message wasn't
	// selected, to avoid FK violations from dangling references.
	if _, err := tx.Exec(`
		UPDATE messages SET reply_to_message_id = NULL
		WHERE reply_to_message_id IS NOT NULL
		  AND reply_to_message_id NOT IN (
			SELECT id FROM messages
		)`); err != nil {
		return nil, fmt.Errorf("clear orphan reply refs: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO message_bodies SELECT * FROM src.message_bodies
		WHERE message_id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("copy message_bodies: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO message_raw SELECT * FROM src.message_raw
		WHERE message_id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("copy message_raw: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO message_recipients
		SELECT * FROM src.message_recipients
		WHERE message_id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("copy message_recipients: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO reactions SELECT * FROM src.reactions
		WHERE message_id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("copy reactions: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO attachments SELECT * FROM src.attachments
		WHERE message_id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("copy attachments: %w", err)
	}

	res, err = tx.Exec(`
		INSERT INTO labels SELECT * FROM src.labels
		WHERE source_id IN (SELECT id FROM sources)
		   OR id IN (
			SELECT label_id FROM src.message_labels
			WHERE message_id IN (SELECT id FROM selected_messages)
		)`)
	if err != nil {
		return nil, fmt.Errorf("copy labels: %w", err)
	}
	if result.Labels, err = res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("labels rows affected: %w", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO message_labels SELECT * FROM src.message_labels
		WHERE message_id IN (SELECT id FROM selected_messages)
		  AND label_id IN (SELECT id FROM labels)`); err != nil {
		return nil, fmt.Errorf("copy message_labels: %w", err)
	}

	if _, err := tx.Exec(
		"DROP TABLE IF EXISTS selected_messages",
	); err != nil {
		return nil, fmt.Errorf("drop temp table: %w", err)
	}

	return result, nil
}

// errNoCommonMessageColumns reports that the source and destination messages
// tables have no column name in common, so there is nothing to copy.
var errNoCommonMessageColumns = errors.New(
	"source and destination share no messages columns")

// copyMessages copies the selected messages, naming the columns the source and
// destination have in common, read from the two schemas at copy time.
//
// A column the source lacks is left out of the INSERT, so the destination's own
// DEFAULT, where it has one, supplies it. Columns the source has and the
// destination does not are dropped.
func copyMessages(tx *sql.Tx) error {
	cols, err := commonColumns(tx, "messages")
	if err != nil {
		return fmt.Errorf("copy messages: %w", err)
	}
	if len(cols) == 0 {
		return fmt.Errorf("copy messages: %w", errNoCommonMessageColumns)
	}
	list := strings.Join(cols, ", ")
	if _, err := tx.Exec(fmt.Sprintf(`
		INSERT INTO messages (%s) SELECT %s FROM src.messages
		WHERE id IN (SELECT id FROM selected_messages)`, list, list)); err != nil {
		return fmt.Errorf("copy messages: %w", err)
	}
	return nil
}

// commonColumns returns the quoted names of the columns `table` has in both the
// destination (main) and the attached source, in destination declaration order.
// Names are matched the way SQLite matches identifiers — case-insensitively
// over ASCII only, see foldIdentifier — and the destination's spelling is
// emitted for both sides of the copy.
//
// A returned name is interpolated into the copy's SQL, so it goes through
// quoteIdentifier — which quotes it and doubles any quote inside it. Names
// outside the intersection are not interpolated and so are not rendered.
func commonColumns(tx *sql.Tx, table string) ([]string, error) {
	dst, err := schemaColumns(tx, "main", table)
	if err != nil {
		return nil, err
	}
	src, err := schemaColumns(tx, "src", table)
	if err != nil {
		return nil, err
	}
	inSrc := make(map[string]struct{}, len(src))
	for _, name := range src {
		inSrc[foldIdentifier(name)] = struct{}{}
	}
	common := make([]string, 0, len(dst))
	for _, name := range dst {
		if _, ok := inSrc[foldIdentifier(name)]; !ok {
			continue
		}
		common = append(common, quoteIdentifier(name))
	}
	return common, nil
}

// foldIdentifier folds an unquoted-identifier name to the form SQLite compares
// on: A-Z map to a-z and every other byte is left alone.
//
// Do not "simplify" this to strings.ToLower. That applies Unicode case
// conversion, which is strictly broader than SQLite's, whose built-in collating
// sequences fold only ASCII (see sqlite3_stricmp). Go lowers, for example,
// "İdentity_is_from_me" (U+0130, capital I with dot above) to
// "identity_is_from_me", which SQLite treats as a different identifier. A
// source-only column so named would then be misclassified as common to both
// schemas, and the copy would name a column src.messages does not have. With
// SQLite's default double-quoted-string misfeature the quoted name degrades to
// a string literal and the copy silently stores that text in the destination
// column instead of its DEFAULT; with SQLITE_DQS=0 the copy fails outright.
func foldIdentifier(name string) string {
	folded := []byte(name)
	for i, c := range folded {
		if c >= 'A' && c <= 'Z' {
			folded[i] = c + ('a' - 'A')
		}
	}
	return string(folded)
}

// schemaColumns lists a table's column names in declaration order.
func schemaColumns(tx *sql.Tx, schema, table string) ([]string, error) {
	rows, err := tx.Query(
		`SELECT name FROM pragma_table_info(?, ?) ORDER BY cid`, table, schema)
	if err != nil {
		return nil, fmt.Errorf("list %s.%s columns: %w", schema, table, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s.%s column: %w", schema, table, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s.%s columns: %w", schema, table, err)
	}
	return names, nil
}

// updateConversationCounts updates the denormalized counts on
// conversations to be consistent with the copied subset.
func updateConversationCounts(db *sql.DB) error {
	_, err := db.Exec(`
		UPDATE conversations SET
			message_count = (
				SELECT COUNT(*) FROM messages
				WHERE conversation_id = conversations.id
			),
			participant_count = (
				SELECT COUNT(*) FROM conversation_participants
				WHERE conversation_id = conversations.id
			),
			last_message_at = (
				SELECT MAX(COALESCE(sent_at, received_at, internal_date))
				FROM messages
				WHERE conversation_id = conversations.id
			)`)
	return err
}

// populateFTS rebuilds the FTS5 index from the copied data.
func populateFTS(db *sql.DB) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO messages_fts(
			rowid, message_id, subject, body,
			from_addr, to_addr, cc_addr
		)
		SELECT m.id, m.id, COALESCE(m.subject, ''),
			COALESCE(mb.body_text, ''),
			COALESCE(
				CASE WHEN m.message_type != 'email' AND m.message_type IS NOT NULL AND m.message_type != ''
				     THEN (SELECT COALESCE(p.phone_number, p.email_address) FROM participants p WHERE p.id = m.sender_id)
				END,
				(SELECT GROUP_CONCAT(p.email_address, ' ')
				 FROM message_recipients mr
				 JOIN participants p ON p.id = mr.participant_id
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = 'from'),
				''
			),
			COALESCE((
				SELECT GROUP_CONCAT(p.email_address, ' ')
				FROM message_recipients mr
				JOIN participants p ON p.id = mr.participant_id
				WHERE mr.message_id = m.id
				  AND mr.recipient_type = 'to'
			), ''),
			COALESCE((
				SELECT GROUP_CONCAT(p.email_address, ' ')
				FROM message_recipients mr
				JOIN participants p ON p.id = mr.participant_id
				WHERE mr.message_id = m.id
				  AND mr.recipient_type = 'cc'
			), '')
		FROM messages m
		LEFT JOIN message_bodies mb ON mb.message_id = m.id`)
	return err
}
