package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestDeduplicateMutualExclusion confirms that passing both --account and
// --collection to the deduplicate command is rejected by cobra.
func TestDeduplicateMutualExclusion(t *testing.T) {
	// Build a minimal parent so Execute() returns errors rather than printing
	// them and swallowing them via the global rootCmd error handler.
	var a, b string
	cmd := &cobra.Command{Use: "dedup-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "deduplicate", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringVar(&a, "account", "", "")
	sub.Flags().StringVar(&b, "collection", "", "")
	sub.MarkFlagsMutuallyExclusive("account", "collection")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"deduplicate", "--account", "alpha@example.com", "--collection", "work"})

	err := cmd.Execute()
	requirepkg.Error(t, err, "expected error when both --account and --collection are set")
	msg := err.Error()
	assertpkg.Contains(t, msg, "account", "error should mention account flag name")
	assertpkg.Contains(t, msg, "collection", "error should mention collection flag name")
	_ = a
	_ = b
}

// TestDeduplicateCollectionResolution confirms that --collection resolves
// successfully when the name matches a real collection in the store.
func TestDeduplicateCollectionResolution(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f, _, collectionName := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, collectionName)
	require.NoError(err)
	require.NotNil(scope.Collection, "expected Collection to be populated")
	assert.Equal(collectionName, scope.Collection.Name, "collection name")
	ids := scope.SourceIDs()
	assert.NotEmpty(ids, "expected non-empty SourceIDs for collection")
}

// TestDeduplicateCollectionResolution_MultiSource confirms SourceIDs expands
// to all members when a collection has more than one source.
func TestDeduplicateCollectionResolution_MultiSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	src2, err := f.Store.GetOrCreateSource("mbox", "backup@example.com")
	require.NoError(err, "GetOrCreateSource src2")

	collName := "two-account-collection"
	_, err = f.Store.CreateCollection(collName, "", []int64{f.Source.ID, src2.ID})
	require.NoError(err, "CreateCollection")

	scope, err := ResolveCollectionFlag(f.Store, collName)
	require.NoError(err)
	ids := scope.SourceIDs()
	assert.Len(ids, 2, "expected 2 source IDs, got %v", ids)
	assert.Equal(collName, scope.DisplayName(), "DisplayName")
}

// TestPrintAccumulatedUndoHint asserts the helper's behavior:
// no-op for <2 batches, prints recipe for ≥2. Iter15 follow-up:
// the exit-on-Execute-error path now also calls this helper so a
// user who hits an error mid-loop still sees how to undo what
// already ran.
func TestPrintAccumulatedUndoHint(t *testing.T) {
	for _, tc := range []struct {
		name         string
		batches      []string
		wantContains []string
		wantNoOutput bool
	}{
		{
			name:         "no batches",
			batches:      nil,
			wantNoOutput: true,
		},
		{
			name:         "single batch",
			batches:      []string{"dedup-1"},
			wantNoOutput: true,
		},
		{
			name:    "two batches",
			batches: []string{"dedup-a", "dedup-b"},
			wantContains: []string{
				"To undo all of the above",
				"--undo dedup-a",
				"--undo dedup-b",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := captureStdout(t)
			printAccumulatedUndoHint(tc.batches)
			out := done()
			if tc.wantNoOutput {
				assertpkg.Empty(t, out, "expected no output")
				return
			}
			for _, want := range tc.wantContains {
				assertpkg.Contains(t, out, want, "output missing %q", want)
			}
		})
	}
}
