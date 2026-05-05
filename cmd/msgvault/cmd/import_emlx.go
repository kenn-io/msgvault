package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/applemail"
	"github.com/wesm/msgvault/internal/importer"
	"github.com/wesm/msgvault/internal/store"
)

var (
	importEmlxSourceType         string
	importEmlxNoResume           bool
	importEmlxCheckpointInterval int
	importEmlxNoAttachments      bool
	importEmlxAccountsDB         string
	importEmlxAccounts           []string
	importEmlxIdentifier         string
	noDefaultIdentityImportEmlx  bool
)

var importEmlxCmd = &cobra.Command{
	Use:   "import-emlx [mail-dir]",
	Short: "Import Apple Mail .emlx files into msgvault",
	Long: `Import Apple Mail .emlx files into msgvault.

By default, auto-discovers accounts from Apple Mail's V10 directory layout
by reading ~/Library/Accounts/Accounts4.sqlite to map account GUIDs to
email addresses.

If mail-dir is omitted, defaults to ~/Library/Mail.

Labels are derived from directory names. Messages that appear in
multiple mailboxes are deduplicated and given labels from each.

Examples:
  # Auto-discover accounts from default Apple Mail location
  msgvault import-emlx

  # Auto-discover accounts from explicit mail directory
  msgvault import-emlx ~/Library/Mail

  # Import only specific account(s)
  msgvault import-emlx --account me@gmail.com
  msgvault import-emlx --account me@gmail.com --account work@company.com

  # Manual fallback: import a single directory with explicit identifier
  msgvault import-emlx ~/Library/Mail/V10/SOME-GUID --identifier me@gmail.com
  msgvault import-emlx ~/Mail/INBOX.mbox/ --identifier me@gmail.com

  # Legacy two-arg form (deprecated, still works)
  msgvault import-emlx me@gmail.com ~/Library/Mail/V10/SOME-GUID
`,
	Args:         cobra.MaximumNArgs(2),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Use a local copy so we don't mutate the package-global
		// flag variable (which persists across Execute() calls).
		identifier := importEmlxIdentifier

		// Legacy two-arg form: import-emlx <identifier> <mail-dir>
		if len(args) == 2 {
			if identifier != "" {
				return fmt.Errorf(
					"cannot use --identifier with two positional arguments",
				)
			}
			identifier = args[0]
			args = args[1:]
		}

		// --identifier requires an explicit mail-dir to avoid
		// accidentally importing the entire ~/Library/Mail tree
		// under one identifier.
		if identifier != "" && len(args) == 0 {
			return fmt.Errorf(
				"--identifier requires a positional mail-dir argument",
			)
		}

		// Determine mail directory.
		var mailDir string
		if len(args) > 0 {
			mailDir = args[0]
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("determine home directory: %w", err)
			}
			mailDir = filepath.Join(home, "Library", "Mail")
		}

		// Expand ~ if present.
		if strings.HasPrefix(mailDir, "~/") {
			home, _ := os.UserHomeDir()
			mailDir = filepath.Join(home, mailDir[2:])
		}

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()
		sigChan := make(chan os.Signal, 2)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		done := make(chan struct{})
		defer func() {
			close(done)
			signal.Stop(sigChan)
			for {
				select {
				case <-sigChan:
				default:
					return
				}
			}
		}()
		go func() {
			signals := 0
			for {
				select {
				case <-done:
					return
				case <-sigChan:
					select {
					case <-done:
						return
					default:
					}
					signals++
					if signals == 1 {
						_, _ = fmt.Fprintln(
							cmd.ErrOrStderr(),
							"\nInterrupted. Saving checkpoint...",
						)
						cancel()
						continue
					}
					_, _ = fmt.Fprintln(
						cmd.ErrOrStderr(),
						"Interrupted again. Exiting immediately.",
					)
					os.Exit(130)
				}
			}
		}()

		dbPath := cfg.DatabaseDSN()
		st, err := store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		defer func() { _ = st.Close() }()

		if err := st.InitSchema(); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
		if err := runStartupMigrationsForIngest(st); err != nil {
			return fmt.Errorf("startup migrations: %w", err)
		}

		attachmentsDir := cfg.AttachmentsDir()
		if importEmlxNoAttachments {
			attachmentsDir = ""
		}

		var importErr error
		if identifier != "" {
			// Manual fallback: single import with explicit identifier.
			importErr = importSingleAccount(ctx, cmd, st, mailDir, identifier, attachmentsDir)
		} else {
			// Auto mode: discover accounts from V10 layout + Accounts4.sqlite.
			importErr = importAutoAccounts(ctx, cmd, st, mailDir, attachmentsDir)
		}

		rebuildCacheAfterWrite(dbPath)
		return importErr
	},
}

