package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

// TestStatsCommand_AccountAndCollectionMutuallyExclusive confirms that passing
// both --account and --collection to the stats command is rejected by cobra.
func TestStatsCommand_AccountAndCollectionMutuallyExclusive(t *testing.T) {
	var a, b string
	cmd := &cobra.Command{Use: "stats-test", SilenceErrors: true}
	sub := &cobra.Command{Use: "stats", RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	sub.Flags().StringVar(&a, "account", "", "")
	sub.Flags().StringVar(&b, "collection", "", "")
	sub.MarkFlagsMutuallyExclusive("account", "collection")
	cmd.AddCommand(sub)
	cmd.SetArgs([]string{"stats", "--account", "foo@example.com", "--collection", "bar"})

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

// TestStatsCommand_EmptyCollectionRejected verifies that
// `stats --collection <name>` errors out when the named collection
// has zero member sources, instead of silently falling through to
// archive-wide stats. Regression test for iter13 codex Medium:
// previously, an empty collection produced a non-IsEmpty Scope but
// SourceIDs() returned an empty slice, and GetStatsForScope treats
// an empty slice as unscoped/global.
func TestStatsCommand_EmptyCollectionRejected(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Pre-create the store and an empty collection. CreateCollection
	// requires at least one source, so create a source, attach, and
	// then remove the source from the collection to leave it empty.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if _, err := st.CreateCollection("empty", "test", []int64{src.ID}); err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if err := st.RemoveSourcesFromCollection("empty", []int64{src.ID}); err != nil {
		t.Fatalf("remove source from collection: %v", err)
	}
	_ = st.Close()

	savedCfg := cfg
	savedLogger := logger
	savedStatsCollection := statsCollection
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		statsCollection = savedStatsCollection
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	statsCollection = "empty"

	testCmd := &cobra.Command{Use: "stats", RunE: statsCmd.RunE}
	testCmd.Flags().StringVar(&statsAccount, "account", "", "")
	testCmd.Flags().StringVar(&statsCollection, "collection", "empty", "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"stats", "--collection", "empty"})

	err = root.Execute()
	if err == nil {
		t.Fatal("expected error for empty collection, got nil")
	}
	if !strings.Contains(err.Error(), "no member accounts") {
		t.Errorf("error message = %q, want substring \"no member accounts\"", err.Error())
	}
}
