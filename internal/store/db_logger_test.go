package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSlog installs a JSON handler over buf as the default slog logger at
// debug level for the duration of a test. Returns a cleanup closure that
// restores the previous default.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(
		&buf, &slog.HandlerOptions{Level: slog.LevelDebug},
	)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// openLoggedMem opens an in-memory sqlite DB wrapped by loggedDB.
func openLoggedMem(t *testing.T) *loggedDB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err, "open mem db")
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(
		"CREATE TABLE t(id INTEGER PRIMARY KEY, val TEXT)",
	)
	require.NoError(t, err, "create table")
	return newLoggedDB(db, nil)
}

// TestLoggedDB_RequestIDFromContext proves the request id stashed via
// WithRequestID flows through loggedDB into the emitted log line, so a slow or
// failing query can be correlated with the API request that issued it.
func TestLoggedDB_RequestIDFromContext(t *testing.T) {
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)

	ctx := WithRequestID(context.Background(), "msgvault-99")
	rows, err := db.QueryContext(ctx, "SELECT id FROM t")
	require.NoError(t, err, "query")
	for rows.Next() {
	}
	require.NoError(t, rows.Close(), "close")

	rec := findLogLine(t, buf)
	assert.Equal(t, "msgvault-99", rec["request_id"],
		"log line must carry the context request id")
}

func TestLoggedDB_ExecLogsStatement(t *testing.T) {
	assert := assert.New(t)
	// Force full trace so every exec shows up at INFO.
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)

	res, err := db.Exec(
		"INSERT INTO t (val) VALUES (?)", "hello",
	)
	require.NoError(t, err, "exec")
	n, _ := res.RowsAffected()
	assert.Equal(int64(1), n, "rows_affected")

	// Find the sql line in the captured output.
	rec := findLogLine(t, buf)
	assert.Equal("exec", rec["kind"], "kind")
	assert.Contains(rec["stmt"], "INSERT INTO t", "stmt")
	assert.InDelta(float64(1), rec["rows_affected"], 1e-9, "rows_affected")
	assert.InDelta(float64(1), rec["nargs"], 1e-9, "nargs")
}

func TestLogStmt_SlowQueryLogsAtInfo(t *testing.T) {
	assert := assert.New(t)
	// Drive the emitter directly with a synthetic elapsed time
	// to avoid flakiness from "actually make a query slow".
	ConfigureSQLLogging(SQLLogOptions{SlowMs: 50})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	logStmtWith(
		"exec", "msgvault-42", "INSERT INTO t VALUES (?)", []any{"v"},
		nil, 100*time.Millisecond,
	)

	rec := findLogLineByMsg(t, buf, "sql slow")
	require.NotNil(t, rec, "no sql slow line found; buf=%s", buf.String())
	assert.Equal("INFO", rec["level"],
		"a statement just over the threshold is a diagnostic, not a warning")
	assert.InDelta(float64(100), rec["duration_ms"], 1e-9, "duration_ms")
	assert.Equal("msgvault-42", rec["request_id"],
		"slow line must carry the issuing request id")
}

func TestLogStmt_VerySlowQueryPromotedToWarn(t *testing.T) {
	assert := assert.New(t)
	ConfigureSQLLogging(SQLLogOptions{SlowMs: 50})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	logStmtWith(
		"exec", "msgvault-42", "INSERT INTO t VALUES (?)", []any{"v"},
		nil, 500*time.Millisecond, // 10x the 50ms threshold
	)

	rec := findLogLineByMsg(t, buf, "sql slow")
	require.NotNil(t, rec, "no sql slow line found; buf=%s", buf.String())
	assert.Equal("WARN", rec["level"], "10x the threshold escalates to Warn")
	assert.InDelta(float64(500), rec["duration_ms"], 1e-9, "duration_ms")
}

