package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync/atomic"
	"unicode/utf8"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"go.kenn.io/msgvault/internal/textutil"
)

// cacheUTF8FunctionName is the DuckDB scalar function cacheTextSQL calls for
// values that are not valid UTF-8. DuckDB's sqlite scanner passes stored bytes
// through unchecked, and its Parquet reader later rejects the whole file.
const cacheUTF8FunctionName = "msgvault_valid_utf8"

// cacheTextRepairs counts text values the cache export had to repair.
type cacheTextRepairs struct {
	count atomic.Int64
}

// sanitize replaces invalid UTF-8 bytes and counts each repaired value.
func (r *cacheTextRepairs) sanitize(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	r.count.Add(1)
	return textutil.SanitizeUTF8(s)
}

// Count reports the number of values sanitized.
func (r *cacheTextRepairs) Count() int64 {
	if r == nil {
		return 0
	}
	return r.count.Load()
}

type cacheUTF8Func struct{ repairs *cacheTextRepairs }

func (f cacheUTF8Func) Config() duckdb.ScalarFuncConfig {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(fmt.Sprintf("DuckDB VARCHAR type info: %v", err)) // static type; cannot fail
	}
	return duckdb.ScalarFuncConfig{
		InputTypeInfos: []duckdb.TypeInfo{varchar},
		ResultTypeInfo: varchar,
	}
}

func (f cacheUTF8Func) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			s, _ := values[0].(string) // NULL input never reaches here
			return f.repairs.sanitize(s), nil
		},
	}
}

// registerCacheTextFunctions installs the fallback on the builder connection.
// Call it before openCacheSourceSnapshot, which holds that connection.
func registerCacheTextFunctions(ctx context.Context, db *sql.DB, repairs *cacheTextRepairs) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve DuckDB connection for cache text functions: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if err := duckdb.RegisterScalarUDF(conn, cacheUTF8FunctionName, cacheUTF8Func{repairs: repairs}); err != nil {
		return fmt.Errorf("register %s: %w", cacheUTF8FunctionName, err)
	}
	return nil
}

// cacheTextSQL returns valid VARCHAR while keeping NULL. DuckDB's native
// validity check uses the Go fallback only when the value is invalid.
func cacheTextSQL(column string) string {
	v := "TRY_CAST(" + column + " AS VARCHAR)"
	return "COALESCE(TRY(decode(encode(" + v + "))), " + cacheUTF8FunctionName + "(" + v + "))"
}

// cacheIdentityTextSQL returns an identity-comparison key unchanged when it
// holds valid UTF-8 and NULL when it does not. Identity attribution is
// ownership-relevant, so two distinct invalid byte sequences must never
// collapse onto one U+FFFD repair and match; NULL never equals anything in
// SQL and flows through the surrounding TRIM/lower/COALESCE/NULLIF wrappers.
// U+FFFD repair stays reserved for exported display and search text
// (cacheTextSQL); 'msgvault repair-encoding' restores attribution for
// archives whose identity bytes are invalid.
func cacheIdentityTextSQL(column string) string {
	return "TRY(decode(encode(TRY_CAST(" + column + " AS VARCHAR))))"
}

// cacheEnvelopePresenceSQL reports whether an envelope column recorded a
// non-empty value at the byte level, mirroring SQLite's guard that the raw
// column is non-NULL and non-blank after TRIM. Presence must not depend on
// UTF-8 validity: the store treats a recorded From envelope with invalid
// bytes as authoritative, while cacheIdentityTextSQL turns those bytes into
// NULL, so testing the identity key alone would misread a damaged envelope
// as absent and let the participant fallback reclassify the message. TRIM
// is unsafe on invalid bytes in DuckDB, so the NULL (invalid) key falls
// back to the encoded byte length (octet_length is the BLOB-safe length;
// length does not bind BLOBs in DuckDB); invalid strings are never
// all-spaces, so non-empty bytes are equivalent to a non-empty TRIM result,
// and TRIM never sees invalid bytes.
func cacheEnvelopePresenceSQL(column string) string {
	v := "TRY_CAST(" + column + " AS VARCHAR)"
	return "CASE WHEN TRY(decode(encode(" + v + "))) IS NULL " +
		"THEN octet_length(encode(COALESCE(" + v + ", ''))) > 0 " +
		"ELSE TRIM(TRY(decode(encode(" + v + ")))) <> '' END"
}

// reportCacheTextRepairs explains the replacement count and how to fix the archive.
func reportCacheTextRepairs(w io.Writer, repairs *cacheTextRepairs) {
	n := repairs.Count()
	if n == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "Warning: %d invalid UTF-8 repair(s) applied while building the analytics cache; affected cache text uses U+FFFD. Run 'msgvault repair-encoding' to repair the archive.\n", n)
}
