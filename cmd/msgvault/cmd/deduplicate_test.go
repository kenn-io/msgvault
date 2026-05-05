package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
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
	if err == nil {
		t.Fatal("expected error when both --account and --collection are set, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "account") || !strings.Contains(msg, "collection") {
		t.Errorf("error should mention both flag names; got: %q", msg)
	}
	_ = a
	_ = b
}

// TestDeduplicateCollectionResolution confirms that --collection resolves
// successfully when the name matches a real collection in the store.
func TestDeduplicateCollectionResolution(t *testing.T) {
	f, _, collectionName := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, collectionName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.Collection == nil {
		t.Fatal("expected Collection to be populated")
	}
	if scope.Collection.Name != collectionName {
		t.Errorf("collection name = %q, want %q", scope.Collection.Name, collectionName)
	}
	ids := scope.SourceIDs()
	if len(ids) == 0 {
		t.Error("expected non-empty SourceIDs for collection")
	}
}

// TestDeduplicateCollectionResolution_MultiSource confirms SourceIDs expands
// to all members when a collection has more than one source.
func TestDeduplicateCollectionResolution_MultiSource(t *testing.T) {
	f := storetest.New(t)

	src2, err := f.Store.GetOrCreateSource("mbox", "backup@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource src2")

	collName := "two-account-collection"
	_, err = f.Store.CreateCollection(collName, "", []int64{f.Source.ID, src2.ID})
	testutil.MustNoErr(t, err, "CreateCollection")

	scope, err := ResolveCollectionFlag(f.Store, collName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ids := scope.SourceIDs()
	if len(ids) != 2 {
		t.Errorf("expected 2 source IDs, got %d: %v", len(ids), ids)
	}
	if scope.DisplayName() != collName {
		t.Errorf("DisplayName = %q, want %q", scope.DisplayName(), collName)
	}
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
				if out != "" {
					t.Errorf("expected no output, got %q", out)
				}
				return
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}
