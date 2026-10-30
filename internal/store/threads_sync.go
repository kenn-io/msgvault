package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Threads sync types are paired with exact request fingerprints. A full
// completion can publish a baseline without changing its request identity.
const (
	ThreadsSyncBaseline = "baseline"
	ThreadsSyncFull     = "full"
	ThreadsSyncPost     = "post"
)

type ThreadsResumeQuery struct {
	BaselineFingerprint, FullFingerprint, PostFingerprint string
}

type ThreadsRoot struct {
	MessageID   int64
	MediaID     string
	PublishedAt time.Time
}

// GetThreadsSyncResumeContext returns one compatible completion and the newest
// recoverable checkpoint after it. Call after acquiring the execution lease,
// which recovers abandoned running rows. Absence is represented by nil rows.
// Targeted and source-wide histories never invalidate each other's progress.
func (s *Store) GetThreadsSyncResumeContext(ctx context.Context, sourceID int64, query ThreadsResumeQuery) (baseline, checkpoint *SyncRun, err error) {
	condition := `((sync_type = ? AND request_fingerprint = ?) OR (sync_type = ? AND request_fingerprint = ?))`
	args := []any{ThreadsSyncBaseline, query.BaselineFingerprint, ThreadsSyncFull, query.FullFingerprint}
	if query.PostFingerprint != "" {
		if query.BaselineFingerprint != "" || query.FullFingerprint != "" {
			return nil, nil, errors.New("Threads targeted resume cannot include source-wide fingerprints")
		}
		condition = `(sync_type = ? AND request_fingerprint = ?)`
		args = []any{ThreadsSyncPost, query.PostFingerprint}
	} else if query.BaselineFingerprint == "" || query.FullFingerprint == "" || query.BaselineFingerprint == query.FullFingerprint {
		return nil, nil, errors.New("Threads source-wide resume requires distinct baseline and full fingerprints")
	}
	const columns = `id, source_id, started_at, completed_at, status,
  messages_processed, messages_added, messages_updated, errors_count,
  error_message, cursor_before, cursor_after, request_fingerprint`
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		read := func(statusCondition string, afterID int64) (*SyncRun, error) {
			params := append([]any{sourceID, afterID}, args...)
			run, readErr := scanSyncRun(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM sync_runs
    WHERE source_id = ? AND id > ? AND `+condition+` AND `+statusCondition+` ORDER BY id DESC LIMIT 1`, params...))
			if errors.Is(readErr, sql.ErrNoRows) {
				return nil, nil //nolint:nilnil // No compatible history is a normal first run.
			}
			return run, readErr
		}
		var readErr error
		baseline, readErr = read(`status = 'completed'`, 0)
		if readErr != nil {
			return readErr
		}
		afterID := int64(0)
		if baseline != nil {
			afterID = baseline.ID
		}
		checkpoint, readErr = read(`status IN ('paused','failed','cancelled') AND cursor_before IS NOT NULL AND cursor_before != ''`, afterID)
		return readErr
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read Threads resume for source %d: %w", sourceID, err)
	}
	return baseline, checkpoint, nil
}

// threadsOwnRootPredicate reads only per-message structural ownership evidence.
// JSON types matter: the string "true" and the number 1 are not verified flags.
func (s *Store) threadsOwnRootPredicate() string {
	const scope = `m.message_type = 'threads' AND EXISTS (
   SELECT 1 FROM sources src WHERE src.id = m.source_id AND src.source_type = 'threads'
  ) AND `
	if s.IsPostgreSQL() {
		return scope + `m.deleted_at IS NULL AND m.sent_at IS NOT NULL
   AND jsonb_typeof(m.metadata->'root_owned') = 'boolean'
   AND m.metadata->>'root_owned' = 'true'
   AND jsonb_typeof(m.metadata->'root_id') = 'string'
   AND m.metadata->>'root_id' = m.source_message_id
   AND (m.metadata->'parent_id' IS NULL OR m.metadata->'parent_id' = 'null'::jsonb)`
	}
	return scope + `m.deleted_at IS NULL AND m.sent_at IS NOT NULL AND CASE WHEN json_valid(m.metadata) THEN
  json_type(m.metadata,'$.root_owned') = 'true'
  AND json_type(m.metadata,'$.root_id') = 'text'
  AND json_extract(m.metadata,'$.root_id') = m.source_message_id
  AND (json_type(m.metadata,'$.parent_id') IS NULL OR json_type(m.metadata,'$.parent_id') = 'null')
  ELSE FALSE END`
}

// ThreadsOwnRootUpperKeyContext freezes a pass at the greatest verified root's
// local message key. Later inserts wait for the next pass, regardless of time.
func (s *Store) ThreadsOwnRootUpperKeyContext(ctx context.Context, sourceID int64) (int64, error) {
	var key int64
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(m.id),0) FROM messages m WHERE m.source_id = ? AND `+s.threadsOwnRootPredicate()), sourceID).Scan(&key)
	if err != nil {
		return 0, fmt.Errorf("read Threads root upper key for source %d: %w", sourceID, err)
	}
	return key, nil
}

