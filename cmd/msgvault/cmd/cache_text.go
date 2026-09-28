package cmd

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"go.kenn.io/msgvault/internal/textutil"
)

// cacheUTF8FunctionName is the DuckDB scalar function cacheTextSQL calls for
// values that are not valid UTF-8. DuckDB's sqlite scanner passes stored bytes
// through unchecked, and its Parquet reader later rejects the whole file.
const cacheUTF8FunctionName = "msgvault_valid_utf8"

// cacheTextRepairs counts repaired text and unknown identity output values.
type cacheTextRepairs struct {
	count      atomic.Int64
	identities atomic.Int64
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

type cacheStringFunc func(string) (any, error)

func (f cacheStringFunc) Config() duckdb.ScalarFuncConfig {
	varchar, err := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR)
	if err != nil {
		panic(fmt.Sprintf("DuckDB VARCHAR type info: %v", err)) // static type; cannot fail
	}
	return duckdb.ScalarFuncConfig{
		InputTypeInfos:      []duckdb.TypeInfo{varchar},
		ResultTypeInfo:      varchar,
		Volatile:            true,
		SpecialNullHandling: true,
	}
}

func (f cacheStringFunc) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{
		RowExecutor: func(values []driver.Value) (any, error) {
			if values[0] == nil {
				return nil, nil
			}
			s, _ := values[0].(string) // NULL input never reaches here
			return f(s)
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
	functions := map[string]cacheStringFunc{
		cacheUTF8FunctionName: func(s string) (any, error) {
			return repairs.sanitize(s), nil
		},
		"msgvault_unknown_identity": func(string) (any, error) {
			repairs.identities.Add(1)
			return nil, nil //nolint:nilnil // A successful DuckDB scalar result uses nil for SQL NULL.
		},
		"msgvault_count_text": func(s string) (any, error) {
			repairs.count.Add(1)
			return s, nil
		},
	}
	for name, fn := range functions {
		if err := duckdb.RegisterScalarUDF(conn, name, fn); err != nil {
			return fmt.Errorf("register %s: %w", name, err)
		}
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
// (cacheTextSQL). repair-encoding does not repair Message-IDs, source message IDs,
// source identifiers, recorded envelope addresses, account identity addresses,
// or participant identifiers; those fields need separate recovery from verified
// original values before rebuilding the cache.
func cacheIdentityTextSQL(column string) string {
	return "TRY(decode(encode(TRY_CAST(" + column + " AS VARCHAR))))"
}

// cacheIdentityExportSQL counts unknown output values, never attribution reads.
// The fallback is volatile so repeated output columns each count their value.
func cacheIdentityExportSQL(column string) string {
	return "COALESCE(" + cacheIdentityTextSQL(column) +
		", msgvault_unknown_identity(TRY_CAST(" + column + " AS VARCHAR)))"
}

// CSV preparation must make text readable before DuckDB sees it. Its damage
// marker survives until the final projection, after export filters and joins.
// Scanner values can instead be checked directly at that same boundary.
func (s *cacheSourceSnapshot) textSQL(column string) string {
	if !s.csvSnapshot {
		return cacheTextSQL(column)
	}
	return "CASE WHEN " + csvInvalidTextSQL(column) +
		" THEN msgvault_count_text(" + column + ") ELSE " + column + " END"
}

func (s *cacheSourceSnapshot) identityExportSQL(column string) string {
	if !s.csvSnapshot {
		return cacheIdentityExportSQL(column)
	}
	return "CASE WHEN " + csvInvalidTextSQL(column) +
		" THEN msgvault_unknown_identity('') ELSE " + column + " END"
}

func csvInvalidTextSQL(column string) string {
	qualifier, name := "", column
	if i := strings.LastIndexByte(column, '.'); i >= 0 {
		qualifier, name = column[:i+1], column[i+1:]
	}
	return "contains(" + qualifier + "__invalid_utf8, '|" + name + "|')"
}

// cacheIdentityPresenceSQL reports whether an identity column recorded a
// non-empty value at the byte level, mirroring SQLite's guard that the raw
// column is non-NULL and non-blank after TRIM. Presence must not depend on
// UTF-8 validity: a damaged envelope or primary email must still suppress
// attribution through another address. cacheIdentityTextSQL turns invalid
// bytes into NULL, so testing that key alone would misread them as absent. TRIM
// is unsafe on invalid bytes in DuckDB, so the NULL (invalid) key falls
// back to the encoded byte length (octet_length is the BLOB-safe length;
// length does not bind BLOBs in DuckDB); invalid strings are never
// all-spaces, so non-empty bytes are equivalent to a non-empty TRIM result,
// and TRIM never sees invalid bytes.
func cacheIdentityPresenceSQL(column string) string {
	v := "TRY_CAST(" + column + " AS VARCHAR)"
	return "CASE WHEN TRY(decode(encode(" + v + "))) IS NULL " +
		"THEN octet_length(encode(COALESCE(" + v + ", ''))) > 0 " +
		"ELSE TRIM(TRY(decode(encode(" + v + ")))) <> '' END"
}

// reportCacheTextRepairs explains the replacement count and archive repair limits.
func reportCacheTextRepairs(w io.Writer, repairs *cacheTextRepairs) {
	if repairs == nil {
		return
	}
	n := repairs.Count()
	if n == 0 && repairs.identities.Load() == 0 {
		return
	}
	if n > 0 {
		_, _ = fmt.Fprintf(w, "Warning: %d invalid UTF-8 repair(s) applied while building the analytics cache; affected cache text uses U+FFFD.\n", n)
	}
	if n := repairs.identities.Load(); n > 0 {
		_, _ = fmt.Fprintf(w, "Warning: %d identity value(s) exported as unknown because of invalid UTF-8.\n", n)
	}
	_, _ = fmt.Fprintln(w, "Run 'msgvault repair-encoding' to repair supported archive fields and rebuild the cache. RFC 822 Message-IDs, source message IDs, source identifiers, recorded envelope addresses, account identity addresses, and participant identifiers require separate recovery if damaged.")
	_, _ = fmt.Fprintln(w, "For damaged List-IDs, run 'msgvault repair-list-ids --apply' to re-derive them from stored raw MIME; recovery requires an intact original List-Id header.")
}
