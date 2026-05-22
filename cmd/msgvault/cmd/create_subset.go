package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

var createSubsetCmd = &cobra.Command{
	Use:   "create-subset",
	Short: "Create a smaller database from the archive",
	Long: `Create a new msgvault database containing a subset of the
most recent messages. Useful for testing, demos, or sharing.

The destination directory will contain a complete msgvault.db with
all referenced data (conversations, participants, labels, etc.)
and can be used directly:

  MSGVAULT_HOME=/path/to/subset msgvault tui`,
	RunE: runCreateSubset,
}

var (
	subsetOutput string
	subsetRows   int
)

func init() {
	createSubsetCmd.Flags().StringVarP(
		&subsetOutput, "output", "o", "",
		"destination directory (msgvault.db created inside)",
	)
	createSubsetCmd.Flags().IntVar(
		&subsetRows, "rows", 0,
		"number of most recent messages to copy",
	)
	_ = createSubsetCmd.MarkFlagRequired("output")
	_ = createSubsetCmd.MarkFlagRequired("rows")
	rootCmd.AddCommand(createSubsetCmd)
}

func runCreateSubset(cmd *cobra.Command, _ []string) error {
	if err := MustBeLocal("create-subset"); err != nil {
		return err
	}

	if subsetRows <= 0 {
		return fmt.Errorf("--rows must be a positive integer")
	}

	srcDBPath := cfg.DatabaseDSN()
	if _, err := os.Stat(srcDBPath); os.IsNotExist(err) {
		return fmt.Errorf(
			"source database not found: %s\n"+
				"Run 'msgvault init-db' and sync first",
			srcDBPath,
		)
	}

	dstDir, err := filepath.Abs(subsetOutput)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"Copying %d messages from %s...\n", subsetRows, srcDBPath,
	)

	result, err := store.CopySubset(srcDBPath, dstDir, subsetRows)
	if err != nil {
		return fmt.Errorf("create subset: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"Created subset (%s)\n", result.Elapsed.Round(time.Millisecond),
	)
	fmt.Printf("Sources:       %d\n", result.Sources)
	fmt.Printf("Messages:      %d\n", result.Messages)
	fmt.Printf("Conversations: %d\n", result.Conversations)
	fmt.Printf("Participants:  %d\n", result.Participants)
	fmt.Printf("Labels:        %d\n", result.Labels)
	fmt.Printf("Database size: %s\n", formatSize(result.DBSize))

	if int64(subsetRows) > result.Messages {
		fmt.Fprintf(os.Stderr,
			"Note: requested %d messages but source only had %d\n",
			subsetRows, result.Messages,
		)
	}

	fmt.Fprintf(os.Stderr,
		"\nTo use: MSGVAULT_HOME=%s msgvault tui\n", dstDir,
	)

	return nil
}
