package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// embedGenStampChunkRows caps how many message ids go into a single
// SetEmbedGen UPDATE. Each statement binds one placeholder per id plus
// one for the target generation, so 500 ids = 501 bound parameters —
// comfortably under SQLite's historical 999 (and the store's 900-param
// convention; see insertInChunks) and PostgreSQL's 65,535. Mirrors the
// store's existing chunking discipline so an oversized embed batch never
// blows the driver bind ceiling. A var (not const) only so tests can
// lower it to exercise the chunk boundary; production never reassigns it.
var embedGenStampChunkRows = 500

// ScanForEmbedding returns up to limit live message ids that still need
// embedding for the target generation — i.e. rows whose embed_gen does
// not already equal target — scanning forward from afterID in id order.
//
// The portable predicate (embed_gen IS NULL OR embed_gen <> ?) covers
// both never-embedded rows (NULL) and rows stamped for a different
// generation, and avoids any IS DISTINCT FROM driver-version doubt. The
// forward bound (id > afterID) lets the caller resume from a per-gen
// watermark; pass 0 for a full scan (the backstop). Results are ordered
// by id so the caller can advance the watermark to the batch's max id.
//
// This runs against the MAIN db (messages + embed_gen live there on both
// backends). On SQLite the embeddings themselves live in vectors.db, so
// this find-work query and the SetEmbedGen stamp cannot share a tx with
// the embeddings upsert — the worker orders the steps (upsert, then
// stamp) and relies on idempotency, see internal/vector/embed/worker.go.
func (s *Store) ScanForEmbedding(ctx context.Context, target int64, afterID int64, limit int) ([]int64, error) {
	return s.ScanForEmbeddingScoped(ctx, target, afterID, limit, nil, nil)
}

// ScanForEmbeddingScoped is ScanForEmbedding limited to the supplied message
// types and source IDs. Empty messageTypes and sourceIDs slices mean the
// full live corpus.
func (s *Store) ScanForEmbeddingScoped(ctx context.Context, target int64, afterID int64, limit int, messageTypes []string, sourceIDs []int64) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	liveWhere, liveArgs := liveMessagesWhereScoped(messageTypes, sourceIDs)
	q := `SELECT id FROM messages
	       WHERE (embed_gen IS NULL OR embed_gen <> ?)
	         AND ` + liveWhere + `
	         AND id > ?
	       ORDER BY id
	       LIMIT ?`
	args := make([]any, 0, 3+len(liveArgs))
	args = append(args, target)
	args = append(args, liveArgs...)
	args = append(args, afterID, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("scan for embedding: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan message id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message ids: %w", err)
	}
	return out, nil
}

