package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestSortSemanticPageByDate(t *testing.T) {
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []daemonclient.CLIHybridSearchResult{{ID: 1, SentAt: old}, {ID: 9, SentAt: recent}, {ID: 3, SentAt: recent}}
	sortSemanticResultsByDate(rows)
	assert.Equal(t, []int64{3, 9, 1}, []int64{rows[0].ID, rows[1].ID, rows[2].ID})
}

func TestSearchRejectsInvalidSortBeforeStore(t *testing.T) {
	for _, tc := range []struct{ sort, wantError string }{
		{"wrong", "invalid --sort"},
		{"date", "--sort date requires --mode vector or hybrid"},
	} {
		t.Run(tc.sort, func(t *testing.T) {
			resetSearchFlags()
			t.Cleanup(resetSearchFlags)
			root := newTestRootCmd()
			root.AddCommand(searchCmd)
			root.SetArgs([]string{"search", "intent", "--sort", tc.sort})
			require.ErrorContains(t, root.Execute(), tc.wantError)
		})
	}
}
