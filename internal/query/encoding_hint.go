package query

import (
	"fmt"
	"strings"
)

const (
	// parquetEncodingErrorSubstring is emitted when DuckDB encounters invalid
	// UTF-8 in an existing Parquet cache.
	parquetEncodingErrorSubstring = "Invalid string encoding found in Parquet file"
	// csvEncodingErrorSubstring is emitted while the cache builder reads its
	// SQLite CSV snapshot on platforms where sqlite_scanner is unavailable.
	csvEncodingErrorSubstring = "Invalid unicode (byte sequence mismatch) detected"
)

// IsEncodingError reports whether err contains a DuckDB encoding error that
// can be resolved by running `msgvault repair-encoding`.
func IsEncodingError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), parquetEncodingErrorSubstring) ||
		strings.Contains(err.Error(), csvEncodingErrorSubstring))
}

// HintRepairEncoding wraps err with a user-facing hint suggesting
// `msgvault repair-encoding` when the error is an encoding error.
// If err is nil or unrelated, it is returned unchanged.
func HintRepairEncoding(err error) error {
	if !IsEncodingError(err) {
		return err
	}
	if strings.Contains(err.Error(), csvEncodingErrorSubstring) {
		return fmt.Errorf("%w\nHint: run 'msgvault repair-encoding' to repair common archived text fields and retry the cache rebuild; if the cache rebuild still fails, report the affected table. DuckDB's ignore_errors setting is not a msgvault option", err)
	}
	return fmt.Errorf("%w\nHint: try running 'msgvault repair-encoding' to fix encoding issues", err)
}
