package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/gvoice"
)

var (
	importGvoiceBefore            string
	importGvoiceAfter             string
	importGvoiceLimit             int
	noDefaultIdentityImportGVoice bool
)

var importGvoiceCmd = &cobra.Command{
	Use:   "import-gvoice <takeout-voice-dir>",
	Short: "Import Google Voice history from Takeout export",
	Long: `Import Google Voice texts, calls, and voicemails from a
Google Takeout export.

The directory should be the "Voice" folder inside the Takeout archive,
containing "Calls/" and "Phones.vcf".

Examples:
  msgvault import-gvoice /path/to/Takeout/Voice
  msgvault import-gvoice /path/to/Takeout/Voice --after 2020-01-01
  msgvault import-gvoice /path/to/Takeout/Voice --limit 100`,
	Args: cobra.ExactArgs(1),
	RunE: runImportGvoice,
}

func runImportGvoice(cmd *cobra.Command, args []string) error {
	takeoutDir := args[0]

	s, err := openStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	clientOpts, err := buildGvoiceOpts()
	if err != nil {
		return err
	}

	client, err := gvoice.NewClient(takeoutDir, clientOpts...)
	if err != nil {
		return fmt.Errorf("open Google Voice takeout: %w", err)
	}
	defer func() { _ = client.Close() }()

	src, err := s.GetOrCreateSource(
		"google_voice", client.Identifier(),
	)
	if err != nil {
		return fmt.Errorf("get or create source: %w", err)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Println("\nInterrupted.")
		cancel()
	}()

	startTime := time.Now()
	fmt.Printf(
		"Importing Google Voice from %s\n", takeoutDir,
	)
	printGvoiceDateFilter()
	if importGvoiceLimit > 0 {
		fmt.Printf("Limit: %d messages\n", importGvoiceLimit)
	}
	fmt.Println()

	summary, err := client.Import(ctx, s, src.ID)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Println("\nImport interrupted.")
			printGvoiceSummary(summary, startTime)
			rebuildCacheAfterWrite(cfg.DatabaseDSN())
			return nil
		}
		return fmt.Errorf("import failed: %w", err)
	}

	phone := client.Identifier()
	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if !noDefaultIdentityImportGVoice && strings.HasPrefix(phone, "+") {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, src.ID, phone, phone, "phone-e164")
	}
	if err := runPostSourceCreateMigrations(s); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	printGvoiceSummary(summary, startTime)
	rebuildCacheAfterWrite(cfg.DatabaseDSN())
	return nil
}

func buildGvoiceOpts() ([]gvoice.ClientOption, error) {
	var opts []gvoice.ClientOption
	opts = append(opts, gvoice.WithLogger(logger))

	if importGvoiceAfter != "" {
		t, err := time.ParseInLocation(
			"2006-01-02", importGvoiceAfter, time.Local,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid --after date: %w (use YYYY-MM-DD format)",
				err,
			)
		}
		opts = append(opts, gvoice.WithAfterDate(t))
	}

	if importGvoiceBefore != "" {
		t, err := time.ParseInLocation(
			"2006-01-02", importGvoiceBefore, time.Local,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"invalid --before date: %w (use YYYY-MM-DD format)",
				err,
			)
		}
		opts = append(opts, gvoice.WithBeforeDate(t))
	}

	if importGvoiceLimit > 0 {
		opts = append(opts, gvoice.WithLimit(importGvoiceLimit))
	}

	return opts, nil
}

func printGvoiceDateFilter() {
	if importGvoiceAfter == "" && importGvoiceBefore == "" {
		return
	}
	parts := []string{}
	if importGvoiceAfter != "" {
		parts = append(parts, "after "+importGvoiceAfter)
	}
	if importGvoiceBefore != "" {
		parts = append(parts, "before "+importGvoiceBefore)
	}
	fmt.Printf("Date filter: %s\n", strings.Join(parts, ", "))
}

func printGvoiceSummary(
	summary *gvoice.ImportSummary,
	startTime time.Time,
) {
	if summary == nil {
		return
	}
	elapsed := time.Since(startTime)
	fmt.Println()
	fmt.Println("Google Voice import complete!")
	fmt.Printf("  Duration:         %s\n", elapsed.Round(time.Second))
	fmt.Printf(
		"  Messages:         %d imported\n",
		summary.MessagesImported,
	)
	fmt.Printf(
		"  Conversations:    %d\n",
		summary.ConversationsImported,
	)
	fmt.Printf(
		"  Participants:     %d resolved\n",
		summary.ParticipantsResolved,
	)
	if summary.Skipped > 0 {
		fmt.Printf("  Skipped:          %d\n", summary.Skipped)
	}
	if summary.MessagesImported > 0 && elapsed.Seconds() > 0 {
		rate := float64(summary.MessagesImported) / elapsed.Seconds()
		fmt.Printf("  Rate:             %.1f messages/sec\n", rate)
	}
}

func init() {
	importGvoiceCmd.Flags().StringVar(
		&importGvoiceBefore, "before", "",
		"only messages before this date (YYYY-MM-DD)",
	)
	importGvoiceCmd.Flags().StringVar(
		&importGvoiceAfter, "after", "",
		"only messages after this date (YYYY-MM-DD)",
	)
	importGvoiceCmd.Flags().IntVar(
		&importGvoiceLimit, "limit", 0,
		"limit number of messages (for testing)",
	)
	importGvoiceCmd.Flags().BoolVar(
		&noDefaultIdentityImportGVoice, "no-default-identity", false,
		noDefaultIdentityHelp,
	)
	rootCmd.AddCommand(importGvoiceCmd)
}
