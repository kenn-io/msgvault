package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrSourceNotFound is returned by GetSourceByID and GetSourceByIdentifier
// when no source row matches. Wrapped via fmt.Errorf("...: %w", ...) so
// callers can use errors.Is to distinguish absence from real DB
// errors.
var ErrSourceNotFound = errors.New("source not found")

// GetSourceByID returns the source with the given ID, or
// ErrSourceNotFound (wrapped) if no row matches.
func (s *Store) GetSourceByID(id int64) (*Source, error) {
	return s.GetSourceByIDContext(context.Background(), id)
}

// GetSourceByIDContext is the request-aware form of GetSourceByID.
func (s *Store) GetSourceByIDContext(ctx context.Context, id int64) (*Source, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, source_type, identifier, display_name, google_user_id,
		       last_sync_at, sync_cursor, sync_config, oauth_app,
		       created_at, updated_at
		FROM sources
		WHERE id = ?
	`, id)

	source, err := scanSource(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("source %d: %w", id, ErrSourceNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("get source by id: %w", err)
	}
	return source, nil
}

// GetSourcesByIdentifier returns all sources matching an identifier,
// regardless of source_type. Use this when the identifier may be
// shared across source types (e.g., gmail + mbox import).
func (s *Store) GetSourcesByIdentifier(
	identifier string,
) ([]*Source, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE identifier = ?
		ORDER BY source_type
	`, identifier)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSourcesByIdentifierOrDisplayName returns all sources whose identifier or
// display_name matches the given value. This is the preferred single-query
// lookup when resolving a user-supplied email or identifier string.
func (s *Store) GetSourcesByIdentifierOrDisplayName(query string) ([]*Source, error) {
	return s.GetSourcesByIdentifierOrDisplayNameContext(context.Background(), query)
}

