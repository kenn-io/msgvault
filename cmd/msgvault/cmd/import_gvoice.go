package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/gvoice"
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
	out := cmd.OutOrStdout()
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	if !isDaemonCLISubprocess() {
		return runDaemonCLICommandHTTPFromCobra(cmd, args)
	}

	takeoutDir := args[0]

	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := s.ValidateNativeImportChoice(cmd.Context(), "google_voice", importNotificationsRequested(cmd)); err != nil {
		return err
	}

	clientOpts, err := buildGvoiceOpts(state.logger)
	if err != nil {
		return err
	}
	clientOpts = append(
		clientOpts, gvoice.WithNotifyImports(importNotificationsRequested(cmd)), gvoice.WithAttachmentsDir(cfg.AttachmentsDir()),
	)

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
		_, _ = fmt.Fprintln(out, "\nInterrupted.")
		cancel()
	}()

	startTime := time.Now()
	_, _ = fmt.Fprintf(out,
		"Importing Google Voice from %s\n", takeoutDir,
	)
	printGvoiceDateFilter(out)
	if importGvoiceLimit > 0 {
		_, _ = fmt.Fprintf(out, "Limit: %d messages\n", importGvoiceLimit)
	}
	_, _ = fmt.Fprintln(out)

	summary, err := client.Import(ctx, s, src.ID)
	if summary != nil {
		recordImportRun(cmd, summary.ImportRunID)
	}
	if err != nil {
		if ctx.Err() != nil {
			markImportCommandIncomplete(cmd)
			_, _ = fmt.Fprintln(out, "\nImport interrupted.")
			printGvoiceSummary(out, summary, startTime)
			return rebuildCacheAfterWrite(cfg.DatabaseDSN(), state)
		}
		return fmt.Errorf("import failed: %w", err)
	}

	phone := client.Identifier()
	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if !noDefaultIdentityImportGVoice && strings.HasPrefix(phone, "+") {
		confirmDefaultIdentity(cmd.OutOrStdout(), s, src.ID, phone, phone, "phone-e164", state.logger)
	}
	if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}

	printGvoiceSummary(out, summary, startTime)
	return rebuildCacheAfterWrite(cfg.DatabaseDSN(), state)
}

func buildGvoiceOpts(logger *slog.Logger) ([]gvoice.ClientOption, error) {
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

func printGvoiceDateFilter(out io.Writer) {
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
	_, _ = fmt.Fprintf(out, "Date filter: %s\n", strings.Join(parts, ", "))
}

func printGvoiceSummary(
	out io.Writer,
	summary *gvoice.ImportSummary,
	startTime time.Time,
) {
	if summary == nil {
		return
	}
	elapsed := time.Since(startTime)
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "Google Voice import complete!")
	_, _ = fmt.Fprintf(out, "  Duration:         %s\n", elapsed.Round(time.Second))
	_, _ = fmt.Fprintf(out,
		"  Messages:         %d imported\n",
		summary.MessagesImported,
	)
	_, _ = fmt.Fprintf(out,
		"  Conversations:    %d\n",
		summary.ConversationsImported,
	)
	_, _ = fmt.Fprintf(out,
		"  Participants:     %d resolved\n",
		summary.ParticipantsResolved,
	)
	if summary.Skipped > 0 {
		_, _ = fmt.Fprintf(out, "  Skipped:          %d\n", summary.Skipped)
	}
	if summary.MessagesImported > 0 && elapsed.Seconds() > 0 {
		rate := float64(summary.MessagesImported) / elapsed.Seconds()
		_, _ = fmt.Fprintf(out, "  Rate:             %.1f messages/sec\n", rate)
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
	addImportNotificationFlag(importGvoiceCmd)
	rootCmd.AddCommand(importGvoiceCmd)
}
