package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// SQLLogOptions controls the store-level SQL logging behaviour.
// Configured once per process via ConfigureSQLLogging(); every
// Store subsequently opened picks up the values atomically.
type SQLLogOptions struct {
	// SlowMs is the threshold above which any query is logged
	// at Warn regardless of the normal logging level. Zero
	// means "never flag as slow". Defaults to 100 when the
	// caller passes a zero-value struct to ConfigureSQLLogging.
	SlowMs int64

	// FullTrace makes every query emit an Info-level log line
	// (not just Debug). Use with care — generates one line per
	// query, which is enormous volume for anything non-trivial.
	FullTrace bool

	// MaxStmtChars truncates logged SQL at this many characters.
	// 0 disables truncation. Defaults to 300.
	MaxStmtChars int
}

// Package-level atomic config so every *loggedDB instance in the
// process reads the same settings without passing options through
// every store.Open call. Atomics avoid a mutex on the hot path —
// these values are read for every query.
var (
	sqlLogSlowMs   atomic.Int64
	sqlLogFull     atomic.Bool
	sqlLogMaxChars atomic.Int64
)

func init() {
	sqlLogSlowMs.Store(100)
	sqlLogMaxChars.Store(300)
}

// ConfigureSQLLogging sets the process-wide SQL logging behaviour.
// Call this after slog.SetDefault but before opening a Store.
func ConfigureSQLLogging(opts SQLLogOptions) {
	slow := opts.SlowMs
	if slow == 0 {
		slow = 100
	}
	maxChars := opts.MaxStmtChars
	if maxChars == 0 {
		maxChars = 300
	}
	sqlLogSlowMs.Store(slow)
	sqlLogFull.Store(opts.FullTrace)
	sqlLogMaxChars.Store(int64(maxChars))
}

// loggedDB wraps *sql.DB and emits slog records for every query
// it executes. It embeds *sql.DB so store methods continue to
// compile against the sql.DB method surface — the Query/Exec
// overrides below shadow the embedded ones.
//
// loggedDB also owns the dialect's placeholder-rebind step: every
// SQL string passed to Query/Exec/QueryRow is run through rebind
// before reaching the driver. Call sites in the store package can
// emit portable `?` placeholders and get the correct `$N` form on
// PostgreSQL without any per-call wrapping.
type loggedDB struct {
	*sql.DB

	rebind func(string) string
}

func newLoggedDB(db *sql.DB, rebind func(string) string) *loggedDB {
	if rebind == nil {
		rebind = identityRebind
	}
	return &loggedDB{DB: db, rebind: rebind}
}

func identityRebind(q string) string { return q }

// Query logs the statement via logStmt and delegates to the
// embedded sql.DB. Uses a background context to match the
// sql.DB.Query semantics.
func (d *loggedDB) Query(
	query string, args ...any,
) (*loggedRows, error) {
	return d.QueryContext(context.Background(), query, args...)
}

// QueryContext returns a *loggedRows whose Close emits the
// real wall-clock duration of prepare + scan. The immediate
// post-Query log line is emitted only on error, because the
// success-case duration that matters is the one measured at
// Close — most queries return *sql.Rows in microseconds and
// then spend their real time inside rows.Next.
func (d *loggedDB) QueryContext(
	ctx context.Context, query string, args ...any,
) (*loggedRows, error) {
	query = d.rebind(query)
	start := time.Now()
	rows, err := d.DB.QueryContext(ctx, query, args...) //nolint:rowserrcheck // caller owns rows.Err
	if err != nil {
		logStmtWith("query", query, args, err, time.Since(start))
		return nil, err
	}
	return &loggedRows{
		Rows:  rows,
		query: query,
		args:  args,
		start: start,
	}, nil
}

// QueryRow logs and delegates. sql.Row does not expose its error
// until Scan, so we can only record issue time + duration here.
func (d *loggedDB) QueryRow(
	query string, args ...any,
) *sql.Row {
	return d.QueryRowContext(context.Background(), query, args...)
}

// QueryRowContext logs and delegates.
func (d *loggedDB) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	query = d.rebind(query)
	start := time.Now()
	row := d.DB.QueryRowContext(ctx, query, args...)
	logStmtWith("queryrow", query, args, nil, time.Since(start))
	return row
}

// Exec logs and delegates.
func (d *loggedDB) Exec(
	query string, args ...any,
) (sql.Result, error) {
	return d.ExecContext(context.Background(), query, args...)
}

// ExecContext logs and delegates. Records rows affected when
// available so write sizes show up in the log.
func (d *loggedDB) ExecContext(
	ctx context.Context, query string, args ...any,
) (sql.Result, error) {
	query = d.rebind(query)
	start := time.Now()
	res, err := d.DB.ExecContext(ctx, query, args...)
	elapsed := time.Since(start)
	rowsAffected := int64(-1)
	if err == nil && res != nil {
		if n, rerr := res.RowsAffected(); rerr == nil {
			rowsAffected = n
		}
	}
	logStmtWith("exec", query, args, err, elapsed,
		slog.Int64("rows_affected", rowsAffected),
	)
	return res, err
}

