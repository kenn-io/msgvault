package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTUIDuckDBOptionsDisableSQLiteScannerByDefault(t *testing.T) {
	opts := tuiDuckDBOptions()

	assert.True(t, opts.DisableSQLiteScanner,
		"TUI must not attach the live SQLite DB through DuckDB sqlite_scanner")
}