func TestLoggedDB_ErrorAlwaysLogged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ConfigureSQLLogging(SQLLogOptions{})
	buf := captureSlog(t)
	db := openLoggedMem(t)

	_, err := db.ExecContext(
		context.Background(), "INSERT INTO no_such_table VALUES (1)",
	)
	require.Error(err, "expected exec error")

	rec := findLogLineByMsg(t, buf, "sql error")
	require.NotNil(rec, "no sql error line; buf=%s", buf.String())
	assert.Equal("WARN", rec["level"], "level")
	_, ok := rec["error"]
	assert.True(ok, "error attr missing: %v", rec)
}

func TestLoggedDB_QueryRowLogsButNoError(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)

	_, err := db.Exec(
		"INSERT INTO t (val) VALUES ('row')",
	)
	require.NoError(err, "seed")
	var got string
	require.NoError(db.QueryRow(
		"SELECT val FROM t WHERE id = ?", 1,
	).Scan(&got), "queryrow")
	assert.Equal("row", got, "got")

	// Expect to see both an exec line and a queryrow line.
	seen := map[string]bool{}
	for _, rec := range decodeAll(t, buf) {
		if kind, ok := rec["kind"].(string); ok {
			seen[kind] = true
		}
	}
	assert.True(seen["exec"] && seen["queryrow"], "missing kinds; seen=%v", seen)
}

// TestLoggedRows_LogsAtClose verifies that the timing log line
// for a streaming Query is emitted on Close, not at QueryContext
// return. This is the behaviour change that gives streaming queries
// honest duration_ms numbers.
func TestLoggedRows_LogsAtClose(t *testing.T) {
	require := require.New(t)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)
	_, err := db.Exec(
		"INSERT INTO t (val) VALUES ('a'), ('b'), ('c')",
	)
	require.NoError(err, "seed")
	// Reset buffer so we only see the post-Query log line(s).
	buf.Reset()

	rows, err := db.Query("SELECT val FROM t ORDER BY id")
	require.NoError(err, "query")
	require.Nil(findLogLineByMsg(t, buf, "sql"),
		"query log emitted before Close; want only at Close. buf=%s", buf.String())

	for rows.Next() {
		var v string
		require.NoError(rows.Scan(&v), "scan")
	}
	require.NoError(rows.Err(), "rows.Err")
	require.NoError(rows.Close(), "close")

	rec := findLogLine(t, buf)
	assert.Equal(t, "query", rec["kind"], "kind")
}

// TestLoggedRows_CloseIdempotent verifies that double-Close
// (e.g. an early-return defer plus an explicit close) does not
// emit two log lines.
func TestLoggedRows_CloseIdempotent(t *testing.T) {
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)
	rows, err := db.Query("SELECT 1")
	require.NoError(t, err, "query")
	for rows.Next() {
		var n int
		_ = rows.Scan(&n)
	}
	_ = rows.Close()
	_ = rows.Close()

	count := 0
	for _, rec := range decodeAll(t, buf) {
		if rec["msg"] == "sql" && rec["kind"] == "query" {
			count++
		}
	}
	assert.Equal(t, 1, count, "query log lines")
}

// TestLoggedRows_QueryErrorLogsImmediately verifies that an
// error returned from db.Query (e.g. bad SQL, no such table)
// is logged at the QueryContext call site, not deferred to a
// Close call that would never happen because no rows handle
// is returned.
func TestLoggedRows_QueryErrorLogsImmediately(t *testing.T) {
	ConfigureSQLLogging(SQLLogOptions{})
	buf := captureSlog(t)
	db := openLoggedMem(t)

	_, err := db.Query("SELECT * FROM no_such_table")
	require.Error(t, err, "expected query error")
	rec := findLogLineByMsg(t, buf, "sql error")
	require.NotNil(t, rec, "no sql error line; buf=%s", buf.String())
}