// GetSourcesByIdentifierOrDisplayNameContext is the request-aware form of
// GetSourcesByIdentifierOrDisplayName.
func (s *Store) GetSourcesByIdentifierOrDisplayNameContext(
	ctx context.Context,
	query string,
) ([]*Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE identifier = ? OR display_name = ?
		ORDER BY source_type
	`, query, query)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSourcesByDisplayName returns all sources with the given display name.
// Use this as a fallback when looking up IMAP sources by their human-readable
// email address rather than the full imaps:// identifier.
// Note: display_name is not constrained to be unique — callers receive all
// matching rows if more than one source shares the same name.
func (s *Store) GetSourcesByDisplayName(displayName string) ([]*Source, error) {
	rows, err := s.db.Query(`
		SELECT id, source_type, identifier, display_name,
		       google_user_id, last_sync_at, sync_cursor, sync_config,
		       oauth_app, created_at, updated_at
		FROM sources
		WHERE display_name = ?
		ORDER BY source_type
	`, displayName)
	if err != nil {
		return nil, fmt.Errorf("query sources by display name: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sources []*Source
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources = append(sources, src)
	}
	return sources, rows.Err()
}

// GetSourcesByTypeAndAccount returns every source of the given source_type
// whose sync_config JSON carries the given account_email.
//
// Config-driven sources (calendar, and any future per-account fan-out) decouple
// their per-source identifier — a natural key like a calendarId — from the
// OAuth account/token key, which lives in sync_config.account_email. A single
// account may own many sources (e.g. several calendars), all sharing one token
// file. Filtering happens in Go after a typed list query so it stays
// dialect-portable (no SQLite json_extract vs PG ->> divergence); the set of one
// account's sources is small, so this is not a hot path. A source whose
// sync_config is NULL or unparseable is skipped rather than aborting the scan.
func (s *Store) GetSourcesByTypeAndAccount(sourceType, accountEmail string) ([]*Source, error) {
	return s.GetSourcesByTypeAndAccountContext(context.Background(), sourceType, accountEmail)
}

// GetSourcesByTypeAndAccountContext is the request-aware form of
// GetSourcesByTypeAndAccount.
func (s *Store) GetSourcesByTypeAndAccountContext(
	ctx context.Context,
	sourceType, accountEmail string,
) ([]*Source, error) {
	all, err := s.ListSourcesContext(ctx, sourceType)
	if err != nil {
		return nil, fmt.Errorf("list sources by type %q: %w", sourceType, err)
	}
	accountEmail = strings.TrimSpace(accountEmail)
	var matched []*Source
	for _, src := range all {
		if !src.SyncConfig.Valid {
			continue
		}
		var cfg struct {
			AccountEmail string `json:"account_email"`
		}
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &cfg); err != nil {
			continue
		}
		if accountEmail != "" && strings.EqualFold(strings.TrimSpace(cfg.AccountEmail), accountEmail) {
			matched = append(matched, src)
		}
	}
	return matched, nil
}

// RemoveSource deletes a source and all its associated data.
// FTS5 rows are cleaned up explicitly (no FK cascade for virtual tables).
// CASCADE handles conversations, messages, labels, attachments, sync state.
// Orphaned participants are left for a future `gc` command.
//
// Runs under runMaintenance: the cascade DELETE removes millions of rows
// across messages/recipients/labels/bodies/raw on a large archive and the
// FTSDelete rewrites every matching tsvector, so the maintenance hatch
// disables the pool-wide 30s statement_timeout for this tx (finding S1).
// No-op timeout reset on SQLite.
func (s *Store) RemoveSource(sourceID int64) error {
	return s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		return s.removeSourceExec(ctx, tx, sourceID)
	})
}

// RemoveSourceSerialized deletes a source while holding an exclusive write
// lock, and reports whether any sync was running at the moment the lock was
// acquired. Callers use hadActiveSync to gate follow-up operations (such as
// attachment file deletion) that must not race with a sync worker.
//
// Running the active-sync check and the cascade in the same transaction
// closes the race where a sync starts between a pre-check and RemoveSource:
// StartSync blocks on our exclusive lock, so either (a) it committed before
// us and we observe the running row, or (b) it has not yet started and will
// fail after we commit because the source is gone. Packed hashes that will
// lose their last attachment reference are collected under the same lock so
// their logical mappings can be deleted atomically with the source cascade.
func (s *Store) RemoveSourceSerialized(
	ctx context.Context, sourceID int64,
) (hadActiveSync bool, packedMappingsRemoved int64, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := s.dialect.BeginExclusive(ctx, conn); err != nil {
		return false, 0, fmt.Errorf("begin exclusive: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	var count int
	if err := conn.QueryRowContext(ctx,
		s.dialect.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE status = 'running'`),
	).Scan(&count); err != nil {
		return false, 0, fmt.Errorf("check active syncs: %w", err)
	}
	hadActiveSync = count > 0

	uniquePackedHashes, err := func() ([]string, error) {
		rows, err := conn.QueryContext(ctx,
			s.dialect.Rebind(packedBlobHashesUniqueToSourceSQL),
			sourceID, sourceID, sourceID,
		)
		if err != nil {
			return nil, fmt.Errorf("list unique packed blobs: %w", err)
		}
		defer rows.Close() //nolint:errcheck // read-only cursor
		var hashes []string
		for rows.Next() {
			var hash string
			if err := rows.Scan(&hash); err != nil {
				return nil, fmt.Errorf("scan unique packed blob: %w", err)
			}
			hashes = append(hashes, hash)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate unique packed blobs: %w", err)
		}
		return hashes, nil
	}()
	if err != nil {
		return hadActiveSync, 0, err
	}

	if s.fts5Available {
		if _, err := conn.ExecContext(
			ctx, s.dialect.FTSDeleteSQL(), sourceID,
		); err != nil {
			return hadActiveSync, 0, fmt.Errorf("delete FTS rows: %w", err)
		}
	}
	if err := s.deleteSourceObservationIdentityCandidatesContext(
		ctx, conn, sourceID,
	); err != nil {
		return hadActiveSync, 0, err
	}

	res, err := conn.ExecContext(
		ctx, s.dialect.Rebind(`DELETE FROM sources WHERE id = ?`), sourceID,
	)
	if err != nil {
		return hadActiveSync, 0, fmt.Errorf("delete source: %w", err)
	}
	deletedSources, err := res.RowsAffected()
	if err != nil {
		return hadActiveSync, 0, fmt.Errorf("check rows affected: %w", err)
	}
	if deletedSources == 0 {
		return hadActiveSync, 0, fmt.Errorf("source %d not found", sourceID)
	}
	if err := s.deleteUnsupportedObservationIdentityConflictsContext(ctx, conn); err != nil {
		return hadActiveSync, 0, err
	}

	const deleteChunkSize = 500
	for start := 0; start < len(uniquePackedHashes); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(uniquePackedHashes))
		chunk := uniquePackedHashes[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, hash := range chunk {
			args[i] = hash
		}
		res, err := conn.ExecContext(ctx, s.dialect.Rebind(
			`DELETE FROM attachment_pack_index WHERE blob_hash IN (`+placeholders+`)`), args...)
		if err != nil {
			return hadActiveSync, 0, fmt.Errorf("delete unique packed blob mappings: %w", err)
		}
		removed, err := res.RowsAffected()
		if err != nil {
			return hadActiveSync, 0, fmt.Errorf("count deleted packed blob mappings: %w", err)
		}
		packedMappingsRemoved += removed
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return hadActiveSync, 0, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return hadActiveSync, packedMappingsRemoved, nil
}

