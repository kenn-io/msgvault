package store_test

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// captureWarnings installs a JSON slog handler over a buffer as the default
// logger for the duration of the test, returning the buffer so the test can
// assert on emitted log lines (e.g. the row-by-row skip warning).
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(
		&buf, &slog.HandlerOptions{Level: slog.LevelDebug},
	)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestPG_BackfillFTS_RowByRowFallbackSkipsBadRow (finding T2) exercises the
// backfillFTSRowByRow skip-and-continue fallback by FORCING a batch failure for
// one specific message id, then asserting the fallback (a) skips ONLY the
// offending row, (b) logs a warning naming the id, (c) still indexes every good
// row in the batch — including ones AFTER the bad one — and (d) BackfillFTS
// returns no error.
//
// Approach: a test-only injection seam (store.SetBackfillFTSBatchErrHookForTest).
// A post-cap tsvector overflow is not reliably reproducible — the 600000-char
// LEFT cap keeps even a pathological many-distinct-token body comfortably under
// PostgreSQL's 1MB tsvector limit — so the row-by-row fallback can only be
// exercised deterministically through a seam that injects the batch error.
//
// PG-only: the fallback exists for the PostgreSQL tsvector-overflow case
// (SQLite's FTS5 has no such limit, so a real backfill never errors there).
func TestPG_BackfillFTS_RowByRowFallbackSkipsBadRow(t *testing.T) {
	skipUnlessPostgres(t)

	f := storetest.New(t)
	requirepkg.True(t, f.Store.FTS5Available(), "FTS must be available on PG")

	// Six messages in a single backfill batch (well under the 5000 batch size).
	// Each gets a distinct, searchable body token so we can prove per-row
	// indexing precisely.
	const total = 6
	const badIdx = 3 // a row in the middle, so rows after it must still index
	ids := f.CreateMessages(total)
	requirepkg.Len(t, ids, total)

	tokens := []string{
		"alphafruit", "betafruit", "gammafruit",
		"deltafruit", "epsilonfruit", "zetafruit",
	}
	for i, id := range ids {
		requirepkg.NoError(t, f.Store.UpsertMessageBody(id,
			sql.NullString{String: tokens[i] + " shared", Valid: true}, sql.NullString{}),
			"attach body %d", i)
	}

	badID := ids[badIdx]

	// Force any batch whose id range covers badID to fail with the SPECIFIC
	// PG tsvector-overflow error (SQLSTATE 54000, program_limit_exceeded). Only
	// that error is classified as size-too-large and is allowed to skip the row
	// and continue — a generic error now ABORTS the backfill (see the abort-path
	// test below), so the injected error MUST be a real *pgconn.PgError{54000}.
	// The whole-batch range [min, min+5000) covers badID, which triggers the
	// row-by-row fallback; in that fallback every row is retried as a single-id
	// range [id, id+1), so only the [badID, badID+1) call errors and is skipped —
	// every other row (including the ones after badID) indexes normally.
	buf := captureWarnings(t)
	restore := store.SetBackfillFTSBatchErrHookForTest(func(fromID, toID int64) error {
		if fromID <= badID && badID < toID {
			return &pgconn.PgError{
				Code:    "54000",
				Message: fmt.Sprintf("string is too long for tsvector (injected for id %d)", badID),
			}
		}
		return nil
	})
	defer restore()

	n, err := f.Store.BackfillFTS(nil)
	requirepkg.NoError(t, err, "BackfillFTS must complete despite a forced bad row")

	// (a) ONLY the bad row was skipped: total-1 rows indexed, exactly one NULL.
	assertpkg.Equal(t, int64(total-1), n, "every row except the bad one should be indexed")
	assertpkg.Equal(t, 1, nullSearchFTSCount(t, f.Store),
		"exactly one row (the bad one) left with NULL search_fts")

	// The bad row specifically is the NULL one.
	var badIsNull bool
	requirepkg.NoError(t, f.Store.DB().QueryRow(
		"SELECT search_fts IS NULL FROM messages WHERE id = $1", badID).Scan(&badIsNull),
		"probe bad row")
	assertpkg.True(t, badIsNull, "the forced-bad row must be left NULL")

	// (c) Good rows BEFORE and AFTER the bad one are indexed and searchable.
	for i, tok := range tokens {
		if i == badIdx {
			// The bad row must NOT be searchable.
			_, badTotal, searchErr := f.Store.SearchMessages(tok, 0, 10)
			requirepkg.NoError(t, searchErr, "SearchMessages %q", tok)
			assertpkg.Equal(t, int64(0), badTotal, "bad row token %q must not be searchable", tok)
			continue
		}
		_, hits, searchErr := f.Store.SearchMessages(tok, 0, 10)
		requirepkg.NoError(t, searchErr, "SearchMessages %q", tok)
		assertpkg.Equal(t, int64(1), hits, "good row token %q must be searchable", tok)
	}

	// Concretely prove a row AFTER the bad one indexed: zetafruit is the last id.
	_, afterHits, err := f.Store.SearchMessages("zetafruit", 0, 10)
	requirepkg.NoError(t, err, "SearchMessages zetafruit")
	assertpkg.Equal(t, int64(1), afterHits, "row after the bad one must be searchable")

	// (b) A warning naming the skipped id was logged.
	logs := buf.String()
	assertpkg.Contains(t, logs, "skipping message in FTS backfill",
		"a skip warning must be logged")
	assertpkg.Contains(t, logs, fmt.Sprintf(`"message_id":%d`, badID),
		"the skip warning must name the skipped message id")
	assertpkg.Equal(t, 1, strings.Count(logs, "skipping message in FTS backfill"),
		"exactly one row should be skipped")
}

