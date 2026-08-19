package cmd

import (
	"errors"
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
	subsetOutput                string
	subsetRows                  int
	subsetIncludeIdentity       bool
	subsetIncludeAttributes     bool
	subsetIncludeProfiles       bool
	subsetIncludeVCardResources bool
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
	createSubsetCmd.Flags().BoolVar(
		&subsetIncludeIdentity, "include-identity", false,
		"copy full identity clusters for included participants; exposes "+
			"identifiers (emails, phone numbers) of "+
			"linked identities that have no messages in the subset",
	)
	createSubsetCmd.Flags().BoolVar(
		&subsetIncludeAttributes, "include-attributes", false,
		"copy person and organization attribute definitions and all current/history values; may expose sensitive values and provenance metadata",
	)
	createSubsetCmd.Flags().BoolVar(
		&subsetIncludeProfiles, "include-profiles", false,
		"copy structured profile values, history, media, contact observations, relationships, employment history with referenced organizations (their profiles, contacts, and media), and provenance; may expose sensitive personal data",
	)
	createSubsetCmd.Flags().BoolVar(
		&subsetIncludeVCardResources, "include-vcard-resources", false,
		"copy included people's complete native vCard bodies and retired-UID aliases; requires --include-profiles; a body is opaque and may carry custom properties and RELATED entries naming people outside the subset",
	)
	_ = createSubsetCmd.MarkFlagRequired("output")
	_ = createSubsetCmd.MarkFlagRequired("rows")
	rootCmd.AddCommand(createSubsetCmd)
}

func runCreateSubset(cmd *cobra.Command, args []string) error {
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}

	if subsetRows <= 0 {
		return usageErr(cmd, errors.New("--rows must be a positive integer"))
	}
	if subsetIncludeVCardResources && !subsetIncludeProfiles {
		return usageErr(cmd, errors.New(
			"--include-vcard-resources requires --include-profiles"))
	}

	srcDBPath := cfg.DatabaseDSN()
	// store.CopySubset uses ATTACH DATABASE + SQLite-only file-stat
	// semantics; refuse early on a PG DSN to mirror the equivalent
	// guard in backupDatabase (cmd/msgvault/cmd/deduplicate.go).
	if store.IsPostgresURL(srcDBPath) {
		return errors.New("create-subset is SQLite-only (uses ATTACH DATABASE); not supported with PostgreSQL stores")
	}
	if _, err := os.Stat(srcDBPath); os.IsNotExist(err) {
		return fmt.Errorf(
			"source database not found: %s\n"+
				"Run 'msgvault init-db' and sync first",
			srcDBPath,
		)
	}

	release, err := acquireDirectSQLiteWriteLock(cfg)
	if err != nil {
		return err
	}
	defer release()

	dstDir, err := filepath.Abs(subsetOutput)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}

	fmt.Fprintf(os.Stderr,
		"Copying %d messages from %s...\n", subsetRows, srcDBPath,
	)
	if subsetIncludeAttributes {
		fmt.Fprintln(os.Stderr,
			"WARNING: --include-attributes copies every included person's and referenced organization's current and historical attribute values, including sensitive content, provenance references, and actor metadata.")
	}
	if subsetIncludeProfiles {
		fmt.Fprintln(os.Stderr,
			"WARNING: --include-profiles copies every included person's current and historical structured profile values, media, contact observations, relationships, and provenance metadata, plus their employment history and the referenced organizations' profiles, contacts, and media.")
	}
	if subsetIncludeVCardResources {
		fmt.Fprintln(os.Stderr,
			"WARNING: --include-vcard-resources copies every included person's complete native vCard bodies and their retired-UID aliases. A body is copied whole and stays opaque, so it may carry custom properties and RELATED entries naming people outside the subset.")
	}

	result, err := store.CopySubsetWithOptions(srcDBPath, dstDir, subsetRows,
		store.CopySubsetOptions{
			IncludeIdentity:       subsetIncludeIdentity,
			IncludeAttributes:     subsetIncludeAttributes,
			IncludeProfiles:       subsetIncludeProfiles,
			IncludeVCardResources: subsetIncludeVCardResources,
		})
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
	if subsetIncludeProfiles {
		fmt.Printf("Organizations: %d\n", result.Organizations)
		fmt.Printf("Employments:   %d\n", result.Employments)
	}
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