// TestLoggedRows_FinalizesAtEndOfScan verifies that duration_ms
// is captured when iteration ends (Next returns false), not when
// Close is eventually called. Most callers defer Close, so any
// time spent between the last row and the deferred Close (count
// queries, batchPopulate, unrelated work) would otherwise be
// charged to the streaming query. The end-of-Next finalizer
// keeps the timing honest.
func TestLoggedRows_FinalizesAtEndOfScan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Keep this test about finalization timing, independent of whether a busy
	// CI host would classify the tiny query as slow.
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true, SlowMs: 60_000})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)
	_, err := db.Exec(
		"INSERT INTO t (val) VALUES ('a'), ('b'), ('c')",
	)
	require.NoError(err, "seed")
	buf.Reset()

	rows, err := db.Query("SELECT val FROM t ORDER BY id")
	require.NoError(err, "query")
	for rows.Next() {
		var v string
		require.NoError(rows.Scan(&v), "scan")
	}
	// The log line must be emitted by the time Next returns
	// false, before any deferred Close fires.
	rec := findLogLine(t, buf)
	assert.Equal("query", rec["kind"], "kind")
	durAtEndOfScan, ok := rec["duration_ms"].(float64)
	require.True(ok, "duration_ms is float64")

	// Simulate caller doing unrelated work between end-of-scan
	// and the deferred Close. The log line must not be re-emitted
	// and the duration must already be recorded.
	time.Sleep(50 * time.Millisecond)
	require.NoError(rows.Close(), "close")

	count := 0
	var lastDuration float64
	for _, r := range decodeAll(t, buf) {
		if r["msg"] == "sql" && r["kind"] == "query" {
			count++
			dur, ok := r["duration_ms"].(float64)
			require.True(ok, "duration_ms is float64")
			lastDuration = dur
		}
	}
	assert.Equal(1, count, "query log lines")
	// Equality proves the duration recorded at end-of-scan did not absorb the
	// 50ms of post-iteration work, without imposing a host-speed ceiling.
	assert.InDelta(durAtEndOfScan, lastDuration, 0, "duration_ms changed after Close")
}

// TestLoggedRows_EarlyExitFinalizesOnClose covers the path where
// the caller breaks out of the Next loop without exhausting rows.
// The finalizer must run from Close on that path so the log line
// is still emitted exactly once.
func TestLoggedRows_EarlyExitFinalizesOnClose(t *testing.T) {
	require := require.New(t)
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	db := openLoggedMem(t)
	_, err := db.Exec(
		"INSERT INTO t (val) VALUES ('a'), ('b'), ('c')",
	)
	require.NoError(err, "seed")
	buf.Reset()

	rows, err := db.Query("SELECT val FROM t ORDER BY id")
	require.NoError(err, "query")
	// Read a single row and break — finalizer should not fire yet.
	require.True(rows.Next(), "expected at least one row")
	var v string
	require.NoError(rows.Scan(&v), "scan")
	require.Nil(findLogLineByMsg(t, buf, "sql"),
		"log line emitted before Close on early-exit path")
	require.NoError(rows.Close(), "close")
	rec := findLogLine(t, buf)
	assert.Equal(t, "query", rec["kind"], "kind")
}

// TestLoggedRows_IterationErrorSurfacedOnClose verifies that a
// context cancellation discovered during rows.Next() is logged
// as an error on Close, even when Rows.Close() itself returns
// nil. Without checking Rows.Err(), a cancelled scan would log
// as a successful query.
func TestLoggedRows_IterationErrorSurfacedOnClose(t *testing.T) {
	require := require.New(t)
	ConfigureSQLLogging(SQLLogOptions{})
	buf := captureSlog(t)
	db := openLoggedMem(t)
	_, err := db.Exec(
		"INSERT INTO t (val) VALUES ('a'), ('b'), ('c')",
	)
	require.NoError(err, "seed")
	buf.Reset()

	ctx, cancel := context.WithCancel(context.Background())
	rows, err := db.QueryContext(ctx, "SELECT val FROM t ORDER BY id")
	require.NoError(err, "query")
	// Cancel before iterating; Next() will see the cancellation
	// and stop, leaving the error on Rows.Err(), not Close().
	cancel()
	for rows.Next() {
	}
	_ = rows.Close()

	rec := findLogLineByMsg(t, buf, "sql error")
	require.NotNil(rec, "expected sql error line for cancelled scan; buf=%s", buf.String())
	errStr, _ := rec["error"].(string)
	assert.NotEmpty(t, errStr, "error attr missing or empty: %v", rec)
}

