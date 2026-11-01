package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
)

type slackdumpCLIOptions struct {
	Me                string
	Limit             int
	MaxMediaMB        int64
	NoDefaultIdentity bool
}

func newImportSlackdumpCmd() *cobra.Command {
	var opts slackdumpCLIOptions
	cmd := &cobra.Command{
		Use:   "import-slackdump <export-dir-or-zip>",
		Short: "Import a Slackdump export",
		Args:  cobra.ExactArgs(1),
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Limit < 0 {
				return usageErr(cmd, errors.New("--limit must be non-negative"))
			}
			if opts.MaxMediaMB < 0 {
				return usageErr(cmd, errors.New("--max-media-mb must be non-negative"))
			}
			if opts.MaxMediaMB > math.MaxInt64>>20 {
				return usageErr(cmd, errors.New("--max-media-mb is too large"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			return runImportSlackdump(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.Me, "me", "", "your Slack user ID or profile email (required)")
	cmd.Flags().IntVar(&opts.Limit, "limit", 0, "maximum messages per conversation (0 = unlimited)")
	cmd.Flags().Int64Var(&opts.MaxMediaMB, "max-media-mb", 0, "maximum exported file size in MiB (0 = configured/default limit)")
	cmd.Flags().BoolVar(&opts.NoDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	_ = cmd.MarkFlagRequired("me")
	return cmd
}

func runImportSlackdump(cmd *cobra.Command, sourcePath string, opts slackdumpCLIOptions) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	info, err := os.Stat(sourcePath)
	if err != nil {
		return fmt.Errorf("source path not found: %w", err)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fmt.Errorf("source path is neither a directory nor a ZIP file: %s", sourcePath)
	}

	dbPath := cfg.DatabaseDSN()
	st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Stopping Slackdump import...")
	defer stop()
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Importing Slackdump export from %s\n", sourcePath)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Me: %s\n\n", opts.Me)
	summary, importErr := slack.ImportSlackdump(ctx, st, sourcePath, slack.SlackdumpImportOptions{
		Me:             opts.Me,
		Limit:          opts.Limit,
		AttachmentsDir: cfg.AttachmentsDir(),
		MediaPolicyForTeam: func(teamID string) attachmentpolicy.Policy {
			return resolveSlackdumpMediaPolicy(teamID, opts.MaxMediaMB, cfg)
		},
		Progress: func(line string) { writeSlackProgress(cmd.OutOrStdout(), line) },
	})
	var postImportErr error
	if summary != nil {
		postImportErr = runExportPostImportMigrations(ctx, cmd.OutOrStdout(), st, summary.SourceID, opts.NoDefaultIdentity, state)
	}
	if importErr != nil {
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nImport interrupted. Re-run the command to import the export again.")
		}
		return errors.Join(
			fmt.Errorf("import Slackdump: %w", importErr),
			postImportErr,
			rebuildCacheAfterWrite(ctx, dbPath, state),
		)
	}
	if postImportErr != nil {
		return errors.Join(postImportErr, rebuildCacheAfterWrite(ctx, dbPath, state))
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nImport complete!")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Duration:      %s\n", summary.Duration.Round(time.Millisecond))
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Conversations: %d\n", summary.ConversationsProcessed)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:      %d processed, %d added, %d updated\n",
		summary.MessagesProcessed, summary.MessagesAdded, summary.MessagesUpdated)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments:   %d stored, %d missing, %d skipped\n",
		summary.AttachmentsDownloaded, summary.AttachmentsMissing, summary.AttachmentsSkipped)
	if summary.Errors > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Errors:        %d\n", summary.Errors)
	}
	return rebuildCacheAfterWrite(ctx, dbPath, state)
}

func runExportPostImportMigrations(ctx context.Context, out io.Writer, st *store.Store, sourceID int64, noDefaultIdentity bool, state *invocation) error {
	if sourceID == 0 {
		return nil
	}
	if state == nil || state.logger == nil {
		return errors.New("invocation state is required")
	}
	var identityErr error
	if !noDefaultIdentity {
		source, err := st.GetSourceByIDContext(ctx, sourceID)
		if err != nil {
			identityErr = fmt.Errorf("read import source: %w", err)
		} else {
			confirmDefaultIdentity(
				ctx,
				out, st, source.ID,
				source.Identifier, source.Identifier, "account-identifier",
				state.logger,
			)
		}
	}
	migrationErr := runPostSourceCreateMigrationsForInvocation(ctx, st, state)
	if migrationErr != nil {
		migrationErr = fmt.Errorf("post-source-create migrations: %w", migrationErr)
	}
	return errors.Join(identityErr, migrationErr)
}

func resolveSlackdumpMediaPolicy(teamID string, overrideMB int64, cfg *config.Config) attachmentpolicy.Policy {
	if cfg == nil {
		return attachmentpolicy.Policy{}
	}
	policy := cfg.Slack.MediaPolicy(teamID)
	if overrideMB > 0 {
		policy.MaxBytes = overrideMB << 20
	}
	return policy
}

func init() {
	rootCmd.AddCommand(newImportSlackdumpCmd())
}
