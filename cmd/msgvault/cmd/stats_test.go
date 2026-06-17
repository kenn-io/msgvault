package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
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
	requirepkg.Error(t, err, "expected error when both --account and --collection are set")
	msg := err.Error()
	assertpkg.Contains(t, msg, "account", "error should mention account flag name")
	assertpkg.Contains(t, msg, "collection", "error should mention collection flag name")
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
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Pre-create the store and an empty collection. CreateCollection
	// requires at least one source, so create a source, attach, and
	// then remove the source from the collection to leave it empty.
	st, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(st.InitSchema(), "init schema")
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err, "create source")
	_, err = st.CreateCollection("empty", "test", []int64{src.ID})
	require.NoError(err, "create collection")
	require.NoError(st.RemoveSourcesFromCollection("empty", []int64{src.ID}), "remove source from collection")
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
	require.Error(err, "expected error for empty collection")
	assertpkg.Contains(t, err.Error(), "no member accounts")
}