// TestLogStmt_SlowQueryIncludesArgsShape verifies that a slow
// query attaches an "args_shape" attr describing each bound
// parameter's type and length, but never the raw value. Raw
// values can carry PII (addresses, subjects, tokens) and must
// not be persisted in logs by default.
func TestLogStmt_SlowQueryIncludesArgsShape(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ConfigureSQLLogging(SQLLogOptions{SlowMs: 50})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	logStmtWith(
		"query", "", "SELECT * FROM t WHERE id = ? AND src = ?",
		[]any{int64(42), "gmail"},
		nil, 100*time.Millisecond,
	)

	rec := findLogLineByMsg(t, buf, "sql slow")
	require.NotNil(rec, "no sql slow line; buf=%s", buf.String())
	gotShape, ok := rec["args_shape"].(string)
	require.True(ok, "args_shape attr missing or wrong type: %v", rec["args_shape"])
	// Type info present.
	assert.Contains(gotShape, "int64", "args_shape int64 type")
	assert.Contains(gotShape, "string(len=5)", "args_shape string length")
	// Raw values must not appear.
	assert.NotContains(gotShape, "42", "args_shape leaked numeric value")
	assert.NotContains(gotShape, "gmail", "args_shape leaked string value")
	// Legacy "args" attr must not be present.
	_, present := rec["args"]
	assert.False(present, "legacy args attr should not be set: %v", rec)
}

// TestLogStmt_FullTraceOmitsArgs verifies that --full-trace mode
// does not attach args or args_shape. nargs is enough at
// high-volume Info level.
func TestLogStmt_FullTraceOmitsArgs(t *testing.T) {
	ConfigureSQLLogging(SQLLogOptions{FullTrace: true})
	t.Cleanup(func() { ConfigureSQLLogging(SQLLogOptions{}) })

	buf := captureSlog(t)
	logStmtWith(
		"query", "", "SELECT * FROM t WHERE id = ?",
		[]any{int64(42)},
		nil, 1*time.Millisecond,
	)

	rec := findLogLine(t, buf)
	_, present := rec["args"]
	assert.False(t, present, "args should not be present on info/full-trace lines: %v", rec)
	_, present = rec["args_shape"]
	assert.False(t, present, "args_shape should not be present on info/full-trace lines: %v", rec)
	assert.InDelta(t, float64(1), rec["nargs"], 1e-9, "nargs")
}

// TestFormatArgsShape_RedactsValues ensures the shape formatter
// emits type and length only, never raw values, even for long
// strings that could carry sensitive content.
func TestFormatArgsShape_RedactsValues(t *testing.T) {
	assert := assert.New(t)
	long := strings.Repeat("x", 200)
	got := formatArgsShape([]any{long, "secret-token", []byte("hello world"), nil, int64(42)})
	assert.NotContains(got, "x", "shape leaked raw string")
	assert.NotContains(got, "secret-token", "shape leaked raw string")
	assert.NotContains(got, "hello world", "shape leaked raw bytes")
	assert.NotContains(got, "42", "shape leaked raw numeric")
	for _, want := range []string{
		"string(len=200)",
		"string(len=12)",
		"bytes(len=11)",
		"nil",
		"int64",
	} {
		assert.Contains(got, want, "shape missing %q", want)
	}
}

func TestNormalizeStmt_CollapsesWhitespace(t *testing.T) {
	in := "SELECT\n  *\nFROM\n\tt WHERE id = ?"
	got := normalizeStmt(in, 0)
	want := "SELECT * FROM t WHERE id = ?"
	assert.Equal(t, want, got)
}