const packedBlobHashesUniqueToSourceSQL = `
	WITH source_blobs(blob_hash) AS (
	    SELECT LOWER(a.content_hash) FROM attachments a
	    WHERE a.content_hash IS NOT NULL AND a.content_hash != ''
	      AND EXISTS (SELECT 1 FROM messages m
	                  WHERE m.id = a.message_id AND m.source_id = ?)
	    UNION
	    SELECT LOWER(a.thumbnail_hash) FROM attachments a
	    WHERE a.thumbnail_hash IS NOT NULL AND a.thumbnail_hash != ''
	      AND EXISTS (SELECT 1 FROM messages m
	                  WHERE m.id = a.message_id AND m.source_id = ?)
	)
	SELECT sb.blob_hash
	FROM source_blobs sb
	WHERE EXISTS (SELECT 1 FROM attachment_pack_index p
	              WHERE p.blob_hash = sb.blob_hash)
	  AND NOT EXISTS (
	      SELECT 1 FROM attachments a2
	      WHERE (LOWER(a2.content_hash) = sb.blob_hash OR LOWER(a2.thumbnail_hash) = sb.blob_hash)
	        AND EXISTS (SELECT 1 FROM messages m2
	                    WHERE m2.id = a2.message_id AND m2.source_id != ?)
	  )
	ORDER BY sb.blob_hash`

// removeSourceExec performs the identity cleanup, FTS cleanup, and source
// deletion in one maintenance transaction.
func (s *Store) removeSourceExec(
	ctx context.Context, tx *loggedTx, sourceID int64,
) error {
	// Identity candidate writes take this lock before validating and writing
	// their polymorphic endpoints. Taking the same lock before source cleanup
	// prevents a candidate from being inserted after cleanup but before the
	// source cascade removes its observation endpoint.
	if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
		return err
	}
	if err := s.lockProfileIdentityKeyTxContext(
		ctx, tx, "source-contact-observation", sourceID,
	); err != nil {
		return err
	}
	if s.fts5Available {
		if _, err := tx.Exec(s.dialect.FTSDeleteSQL(), sourceID); err != nil {
			return fmt.Errorf("delete FTS rows: %w", err)
		}
	}
	if err := s.deleteSourceObservationIdentityCandidatesContext(
		ctx, tx, sourceID,
	); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM sources WHERE id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("source %d not found", sourceID)
	}
	if err := s.deleteUnsupportedObservationIdentityConflictsContext(ctx, tx); err != nil {
		return err
	}
	return nil
}