// Begin returns a *loggedTx that inherits the rebind function, so
// statements issued inside the transaction are also rebound before
// reaching the driver. Wrapping Begin (not just Exec/Query) is what
// keeps the auto-rebind promise intact across transactional code.
func (d *loggedDB) Begin() (*loggedTx, error) {
	return d.BeginTx(context.Background(), nil)
}

// BeginTx matches the sql.DB signature but returns *loggedTx.
func (d *loggedDB) BeginTx(
	ctx context.Context, opts *sql.TxOptions,
) (*loggedTx, error) {
	tx, err := d.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &loggedTx{Tx: tx, rebind: d.rebind}, nil
}

// loggedTx mirrors loggedDB for *sql.Tx: it embeds the raw
// transaction and rebinds every query before dispatching. Store
// code that previously took *sql.Tx takes *loggedTx instead.
type loggedTx struct {
	*sql.Tx

	rebind func(string) string
}

// Exec rebinds before delegating. Transaction-scoped queries are
// not individually logged — the per-tx duration from Store.withTx
// gives enough signal.
func (t *loggedTx) Exec(
	query string, args ...any,
) (sql.Result, error) {
	return t.ExecContext(context.Background(), query, args...)
}

// ExecContext rebinds before delegating.
func (t *loggedTx) ExecContext(
	ctx context.Context, query string, args ...any,
) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.rebind(query), args...)
}

// Query rebinds and returns *loggedRows so transactional
// queries also get accurate scan-close timing. The wrapper
// only logs on Close; if Query itself fails we surface the
// error without a wrapper since there are no rows to scan.
func (t *loggedTx) Query(
	query string, args ...any,
) (*loggedRows, error) {
	return t.QueryContext(context.Background(), query, args...)
}

// QueryContext rebinds and returns *loggedRows.
func (t *loggedTx) QueryContext(
	ctx context.Context, query string, args ...any,
) (*loggedRows, error) {
	query = t.rebind(query)
	start := time.Now()
	rows, err := t.Tx.QueryContext(ctx, query, args...) //nolint:rowserrcheck // caller owns rows.Err
	if err != nil {
		logStmtWith("query", query, args, err, time.Since(start))
		return nil, err
	}
	return &loggedRows{
		Rows:  rows,
		query: query,
		args:  args,
		start: start,
	}, nil
}

// QueryRow rebinds before delegating.
func (t *loggedTx) QueryRow(
	query string, args ...any,
) *sql.Row {
	return t.QueryRowContext(context.Background(), query, args...)
}

// QueryRowContext rebinds before delegating.
func (t *loggedTx) QueryRowContext(
	ctx context.Context, query string, args ...any,
) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.rebind(query), args...)
}

// loggedRows wraps *sql.Rows so the timing log emitted for a
// streaming query reflects total wall-clock time (prepare +
// scan), not just the time db.Query took to return. Without
// this wrapper, every duration_ms reported for a SELECT was
// effectively the cost of preparing the statement and reading
// the first row from the driver — typically sub-millisecond
// even when the full scan took hundreds of milliseconds.
//
// loggedRows embeds *sql.Rows so callers continue to use it
// like a raw *sql.Rows: Scan, Err, and Columns are satisfied
// by the embedded pointer. Next and Close are overridden:
//
//   - Next finalizes the timing log when iteration ends
//     (Next returns false), so duration_ms reflects scan
//     completion rather than whatever happens between the last
//     row and the deferred Close (count queries, batchPopulate,
//     etc).
//   - Close finalizes too, covering early returns where the
//     caller breaks out of the loop before exhausting rows.
//
// Both paths route through finalize, which is idempotent — only
// the first caller emits a log line.
type loggedRows struct {
	*sql.Rows

	query     string
	args      []any
	start     time.Time
	finalized bool
}

// Next delegates to the embedded *sql.Rows but, on the first
// false return, runs the timing finalizer so duration_ms is
// captured at end-of-scan rather than at deferred Close. The
// caller's existing Close defer still fires; the finalizer
// guard makes that a no-op.
func (r *loggedRows) Next() bool {
	if r.Rows == nil {
		return false
	}
	if r.Rows.Next() {
		return true
	}
	r.finalize(nil)
	return false
}

// Close finalizes timing for the early-exit path (caller broke
// out of the Next loop) and always closes the underlying Rows.
// Repeated calls remain safe: finalize is idempotent and
// *sql.Rows.Close is documented to be safe to call multiple
// times.
func (r *loggedRows) Close() error {
	err := r.Rows.Close()
	r.finalize(err)
	return err
}

// finalize emits the timing log line exactly once. closeErr is
// the error returned by Rows.Close on the explicit-Close path
// and nil on the end-of-scan path; either way, when no close
// error is present we still consult Rows.Err() so iteration
// failures (context cancellation, driver scan errors) get
// logged as "sql error" instead of as a successful query.
func (r *loggedRows) finalize(closeErr error) {
	if r.finalized {
		return
	}
	r.finalized = true
	logErr := closeErr
	if logErr == nil {
		logErr = r.Err()
	}
	logStmtWith("query", r.query, r.args, logErr, time.Since(r.start))
}