func TestNormalizeStmt_TruncatesLong(t *testing.T) {
	// Long uniform input gets a head + " ... " + tail split.
	// The truncation budget includes the separator, so the
	// final string is exactly maxChars long.
	in := strings.Repeat("a", 500)
	got := normalizeStmt(in, 100)
	const sep = " ... "
	assert.Contains(t, got, sep, "missing separator")
	assert.Len(t, got, 100, "bad length")
}

// TestNormalizeStmt_KeepsWhereClause is the guard for the bug
// that motivated head+tail truncation: a long SELECT whose only
// distinguishing feature is the WHERE clause must remain
// distinguishable in the logs.
func TestNormalizeStmt_KeepsWhereClause(t *testing.T) {
	in := "SELECT m.id, m.source_id, s.source_type, s.identifier, " +
		"m.source_message_id, COALESCE(m.subject, ''), m.sent_at, " +
		"m.archived_at, (CASE WHEN mr.message_id IS NOT NULL THEN 1 " +
		"ELSE 0 END) AS has_raw, (SELECT COUNT(*) FROM message_labels " +
		"ml WHERE ml.message_id = m.id) AS label_count, " +
		"COALESCE(m.is_from_me, 0) AS is_from_me " +
		"FROM messages m JOIN sources s ON s.id = m.source_id " +
		"WHERE m.rfc822_message_id = ? AND m.deleted_at IS NULL"
	got := normalizeStmt(in, 300)
	assert.Contains(t, got, "WHERE m.rfc822_message_id",
		"WHERE clause missing from truncated stmt")
}

// TestNormalizeStmt_TinyBudgetFallsBackToHead protects the
// edge case where the budget is too small for a meaningful
// head+tail split.
func TestNormalizeStmt_TinyBudgetFallsBackToHead(t *testing.T) {
	in := strings.Repeat("a", 50)
	got := normalizeStmt(in, 8)
	assert.True(t, strings.HasSuffix(got, "..."),
		"expected trailing ellipsis on tiny budget; got %q", got)
	assert.NotContains(t, got, " ... ",
		"did not expect head+tail split on tiny budget")
}

// TestNormalizeStmt_UTF8Safe ensures truncation respects rune
// boundaries — multi-byte characters in SQL literals or comments
// must not be split, which would emit invalid UTF-8 to logs.
func TestNormalizeStmt_UTF8Safe(t *testing.T) {
	// Each "café — 漢" is 13 bytes / 7 runes; repeat to exceed
	// any reasonable budget.
	in := strings.Repeat("café — 漢 ", 30)
	got := normalizeStmt(in, 50)
	assert.True(t, utf8.ValidString(got), "normalizeStmt returned invalid UTF-8: %q", got)
	// Tiny-budget head-only path.
	got2 := normalizeStmt(in, 8)
	assert.True(t, utf8.ValidString(got2),
		"tiny-budget normalizeStmt returned invalid UTF-8: %q", got2)
}

// ---- test helpers ----

// findLogLine returns the first record whose msg is exactly "sql".
func findLogLine(
	t *testing.T, buf *bytes.Buffer,
) map[string]any {
	t.Helper()
	const msg = "sql"
	for _, rec := range decodeAll(t, buf) {
		if rec["msg"] == msg {
			return rec
		}
	}
	require.FailNow(t, fmt.Sprintf("no log line with msg=%q; buf=%s", msg, buf.String()))
	return nil
}

// findLogLineByMsg is like findLogLine but returns nil rather
// than failing so callers can assert absence.
func findLogLineByMsg(
	t *testing.T, buf *bytes.Buffer, msg string,
) map[string]any {
	t.Helper()
	for _, rec := range decodeAll(t, buf) {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

func decodeAll(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			break
		}
		out = append(out, rec)
	}
	return out
}