// SetEmbedGen stamps embed_gen = target on the given message ids,
// marking them covered for that generation. Used by the embed worker
// after a successful upsert (the rows now have embeddings for target) or
// to skip-mark rows that are missing/empty and will never produce an
// embedding. Idempotent: re-stamping an already-stamped row is a no-op.
//
// The ids are processed in chunks (see embedGenStampChunkRows) to stay
// under the driver's bind limit; chunks are not wrapped in a single
// transaction because each chunk's UPDATE is independently idempotent and
// the cross-DB worker contract already tolerates a partial stamp (the
// next scan re-finds any unstamped rows and re-runs an idempotent batch).
func (s *Store) SetEmbedGen(ctx context.Context, ids []int64, target int64) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += embedGenStampChunkRows {
		end := min(start+embedGenStampChunkRows, len(ids))
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, 1+len(chunk))
		args = append(args, target)
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q := `UPDATE messages SET embed_gen = ? WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("set embed_gen: %w", err)
		}
	}
	return nil
}

// EmbedGenStamp pairs a message id with the last_modified token captured
// when the worker read that message's content. SetEmbedGenIfUnchanged
// stamps embed_gen only while last_modified still equals this value.
//
// LastModified is carried as an opaque `any` so the worker can round-trip
// whatever the driver scanned without the store needing a backend-specific
// type: on SQLite the worker scans CAST(last_modified AS TEXT) into a string
// (defeating go-sqlite3's DATETIME→time.Time coercion, which would otherwise
// reformat the value and break equality on the round-trip) and binds the same
// string back; on PostgreSQL it scans a time.Time and binds the same
// time.Time back. The WHERE comparison runs entirely server-side against the
// stored value.
type EmbedGenStamp struct {
	ID           int64
	LastModified any
}

// EmbedGenMetadataVersion is the store-owned compare-and-set token for
// conversation metadata used by a contextual document. It intentionally does
// not depend on internal/vector/embed, which already imports store.
//
// Digest is the canonical SHA-256 digest of the conversation id and title,
// followed by each participant in participant-id order with its role, rendered
// display name, and driver-canonical updated_at text. The encoding is kept
// byte-compatible with embed.conversationMetadataDigest.
type EmbedGenMetadataVersion struct {
	ConversationID int64
	Digest         string
}

// SetEmbedGenGroupIfUnchanged atomically stamps every member of one published
// document. It returns false without stamping any member when a source token,
// live membership, conversation identity, or metadata digest no longer matches
// the assembly snapshot.
//
// SQLite starts with BEGIN IMMEDIATE, so a writer cannot enter between verify
// and stamp. PostgreSQL first takes the embedding-change clock's exclusive
// transaction lock, then locks every message and every metadata row used by
// the digest. Persistence takes the shared clock lock before its row locks, so
// this ordering prevents a message/conversation lock inversion. Locking the
// conversation row also serializes membership inserts via their foreign-key
// check, closing the phantom-member race.
//
// The embed_gen update fires the row-level last_modified trigger. This group
// path restores each verified token inside the same write transaction because
// contextual document revisions include those tokens. Without the restore,
// the worker's own coverage bookkeeping changes the revision and an initial
// reconciliation pays to publish the same document again. The ordinary
// per-message stamp keeps its established row-watermark behavior.
func (s *Store) SetEmbedGenGroupIfUnchanged(
	ctx context.Context,
	versions []EmbedGenStamp,
	metadataVersion EmbedGenMetadataVersion,
	target int64,
) (stamped bool, retErr error) {
	if len(versions) == 0 {
		return false, nil
	}
	seen := make(map[int64]struct{}, len(versions))
	for _, version := range versions {
		if version.ID == 0 {
			return false, nil
		}
		if _, duplicate := seen[version.ID]; duplicate {
			return false, nil
		}
		seen[version.ID] = struct{}{}
	}
	orderedVersions := append([]EmbedGenStamp(nil), versions...)
	sort.Slice(orderedVersions, func(i, j int) bool {
		return orderedVersions[i].ID < orderedVersions[j].ID
	})

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire embed_gen group connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, s.dialect.BeginWriteSQL()); err != nil {
		return false, fmt.Errorf("begin embed_gen group transaction: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), manualTransactionCleanupTimeout)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(rollbackCtx, "ROLLBACK"); rollbackErr != nil && retErr == nil {
			retErr = fmt.Errorf("rollback embed_gen group transaction: %w", rollbackErr)
		}
	}()
	if s.IsPostgreSQL() {
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_xact_lock(
			hashtextextended('msgvault.embedding_change_clock', 0))`); err != nil {
			return false, fmt.Errorf("lock embed_gen group journal boundary: %w", err)
		}
	}

	lastModified := "last_modified"
	if !s.IsPostgreSQL() {
		lastModified = "CAST(last_modified AS TEXT)"
	}
	for _, version := range orderedVersions {
		query := fmt.Sprintf(`SELECT id FROM messages
			WHERE id = ? AND %s = ?
			  AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, lastModified)
		args := []any{version.ID, version.LastModified}
		if metadataVersion.ConversationID != 0 {
			query += ` AND conversation_id = ?`
			args = append(args, metadataVersion.ConversationID)
		}
		query += s.dialect.SelectForUpdate()
		var id int64
		err := conn.QueryRowContext(ctx, s.dialect.Rebind(query), args...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("lock embed_gen group member %d: %w", version.ID, err)
		}
	}

	if metadataVersion.ConversationID != 0 {
		digest, found, err := s.embedGenMetadataDigest(ctx, conn, metadataVersion.ConversationID)
		if err != nil {
			return false, err
		}
		if !found || digest != metadataVersion.Digest {
			return false, nil
		}
	}

	for _, version := range orderedVersions {
		res, err := conn.ExecContext(ctx, s.dialect.Rebind(
			`UPDATE messages SET embed_gen = ? WHERE id = ? AND last_modified = ?`),
			target, version.ID, version.LastModified)
		if err != nil {
			return false, fmt.Errorf("stamp embed_gen group member %d: %w", version.ID, err)
		}
		matched, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("rows affected for embed_gen group member %d: %w", version.ID, err)
		}
		if matched != 1 {
			return false, nil
		}
		restored, err := conn.ExecContext(ctx, s.dialect.Rebind(
			`UPDATE messages SET last_modified = ? WHERE id = ? AND embed_gen = ?`),
			version.LastModified, version.ID, target)
		if err != nil {
			return false, fmt.Errorf("restore embed_gen group member %d revision token: %w", version.ID, err)
		}
		restoredRows, err := restored.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("rows affected restoring embed_gen group member %d revision token: %w", version.ID, err)
		}
		if restoredRows != 1 {
			return false, nil
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, fmt.Errorf("commit embed_gen group transaction: %w", err)
	}
	committed = true
	return true, nil
}

// ParticipantRevisionSQLite and ParticipantRevisionPostgres render
// participants.updated_at (aliased p) for the embedding metadata digest. The
// coverage CAS here and the assembler's snapshot read must produce
// byte-identical revisions, so both use these exact expressions. The
// PostgreSQL form is pinned with to_char because CAST(timestamptz AS TEXT)
// renders per-session TimeZone/DateStyle — a divergence would make the CAS
// miss on every scope and republish forever.
const (
	ParticipantRevisionSQLite   = "CAST(p.updated_at AS TEXT)"
	ParticipantRevisionPostgres = "to_char(p.updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US')"
)

func (s *Store) embedGenMetadataDigest(
	ctx context.Context, conn *sql.Conn, conversationID int64,
) (string, bool, error) {
	var id int64
	var title string
	err := conn.QueryRowContext(ctx, s.dialect.Rebind(
		`SELECT id, COALESCE(title, '') FROM conversations WHERE id = ?`+
			s.dialect.SelectForUpdate()), conversationID).Scan(&id, &title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("lock embedding conversation %d: %w", conversationID, err)
	}

	lockClause := s.dialect.SelectForUpdate()
	participantRevision := ParticipantRevisionSQLite
	if lockClause != "" {
		lockClause = " FOR UPDATE OF cp, p"
		participantRevision = ParticipantRevisionPostgres
	}
	rows, err := conn.QueryContext(ctx, s.dialect.Rebind(fmt.Sprintf(`
		SELECT cp.participant_id, COALESCE(cp.role, ''),
		       COALESCE(NULLIF(TRIM(p.display_name), ''),
		                NULLIF(p.email_address, ''), NULLIF(p.phone_number, ''), ''),
		       %s
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = ?
		ORDER BY cp.participant_id`+lockClause, participantRevision)), conversationID)
	if err != nil {
		return "", false, fmt.Errorf("lock embedding conversation participants %d: %w", conversationID, err)
	}
	defer func() { _ = rows.Close() }()

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%d\x00%s\x00", id, title)
	for rows.Next() {
		var participantID int64
		var role, displayName, revision string
		if err := rows.Scan(&participantID, &role, &displayName, &revision); err != nil {
			return "", false, fmt.Errorf("scan embedding conversation participant %d: %w", conversationID, err)
		}
		_, _ = fmt.Fprintf(h, "%d\x00%s\x00%s\x00%v\x00",
			participantID, role, displayName, revision)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("iterate embedding conversation participants %d: %w", conversationID, err)
	}
	return hex.EncodeToString(h.Sum(nil)), true, nil
}

// SetEmbedGenIfUnchanged stamps embed_gen = target on each message, but
// ONLY if its last_modified still equals the value captured at content-read
// time (optimistic CAS). A message whose last_modified changed between read
// and stamp — e.g. repair-encoding (or any concurrent content edit) rewrote
// its text, which the DB triggers reflected by bumping last_modified — is
// NOT stamped (its UPDATE matches 0 rows); it stays "needs embedding" and is
// re-found and re-embedded with the corrected content on the next scan. This
// closes the read→stamp race that an unconditional stamp would lose by
// marking the row embedded-with-stale-content.
//
// The worker's own stamp UPDATE bumps last_modified on BOTH backends via
// their triggers: this UPDATE sets only embed_gen (not last_modified), so the
// SQLite AFTER-UPDATE trigger fires (its WHEN OLD.last_modified = NEW... holds)
// and re-stamps last_modified, and the PG BEFORE-UPDATE trigger fires too (its
// WHEN OLD.last_modified IS NOT DISTINCT FROM NEW... holds) and sets
// last_modified = CURRENT_TIMESTAMP. The WHERE comparison matches against the
// PRE-trigger value, so a legitimate stamp still affects exactly 1 row (it is
// NOT a CAS miss); only a value that changed BEFORE this UPDATE ran blocks it.
// The post-stamp bump is correctness-neutral: once embed_gen = target the row
// is terminal/covered and excluded by the scan predicate, so no later scan
// re-finds it on account of the bumped last_modified.
//
// Each row is a separate UPDATE because every message carries a distinct
// last_modified token. Statements are not wrapped in one transaction: each is
// independently correct, and the cross-DB worker contract already tolerates a
// partial stamp (the next scan re-finds any unstamped row and re-runs an
// idempotent batch). Used by the embed worker's content read→stamp path; the
// backfill path keeps the plain SetEmbedGen (it has no read→stamp window).
//
// Returns the ids whose per-row UPDATE matched 0 rows — the CAS MISSES. A miss
// means last_modified moved between the worker's content read and this stamp
// (a concurrent repair/edit bumped it via the DB triggers), so the row was NOT
// stamped and stays "needs embedding". The worker surfaces these (logs them and
// excludes them from its success accounting) but does NOT hold the watermark
// back: a missed row's last_modified moved (and its embed_gen may be NULL), so
// the auto-backstop's watermark-ignoring full scan re-finds and re-embeds it
// with the corrected content. A real driver error still aborts (returns err).
//
// ACCEPTED RESIDUAL — 1-second CAS resolution (single-user). The CAS token is
// last_modified, defaulted/bumped by CURRENT_TIMESTAMP (schema.sql:310 and the
// AFTER/BEFORE-UPDATE triggers), which has 1-SECOND resolution on both backends.
// So a content edit that lands in the SAME WHOLE SECOND as the worker's content
// read leaves last_modified textually UNCHANGED — this CAS then matches and
// stamps embed_gen=target on an embedding built from the now-stale text, a
// missed staleness the sub-second window cannot detect. This is an accepted
// residual for the single-user tool (an edit and an embed of the same message in
// the same second is rare) and is NOT closed by schema/behavior change. It
// self-recovers: the next edit to that message (repair-encoding or any sync
// update) bumps last_modified and clears embed_gen (repair) / re-finds it, and a
// full rebuild or the auto-backstop re-embeds it regardless. See
// docs/usage/vector-search.md ("CAS resolution").
func (s *Store) SetEmbedGenIfUnchanged(ctx context.Context, items []EmbedGenStamp, target int64) (missed []int64, err error) {
	for _, it := range items {
		q := `UPDATE messages SET embed_gen = ? WHERE id = ? AND last_modified = ?`
		res, err := s.db.ExecContext(ctx, q, target, it.ID, it.LastModified)
		if err != nil {
			return missed, fmt.Errorf("set embed_gen if unchanged (id=%d): %w", it.ID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return missed, fmt.Errorf("rows affected (id=%d): %w", it.ID, err)
		}
		if n == 0 {
			missed = append(missed, it.ID)
		}
	}
	return missed, nil
}

// ResetEmbedGen clears embed_gen (sets it back to NULL) on the given
// message ids, marking them as needing embedding again. Used by
// repair-encoding after rewriting a message's text so the scan-and-fill
// worker re-embeds it with the corrected content on its next run. Chunked
// to stay under the driver's bind limit; idempotent.
func (s *Store) ResetEmbedGen(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	for start := 0; start < len(ids); start += embedGenStampChunkRows {
		end := min(start+embedGenStampChunkRows, len(ids))
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		q := `UPDATE messages SET embed_gen = NULL WHERE id IN (` +
			strings.Join(placeholders, ",") + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("reset embed_gen: %w", err)
		}
	}
	return nil
}

// CoverageCounts reports embedding coverage for activeGen, computed from
// the MAIN db (messages + embed_gen) so it is a single-DB query on both
// backends and needs no access to the embeddings store.
//
//   - live:     total live messages (the embedding universe).
//   - stamped:  live messages stamped embed_gen = activeGen. This is the
//     2nd return value (historically named "embedded"). It counts every
//     row the worker has marked DONE for the generation, INCLUDING blanks —
//     messages with no extractable body that were stamped terminal but
//     never produced a vector. It is therefore an UPPER bound on the true
//     embedded count; the embedded/blank split is resolved at the display
//     layer via the backend's EmbeddedMessageCount (the embeddings table
//     lives in a separate DB on SQLite, so this single-DB query cannot do
//     it). blank = stamped - embedded.
//   - blank:    the 3rd return value is always 0 here — it cannot be
//     computed without the embeddings table. The real blank count is
//     derived by the caller as stamped - backend.EmbeddedMessageCount(gen)
//     (see cmd/msgvault/cmd/embeddings_manage.go). Kept in the signature
//     so callers that only need missing (the scheduler/CLI activation gate)
//     do not have to change.
//   - missing:  live messages still needing work for activeGen
//     (embed_gen IS NULL OR embed_gen <> activeGen). live = stamped +
//     missing exactly. With the display-layer split: live = embedded +
//     blank + missing.
//
// activeGen == 0 means "no active/target generation"; then everything
// live is missing and stamped is 0.
func (s *Store) CoverageCounts(ctx context.Context, activeGen int64) (live, stamped, blank, missing int64, err error) {
	return s.CoverageCountsScoped(ctx, activeGen, nil, nil)
}

// CoverageCountsScoped is CoverageCounts limited to the supplied message
// types and source IDs. Empty messageTypes and sourceIDs slices mean the
// full live corpus.
func (s *Store) CoverageCountsScoped(ctx context.Context, activeGen int64, messageTypes []string, sourceIDs []int64) (live, stamped, blank, missing int64, err error) {
	live, err = s.countLiveMessagesScoped(ctx, messageTypes, sourceIDs)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activeGen != 0 {
		liveWhere, liveArgs := liveMessagesWhereScoped(messageTypes, sourceIDs)
		q := `SELECT COUNT(*) FROM messages
		       WHERE embed_gen = ? AND ` + liveWhere
		args := append([]any{activeGen}, liveArgs...)
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&stamped); err != nil {
			return 0, 0, 0, 0, fmt.Errorf("count stamped: %w", err)
		}
	}
	missing = max(live-stamped, 0)
	return live, stamped, 0, missing, nil
}

// EmbeddingConvergenceCounts partitions message coverage for the contextual
// worker without changing the historical CoverageCounts contract. Contextual
// rows are exactly live Beeper and meeting-transcript messages. Every other
// live message type is ordinary. The three total fields always equal the sum
// of their contextual and ordinary counterparts.
type EmbeddingConvergenceCounts struct {
	Live    int64
	Stamped int64
	Missing int64

	ContextualLive    int64
	ContextualStamped int64
	ContextualMissing int64

	OrdinaryLive    int64
	OrdinaryStamped int64
	OrdinaryMissing int64
}

// ContextualConvergenceCounts reports the coverage partition used by
// contextual-generation convergence checks. activeGen == 0 stamps no row, so
// every live row is missing in both partitions.
func (s *Store) ContextualConvergenceCounts(ctx context.Context, activeGen int64) (EmbeddingConvergenceCounts, error) {
	liveWhere := LiveMessagesWhere("", true)
	query := `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN ? <> 0 AND embed_gen = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN message_type IN ('beeper', 'meeting_transcript') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN message_type IN ('beeper', 'meeting_transcript')
		                       AND ? <> 0 AND embed_gen = ? THEN 1 ELSE 0 END), 0)
		FROM messages WHERE ` + liveWhere
	var counts EmbeddingConvergenceCounts
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(query),
		activeGen, activeGen, activeGen, activeGen).Scan(
		&counts.Live, &counts.Stamped,
		&counts.ContextualLive, &counts.ContextualStamped,
	)
	if err != nil {
		return EmbeddingConvergenceCounts{}, fmt.Errorf("count contextual embedding convergence: %w", err)
	}
	counts.Missing = max(counts.Live-counts.Stamped, 0)
	counts.ContextualMissing = max(counts.ContextualLive-counts.ContextualStamped, 0)
	counts.OrdinaryLive = max(counts.Live-counts.ContextualLive, 0)
	counts.OrdinaryStamped = max(counts.Stamped-counts.ContextualStamped, 0)
	counts.OrdinaryMissing = max(counts.OrdinaryLive-counts.OrdinaryStamped, 0)
	return counts, nil
}

// MissingCount returns just the "missing" coverage figure for activeGen
// (live messages still needing work: embed_gen IS NULL OR embed_gen <>
// activeGen). It is a thin accessor for the scheduler/CLI activation
// gates, which only consult the missing count; missing = live - stamped.
func (s *Store) MissingCount(ctx context.Context, activeGen int64) (int64, error) {
	return s.MissingCountScoped(ctx, activeGen, nil, nil)
}

// MissingCountScoped is MissingCount limited to the supplied message types
// and source IDs. Empty messageTypes and sourceIDs slices mean the full
// live corpus.
func (s *Store) MissingCountScoped(ctx context.Context, activeGen int64, messageTypes []string, sourceIDs []int64) (int64, error) {
	live, err := s.countLiveMessagesScoped(ctx, messageTypes, sourceIDs)
	if err != nil {
		return 0, err
	}
	if activeGen == 0 {
		return live, nil
	}
	var stamped int64
	liveWhere, liveArgs := liveMessagesWhereScoped(messageTypes, sourceIDs)
	q := `SELECT COUNT(*) FROM messages
	       WHERE embed_gen = ? AND ` + liveWhere
	args := append([]any{activeGen}, liveArgs...)
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&stamped); err != nil {
		return 0, fmt.Errorf("count stamped: %w", err)
	}
	return max(live-stamped, 0), nil
}

// countLiveMessages returns the total live-message count. Shared by
// CoverageCounts; kept separate so the live-predicate stays in one place.
func (s *Store) countLiveMessagesScoped(ctx context.Context, messageTypes []string, sourceIDs []int64) (int64, error) {
	var n int64
	liveWhere, args := liveMessagesWhereScoped(messageTypes, sourceIDs)
	q := `SELECT COUNT(*) FROM messages WHERE ` + liveWhere
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live messages: %w", err)
	}
	return n, nil
}

// liveMessagesWhereScoped builds the live-messages predicate narrowed by the
// embed build scope: message_type IN (...) and/or source_id IN (...). Empty
// slices leave that dimension unrestricted.
func liveMessagesWhereScoped(messageTypes []string, sourceIDs []int64) (string, []any) {
	where := LiveMessagesWhere("", true)
	var args []any
	types := normalizeMessageTypes(messageTypes)
	if len(types) > 0 {
		placeholders := make([]string, len(types))
		for i, typ := range types {
			placeholders[i] = "?"
			args = append(args, typ)
		}
		where += " AND message_type IN (" + strings.Join(placeholders, ",") + ")"
	}
	ids := normalizeScopeSourceIDs(sourceIDs)
	if len(ids) > 0 {
		placeholders := make([]string, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where += " AND source_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	return where, args
}

func normalizeMessageTypes(messageTypes []string) []string {
	if len(messageTypes) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(messageTypes))
	out := make([]string, 0, len(messageTypes))
	for _, typ := range messageTypes {
		typ = strings.TrimSpace(strings.ToLower(typ))
		if typ == "" {
			continue
		}
		if _, ok := seen[typ]; ok {
			continue
		}
		seen[typ] = struct{}{}
		out = append(out, typ)
	}
	return out
}

// normalizeScopeSourceIDs de-duplicates scope source IDs and drops
// non-positive values so the IN clause binds a minimal, valid set.
func normalizeScopeSourceIDs(sourceIDs []int64) []int64 {
	if len(sourceIDs) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(sourceIDs))
	out := make([]int64, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