// logStmtWith is the explicit form that lets callers add extra
// structured attributes (used by Exec to report rows_affected).
//
// On WARN-level lines (slow + error) we attach a redacted shape
// summary of the bound args (type + length, no raw values) so
// the query is debuggable without persisting subjects, body
// excerpts, addresses, tokens, or other potentially sensitive
// values. Info/Debug get only nargs.
func logStmtWith(
	kind, query string, args []any,
	err error, elapsed time.Duration, extra ...slog.Attr,
) {
	stmt := normalizeStmt(query, int(sqlLogMaxChars.Load()))
	ms := elapsed.Milliseconds()
	slowMs := sqlLogSlowMs.Load()
	fullTrace := sqlLogFull.Load()

	// Base attributes that are on every line.
	attrs := []any{
		"kind", kind,
		"stmt", stmt,
		"nargs", len(args),
		"duration_ms", ms,
	}
	for _, a := range extra {
		attrs = append(attrs, a)
	}

	switch {
	case err != nil:
		attrs = append(attrs, "error", err.Error())
		if isBenignMigrationError(err) {
			// Expected during idempotent migrations; don't
			// spam WARN in the per-run log for every startup.
			slog.Debug("sql benign error", attrs...)
		} else {
			attrs = append(attrs, "args_shape", formatArgsShape(args))
			slog.Warn("sql error", attrs...)
		}
	case slowMs > 0 && ms >= slowMs:
		attrs = append(attrs, "args_shape", formatArgsShape(args))
		slog.Warn("sql slow", attrs...)
	case fullTrace:
		slog.Info("sql", attrs...)
	default:
		// Debug level: only visible when the handler is at
		// Debug (e.g. --verbose). Produces zero allocations
		// when the handler short-circuits on Enabled().
		slog.Debug("sql", attrs...)
	}
}

// formatArgsShape renders bound parameters as a redacted shape
// summary — type plus, where meaningful, length — without ever
// emitting the raw value. Strings, []byte, and nil are shaped;
// numerics and bools are shown as their type name only. Lets a
// slow/failing query be diagnosed (placeholder count, a NULL
// bind in the wrong slot, an unexpectedly huge string) without
// persisting addresses, subjects, tokens, or message bodies.
func formatArgsShape(args []any) string {
	if len(args) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(args) * 12)
	b.WriteByte('[')
	for i, a := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch v := a.(type) {
		case nil:
			b.WriteString("nil")
		case string:
			fmt.Fprintf(&b, "string(len=%d)", utf8.RuneCountInString(v))
		case []byte:
			fmt.Fprintf(&b, "bytes(len=%d)", len(v))
		default:
			fmt.Fprintf(&b, "%T", v)
		}
	}
	b.WriteByte(']')
	return b.String()
}

// isBenignMigrationError returns true for SQLite errors that the
// store layer intentionally tolerates: idempotent ALTER TABLE
// migrations that re-run on every InitSchema (duplicate column),
// and optional FTS5 module missing in builds without the fts5
// tag. Matching on error substrings is pragmatic because those
// messages are stable across go-sqlite3 versions.
func isBenignMigrationError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate column name") ||
		strings.Contains(msg, "no such module: fts5")
}

// normalizeStmt collapses whitespace in a SQL statement and
// truncates it to maxChars (measured in runes, not bytes, so
// multi-byte UTF-8 in literals or comments is never split).
// When truncation is needed it keeps roughly the first 60% and
// last 40% of the budget joined by " ... ", so the WHERE /
// GROUP BY / ORDER BY tail — usually the part that
// distinguishes one logged query from another — survives.
// Intended for human log reading; not for reconstructing the
// exact SQL.
func normalizeStmt(q string, maxChars int) string {
	// Fast path: if there's no whitespace to collapse AND the
	// statement is within budget (by rune count), skip the
	// allocation. utf8.RuneCountInString is O(n) on the bytes
	// but no allocation, same as strings.ContainsAny.
	if !strings.ContainsAny(q, "\n\t") &&
		(maxChars <= 0 || utf8.RuneCountInString(q) <= maxChars) {
		return strings.TrimSpace(q)
	}

	var b strings.Builder
	b.Grow(len(q))
	prevSpace := false
	for _, r := range q {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	s := strings.TrimSpace(b.String())
	if maxChars <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	const sep = " ... "
	// Refuse the head+tail split if the budget can't carry both
	// ends meaningfully; fall back to head-only truncation.
	if maxChars <= len(sep)+8 {
		return string(runes[:maxChars]) + "..."
	}
	budget := maxChars - len(sep)
	headLen := budget * 6 / 10
	tailLen := budget - headLen
	return string(runes[:headLen]) + sep + string(runes[len(runes)-tailLen:])
}
