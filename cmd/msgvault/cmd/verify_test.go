package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsFTSIntegrityError_Classification verifies that the hint-classifier
// cleanly separates FTS5 shadow-table errors (which rebuild-fts can fix)
// from core-table errors (which need .recover). Messages come from real
// PRAGMA integrity_check output; the shapes below are what users will see.
func TestIsFTSIntegrityError_Classification(t *testing.T) {
	tests := []struct {
		msg    string
		wantFT bool
	}{
		{
			msg:    "malformed inverted index for FTS5 table main.messages_fts",
			wantFT: true,
		},
		{
			msg:    "row 42 missing from index messages_fts_idx",
			wantFT: true,
		},
		{
			msg:    "Tree 26 page 8231140 cell 2: Rowid 421177 out of order",
			wantFT: false,
		},
		{
			msg:    "non-unique entry in index sqlite_autoindex_messages_1",
			wantFT: false,
		},
		{
			msg:    "",
			wantFT: false,
		},
	}

	for _, tc := range tests {
		got := isFTSIntegrityError(tc.msg)
		assert.Equal(t, tc.wantFT, got, "isFTSIntegrityError(%q)", tc.msg)
	}
}
