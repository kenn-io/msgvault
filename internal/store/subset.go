package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vcard"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// CopyResult holds the summary of a subset copy operation.
type CopyResult struct {
	Messages                  int64
	Conversations             int64
	Participants              int64
	Labels                    int64
	Sources                   int64
	Organizations             int64
	Employments               int64
	PersonMergePackets        int64
	OmittedPersonMergePackets int64
	DBSize                    int64
	Elapsed                   time.Duration
}

// ErrSubsetVCardResourcesRequireProfiles reports IncludeVCardResources
// without IncludeProfiles: native vCard bodies are only copied alongside the
// structured profiles they project into.
var ErrSubsetVCardResourcesRequireProfiles = errors.New(
	"vCard resources require profiles: set IncludeProfiles with IncludeVCardResources")

// CopySubsetOptions controls optional sensitive metadata included in a subset.
type CopySubsetOptions struct {
	IncludeIdentity       bool
	IncludeAttributes     bool
	IncludeProfiles       bool
	IncludeVCardResources bool
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
// dependencies require the separate IncludeProfiles opt-in, and the native
// vCard bodies they were projected from require IncludeVCardResources on top
// of it. Person attribute definitions and values also require callers to
// explicitly use CopySubsetWithOptions with IncludeAttributes. When attributes
// are included, person-valued references
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
// IncludeVCardResources copies the native vCard resources of copied people:
// the opaque original wire bodies and the retired-UID aliases that resolve to
// them. A body is copied whole rather than decomposed, so it carries whatever
// the contact source recorded — custom properties, RELATED entries naming
// people outside the subset, and residue no structured table represents —
// which is why it needs its own authorization instead of riding
// IncludeProfiles. Complete merge packets also contain immutable merge-time
// snapshots, including values later redacted from live profile tables. Packets
// require IncludeAttributes in addition to IncludeProfiles and
// IncludeVCardResources; without it, scoped packets are counted as omitted.
// Native bodies require IncludeProfiles, whose structured fields they project
// into; asking for the bodies without the profiles is an error rather than a
// silent no-op.
func CopySubsetWithOptions(
	srcDBPath, dstDir string, rowCount int, options CopySubsetOptions,
) (*CopyResult, error) {
	if rowCount <= 0 {
		return nil, fmt.Errorf("rowCount must be positive, got %d", rowCount)
	}
	if options.IncludeVCardResources && !options.IncludeProfiles {
		return nil, ErrSubsetVCardResourcesRequireProfiles
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

	if err := db.Close(); err != nil {
		cleanup()
		return nil, fmt.Errorf("close copied subset database: %w", err)
	}
	if options.IncludeAttributes {
		normalized, err := Open(dstDBPath)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("open copied subset for attribute reconciliation: %w", err)
		}
		if err := normalized.InitSchema(); err != nil {
			_ = normalized.Close()
			cleanup()
			return nil, fmt.Errorf("reconcile copied subset attributes: %w", err)
		}
		if err := normalized.Close(); err != nil {
			cleanup()
			return nil, fmt.Errorf("close reconciled subset database: %w", err)
		}
	}

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

	if err := copyParticipantLinks(tx, options.IncludeProfiles); err != nil {
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
		if _, err := copyByName(tx, "person_tracking",
			`person_id IN (SELECT id FROM persons)`); err != nil {
			return nil, fmt.Errorf("copy person tracking: %w", err)
		}
		if options.IncludeVCardResources {
			if err := copyVCardResourceEnvelopes(tx); err != nil {
				return nil, err
			}
		}
		if err := copySubsetRelationships(tx); err != nil {
			return nil, err
		}
		if err := copyEmploymentData(tx, result); err != nil {
			return nil, err
		}
	}

	if options.IncludeAttributes {
		hasSensitive, err := sourceColumnExists(
			tx, "attribute_definitions", "is_sensitive")
		if err != nil {
			return nil, fmt.Errorf("inspect source attribute definitions: %w", err)
		}
		sensitiveExpression := "FALSE"
		if hasSensitive {
			sensitiveExpression = "is_sensitive"
		}
		if _, err := tx.Exec(`CREATE TEMP TABLE provisional_attribute_definition_ids AS
			SELECT universal_id, id FROM attribute_definitions`); err != nil {
			return nil, fmt.Errorf("remember provisional subset attribute definitions: %w", err)
		}
		// The destination is a brand-new archive and has no attribute values.
		// Remove its provisional seeds so source slugs cross the archive boundary
		// unchanged; post-copy InitSchema installs any missing seeds afterward.
		if _, err := tx.Exec(`DELETE FROM attribute_definitions`); err != nil {
			return nil, fmt.Errorf("clear provisional subset attribute definitions: %w", err)
		}
		// Definitions remain portable by universal_id. When a shipped definition
		// already had the same provisional ID in both archives, retain that ID so
		// merge snapshots and immutable results stay self-consistent. Custom or
		// otherwise remapped definitions still receive destination-local IDs; a
		// merge packet that embeds one is conservatively omitted below.
		if _, err := tx.Exec(fmt.Sprintf(`
		INSERT INTO attribute_definitions (
		    id, universal_id, object_type, slug, label, description,
		    value_type, field_type, record_target, cardinality, display_order,
		    is_required, ownership, ui_creatable, ui_editable, api_mutable,
		    is_searchable, is_sensitive, is_audited, is_deletable, history_exempt,
		    derived_source, options, vcard_property, is_active, revision,
		    created_at, updated_at
		)
		SELECT
		    CASE WHEN provisional.id = source.id THEN source.id ELSE NULL END,
		    source.universal_id, source.object_type, source.slug, source.label,
		    source.description,
		    value_type, field_type, record_target, cardinality, display_order,
		    is_required, ownership, ui_creatable, ui_editable, api_mutable,
		    is_searchable, %s AS is_sensitive, is_audited, is_deletable, history_exempt,
		    derived_source, options, vcard_property, is_active, revision,
		    created_at, updated_at
		FROM src.attribute_definitions source
		LEFT JOIN provisional_attribute_definition_ids provisional
		  ON provisional.universal_id = source.universal_id
		WHERE source.object_type IN ('person', 'organization')
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
		    is_sensitive = excluded.is_sensitive,
		    is_audited = excluded.is_audited,
		    is_deletable = excluded.is_deletable,
		    history_exempt = excluded.history_exempt,
		    derived_source = excluded.derived_source,
		    options = excluded.options,
		    vcard_property = excluded.vcard_property,
		    is_active = excluded.is_active,
		    revision = excluded.revision,
		    created_at = excluded.created_at,
		    updated_at = excluded.updated_at`, sensitiveExpression)); err != nil {
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
	if err := copyPersonMergePackets(tx, options, result); err != nil {
		return nil, err
	}
	// Every table a native vCard mapping can own a row in — profile
	// components, relationships, employments, attribute values — has been
	// copied or deliberately skipped by now, so this is the first point at
	// which a mapping's owner being absent means the subset left it behind.
	if options.IncludeProfiles && options.IncludeVCardResources {
		if err := releaseVCardMappingsToMissingOwners(tx); err != nil {
			return nil, err
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

	if _, err := copyByName(tx, "attachments",
		`message_id IN (SELECT id FROM selected_messages)`); err != nil {
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

// copyPersonMergePackets copies an audit packet only when the scoped archive
// contains every live person and participant needed to interpret it. The
// snapshot can contain all profile and native-card fields from both original
// people, so packets are limited to the strongest profile-data opt-in; an
// incomplete packet is less useful than no packet because it falsely promises
// that the historical merge can still be reversed.
func copyPersonMergePackets(
	tx *sql.Tx, options CopySubsetOptions, result *CopyResult,
) error {
	hasMerges, err := sourceTableExists(tx, "person_merges")
	if err != nil {
		return fmt.Errorf("check person merge schema: %w", err)
	}
	if !hasMerges {
		return nil
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE selected_person_merges (
		id INTEGER PRIMARY KEY
	)`); err != nil {
		return fmt.Errorf("create selected person merges: %w", err)
	}
	if options.IncludeProfiles && options.IncludeVCardResources {
		if err := tx.QueryRow(`SELECT COUNT(*) FROM src.person_merges merge_record
			WHERE merge_record.current_person_id IN (SELECT id FROM persons)
			   OR EXISTS (SELECT 1 FROM src.person_splits split_record
				WHERE split_record.merge_id = merge_record.id
				  AND (split_record.source_person_id IN (SELECT id FROM persons)
					OR split_record.new_person_id IN (SELECT id FROM persons)))
			   OR EXISTS (SELECT 1 FROM src.person_merge_participants lineage
				JOIN person_participants binding
				  ON binding.participant_id = lineage.participant_id
				WHERE lineage.merge_id = merge_record.id)`,
		).Scan(&result.PersonMergePackets); err != nil {
			return fmt.Errorf("count scoped person merge packets: %w", err)
		}
	}
	if options.IncludeProfiles && options.IncludeAttributes && options.IncludeVCardResources {
		if _, err := tx.Exec(`INSERT INTO selected_person_merges (id)
			SELECT merge_record.id FROM src.person_merges merge_record
			WHERE (
				merge_record.current_person_id IN (SELECT id FROM persons)
				OR EXISTS (SELECT 1 FROM src.person_splits split_record
					WHERE split_record.merge_id = merge_record.id
					  AND (split_record.source_person_id IN (SELECT id FROM persons)
						OR split_record.new_person_id IN (SELECT id FROM persons)))
				OR EXISTS (SELECT 1 FROM src.person_merge_participants lineage
					JOIN person_participants binding
					  ON binding.participant_id = lineage.participant_id
					WHERE lineage.merge_id = merge_record.id)
			)
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_merge_participants lineage
				WHERE lineage.merge_id = merge_record.id
				  AND lineage.participant_id NOT IN (SELECT id FROM participants)
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_splits split_record
				WHERE split_record.merge_id = merge_record.id
				  AND split_record.source_person_id NOT IN (SELECT id FROM persons)
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_splits split_record
				WHERE split_record.merge_id = merge_record.id
				  AND split_record.new_person_id NOT IN (SELECT id FROM persons)
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_merge_review_candidates candidate
				WHERE candidate.merge_id = merge_record.id
				  AND (
					candidate.survivor_person_id NOT IN (SELECT id FROM persons)
					OR candidate.definition_id NOT IN (
						SELECT destination_definition.id
						FROM src.attribute_definitions source_definition
						JOIN attribute_definitions destination_definition
						  ON destination_definition.universal_id = source_definition.universal_id
						WHERE source_definition.id = candidate.definition_id
					)
					OR candidate.survivor_value_id NOT IN (
						SELECT id FROM person_attribute_values
					)
					OR candidate.absorbed_value_id NOT IN (
						SELECT id FROM person_attribute_values
					)
					OR (candidate.resolution_value_id IS NOT NULL
						AND candidate.resolution_value_id NOT IN (
							SELECT id FROM person_attribute_values
						))
				  )
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_merge_rows journal
				JOIN src.person_attribute_values value
				  ON value.id = journal.original_row_id
				JOIN src.attribute_definitions source_definition
				  ON source_definition.id = value.definition_id
				JOIN attribute_definitions destination_definition
				  ON destination_definition.universal_id = source_definition.universal_id
				WHERE journal.merge_id = merge_record.id
				  AND journal.table_name = 'person_attribute_values'
				  AND source_definition.id <> destination_definition.id
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM src.person_merge_rows journal
				WHERE journal.merge_id = merge_record.id
				  AND journal.table_name = 'daily_note_entry_persons'
			  )`); err != nil {
			return fmt.Errorf("select complete person merge packets: %w", err)
		}
	}
	if options.IncludeProfiles && options.IncludeVCardResources {
		if err := pruneIncompletePersonMergePacketsAndAliases(tx); err != nil {
			return err
		}
	}
	var selectedPackets int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM selected_person_merges`).Scan(&selectedPackets); err != nil {
		return fmt.Errorf("count copied person merge packets: %w", err)
	}
	result.OmittedPersonMergePackets = result.PersonMergePackets - selectedPackets
	result.PersonMergePackets = selectedPackets
	// Operation results retain the identity revision committed in the source
	// archive. Preserve that archive's current revision whenever a complete
	// packet crosses the subset boundary, so replayed results never claim a
	// revision ahead of the destination's cache authority. Keep the larger
	// value if a future caller ever copies into a pre-populated destination.
	if _, err := tx.Exec(`INSERT INTO archive_metadata (key, value)
		SELECT 'identity_revision', source_revision.value
		FROM src.archive_metadata source_revision
		WHERE source_revision.key = 'identity_revision'
		  AND EXISTS (SELECT 1 FROM selected_person_merges)
		ON CONFLICT(key) DO UPDATE SET value = CASE
			WHEN CAST(excluded.value AS INTEGER) > CAST(archive_metadata.value AS INTEGER)
			THEN excluded.value ELSE archive_metadata.value END`); err != nil {
		return fmt.Errorf("preserve person merge identity revision: %w", err)
	}
	if _, err := copyByName(tx, "person_merges",
		`id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person merges: %w", err)
	}
	if _, err := copyByName(tx, "person_splits",
		`merge_id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person splits: %w", err)
	}
	if _, err := copyByName(tx, "person_merge_participants",
		`merge_id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person merge participants: %w", err)
	}
	if _, err := copyByName(tx, "person_merge_rows",
		`merge_id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person merge rows: %w", err)
	}
	if _, err := copyByName(tx, "person_merge_row_person_refs",
		`merge_id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person merge row person references: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO person_merge_review_candidates (
		id, merge_id, survivor_person_id, definition_id,
		survivor_value_id, absorbed_value_id, state, resolution_value_id,
		reviewed_by, reviewed_at, created_at
	) SELECT
		candidate.id, candidate.merge_id, candidate.survivor_person_id,
		destination_definition.id, candidate.survivor_value_id,
		candidate.absorbed_value_id, candidate.state, candidate.resolution_value_id,
		candidate.reviewed_by, candidate.reviewed_at, candidate.created_at
	FROM src.person_merge_review_candidates candidate
	JOIN src.attribute_definitions source_definition
	  ON source_definition.id = candidate.definition_id
	JOIN attribute_definitions destination_definition
	  ON destination_definition.universal_id = source_definition.universal_id
	WHERE candidate.merge_id IN (SELECT id FROM selected_person_merges)`); err != nil {
		return fmt.Errorf("copy person merge review candidates: %w", err)
	}
	if selectedPackets > 0 {
		if err := reservePersonMergePacketIDs(tx); err != nil {
			return err
		}
	}
	return nil
}

func reservePersonMergePacketIDs(tx *sql.Tx) error {
	autoincrementTables := map[string]struct{}{}
	tableRows, err := tx.Query(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND instr(upper(sql), 'AUTOINCREMENT') > 0
		ORDER BY name`)
	if err != nil {
		return fmt.Errorf("load subset AUTOINCREMENT tables: %w", err)
	}
	defer func() { _ = tableRows.Close() }()
	for tableRows.Next() {
		var table string
		if err := tableRows.Scan(&table); err != nil {
			return fmt.Errorf("scan subset AUTOINCREMENT table: %w", err)
		}
		autoincrementTables[table] = struct{}{}
	}
	if err := tableRows.Err(); err != nil {
		return fmt.Errorf("iterate subset AUTOINCREMENT tables: %w", err)
	}
	if err := tableRows.Close(); err != nil {
		return fmt.Errorf("close subset AUTOINCREMENT tables: %w", err)
	}

	ceilings := map[string]int64{}
	var historicalPersonID int64
	if err := tx.QueryRow(`SELECT MAX(person_id) FROM (
		SELECT survivor_person_id_at_merge AS person_id FROM person_merges
		UNION ALL SELECT absorbed_person_id FROM person_merges
		UNION ALL SELECT source_person_id FROM person_splits
		UNION ALL SELECT new_person_id FROM person_splits
	)`).Scan(&historicalPersonID); err != nil {
		return fmt.Errorf("read historical person ID ceiling: %w", err)
	}
	ceilings["persons"] = historicalPersonID

	snapshotRows, err := tx.Query(`SELECT snapshot_blob, snapshot_sha256
		FROM person_merges ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load copied person merge snapshots: %w", err)
	}
	defer func() { _ = snapshotRows.Close() }()
	for snapshotRows.Next() {
		var blob []byte
		var sha256 string
		if err := snapshotRows.Scan(&blob, &sha256); err != nil {
			return fmt.Errorf("scan copied person merge snapshot: %w", err)
		}
		snapshot, err := decodePersonMergeSnapshot(blob, sha256)
		if err != nil {
			return fmt.Errorf("reserve copied person merge snapshot IDs: %w", err)
		}
		for _, person := range snapshot.Persons {
			ceilings["persons"] = max(ceilings["persons"], person.ID)
		}
		for _, row := range snapshot.Rows {
			if _, ok := autoincrementTables[row.TableName]; ok && row.RowID > 0 {
				ceilings[row.TableName] = max(ceilings[row.TableName], row.RowID)
			}
		}
	}
	if err := snapshotRows.Err(); err != nil {
		return fmt.Errorf("iterate copied person merge snapshots: %w", err)
	}
	if err := snapshotRows.Close(); err != nil {
		return fmt.Errorf("close copied person merge snapshots: %w", err)
	}

	tables := make([]string, 0, len(ceilings))
	for table := range ceilings {
		if _, ok := autoincrementTables[table]; ok {
			tables = append(tables, table)
		}
	}
	sort.Strings(tables)
	for _, table := range tables {
		if err := advanceSubsetSQLiteSequence(tx, table, ceilings[table]); err != nil {
			return err
		}
	}
	return nil
}

func advanceSubsetSQLiteSequence(tx *sql.Tx, table string, ceiling int64) error {
	if _, err := tx.Exec(`UPDATE sqlite_sequence SET seq = CASE
		WHEN seq < ? THEN ? ELSE seq END WHERE name = ?`, ceiling, ceiling, table); err != nil {
		return fmt.Errorf("advance subset %s sequence: %w", table, err)
	}
	if _, err := tx.Exec(`INSERT INTO sqlite_sequence (name, seq)
		SELECT ?, ? WHERE NOT EXISTS (
			SELECT 1 FROM sqlite_sequence WHERE name = ?
		)`, table, ceiling, table); err != nil {
		return fmt.Errorf("initialize subset %s sequence: %w", table, err)
	}
	return nil
}

func pruneIncompletePersonMergePacketsAndAliases(tx *sql.Tx) error {
	for {
		var selectedBefore int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM selected_person_merges`).Scan(
			&selectedBefore,
		); err != nil {
			return fmt.Errorf("count selected person merge packets: %w", err)
		}
		if err := pruneIncompletePersonMergePackets(tx); err != nil {
			return err
		}
		aliasResult, err := tx.Exec(`DELETE FROM person_uid_aliases
			WHERE retired_uid IN (
				SELECT omitted.absorbed_uid FROM src.person_merges omitted
				WHERE omitted.id NOT IN (SELECT id FROM selected_person_merges)
			)
			AND retired_uid NOT IN (
				SELECT selected.absorbed_uid FROM src.person_merges selected
				WHERE selected.id IN (SELECT id FROM selected_person_merges)
			)`)
		if err != nil {
			return fmt.Errorf("remove incomplete person merge aliases: %w", err)
		}
		aliasesRemoved, err := aliasResult.RowsAffected()
		if err != nil {
			return fmt.Errorf("count removed incomplete person merge aliases: %w", err)
		}
		var selectedAfter int64
		if err := tx.QueryRow(`SELECT COUNT(*) FROM selected_person_merges`).Scan(
			&selectedAfter,
		); err != nil {
			return fmt.Errorf("recount selected person merge packets: %w", err)
		}
		if selectedBefore == selectedAfter && aliasesRemoved == 0 {
			return nil
		}
	}
}

func pruneIncompletePersonMergePackets(tx *sql.Tx) error {
	mergeRows, err := tx.Query(`SELECT id FROM selected_person_merges ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load selected person merge packets: %w", err)
	}
	defer func() { _ = mergeRows.Close() }()
	mergeIDs := []int64{}
	for mergeRows.Next() {
		var mergeID int64
		if err := mergeRows.Scan(&mergeID); err != nil {
			_ = mergeRows.Close()
			return fmt.Errorf("scan selected person merge packet: %w", err)
		}
		mergeIDs = append(mergeIDs, mergeID)
	}
	if err := mergeRows.Err(); err != nil {
		_ = mergeRows.Close()
		return fmt.Errorf("iterate selected person merge packets: %w", err)
	}
	if err := mergeRows.Close(); err != nil {
		return fmt.Errorf("close selected person merge packets: %w", err)
	}

	for _, mergeID := range mergeIDs {
		complete, err := personMergePacketRowsComplete(tx, mergeID)
		if err != nil {
			return err
		}
		if complete {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM selected_person_merges WHERE id = ?`, mergeID); err != nil {
			return fmt.Errorf("omit incomplete person merge packet: %w", err)
		}
	}
	return nil
}

func personMergePacketRowsComplete(tx *sql.Tx, mergeID int64) (bool, error) {
	var omittedSplitOwner int
	if err := tx.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM src.person_merge_participants lineage
		JOIN src.person_splits split_record ON split_record.id = lineage.split_id
		WHERE lineage.merge_id = ?
		  AND split_record.merge_id NOT IN (SELECT id FROM selected_person_merges)
	)`, mergeID).Scan(&omittedSplitOwner); err != nil {
		return false, fmt.Errorf("validate person merge split dependencies: %w", err)
	}
	if omittedSplitOwner != 0 {
		return false, nil
	}
	var snapshotBlob []byte
	var snapshotSHA256 string
	if err := tx.QueryRow(`SELECT snapshot_blob, snapshot_sha256
		FROM src.person_merges WHERE id = ?`, mergeID).Scan(
		&snapshotBlob, &snapshotSHA256,
	); err != nil {
		return false, fmt.Errorf("load person merge packet snapshot: %w", err)
	}
	snapshot, err := decodePersonMergeSnapshot(snapshotBlob, snapshotSHA256)
	if err != nil {
		return false, fmt.Errorf("verify person merge packet %d snapshot: %w", mergeID, err)
	}
	rows, err := tx.Query(`SELECT table_name, current_row_id, current_row_key,
		action, provenance_kind, split_id, snapshot_path
		FROM src.person_merge_rows WHERE merge_id = ?
		ORDER BY table_name, original_row_key`, mergeID)
	if err != nil {
		return false, fmt.Errorf("load person merge packet rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	packetRows := []personMergePacketRow{}
	for rows.Next() {
		var row personMergePacketRow
		if err := rows.Scan(
			&row.table, &row.currentID, &row.currentKey,
			&row.action, &row.provenance, &row.splitID, &row.snapshotPath,
		); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("scan person merge packet row: %w", err)
		}
		packetRows = append(packetRows, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, fmt.Errorf("iterate person merge packet rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close person merge packet rows: %w", err)
	}
	complete, err := personMergeSnapshotRowsComplete(tx, snapshot, packetRows)
	if err != nil || !complete {
		return complete, err
	}
	for _, row := range packetRows {
		if row.table == "daily_note_entry_persons" {
			return false, nil
		}
		if row.table == "person_merges" {
			if !row.currentID.Valid {
				return false, nil
			}
			var exists int
			err := tx.QueryRow(`SELECT 1 FROM selected_person_merges WHERE id = ?`,
				row.currentID.Int64).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("validate referenced person merge packet: %w", err)
			}
			continue
		}
		if row.table == personMergeReviewCandidatesTableName {
			if !row.currentID.Valid {
				return false, nil
			}
			var exists int
			err := tx.QueryRow(`SELECT 1 FROM src.person_merge_review_candidates candidate
				WHERE candidate.id = ? AND candidate.merge_id IN (
					SELECT id FROM selected_person_merges
				)`, row.currentID.Int64).Scan(&exists)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf("validate referenced merge candidate packet: %w", err)
			}
			continue
		}
		if row.table == personRelationshipReviewsTableName && row.currentID.Valid {
			var dependenciesComplete int
			err := tx.QueryRow(`SELECT 1 FROM src.person_relationship_reviews review
				WHERE review.id = ?
				  AND (
					review.matched_person_id IS NULL
					OR review.matched_person_id IN (SELECT id FROM persons)
				  )
				  AND (
					review.accepted_relationship_id IS NULL
					OR review.accepted_relationship_id IN (SELECT id FROM person_relationships)
				  )`, row.currentID.Int64).Scan(&dependenciesComplete)
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, fmt.Errorf(
					"validate person relationship review packet dependencies: %w", err)
			}
		}
		if row.splitID.Valid || row.action == "deleted_snapshot" || row.action == "recomputed" ||
			row.provenance == string(personMergeProvenanceDerived) {
			continue
		}
		spec, ok := personMergeTableRegistry[row.table]
		if !ok {
			return false, fmt.Errorf("validate person merge packet: unregistered table %q", row.table)
		}
		where, args, err := personSplitCurrentRowWhere(spec, personSplitJournalRow{
			currentRowID: row.currentID, currentKey: row.currentKey,
		})
		if err != nil {
			return false, err
		}
		var exists int
		err = tx.QueryRow(`SELECT 1 FROM `+personSplitIdentifier(row.table)+
			` WHERE `+where+` LIMIT 1`, args...).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("validate person merge packet %s row: %w", row.table, err)
		}
	}
	return true, nil
}

type personMergePacketRow struct {
	table, action, provenance, snapshotPath string
	currentID                               sql.NullInt64
	currentKey                              sql.NullString
	splitID                                 sql.NullInt64
}

// personMergeSnapshotRowsComplete proves that every immutable snapshot row is
// inside the destination's selected-data closure. Journal rows alone are not
// sufficient: unchanged survivor-side rows are deliberately pruned from the
// journal, but remain in snapshot_blob with their full historical contents.
func personMergeSnapshotRowsComplete(
	tx *sql.Tx, snapshot personMergeSnapshot, packetRows []personMergePacketRow,
) (bool, error) {
	lineagePeople := make(map[int64]struct{}, len(snapshot.Persons))
	for _, person := range snapshot.Persons {
		lineagePeople[person.ID] = struct{}{}
		for _, participantID := range person.ParticipantIDs {
			present, err := subsetRowIDExists(tx, "participants", participantID)
			if err != nil || !present {
				return present, err
			}
		}
	}
	journalByPath := make(map[string]personMergePacketRow, len(packetRows))
	for _, row := range packetRows {
		journalByPath[row.snapshotPath] = row
	}
	for index, row := range snapshot.Rows {
		if row.TableName == "daily_note_entry_persons" {
			// These rows can embed message, owner, or note data outside the
			// selected archive. The immutable snapshot cannot be redacted.
			return false, nil
		}
		journal, hasJournal := journalByPath["rows/"+strconv.Itoa(index)]
		present, err := personMergeSnapshotRowPresent(tx, row, journal, hasJournal)
		if err != nil || !present {
			return present, err
		}
		complete, err := personMergeSnapshotDependenciesComplete(tx, row, lineagePeople)
		if err != nil || !complete {
			return complete, err
		}
	}
	return true, nil
}

func personMergeSnapshotRowPresent(
	tx *sql.Tx, row personMergeSnapshotRow, journal personMergePacketRow, hasJournal bool,
) (bool, error) {
	if hasJournal && journal.action == "deleted_snapshot" {
		return true, nil
	}
	keys := []string{row.RowKey}
	if hasJournal && journal.currentKey.Valid && !journal.splitID.Valid {
		keys = append(keys, journal.currentKey.String)
	}
	seen := map[string]struct{}{}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		switch row.TableName {
		case "person_merges":
			id, ok, err := personMergeSnapshotSingleIntegerKey(key, "id")
			if err != nil {
				return false, err
			}
			if !ok {
				continue
			}
			var present int
			err = tx.QueryRow(`SELECT 1 FROM selected_person_merges WHERE id = ?`, id).Scan(&present)
			if err == nil {
				return true, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("validate snapshot person merge dependency: %w", err)
			}
		case personMergeReviewCandidatesTableName:
			id, ok, err := personMergeSnapshotSingleIntegerKey(key, "id")
			if err != nil {
				return false, err
			}
			if !ok {
				continue
			}
			var present int
			err = tx.QueryRow(`SELECT 1 FROM src.person_merge_review_candidates candidate
				WHERE candidate.id = ?
				  AND candidate.merge_id IN (SELECT id FROM selected_person_merges)`, id).Scan(&present)
			if err == nil {
				return true, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("validate snapshot review candidate dependency: %w", err)
			}
		default:
			spec, ok := personMergeTableRegistry[row.TableName]
			if !ok {
				return false, fmt.Errorf("validate person merge snapshot: unregistered table %q", row.TableName)
			}
			where, args, err := personSplitRowKeyWhere(spec, key)
			if err != nil {
				return false, err
			}
			var present int
			err = tx.QueryRow(`SELECT 1 FROM `+personSplitIdentifier(row.TableName)+
				` WHERE `+where+` LIMIT 1`, args...).Scan(&present)
			if err == nil {
				return true, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("validate snapshot %s row: %w", row.TableName, err)
			}
		}
	}
	return false, nil
}

func personMergeSnapshotDependenciesComplete(
	tx *sql.Tx, row personMergeSnapshotRow, lineagePeople map[int64]struct{},
) (bool, error) {
	spec, ok := personMergeTableRegistry[row.TableName]
	if !ok {
		return false, fmt.Errorf("validate person merge snapshot dependencies: unregistered table %q", row.TableName)
	}
	for _, reference := range spec.PersonReferences {
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(row, reference.KindColumn) != reference.KindValue {
			continue
		}
		personID, present := personMergeSnapshotIntegerColumn(row, reference.IDColumn)
		if !present {
			continue
		}
		if _, lineage := lineagePeople[personID]; lineage {
			continue
		}
		included, err := subsetRowIDExists(tx, "persons", personID)
		if err != nil || !included {
			return included, err
		}
	}
	for _, catalog := range []struct {
		table, column string
	}{
		{table: "relationship_types", column: "relationship_type_id"},
		{table: "attribute_definitions", column: "definition_id"},
	} {
		usesCatalog := (row.TableName == personRelationshipsTableName &&
			catalog.table == "relationship_types") ||
			((row.TableName == personAttributeValuesTableName ||
				row.TableName == "organization_attribute_values" ||
				row.TableName == personMergeReviewCandidatesTableName) &&
				catalog.table == "attribute_definitions")
		if !usesCatalog {
			continue
		}
		sourceID, present := personMergeSnapshotIntegerColumn(row, catalog.column)
		if !present {
			continue
		}
		preserved, err := personMergeSnapshotCatalogIDPreserved(tx, catalog.table, sourceID)
		if err != nil || !preserved {
			return preserved, err
		}
	}

	dependencies := map[string][]struct {
		column, table string
	}{
		personRelationshipsTableName:       {{"relationship_type_id", "relationship_types"}},
		personRelationshipReviewsTableName: {{"accepted_relationship_id", personRelationshipsTableName}},
		personAttributeValuesTableName:     {{"definition_id", "attribute_definitions"}},
		"organization_attribute_values": {
			{"organization_id", "organizations"}, {"definition_id", "attribute_definitions"},
		},
		personMergeReviewCandidatesTableName: {
			{"merge_id", "selected_person_merges"},
			{"definition_id", "attribute_definitions"},
			{"survivor_value_id", personAttributeValuesTableName},
			{"absorbed_value_id", personAttributeValuesTableName},
			{"resolution_value_id", personAttributeValuesTableName},
		},
		"identity_match_candidate_redirects": {
			{"retired_candidate_id", identityMatchCandidatesTableName},
			{"surviving_candidate_id", identityMatchCandidatesTableName},
		},
		identityMatchCandidateSourcesTableName: {
			{"candidate_id", identityMatchCandidatesTableName}, {sourceIDColumnName, "sources"},
		},
		identityMatchEvidenceTableName: {
			{"candidate_id", identityMatchCandidatesTableName},
		},
		identityMatchEvidenceSourcesTableName: {
			{"evidence_id", identityMatchEvidenceTableName}, {sourceIDColumnName, "sources"},
		},
		"employments": {
			{"organization_id", "organizations"}, {"address_id", "organization_addresses"},
		},
	}
	for _, dependency := range dependencies[row.TableName] {
		id, present := personMergeSnapshotIntegerColumn(row, dependency.column)
		if !present {
			continue
		}
		included, err := subsetRowIDExists(tx, dependency.table, id)
		if err != nil || !included {
			return included, err
		}
	}

	if row.TableName == identityMatchCandidatesTableName {
		for _, side := range []string{"left", "right"} {
			kind := personSplitSnapshotRowText(row, side+"_kind")
			id, present := personMergeSnapshotIntegerColumn(row, side+"_id")
			if !present {
				return false, nil
			}
			table := map[string]string{
				"person": "persons", "participant": "participants",
				"observation":   "participant_contact_observations",
				"contact_point": personContactPointsTableName,
			}[kind]
			if table == "" {
				return false, nil
			}
			if kind == "person" {
				if _, lineage := lineagePeople[id]; lineage {
					continue
				}
			}
			included, err := subsetRowIDExists(tx, table, id)
			if err != nil || !included {
				return included, err
			}
		}
	}
	for _, table := range []string{personContactPointsTableName, identityMatchCandidatesTableName} {
		if row.TableName != table {
			continue
		}
		serviceID, present := personMergeSnapshotIntegerColumn(row, "service_id")
		if present {
			preserved, err := personMergeSnapshotServiceIDPreserved(tx, serviceID)
			if err != nil || !preserved {
				return preserved, err
			}
		}
	}
	return true, nil
}

func personMergeSnapshotServiceIDPreserved(tx *sql.Tx, sourceID int64) (bool, error) {
	var destinationID int64
	err := tx.QueryRow(`SELECT destination_id FROM selected_profile_service_map
		WHERE source_id = ?`, sourceID).Scan(&destinationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate snapshot communication service mapping: %w", err)
	}
	// Snapshot blobs are immutable audit records. If the database-local ID was
	// remapped, copying the blob would make a later split restore the wrong
	// service; omit the packet rather than rewriting and re-signing history.
	return destinationID == sourceID, nil
}

func personMergeSnapshotCatalogIDPreserved(
	tx *sql.Tx, table string, sourceID int64,
) (bool, error) {
	identifier := personSplitIdentifier(table)
	var destinationID int64
	err := tx.QueryRow(`SELECT destination.id
		FROM src.`+identifier+` source
		JOIN `+identifier+` destination
		  ON destination.universal_id = source.universal_id
		WHERE source.id = ?`, sourceID).Scan(&destinationID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate snapshot %s mapping: %w", table, err)
	}
	return destinationID == sourceID, nil
}

func personMergeSnapshotIntegerColumn(row personMergeSnapshotRow, name string) (int64, bool) {
	for _, column := range row.Columns {
		if column.Name == name && column.Value.Integer != nil {
			return *column.Value.Integer, true
		}
	}
	return 0, false
}

func personMergeSnapshotSingleIntegerKey(encoded, name string) (int64, bool, error) {
	var key []personMergeSnapshotColumn
	if err := json.Unmarshal([]byte(encoded), &key); err != nil {
		return 0, false, fmt.Errorf("decode person merge snapshot row key: %w", err)
	}
	if len(key) != 1 || key[0].Name != name || key[0].Value.Integer == nil {
		return 0, false, nil
	}
	return *key[0].Value.Integer, true, nil
}

func subsetRowIDExists(tx *sql.Tx, table string, id int64) (bool, error) {
	var present int
	err := tx.QueryRow(`SELECT 1 FROM `+personSplitIdentifier(table)+` WHERE id = ? LIMIT 1`, id).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate subset %s dependency: %w", table, err)
	}
	return true, nil
}

type subsetServiceReference struct {
	table string
	where string
}

// copyParticipantLinks copies the link forest by name. The identity-match
// ownership column was added to upgraded databases after created_at, while a
// fresh schema declares it before created_at; a positional copy would swap
// those values. Ownership is profile metadata, so omit it when profiles are
// not requested even if the source has the column.
func copyParticipantLinks(tx *sql.Tx, includeProfiles bool) error {
	hasCandidateOwner, err := sourceColumnExists(
		tx, "participant_links", "identity_match_candidate_id",
	)
	if err != nil {
		return err
	}
	ownerExpression := "NULL"
	if includeProfiles && hasCandidateOwner {
		ownerExpression = "source_row.identity_match_candidate_id"
	}
	_, err = tx.Exec(fmt.Sprintf(`
		INSERT INTO participant_links (
			participant_a, participant_b, identity_match_candidate_id, created_at
		)
		SELECT source_row.participant_a, source_row.participant_b, %s,
		       source_row.created_at
		FROM src.participant_links source_row
		WHERE source_row.participant_a IN (SELECT id FROM participants)
		  AND source_row.participant_b IN (SELECT id FROM participants)`,
		ownerExpression,
	))
	return err
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
				table: identityMatchCandidatesTableName,
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
	hasDiscoveries, err := sourceTableExists(tx, "communication_service_discoveries")
	if err != nil {
		return fmt.Errorf("check communication service discovery schema: %w", err)
	}
	if hasDiscoveries {
		if _, err := tx.Exec(`INSERT INTO communication_service_discoveries (
				service_id, provider, discovery_kind
			)
			SELECT service_map.destination_id, discovery.provider, discovery.discovery_kind
			FROM src.communication_service_discoveries discovery
			JOIN selected_profile_service_map service_map
			  ON service_map.source_id = discovery.service_id
			ON CONFLICT(service_id, provider, discovery_kind) DO NOTHING`); err != nil {
			return fmt.Errorf("copy communication service discoveries: %w", err)
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
	// source_resource_uid arrived after the relationship tables; a source
	// that predates it has no column to read, and its rows carry no resource.
	edgeResourceUID, err := sourceColumnExpression(
		tx, personRelationshipsTableName, "source_resource_uid", "edge")
	if err != nil {
		return err
	}
	reviewResourceUID, err := sourceColumnExpression(
		tx, "person_relationship_reviews", "source_resource_uid", "review")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO person_relationships (
		    id, source_person_id, target_person_id, relationship_type_id,
		    start_year, start_month, start_day, end_year, end_month, end_day,
		    status, notes, source, source_ref, source_resource_uid, confidence,
		    vcard_property, vcard_group, vcard_prop_id, vcard_pid, vcard_altid,
		    created_by, updated_by, revision, created_at, updated_at
		)
		SELECT
		    edge.id, edge.source_person_id, edge.target_person_id,
		    destination_type.id,
		    edge.start_year, edge.start_month, edge.start_day,
		    edge.end_year, edge.end_month, edge.end_day,
		    edge.status, edge.notes, edge.source, edge.source_ref,
		    ` + edgeResourceUID + `, edge.confidence,
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
		    source_ref, source_resource_uid, vcard_property, vcard_group,
		    vcard_prop_id, vcard_pid, vcard_altid, created_by, reviewed_by,
		    reviewed_at, created_at, updated_at
		)
		SELECT
		    review.id, review.person_id, review.raw_related_value,
		    review.raw_related_type, review.value_kind,
		    CASE WHEN review.matched_person_id IN (SELECT id FROM persons)
		         THEN review.matched_person_id END,
		    CASE WHEN review.accepted_relationship_id IN (SELECT id FROM person_relationships)
		         THEN review.accepted_relationship_id END,
		    review.status, review.source, review.source_ref,
		    ` + reviewResourceUID + `,
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
func copyEmploymentData(tx *sql.Tx, result *CopyResult) error {
	hasEmployments, err := sourceTableExists(tx, "employments")
	if err != nil {
		return fmt.Errorf("check employment schema: %w", err)
	}
	if !hasEmployments {
		return nil
	}
	organizationsCopied, err := copyByName(tx, "organizations", `id IN (
			SELECT DISTINCT organization_id FROM src.employments
			WHERE person_id IN (SELECT id FROM persons)
		)`)
	if err != nil {
		return fmt.Errorf("copy organizations: %w", err)
	}
	if result.Organizations, err = organizationsCopied.RowsAffected(); err != nil {
		return fmt.Errorf("organizations rows affected: %w", err)
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
	employmentsCopied, err := copyByName(tx, "employments",
		`person_id IN (SELECT id FROM persons)`)
	if err != nil {
		return fmt.Errorf("copy employments: %w", err)
	}
	if result.Employments, err = employmentsCopied.RowsAffected(); err != nil {
		return fmt.Errorf("employments rows affected: %w", err)
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
		"person_fact_pin_events",
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
	hasCandidates, err := sourceTableExists(tx, identityMatchCandidatesTableName)
	if err != nil {
		return 0, fmt.Errorf("check identity match candidate schema: %w", err)
	}
	if hasCandidates {
		if err := copyByNameWithCommunicationServiceMap(
			tx, identityMatchCandidatesTableName, subsetSourceIdentityMatchCandidateWhere,
		); err != nil {
			return 0, fmt.Errorf("copy identity_match_candidates: %w", err)
		}
		hasEvidence, err := sourceTableExists(tx, identityMatchEvidenceTableName)
		if err != nil {
			return 0, fmt.Errorf("check identity match evidence schema: %w", err)
		}
		if hasEvidence {
			if _, err := copyByName(tx, identityMatchEvidenceTableName,
				`candidate_id IN (SELECT id FROM identity_match_candidates)`); err != nil {
				return 0, fmt.Errorf("copy identity_match_evidence: %w", err)
			}
		}

		supportTables := []struct {
			table      string
			ownerTable string
			ownerKey   string
		}{
			{
				table: identityMatchCandidateSourcesTableName, ownerTable: identityMatchCandidatesTableName,
				ownerKey: "candidate_id",
			},
			{
				table: identityMatchEvidenceSourcesTableName, ownerTable: identityMatchEvidenceTableName,
				ownerKey: "evidence_id",
			},
		}
		for _, support := range supportTables {
			hasSupport, err := sourceTableExists(tx, support.table)
			if err != nil {
				return 0, fmt.Errorf("check %s schema: %w", support.table, err)
			}
			if !hasSupport {
				continue
			}
			// Archives upgraded before source-support provenance existed
			// carry conservative associations to every source. They keep
			// source-removal cleanup safe, but must not pull unrelated source
			// metadata into a shared subset. Explicit rows may expand the
			// subset's source set; conservative rows are retained only when
			// their source is already present for another reason.
			hasConservativeMarker, err := sourceColumnExists(
				tx, support.table, "is_conservative",
			)
			if err != nil {
				return 0, fmt.Errorf("check %s provenance schema: %w", support.table, err)
			}
			if !hasConservativeMarker {
				continue
			}
			ownerWhere := support.ownerKey + ` IN (SELECT id FROM ` + support.ownerTable + `)`
			explicitSupportWhere := ownerWhere + `
				AND is_conservative = FALSE`
			sourceResult, err := copyByName(tx, "sources", `id IN (
				SELECT source_id FROM src.`+support.table+`
				WHERE `+explicitSupportWhere+`
			) AND id NOT IN (SELECT id FROM sources)`)
			if err != nil {
				return 0, fmt.Errorf("copy %s sources: %w", support.table, err)
			}
			copiedSources, err := sourceResult.RowsAffected()
			if err != nil {
				return 0, fmt.Errorf("%s sources rows affected: %w", support.table, err)
			}
			extraSources += copiedSources
			if _, err := copyByName(tx, support.table,
				ownerWhere+`
					 AND source_id IN (SELECT id FROM sources)`); err != nil {
				return 0, fmt.Errorf("copy %s: %w", support.table, err)
			}
		}
	}
	return extraSources, nil
}

// copyVCardResourceEnvelopes copies the native vCard resources of people
// already inside the subset. Callers gate it on IncludeVCardResources: an
// envelope body is opaque here, so the person boundary is the only boundary
// this copy can enforce over its contents. The source-table checks retain
// compatibility with archives created before the envelope and retired-UID
// tables existed.
func copyVCardResourceEnvelopes(tx *sql.Tx) error {
	hasEnvelopes, err := sourceTableExists(tx, "vcard_resource_envelopes")
	if err != nil {
		return fmt.Errorf("check vCard resource envelope schema: %w", err)
	}
	if hasEnvelopes {
		if _, err := copyByName(tx, "vcard_resource_envelopes",
			`person_id IN (SELECT id FROM persons)`); err != nil {
			return fmt.Errorf("copy vCard resource envelopes: %w", err)
		}
	}

	hasAliases, err := sourceTableExists(tx, "person_uid_aliases")
	if err != nil {
		return fmt.Errorf("check retired person UID alias schema: %w", err)
	}
	if hasAliases {
		if _, err := copyByName(tx, "person_uid_aliases",
			`surviving_person_id IN (SELECT id FROM persons)`); err != nil {
			return fmt.Errorf("copy retired person UID aliases: %w", err)
		}
	}
	return nil
}

// vcardMappingOwnerTables are the tables a native vCard mapping may name as
// the owner of an occurrence, matching the projection's owner fields in
// internal/vcardmap. Every one of them has an integer id primary key. A mapping
// on a table not listed here is left as it is: the projection leaves such
// mappings alone too.
var vcardMappingOwnerTables = map[string]struct{}{
	"persons": {}, "person_names": {}, "person_contact_points": {},
	"person_addresses": {}, "person_dates": {}, "person_categories": {},
	"person_media": {}, "person_attribute_values": {}, "employments": {},
	personRelationshipsTableName: {}, "person_relationship_reviews": {},
}

// releaseVCardMappingsToMissingOwners drops, from every copied envelope, the
// native mappings whose owner rows the subset did not copy — an edge to a
// person outside it, an attribute value the options excluded — and returns
// their occurrences to the residue. The body is copied verbatim; only the
// metadata's claim of ownership goes, so a later projection sees an unmanaged
// occurrence to keep rather than a stale owner whose occurrence it retires. It
// must run only after every owner table has been copied.
func releaseVCardMappingsToMissingOwners(tx *sql.Tx) error {
	copied, err := listCopiedVCardEnvelopeMetadata(tx)
	if err != nil {
		return err
	}
	for _, envelope := range copied {
		if err := releaseEnvelopeMappingsToMissingOwners(tx, envelope.id, envelope.metadata); err != nil {
			return err
		}
	}
	return nil
}

type copiedVCardEnvelope struct {
	id       int64
	metadata string
}

// listCopiedVCardEnvelopeMetadata reads every copied envelope's metadata up
// front, so the mapping release below can update rows without a cursor open.
func listCopiedVCardEnvelopeMetadata(tx *sql.Tx) ([]copiedVCardEnvelope, error) {
	rows, err := tx.Query(`SELECT id, resource_metadata FROM vcard_resource_envelopes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list copied vCard resource envelopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	copied := make([]copiedVCardEnvelope, 0)
	for rows.Next() {
		var envelope copiedVCardEnvelope
		if err := rows.Scan(&envelope.id, &envelope.metadata); err != nil {
			return nil, fmt.Errorf("scan copied vCard resource envelope: %w", err)
		}
		copied = append(copied, envelope)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list copied vCard resource envelopes: %w", err)
	}
	return copied, nil
}

func releaseEnvelopeMappingsToMissingOwners(tx *sql.Tx, id int64, metadata string) error {
	envelope, err := vcard.UnmarshalResourceMetadata([]byte(metadata))
	if err != nil {
		return fmt.Errorf("decode copied vCard resource envelope %d: %w", id, err)
	}
	kept := make([]vcard.NativeMapping, 0, len(envelope.NativeMappings))
	for _, mapping := range envelope.NativeMappings {
		present, err := vcardMappingOwnerCopied(tx, mapping)
		if err != nil {
			return fmt.Errorf("check owner of vCard resource envelope %d mapping: %w", id, err)
		}
		if present {
			kept = append(kept, mapping)
		}
	}
	if len(kept) == len(envelope.NativeMappings) {
		return nil
	}
	envelope.NativeMappings = kept
	envelope.Residue = vcard.ResidueWithMappings(envelope.PropertyTree, kept)
	updated, err := vcard.MarshalResourceMetadata(envelope)
	if err != nil {
		return fmt.Errorf("encode copied vCard resource envelope %d: %w", id, err)
	}
	if _, err := tx.Exec(`UPDATE vcard_resource_envelopes SET resource_metadata = ? WHERE id = ?`,
		string(updated), id); err != nil {
		return fmt.Errorf("release vCard resource envelope %d mappings: %w", id, err)
	}
	return nil
}

// vcardMappingOwnerCopied reports whether the mapping's owner row exists in
// the destination and still stands behind its occurrence. Owners on tables
// outside vcardMappingOwnerTables count as present, since nothing here can
// check them. A relationship review whose accepted edge this copy filtered
// out keeps its ledger row but will never reappear in a projection snapshot,
// so its mapping counts as released too.
func vcardMappingOwnerCopied(tx *sql.Tx, mapping vcard.NativeMapping) (bool, error) {
	if _, known := vcardMappingOwnerTables[mapping.Table]; !known {
		return true, nil
	}
	var one int
	err := tx.QueryRow(`SELECT 1 FROM `+mapping.Table+` WHERE id = ?`, mapping.RowID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if mapping.Table != "person_relationship_reviews" {
		return true, nil
	}
	var edgeCleared bool
	err = tx.QueryRow(`
		SELECT copied.accepted_relationship_id IS NULL
		   AND original.accepted_relationship_id IS NOT NULL
		FROM person_relationship_reviews copied
		JOIN src.person_relationship_reviews original ON original.id = copied.id
		WHERE copied.id = ?`, mapping.RowID).Scan(&edgeCleared)
	if err != nil {
		return false, fmt.Errorf("check copied review %d accepted edge: %w", mapping.RowID, err)
	}
	return !edgeCleared, nil
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

// sourceColumnExpression returns the qualified column reference when the
// source table has the column and NULL when it does not, so an explicit
// column list can read archives that predate the column.
func sourceColumnExpression(tx *sql.Tx, table, column, alias string) (string, error) {
	present, err := sourceColumnExists(tx, table, column)
	if err != nil {
		return "", fmt.Errorf("check %s.%s: %w", table, column, err)
	}
	if !present {
		return "NULL", nil
	}
	return alias + "." + column, nil
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
