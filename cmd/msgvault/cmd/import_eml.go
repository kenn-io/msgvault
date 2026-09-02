package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

type importEMLFlags struct {
	identifier         string
	sourceType         string
	noResume           bool
	checkpointInterval int
	noAttachments      bool
	noDefaultIdentity  bool
}

func newImportEMLCommand() *cobra.Command {
	var flags importEMLFlags
	cmd := &cobra.Command{
		Use:   "import-eml <mail-dir>",
		Short: "Import a tree of RFC 5322 .eml files",
		Long: `Import RFC 5322 .eml files stored in MailMate-style .mailbox
directories. Nested mailbox names become slash-separated labels, and exact
duplicate messages receive every mailbox label where they appear.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErr(cmd, errors.New("import-eml requires exactly 1 arg: <mail-dir>"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			if flags.identifier == "" {
				return usageErr(cmd, errors.New("--identifier is required"))
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			st, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()

			attachmentsDir := cfg.AttachmentsDir()
			if flags.noAttachments {
				attachmentsDir = ""
			}
			dbPath := cfg.DatabaseDSN()
			summary, importErr := importer.ImportEMLDir(ctx, st, args[0], importer.EMLImportOptions{
				SourceType:         flags.sourceType,
				Identifier:         flags.identifier,
				NoResume:           flags.noResume,
				CheckpointInterval: flags.checkpointInterval,
				AttachmentsDir:     attachmentsDir,
				Logger:             logger,
			})
			if importErr != nil {
				return errors.Join(importErr, rebuildCacheAfterWrite(dbPath))
			}

			if err := runEMLPostImportMigrations(
				cmd.OutOrStdout(), st, summary, flags,
			); err != nil {
				return errors.Join(err, rebuildCacheAfterWrite(dbPath))
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Mailboxes: %d/%d\n", summary.MailboxesImported, summary.MailboxesTotal)
			_, _ = fmt.Fprintf(out, "Messages: %d added, %d updated, %d skipped, %d error(s)\n",
				summary.MessagesAdded, summary.MessagesUpdated, summary.MessagesSkipped, summary.Errors)
			if summary.WasResumed {
				_, _ = fmt.Fprintln(out, "Resumed from the previous checkpoint.")
			}

			resultErr := ctx.Err()
			if resultErr == nil && summary.HardErrors {
				resultErr = fmt.Errorf("import completed with %d errors", summary.Errors)
			}
			return errors.Join(resultErr, rebuildCacheAfterWrite(dbPath))
		},
	}

	cmd.Flags().StringVar(&flags.identifier, "identifier", "", "Account identifier for imported messages (required)")
	cmd.Flags().StringVar(&flags.sourceType, "source-type", "eml", "Source type stored for imported messages")
	cmd.Flags().BoolVar(&flags.noResume, "no-resume", false, "Start a new import instead of resuming an active checkpoint")
	cmd.Flags().IntVar(&flags.checkpointInterval, "checkpoint-interval", 200, "Save progress after this many messages")
	cmd.Flags().BoolVar(&flags.noAttachments, "no-attachments", false, "Do not write attachment files")
	cmd.Flags().BoolVar(&flags.noDefaultIdentity, "no-default-identity", false, "Do not confirm the identifier as the source's default identity")
	return cmd
}

func runEMLPostImportMigrations(
	out io.Writer,
	st *store.Store,
	summary *importer.EMLImportSummary,
	flags importEMLFlags,
) error {
	if summary == nil || summary.SourceID == 0 {
		return nil
	}
	// Establish the source identifier before retrying the legacy migration,
	// including after a partial import. Otherwise migrated legacy identities
	// can suppress the source's own identifier on the next resume.
	if !flags.noDefaultIdentity && store.SourceTypeUsesEmailIdentity(flags.sourceType) {
		confirmDefaultIdentity(
			out, st, summary.SourceID,
			flags.identifier, flags.identifier, "account-identifier",
		)
	}
	if err := runPostSourceCreateMigrations(st); err != nil {
		return fmt.Errorf("post-source-create migrations: %w", err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newImportEMLCommand())
}