func importSingleAccount(
	ctx context.Context,
	cmd *cobra.Command,
	st *store.Store,
	mailDir, identifier, attachmentsDir string,
) error {
	summary, err := importer.ImportEmlxDir(
		ctx, st, mailDir, importer.EmlxImportOptions{
			SourceType:         importEmlxSourceType,
			Identifier:         identifier,
			NoResume:           importEmlxNoResume,
			CheckpointInterval: importEmlxCheckpointInterval,
			AttachmentsDir:     attachmentsDir,
			Logger:             logger,
		},
	)
	if err != nil {
		return err
	}

	// Auto-default-identity must run BEFORE the legacy migration
	// retry — see comment in account_identity.go.
	if ctx.Err() == nil && !summary.HardErrors && !noDefaultIdentityImportEmlx {
		if summary.SourceID != 0 {
			confirmDefaultIdentity(cmd.OutOrStdout(), st, summary.SourceID, identifier, identifier, "account-identifier")
		} else {
			logger.Warn("auto-default-identity: missing source id", "identifier", identifier)
		}
	}

	if summary.SourceID != 0 {
		// Migration error returns before printImportSummary on
		// purpose: this is the minimal in-scope shape for the deferred
		// legacy [identity] migration retry that #304 introduces. The
		// alternative (capture error, print summary, return after)
		// would restructure pre-existing summary-print code that this
		// PR otherwise leaves alone. The migration is idempotent — the
		// next invocation retries and prints the summary then. UX
		// polish tracked separately in
		// private/drafts/2026-05-02-issue-import-migration-error-ux.md.
		if err := runPostSourceCreateMigrations(st); err != nil {
			return fmt.Errorf("post-source-create migrations: %w", err)
		}
	}

	printImportSummary(cmd, ctx, *summary)
	return importResultError(ctx, *summary)
}

