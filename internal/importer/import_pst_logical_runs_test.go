package importer

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func nativeSyntheticPST(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic-events.pst")
	Require.NoError(t, os.WriteFile(path, syntheticPSTFixture, 0o600))
	return path
}

func TestNativePSTImportOwnsFreshSilentLogicalRuns(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	caller := st.WithIngestContext(store.IngestContext{Mode: store.IngestLive})
	path := nativeSyntheticPST(t)
	for range 2 {
		result, err := ImportPst(t.Context(), caller, path, PstImportOptions{Identifier: "owner@example.net", NoResume: true})
		require.NoError(err)
		require.NotNil(result)
		require.False(result.HardErrors)
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM import_runs WHERE import_format='pst' AND completed=TRUE AND notify_imports=FALSE`: 2,
		`SELECT COUNT(*) FROM sync_runs WHERE status='completed'`:                                                2,
		`SELECT COUNT(*) FROM messages`:                                                                          1,
		`SELECT COUNT(*) FROM import_message_decisions`:                                                          1,
		`SELECT COUNT(*) FROM mcp_event_log`:                                                                     0,
	} {
		var got int
		require.NoError(st.DB().QueryRow(query).Scan(&got))
		Assert.Equal(t, want, got, query)
	}
}
