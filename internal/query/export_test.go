package query

// HasSQLiteScanner reports whether DuckDB loaded its sqlite extension, which
// varies by build environment.
var HasSQLiteScanner = (*DuckDBEngine).hasSQLite