// ListThreadsOwnRootsContext walks (afterID, throughID] in stable local-key
// order, including old roots with no known replies. A zero hotSince selects
// all roots; hot passes must reuse their saved lower time on resumption.
func (s *Store) ListThreadsOwnRootsContext(ctx context.Context, sourceID, afterID, throughID int64, hotSince time.Time, limit int) ([]ThreadsRoot, error) {
	if afterID < 0 || throughID < 0 || limit <= 0 {
		return nil, errors.New("Threads root keys must be nonnegative and limit positive")
	}
	query := `SELECT ` + s.threadsRootColumns() + ` FROM messages m
  WHERE m.source_id = ? AND m.id > ? AND m.id <= ? AND ` + s.threadsOwnRootPredicate()
	// SQLite stores provider timestamps as text, possibly with different offsets
	// and fractional precision. Its coarse instant filter finds candidates; the
	// Go comparison below preserves the exact inclusive boundary. Refill pages
	// after discarding candidates so a short page cannot prematurely end a pass.
	var timeBound any
	if !hotSince.IsZero() {
		if s.IsPostgreSQL() {
			query += ` AND m.sent_at >= ?`
			timeBound = hotSince.UTC().Truncate(time.Microsecond)
		} else {
			query += ` AND julianday(m.sent_at) >= julianday(?)`
			timeBound = hotSince.UTC().Truncate(time.Second)
		}
	}
	query = s.Rebind(query + ` ORDER BY m.id LIMIT ?`)
	roots := make([]ThreadsRoot, 0)
	for len(roots) < limit {
		pageLimit := limit - len(roots)
		args := []any{sourceID, afterID, throughID}
		if timeBound != nil {
			args = append(args, timeBound)
		}
		args = append(args, pageLimit)
		count := 0
		err := func() error {
			rows, err := s.db.QueryContext(ctx, query, args...)
			if err != nil {
				return fmt.Errorf("list Threads roots for source %d: %w", sourceID, err)
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var root ThreadsRoot
				var published requiredTimestamp
				var exact sql.NullString
				if err := rows.Scan(&root.MessageID, &root.MediaID, &published, &exact); err != nil {
					return fmt.Errorf("scan Threads root: %w", err)
				}
				root.PublishedAt, err = threadsRootTimestamp(published.Time, exact)
				if err != nil {
					return err
				}
				count++
				afterID = root.MessageID
				if !hotSince.IsZero() && root.PublishedAt.Before(hotSince) {
					continue
				}
				roots = append(roots, root)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("read Threads roots: %w", err)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
		if count < pageLimit {
			break
		}
	}
	return roots, nil
}

// GetThreadsOwnRootContext authorizes a single archived root by the same
// structural predicate used by sweeps, without loading an entire source.
func (s *Store) GetThreadsOwnRootContext(ctx context.Context, sourceID int64, mediaID string) (ThreadsRoot, error) {
	var root ThreadsRoot
	var published requiredTimestamp
	var exact sql.NullString
	err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+s.threadsRootColumns()+` FROM messages m WHERE m.source_id=? AND m.source_message_id=? AND `+s.threadsOwnRootPredicate()), sourceID, mediaID).Scan(&root.MessageID, &root.MediaID, &published, &exact)
	if err != nil {
		return root, fmt.Errorf("get verified Threads root: %w", err)
	}
	root.PublishedAt, err = threadsRootTimestamp(published.Time, exact)
	return root, err
}

// The provider timestamp retains precision beyond PostgreSQL's microseconds.
// Older archive rows without it keep their stored timestamp as a fallback.
func (s *Store) threadsRootColumns() string {
	if s.IsPostgreSQL() {
		return `m.id,m.source_message_id,m.sent_at,m.metadata->>'timestamp'`
	}
	return `m.id,m.source_message_id,m.sent_at,CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata,'$.timestamp') END`
}
func threadsRootTimestamp(fallback time.Time, exact sql.NullString) (time.Time, error) {
	if !exact.Valid {
		return fallback.UTC(), nil
	}
	at, err := time.Parse(time.RFC3339Nano, exact.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid archived Threads timestamp: %w", err)
	}
	return at.UTC(), nil
}