func importAutoAccounts(
	ctx context.Context,
	cmd *cobra.Command,
	st *store.Store,
	mailDir, attachmentsDir string,
) error {
	accountsDBPath := importEmlxAccountsDB
	if strings.HasPrefix(accountsDBPath, "~/") {
		home, _ := os.UserHomeDir()
		accountsDBPath = filepath.Join(home, accountsDBPath[2:])
	}

	out := cmd.OutOrStdout()

	accounts, err := applemail.DiscoverV10Accounts(mailDir, accountsDBPath, logger)
	if err != nil {
		return fmt.Errorf("discover accounts: %w", err)
	}

	if len(accounts) == 0 {
		return fmt.Errorf(
			"no V10 accounts found in %s\n\n"+
				"If this is not an Apple Mail V10 directory, use --identifier to specify\n"+
				"the account email manually:\n\n"+
				"  msgvault import-emlx %s --identifier you@gmail.com",
			mailDir, mailDir,
		)
	}

	// Filter by --account flags if set.
	if len(importEmlxAccounts) > 0 {
		filter := make(map[string]bool)
		for _, a := range importEmlxAccounts {
			filter[strings.ToLower(a)] = true
		}

		var filtered []applemail.AccountInfo
		for _, a := range accounts {
			if filter[strings.ToLower(a.Email)] || filter[strings.ToLower(a.Identifier())] {
				filtered = append(filtered, a)
			}
		}

		if len(filtered) == 0 {
			var available []string
			for _, a := range accounts {
				available = append(available, a.Identifier())
			}
			return fmt.Errorf(
				"no matching accounts found for --account filter\n"+
					"Available accounts: %s",
				strings.Join(available, ", "),
			)
		}
		accounts = filtered
	}

	_, _ = fmt.Fprintf(out, "Discovered %d account(s):\n", len(accounts))
	for _, a := range accounts {
		if a.Email != "" {
			_, _ = fmt.Fprintf(out, "  - %s (%s)\n", a.Email, a.Description)
		} else {
			_, _ = fmt.Fprintf(out, "  - %s\n", a.Description)
		}
	}
	_, _ = fmt.Fprintln(out)

	var grandTotal importer.EmlxImportSummary
	var importErrors []error

	for _, account := range accounts {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(out, "Import interrupted between accounts.")
			break
		}

		identifier := account.Identifier()
		accountDir, err := applemail.V10AccountDir(mailDir, account.GUID)
		if err != nil {
			importErrors = append(importErrors, fmt.Errorf("%s: %w", identifier, err))
			_, _ = fmt.Fprintf(out, "Skipping %s: %v\n", identifier, err)
			continue
		}

		if account.Email != "" {
			_, _ = fmt.Fprintf(out, "Importing %s (%s)...\n", account.Email, account.Description)
		} else {
			_, _ = fmt.Fprintf(out, "Importing %s...\n", account.Description)
		}

		summary, err := importer.ImportEmlxDir(
			ctx, st, accountDir, importer.EmlxImportOptions{
				SourceType:         importEmlxSourceType,
				Identifier:         identifier,
				NoResume:           importEmlxNoResume,
				CheckpointInterval: importEmlxCheckpointInterval,
				AttachmentsDir:     attachmentsDir,
				Logger:             logger,
			},
		)
		if err != nil {
			importErrors = append(importErrors, fmt.Errorf("%s: %w", identifier, err))
			continue
		}

		if summary.MailboxesTotal == 0 {
			_, _ = fmt.Fprintf(out, "Skipping %s: no mailboxes found\n", identifier)
			continue
		}

		// Auto-default-identity must run BEFORE the legacy migration
		// retry — see comment in account_identity.go.
		if ctx.Err() == nil && !summary.HardErrors && !noDefaultIdentityImportEmlx {
			accountDisplay := account.Identifier()
			if account.Email != "" {
				accountDisplay = account.Email
			}
			if summary.SourceID != 0 {
				confirmDefaultIdentity(cmd.OutOrStdout(), st, summary.SourceID, accountDisplay, identifier, "account-identifier")
			} else {
				logger.Warn("auto-default-identity: missing source id", "identifier", identifier)
			}
		}

		if summary.SourceID != 0 {
			// `continue` skips the per-account summary print on
			// purpose — same scope rationale as importSingleAccount
			// above. UX polish tracked in
			// private/drafts/2026-05-02-issue-import-migration-error-ux.md
			// for a follow-up PR.
			if err := runPostSourceCreateMigrations(st); err != nil {
				importErrors = append(importErrors, fmt.Errorf("%s: post-source-create migrations: %w", identifier, err))
				continue
			}
		}

		printImportSummary(cmd, ctx, *summary)
		_, _ = fmt.Fprintln(out)

		// Accumulate totals.
		grandTotal.MailboxesTotal += summary.MailboxesTotal
		grandTotal.MailboxesImported += summary.MailboxesImported
		grandTotal.MessagesProcessed += summary.MessagesProcessed
		grandTotal.MessagesAdded += summary.MessagesAdded
		grandTotal.MessagesUpdated += summary.MessagesUpdated
		grandTotal.MessagesSkipped += summary.MessagesSkipped
		grandTotal.Errors += summary.Errors
		if summary.HardErrors {
			grandTotal.HardErrors = true
		}
	}

	if len(accounts) > 1 {
		_, _ = fmt.Fprintln(out, "=== Grand Total ===")
		printImportStats(out, grandTotal)
	}

	if len(importErrors) > 0 {
		for _, e := range importErrors {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Error: %v\n", e)
		}
		return fmt.Errorf("import completed with %d account error(s)", len(importErrors))
	}

	if ctx.Err() != nil {
		return context.Canceled
	}

	if grandTotal.HardErrors {
		return fmt.Errorf("import completed with %d errors", grandTotal.Errors)
	}

	return nil
}