// TestPG_BackfillFTS_NonSizeErrorAborts is the regression guard for the HIGH
// finding: a NON-size batch error (anything that is NOT the PG tsvector-overflow
// SQLSTATE 54000) must ABORT BackfillFTS and propagate the error, instead of
// being masked by the row-by-row fallback as a silent success. Before the fix,
// any batch error fell to row-by-row, which swallowed EVERY per-row error and
// returned (indexed, nil) — so a systemic failure (dead connection, etc.) would
// clear FTS, skip everything with warnings, and still report success.
//
// PG-only: the discriminating classifier (IsFTSValueTooLargeError) is a no-op on
// SQLite (always false), so on SQLite every error already aborts.
func TestPG_BackfillFTS_NonSizeErrorAborts(t *testing.T) {
	skipUnlessPostgres(t)

	f := storetest.New(t)
	requirepkg.True(t, f.Store.FTS5Available(), "FTS must be available on PG")

	const total = 6
	ids := f.CreateMessages(total)
	requirepkg.Len(t, ids, total)
	for i, id := range ids {
		requirepkg.NoError(t, f.Store.UpsertMessageBody(id,
			sql.NullString{String: fmt.Sprintf("body%d shared", i), Valid: true}, sql.NullString{}),
			"attach body %d", i)
	}

	// Case 1: a plain (non-pg) error must abort.
	t.Run("plain_error", func(t *testing.T) {
		sentinel := errors.New("simulated dead connection")
		restore := store.SetBackfillFTSBatchErrHookForTest(func(fromID, toID int64) error {
			return sentinel
		})
		defer restore()

		_, err := f.Store.BackfillFTS(nil)
		requirepkg.Error(t, err, "BackfillFTS must ABORT (not silently succeed) on a non-size error")
		assertpkg.ErrorIs(t, err, sentinel, "the original error must propagate")
	})

	// Case 2: a different (non-54000) SQLSTATE must also abort.
	t.Run("other_sqlstate", func(t *testing.T) {
		restore := store.SetBackfillFTSBatchErrHookForTest(func(fromID, toID int64) error {
			return &pgconn.PgError{Code: "08006", Message: "connection failure (injected)"}
		})
		defer restore()

		_, err := f.Store.BackfillFTS(nil)
		requirepkg.Error(t, err, "BackfillFTS must ABORT on a non-size SQLSTATE")
		var pgErr *pgconn.PgError
		requirepkg.ErrorAs(t, err, &pgErr, "a PgError must propagate")
		assertpkg.Equal(t, "08006", pgErr.Code, "the original SQLSTATE must propagate")
	})
}
