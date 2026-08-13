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
	IncludeProfiles   bool
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
// shared person bindings until every included cluster and person binding set
// is complete, which exposes identifiers of linked identities that have no
// messages in the subset. Structured profile values and their provenance
// dependencies require the separate IncludeProfiles opt-in. Person attribute
// definitions and values also require callers to explicitly use
// CopySubsetWithOptions with IncludeAttributes. When attributes are included, person-valued references
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
// metadata. IncludeAttributes copies current and historical attribute values —
// person- and organization-scoped — including their value content, provenance
// references, and actor metadata. IncludeProfiles copies current and
// historical structured profile values, media, contact observations,
// identity-review candidates and evidence, relationship types and edges
// between copied persons with their decision ledger, their provenance
// dependencies, and employment history: employments of copied people together
// with the organizations they reference and those organizations' profile
// rows. Organization attribute values ride IncludeAttributes but only exist
// in the subset when IncludeProfiles also ran, because the organizations
// themselves cross the boundary through employment references.
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
	if _, err := tx.Exec(`CREATE TEMP TABLE selected_message_sources AS
		SELECT DISTINCT source_id FROM src.messages
		WHERE id IN (SELECT id FROM selected_messages)`); err != nil {
		return nil, fmt.Errorf("select message sources: %w", err)
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
		WHERE id IN (SELECT source_id FROM selected_message_sources)`)
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
			WHERE id IN (SELECT source_id FROM selected_message_sources)`)
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

	res, err = copyByName(tx, "conversations", `id IN (
			SELECT DISTINCT conversation_id FROM src.messages
			WHERE id IN (SELECT id FROM selected_messages)
		)`)
	if err != nil {
		return nil, fmt.Errorf("copy conversations: %w", err)
	}
	if result.Conversations, err = res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("conversations rows affected: %w", err)
	}

	res, err = copyByName(tx, "participants", `id IN (
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
		// Reference edges follow person-valued attribute record references out
		// of the included identity set. Organization attributes reached through
		// an included person's employments contribute the same kind of edge,
		// but only on sources whose schema has those tables, and only when the
		// organizations themselves cross the boundary (IncludeProfiles) with
		// attributes opted in.
		referenceEdges := `SELECT owner_pp.participant_id, target_pp.participant_id
					FROM src.person_attribute_values value
					JOIN src.person_participants owner_pp
					  ON owner_pp.person_id = value.person_id
					JOIN src.person_participants target_pp
					  ON target_pp.person_id = value.value_record_id
					WHERE value.value_record_type = 'person'
					  AND ?`
		args := []any{options.IncludeAttributes}
		hasEmployments, err := sourceTableExists(tx, "employments")
		if err != nil {
			return nil, fmt.Errorf("check employment schema: %w", err)
		}
		hasOrganizationValues, err := sourceTableExists(tx, "organization_attribute_values")
		if err != nil {
			return nil, fmt.Errorf("check organization attribute schema: %w", err)
		}
		if hasEmployments && hasOrganizationValues {
			referenceEdges += `
					UNION ALL
					SELECT owner_pp.participant_id, target_pp.participant_id
					FROM src.employments employment
					JOIN src.person_participants owner_pp
					  ON owner_pp.person_id = employment.person_id
					JOIN src.organization_attribute_values value
					  ON value.organization_id = employment.organization_id
					JOIN src.person_participants target_pp
					  ON target_pp.person_id = value.value_record_id
					WHERE value.value_record_type = 'person'
					  AND ?`
			args = append(args, options.IncludeAttributes && options.IncludeProfiles)
		}
		res, err = copyByName(tx, "participants", `id IN (
				WITH RECURSIVE symmetric_edge(a, b) AS (
					SELECT participant_a, participant_b FROM src.participant_links
					UNION ALL
					SELECT pp1.participant_id, pp2.participant_id
					FROM src.person_participants pp1
					JOIN src.person_participants pp2
					  ON pp2.person_id = pp1.person_id
					 AND pp2.participant_id != pp1.participant_id
				), reference_edge(a, b) AS (
					`+referenceEdges+`
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
			  AND id NOT IN (SELECT id FROM participants)`, args...)
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
	if err := reconcileSubsetCommunicationServices(tx, options.IncludeProfiles); err != nil {
		return nil, err
	}

	if options.IncludeProfiles {
		extraSources, err := copyStructuredProfiles(tx)
		if err != nil {
			return nil, err
		}
		result.Sources += extraSources
		if err := copySubsetRelationships(tx); err != nil {
			return nil, err
		}
		if err := copyEmploymentData(tx); err != nil {
			return nil, err
		}
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
		WHERE object_type IN ('person', 'organization')
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

		// Organization attribute values follow the same universal_id mapping and
		// record-reference boundary. The organizations themselves only cross the
		// subset boundary through employment references (IncludeProfiles), so
		// without that opt-in this copy is vacuous.
		hasOrganizationValues, err := sourceTableExists(tx, "organization_attribute_values")
		if err != nil {
			return nil, fmt.Errorf("check organization attribute schema: %w", err)
		}
		if hasOrganizationValues {
			if _, err := tx.Exec(`
			INSERT INTO organization_attribute_values (
			    id, organization_id, definition_id, ordinal,
			    value_text, value_integer, value_real, value_boolean,
			    value_date, value_timestamp, value_json,
			    value_record_type, value_record_id,
			    active_from, active_until, created_at, superseded_at,
			    source, source_ref, confidence, actor
			)
			SELECT
			    value.id, value.organization_id, destination_definition.id,
			    value.ordinal, value.value_text, value.value_integer,
			    value.value_real, value.value_boolean, value.value_date,
			    value.value_timestamp, value.value_json, value.value_record_type,
			    value.value_record_id, value.active_from, value.active_until,
			    value.created_at, value.superseded_at, value.source,
			    value.source_ref, value.confidence, value.actor
			FROM src.organization_attribute_values value
			JOIN src.attribute_definitions source_definition
			  ON source_definition.id = value.definition_id
			JOIN attribute_definitions destination_definition
			  ON destination_definition.universal_id = source_definition.universal_id
			WHERE value.organization_id IN (SELECT id FROM organizations)
			  AND (
			    value.value_record_type IS NULL
			    OR (
			      value.value_record_type = 'person'
			      AND value.value_record_id IN (SELECT id FROM persons)
			    )
			  )`); err != nil {
				return nil, fmt.Errorf("copy organization attribute values: %w", err)
			}
		}
	}

	if err := copyByNameWithCommunicationServiceMap(tx, "participant_identifiers",
		`participant_id IN (SELECT id FROM participants)`); err != nil {
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

	if _, err := copyByName(tx, "message_recipients",
		`message_id IN (SELECT id FROM selected_messages)`); err != nil {
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

	res, err = copyByName(tx, "labels", `source_id IN (SELECT source_id FROM selected_message_sources)
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
	if _, err := tx.Exec(
		"DROP TABLE IF EXISTS selected_message_sources",
	); err != nil {
		return nil, fmt.Errorf("drop source temp table: %w", err)
	}

	return result, nil
}

type subsetServiceReference struct {
	table string
	where string
}

const subsetSourceIdentityMatchCandidateWhere = `(
	(left_kind = 'participant' AND left_id IN (SELECT id FROM participants))
	OR (left_kind = 'person' AND left_id IN (SELECT id FROM persons))
	OR (left_kind = 'observation' AND left_id IN (
		SELECT id FROM src.participant_contact_observations
		WHERE participant_id IN (SELECT id FROM participants)
	))
	OR (left_kind = 'contact_point' AND left_id IN (
		SELECT id FROM src.person_contact_points
		WHERE person_id IN (SELECT id FROM persons)
	))
) AND (
	(right_kind = 'participant' AND right_id IN (SELECT id FROM participants))
	OR (right_kind = 'person' AND right_id IN (SELECT id FROM persons))
	OR (right_kind = 'observation' AND right_id IN (
		SELECT id FROM src.participant_contact_observations
		WHERE participant_id IN (SELECT id FROM participants)
	))
	OR (right_kind = 'contact_point' AND right_id IN (
		SELECT id FROM src.person_contact_points
		WHERE person_id IN (SELECT id FROM persons)
	))
)`

// reconcileSubsetCommunicationServices copies every service referenced by a
// row that will cross the subset boundary. Service IDs are database-local, so
// the destination resolves them through the immutable slug and records an
// explicit source-to-destination map for the dependent row copies.
func reconcileSubsetCommunicationServices(tx *sql.Tx, includeProfiles bool) error {
	if _, err := tx.Exec(`CREATE TEMP TABLE selected_profile_services (
		source_id INTEGER PRIMARY KEY
	)`); err != nil {
		return fmt.Errorf("create selected profile services: %w", err)
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE selected_profile_service_map (
		source_id INTEGER PRIMARY KEY,
		destination_id INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create profile service map: %w", err)
	}

	references := []subsetServiceReference{
		{table: "participant_identifiers", where: `participant_id IN (SELECT id FROM participants)`},
	}
	if includeProfiles {
		references = append(references,
			subsetServiceReference{
				table: "person_contact_points",
				where: `person_id IN (SELECT id FROM persons)`,
			},
			subsetServiceReference{
				table: "participant_contact_observations",
				where: `participant_id IN (SELECT id FROM participants)`,
			},
			subsetServiceReference{
				table: "identity_match_candidates",
				where: subsetSourceIdentityMatchCandidateWhere,
			},
		)
		hasEmployments, err := sourceTableExists(tx, "employments")
		if err != nil {
			return fmt.Errorf("check employment schema: %w", err)
		}
		if hasEmployments {
			references = append(references, subsetServiceReference{
				table: "organization_contact_points",
				where: `organization_id IN (
					SELECT DISTINCT organization_id FROM src.employments
					WHERE person_id IN (SELECT id FROM persons)
				)`,
			})
		}
	}
	for _, reference := range references {
		hasServiceID, err := sourceColumnExists(tx, reference.table, "service_id")
		if err != nil {
			return fmt.Errorf("check %s service column: %w", reference.table, err)
		}
		if !hasServiceID {
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO selected_profile_services (source_id)
			SELECT service_id FROM src.` + reference.table + `
			WHERE ` + reference.where + ` AND service_id IS NOT NULL`); err != nil {
			return fmt.Errorf("select %s services: %w", reference.table, err)
		}
	}

	hasServices, err := sourceTableExists(tx, "communication_services")
	if err != nil {
		return fmt.Errorf("check communication service schema: %w", err)
	}
	if !hasServices {
		var selected int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM selected_profile_services`).Scan(&selected); err != nil {
			return fmt.Errorf("count referenced communication services: %w", err)
		}
		if selected != 0 {
			return errors.New("copy communication services: source catalog is missing")
		}
		return nil
	}
	var missing int
	if err := tx.QueryRow(`SELECT COUNT(*)
		FROM selected_profile_services selected
		LEFT JOIN src.communication_services service ON service.id = selected.source_id
		WHERE service.id IS NULL`).Scan(&missing); err != nil {
		return fmt.Errorf("check referenced communication services: %w", err)
	}
	if missing != 0 {
		return fmt.Errorf("copy communication services: %d referenced services are missing", missing)
	}

	if _, err := tx.Exec(`INSERT INTO communication_services (
			slug, display_label, scope_policy, default_scope_kind,
			normalization, normalization_version, uri_scheme,
			profile_url_template, is_system, is_active, created_at, updated_at
		)
		SELECT service.slug, service.display_label, service.scope_policy,
			service.default_scope_kind, service.normalization,
			service.normalization_version, service.uri_scheme,
			service.profile_url_template, service.is_system, service.is_active,
			service.created_at, service.updated_at
		FROM src.communication_services service
		JOIN selected_profile_services selected ON selected.source_id = service.id
		ON CONFLICT(slug) DO UPDATE SET
			display_label = excluded.display_label,
			scope_policy = excluded.scope_policy,
			default_scope_kind = excluded.default_scope_kind,
			normalization = excluded.normalization,
			normalization_version = excluded.normalization_version,
			uri_scheme = excluded.uri_scheme,
			profile_url_template = excluded.profile_url_template,
			is_system = excluded.is_system,
			is_active = excluded.is_active,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at`); err != nil {
		return fmt.Errorf("copy communication services: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO selected_profile_service_map (source_id, destination_id)
		SELECT source.id, destination.id
		FROM src.communication_services source
		JOIN selected_profile_services selected ON selected.source_id = source.id
		JOIN communication_services destination ON destination.slug = source.slug`); err != nil {
		return fmt.Errorf("map communication services: %w", err)
	}

	hasAliases, err := sourceTableExists(tx, "communication_service_aliases")
	if err != nil {
		return fmt.Errorf("check communication service alias schema: %w", err)
	}
	if hasAliases {
		if _, err := tx.Exec(`INSERT INTO communication_service_aliases (alias, service_id)
			SELECT alias.alias, service_map.destination_id
			FROM src.communication_service_aliases alias
			JOIN selected_profile_service_map service_map
			  ON service_map.source_id = alias.service_id
			ON CONFLICT(alias) DO UPDATE SET service_id = excluded.service_id`); err != nil {
			return fmt.Errorf("copy communication service aliases: %w", err)
		}
	}
	return nil
}

// copySubsetRelationships mirrors the source's relationship type catalog and
// copies the curated relationship data of copied persons. Types reconcile
// through universal_id because the destination schema is seeded with its own
// local ids; edges are copied only when both endpoints crossed the subset
// boundary; and the decision ledger keeps its rows, so a re-import into the
// subset honors the same decisions the source recorded — including deletion
// tombstones. References that did not cross the boundary (a matched person or
// accepted edge outside the subset) are cleared exactly as the live schema
// clears them on deletion.
func copySubsetRelationships(tx *sql.Tx) error {
	hasRelationships, err := sourceTableExists(tx, "relationship_types")
	if err != nil {
		return fmt.Errorf("check relationship schema: %w", err)
	}
	if !hasRelationships {
		return nil
	}
	// The destination holds only its own freshly seeded rows with nothing
	// referencing them, so clearing every vCard mapping first lets the
	// mirrored source values land without transient unique collisions (the
	// source may have moved a mapping between types).
	if _, err := tx.Exec(`UPDATE relationship_types SET vcard_related_type = NULL`); err != nil {
		return fmt.Errorf("clear destination relationship type mappings: %w", err)
	}
	// inverse_type_id is remapped inline because SQLite evaluates CHECK
	// constraints on the candidate row before conflict resolution: the
	// non-canonical 'child' candidate needs a non-NULL inverse even when it
	// resolves to the DO UPDATE path.
	if _, err := tx.Exec(`
		INSERT INTO relationship_types (
		    universal_id, slug, forward_label, reverse_label,
		    is_symmetric, is_canonical, inverse_type_id, vcard_related_type,
		    color, icon, description, ownership, is_deletable,
		    revision, created_at, updated_at
		)
		SELECT
		    source_type.universal_id, source_type.slug,
		    source_type.forward_label, source_type.reverse_label,
		    source_type.is_symmetric, source_type.is_canonical,
		    (
		        SELECT destination_inverse.id
		        FROM src.relationship_types source_inverse
		        JOIN relationship_types destination_inverse
		          ON destination_inverse.universal_id = source_inverse.universal_id
		        WHERE source_inverse.id = source_type.inverse_type_id
		    ),
		    source_type.vcard_related_type,
		    source_type.color, source_type.icon, source_type.description,
		    source_type.ownership, source_type.is_deletable,
		    source_type.revision, source_type.created_at, source_type.updated_at
		FROM src.relationship_types source_type
		WHERE TRUE
		ON CONFLICT(universal_id) DO UPDATE SET
		    slug = excluded.slug,
		    forward_label = excluded.forward_label,
		    reverse_label = excluded.reverse_label,
		    is_symmetric = excluded.is_symmetric,
		    is_canonical = excluded.is_canonical,
		    vcard_related_type = excluded.vcard_related_type,
		    color = excluded.color,
		    icon = excluded.icon,
		    description = excluded.description,
		    ownership = excluded.ownership,
		    is_deletable = excluded.is_deletable,
		    revision = excluded.revision,
		    created_at = excluded.created_at,
		    updated_at = excluded.updated_at`); err != nil {
		return fmt.Errorf("copy relationship types: %w", err)
	}
	// inverse_type_id is a database-local key; relink it through universal_id
	// once every type row exists. Destination-only seeds (absent from an older
	// source) keep their own links.
	if _, err := tx.Exec(`
		UPDATE relationship_types
		SET inverse_type_id = (
		    SELECT destination_inverse.id
		    FROM src.relationship_types source_type
		    JOIN src.relationship_types source_inverse
		      ON source_inverse.id = source_type.inverse_type_id
		    JOIN relationship_types destination_inverse
		      ON destination_inverse.universal_id = source_inverse.universal_id
		    WHERE source_type.universal_id = relationship_types.universal_id
		)
		WHERE EXISTS (
		    SELECT 1 FROM src.relationship_types source_type
		    WHERE source_type.universal_id = relationship_types.universal_id
		)`); err != nil {
		return fmt.Errorf("link relationship type inverses: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO person_relationships (
		    id, source_person_id, target_person_id, relationship_type_id,
		    start_year, start_month, start_day, end_year, end_month, end_day,
		    status, notes, source, source_ref, confidence,
		    vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid,
		    created_by, updated_by, revision, created_at, updated_at
		)
		SELECT
		    edge.id, edge.source_person_id, edge.target_person_id,
		    destination_type.id,
		    edge.start_year, edge.start_month, edge.start_day,
		    edge.end_year, edge.end_month, edge.end_day,
		    edge.status, edge.notes, edge.source, edge.source_ref, edge.confidence,
		    edge.vcard_property, edge.vcard_group, edge.vcard_prop_id,
		    edge.vcard_pid, edge.vcard_altid,
		    edge.created_by, edge.updated_by, edge.revision,
		    edge.created_at, edge.updated_at
		FROM src.person_relationships edge
		JOIN src.relationship_types source_type
		  ON source_type.id = edge.relationship_type_id
		JOIN relationship_types destination_type
		  ON destination_type.universal_id = source_type.universal_id
		WHERE edge.source_person_id IN (SELECT id FROM persons)
		  AND edge.target_person_id IN (SELECT id FROM persons)`); err != nil {
		return fmt.Errorf("copy person relationships: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO person_relationship_reviews (
		    id, person_id, raw_related_value, raw_related_type, value_kind,
		    matched_person_id, accepted_relationship_id, status, source,
		    source_ref, vcard_property, vcard_group, vcard_prop_id, vcard_pid,
		    vcard_altid, created_by, reviewed_by, reviewed_at,
		    created_at, updated_at
		)
		SELECT
		    review.id, review.person_id, review.raw_related_value,
		    review.raw_related_type, review.value_kind,
		    CASE WHEN review.matched_person_id IN (SELECT id FROM persons)
		         THEN review.matched_person_id END,
		    CASE WHEN review.accepted_relationship_id IN (SELECT id FROM person_relationships)
		         THEN review.accepted_relationship_id END,
		    review.status, review.source, review.source_ref,
		    review.vcard_property, review.vcard_group, review.vcard_prop_id,
		    review.vcard_pid, review.vcard_altid,
		    review.created_by, review.reviewed_by, review.reviewed_at,
		    review.created_at, review.updated_at
		FROM src.person_relationship_reviews review
		WHERE review.person_id IN (SELECT id FROM persons)`); err != nil {
		return fmt.Errorf("copy person relationship reviews: %w", err)
	}
	return nil
}

// copyEmploymentData copies the employments of copied people together with
// the organizations those employments reference and the organizations'
// profile rows. Employment history is curated person data, so it rides the
// same IncludeProfiles opt-in as structured person profiles. Employments
// cannot reference merged organizations, so the copied organizations never
// need a merge-redirect closure.
func copyEmploymentData(tx *sql.Tx) error {
	hasEmployments, err := sourceTableExists(tx, "employments")
	if err != nil {
		return fmt.Errorf("check employment schema: %w", err)
	}
	if !hasEmployments {
		return nil
	}
	if _, err := copyByName(tx, "organizations", `id IN (
			SELECT DISTINCT organization_id FROM src.employments
			WHERE person_id IN (SELECT id FROM persons)
		)`); err != nil {
		return fmt.Errorf("copy organizations: %w", err)
	}
	for _, table := range []string{
		"organization_names", "organization_identifiers",
		"organization_addresses", "organization_media",
		"organization_categories",
	} {
		if _, err := copyByName(tx, table,
			`organization_id IN (SELECT id FROM organizations)`); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	if err := copyByNameWithCommunicationServiceMap(tx, "organization_contact_points",
		`organization_id IN (SELECT id FROM organizations)`); err != nil {
		return fmt.Errorf("copy organization_contact_points: %w", err)
	}
	if _, err := copyByName(tx, "employments",
		`person_id IN (SELECT id FROM persons)`); err != nil {
		return fmt.Errorf("copy employments: %w", err)
	}
	return nil
}

func copyStructuredProfiles(tx *sql.Tx) (int64, error) {
	hasProfiles, err := sourceTableExists(tx, "person_names")
	if err != nil {
		return 0, fmt.Errorf("check structured profile schema: %w", err)
	}
	if !hasProfiles {
		return 0, nil
	}

	// Observations keep their source foreign key, even when that source had no
	// selected message. A complete copied profile must retain that provenance.
	sourceResult, err := copyByName(tx, "sources", `id IN (
			SELECT DISTINCT source_id FROM src.participant_contact_observations
			WHERE participant_id IN (SELECT id FROM participants)
			  AND source_id IS NOT NULL
		) AND id NOT IN (SELECT id FROM sources)`)
	if err != nil {
		return 0, fmt.Errorf("copy structured profile sources: %w", err)
	}
	extraSources, err := sourceResult.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("structured profile sources rows affected: %w", err)
	}

	for _, table := range []string{
		"person_names", "person_addresses",
		"person_dates", "person_categories", "person_media",
	} {
		if _, err := copyByName(tx, table, `person_id IN (SELECT id FROM persons)`); err != nil {
			return 0, fmt.Errorf("copy %s: %w", table, err)
		}
	}
	if err := copyByNameWithCommunicationServiceMap(
		tx, "person_contact_points", `person_id IN (SELECT id FROM persons)`,
	); err != nil {
		return 0, fmt.Errorf("copy person_contact_points: %w", err)
	}
	if err := copyByNameWithCommunicationServiceMap(tx, "participant_contact_observations",
		`participant_id IN (SELECT id FROM participants)`); err != nil {
		return 0, fmt.Errorf("copy participant_contact_observations: %w", err)
	}
	hasCandidates, err := sourceTableExists(tx, "identity_match_candidates")
	if err != nil {
		return 0, fmt.Errorf("check identity match candidate schema: %w", err)
	}
	if hasCandidates {
		if err := copyByNameWithCommunicationServiceMap(
			tx, "identity_match_candidates", subsetSourceIdentityMatchCandidateWhere,
		); err != nil {
			return 0, fmt.Errorf("copy identity_match_candidates: %w", err)
		}
		hasEvidence, err := sourceTableExists(tx, "identity_match_evidence")
		if err != nil {
			return 0, fmt.Errorf("check identity match evidence schema: %w", err)
		}
		if hasEvidence {
			if _, err := copyByName(tx, "identity_match_evidence",
				`candidate_id IN (SELECT id FROM identity_match_candidates)`); err != nil {
				return 0, fmt.Errorf("copy identity_match_evidence: %w", err)
			}
		}
	}
	return extraSources, nil
}

// copyMessages copies the selected messages, naming the columns the source and
// destination have in common, read from the two schemas at copy time.
func copyMessages(tx *sql.Tx) error {
	if _, err := copyByName(tx, "messages",
		`id IN (SELECT id FROM selected_messages)`); err != nil {
		return fmt.Errorf("copy messages: %w", err)
	}
	return nil
}

// copyByName copies the rows of src.<table> satisfying the where clause into
// the destination table, naming the columns the source and destination have in
// common, read from the two schemas at copy time (see commonColumns).
//
// Naming the columns keeps the copy independent of declaration order: on a
// database upgraded by the legacy ALTER TABLE ADD COLUMN migrations a
// late-added column (labels.system_role, participants.phone_number,
// conversations.title, ...) sits at the end of the table, while a fresh
// schema.sql database declares it mid-table, so a positional SELECT * copy
// from one into the other lands values in the wrong columns. A column the
// source lacks is left out of the INSERT, so the destination's own DEFAULT,
// where it has one, supplies it. Columns the source has and the destination
// does not are dropped.
func copyByName(tx *sql.Tx, table, where string, args ...any) (sql.Result, error) {
	cols, err := commonColumns(tx, table)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf(
			"source and destination share no %s columns", table)
	}
	list := strings.Join(cols, ", ")
	res, err := tx.Exec(fmt.Sprintf(`
		INSERT INTO %s (%s) SELECT %s FROM src.%s
		WHERE %s`, table, list, list, table, where), args...)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func copyByNameWithCommunicationServiceMap(
	tx *sql.Tx, table, where string,
) error {
	cols, err := commonColumns(tx, table)
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		return fmt.Errorf(
			"source and destination share no %s columns", table)
	}
	selectExpressions := make([]string, len(cols))
	serviceColumn := quoteIdentifier("service_id")
	hasServiceColumn := false
	for index, column := range cols {
		selectExpressions[index] = "source_row." + column
		if column == serviceColumn {
			hasServiceColumn = true
			selectExpressions[index] = `CASE
				WHEN source_row.` + serviceColumn + ` IS NULL THEN NULL
				ELSE service_map.destination_id
			END`
		}
	}
	if !hasServiceColumn {
		_, err := copyByName(tx, table, where)
		return err
	}
	_, err = tx.Exec(fmt.Sprintf(`
		INSERT INTO %s (%s)
		SELECT %s FROM src.%s source_row
		LEFT JOIN selected_profile_service_map service_map
		  ON service_map.source_id = source_row.%s
		WHERE %s`,
		table, strings.Join(cols, ", "), strings.Join(selectExpressions, ", "),
		table, serviceColumn, where,
	))
	return err
}

func sourceTableExists(tx *sql.Tx, table string) (bool, error) {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM src.sqlite_master
		WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func sourceColumnExists(tx *sql.Tx, table, column string) (bool, error) {
	columns, err := schemaColumns(tx, "src", table)
	if err != nil {
		return false, err
	}
	foldedColumn := foldIdentifier(column)
	for _, candidate := range columns {
		if foldIdentifier(candidate) == foldedColumn {
			return true, nil
		}
	}
	return false, nil
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
