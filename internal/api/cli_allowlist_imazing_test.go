package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCLIAllowlistIncludesIMazingCSVImport(t *testing.T) {
	t.Parallel()
	assert.True(t, cliRunCommandAllowed([]string{
		"import-imazing-csv", "/archive", "--me", "+15550000001", "--timezone", "UTC",
	}))
}