// deleteSourceObservationIdentityCandidatesContext removes generated
// participant conflicts and candidates with observation endpoints before the
// source cascade removes those observations. Evidence for deleted candidates
// follows its candidate foreign-key cascade. After the source deletion, the
// shared cleanup demotes promoted participant candidates whose support is gone.
func (s *Store) deleteSourceObservationIdentityCandidatesContext(
	ctx context.Context, execer contextQuerier, sourceID int64,
) error {
	// Automatically generated conflicts use participant endpoints so they can
	// be reviewed as person-link candidates. Recompute their backing contact
	// observations while the source rows still exist: remove a conflict only
	// when the deleted source participates in it and no genuinely conflicting
	// current observation pair remains outside that source.
	if _, err := execer.ExecContext(ctx, s.dialect.Rebind(`
		WITH stale_conflicts AS (
			SELECT c.id
			FROM identity_match_candidates c
			WHERE c.left_kind = 'participant'
			  AND c.right_kind = 'participant'
			  AND c.state = 'conflict'
			  AND c.observation_conflict_origin = 'generated'
			  AND c.normalized_value IS NOT NULL
			  AND c.basis IN ('email', 'phone', 'service_scope_username')
			  AND EXISTS (
				SELECT 1 FROM participant_contact_observations removed
				WHERE removed.source_id = ?
				  AND removed.participant_id IN (c.left_id, c.right_id)
				  AND `+identityCandidateObservationMatchSQL("removed")+`
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM participant_contact_observations kept_left
				WHERE kept_left.participant_id = c.left_id
				  AND (kept_left.source_id IS NULL OR kept_left.source_id != ?)
				  AND `+identityCandidateObservationMatchSQL("kept_left")+`
				  AND EXISTS (
					SELECT 1 FROM participant_contact_observations kept_right
					WHERE kept_right.participant_id = c.right_id
					  AND (kept_right.source_id IS NULL OR kept_right.source_id != ?)
					  AND `+identityCandidateObservationMatchSQL("kept_right")+`
					  AND `+identityCandidateObservationProviderConflictSQL(
		"kept_left", "kept_right",
	)+`
				  )
			  )
		)
		DELETE FROM identity_match_candidates
		WHERE id IN (SELECT id FROM stale_conflicts)`),
		sourceID, sourceID, sourceID); err != nil {
		return fmt.Errorf("delete stale source observation conflicts: %w", err)
	}
	if _, err := execer.ExecContext(ctx, s.dialect.Rebind(`
		DELETE FROM identity_match_candidates
		WHERE (left_kind = 'observation' AND left_id IN (
			SELECT id FROM participant_contact_observations WHERE source_id = ?
		)) OR (right_kind = 'observation' AND right_id IN (
			SELECT id FROM participant_contact_observations WHERE source_id = ?
		))`), sourceID, sourceID); err != nil {
		return fmt.Errorf("delete source observation identity candidates: %w", err)
	}
	return nil
}

func identityCandidateObservationMatchSQL(observationAlias string) string {
	return `(c.basis = 'email' AND ` + observationAlias + `.address_kind = 'email'
		OR c.basis = 'phone' AND ` + observationAlias + `.address_kind = 'phone'
		OR c.basis = 'service_scope_username' AND ` + observationAlias + `.address_kind NOT IN ('email', 'phone'))
		AND (` + observationAlias + `.service_id = c.service_id OR
			(` + observationAlias + `.service_id IS NULL AND c.service_id IS NULL))
		AND (` + observationAlias + `.scope_kind = c.scope_kind OR
			(` + observationAlias + `.scope_kind IS NULL AND c.scope_kind IS NULL))
		AND (` + observationAlias + `.scope_value = c.scope_value OR
			(` + observationAlias + `.scope_value IS NULL AND c.scope_value IS NULL))
		AND ` + observationAlias + `.normalized_value = c.normalized_value
		AND ` + observationAlias + `.active_until IS NULL
		AND ` + observationAlias + `.superseded_at IS NULL`
}