func importResultError(ctx context.Context, summary importer.EmlxImportSummary) error {
	if ctx.Err() != nil {
		return context.Canceled
	}
	if summary.HardErrors {
		return fmt.Errorf("import completed with %d errors", summary.Errors)
	}
	return nil
}

func printImportSummary(cmd *cobra.Command, ctx context.Context, summary importer.EmlxImportSummary) {
	out := cmd.OutOrStdout()

	if ctx.Err() != nil {
		_, _ = fmt.Fprintln(out, "Import interrupted. Run again to resume.")
	} else if summary.Errors > 0 {
		_, _ = fmt.Fprintln(out, "Import complete (with errors).")
	} else {
		_, _ = fmt.Fprintln(out, "Import complete.")
	}

	printImportStats(out, summary)
}

func printImportStats(out io.Writer, summary importer.EmlxImportSummary) {
	_, _ = fmt.Fprintf(out,
		"  Mailboxes:      %d discovered, %d imported\n",
		summary.MailboxesTotal, summary.MailboxesImported,
	)
	_, _ = fmt.Fprintf(out,
		"  Processed:      %d messages\n",
		summary.MessagesProcessed,
	)
	_, _ = fmt.Fprintf(out,
		"  Added:          %d messages\n",
		summary.MessagesAdded,
	)
	_, _ = fmt.Fprintf(out,
		"  Updated:        %d messages\n",
		summary.MessagesUpdated,
	)
	_, _ = fmt.Fprintf(out,
		"  Skipped (dup):  %d messages\n",
		summary.MessagesSkipped,
	)
	_, _ = fmt.Fprintf(out,
		"  Errors:         %d\n",
		summary.Errors,
	)
}

func init() {
	rootCmd.AddCommand(importEmlxCmd)

	importEmlxCmd.Flags().StringVar(
		&importEmlxSourceType, "source-type", "apple-mail",
		"Source type to record in the database",
	)
	importEmlxCmd.Flags().BoolVar(
		&importEmlxNoResume, "no-resume", false,
		"Do not resume from an interrupted import",
	)
	importEmlxCmd.Flags().IntVar(
		&importEmlxCheckpointInterval, "checkpoint-interval", 200,
		"Save progress every N messages",
	)
	importEmlxCmd.Flags().BoolVar(
		&importEmlxNoAttachments, "no-attachments", false,
		"Do not store attachments on disk",
	)
	importEmlxCmd.Flags().StringVar(
		&importEmlxAccountsDB, "accounts-db", applemail.DefaultAccountsDBPath(),
		"Path to Apple's Accounts4.sqlite database",
	)
	importEmlxCmd.Flags().StringSliceVar(
		&importEmlxAccounts, "account", nil,
		"Filter to specific account email(s) (repeatable)",
	)
	importEmlxCmd.Flags().StringVar(
		&importEmlxIdentifier, "identifier", "",
		"Explicit email/identifier for single-directory import (manual fallback)",
	)
	importEmlxCmd.Flags().BoolVar(
		&noDefaultIdentityImportEmlx, "no-default-identity", false,
		noDefaultIdentityHelp,
	)
}
