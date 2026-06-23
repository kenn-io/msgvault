package store

import (
	"context"
	"fmt"
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
	if limit <= 0 {
		return nil, nil
	}
	q := `SELECT id FROM messages
	       WHERE (embed_gen IS NULL OR embed_gen <> ?)
	         AND ` + LiveMessagesWhere("", true) + `
	         AND id > ?
	       ORDER BY id
	       LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, target, afterID, limit)
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
	live, err = s.countLiveMessages(ctx)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if activeGen != 0 {
		q := `SELECT COUNT(*) FROM messages
		       WHERE embed_gen = ? AND ` + LiveMessagesWhere("", true)
		if err := s.db.QueryRowContext(ctx, q, activeGen).Scan(&stamped); err != nil {
			return 0, 0, 0, 0, fmt.Errorf("count stamped: %w", err)
		}
	}
	missing = max(live-stamped, 0)
	return live, stamped, 0, missing, nil
}

// MissingCount returns just the "missing" coverage figure for activeGen
// (live messages still needing work: embed_gen IS NULL OR embed_gen <>
// activeGen). It is a thin accessor for the scheduler/CLI activation
// gates, which only consult the missing count; missing = live - stamped.
func (s *Store) MissingCount(ctx context.Context, activeGen int64) (int64, error) {
	live, err := s.countLiveMessages(ctx)
	if err != nil {
		return 0, err
	}
	if activeGen == 0 {
		return live, nil
	}
	var stamped int64
	q := `SELECT COUNT(*) FROM messages
	       WHERE embed_gen = ? AND ` + LiveMessagesWhere("", true)
	if err := s.db.QueryRowContext(ctx, q, activeGen).Scan(&stamped); err != nil {
		return 0, fmt.Errorf("count stamped: %w", err)
	}
	return max(live-stamped, 0), nil
}

// countLiveMessages returns the total live-message count. Shared by
// CoverageCounts; kept separate so the live-predicate stays in one place.
func (s *Store) countLiveMessages(ctx context.Context) (int64, error) {
	var n int64
	q := `SELECT COUNT(*) FROM messages WHERE ` + LiveMessagesWhere("", true)
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count live messages: %w", err)
	}
	return n, nil
}
