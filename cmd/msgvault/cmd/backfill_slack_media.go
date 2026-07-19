package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/slack"
)

func newBackfillSlackMediaCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill-slack-media [team-id]",
		Short: "Retry pending Slack file downloads",
		Long: `Retry pending Slack file downloads.

Files that failed to download during sync (outages, size caps since raised)
leave pending markers. This command re-reads the archived message JSON and
retries the downloads. Idempotent: already-downloaded files are never
re-fetched.

Examples:
  msgvault backfill-slack-media
  msgvault backfill-slack-media T0123456789`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			flagTeam := ""
			if len(args) > 0 {
				flagTeam = args[0]
			}
			s, cleanup, err := openWritableStoreAndInitForIngest()
			if err != nil {
				return err
			}
			defer cleanup()
			sources, err := resolveSlackSyncSources(s, flagTeam)
			if err != nil {
				return err
			}
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted.")
			defer stop()

			var runErrors []string
			for _, src := range sources {
				if ctx.Err() != nil {
					break
				}
				teamID, userID, ok := splitSlackIdentifier(src.Identifier)
				if !ok {
					runErrors = append(runErrors, src.Identifier+": malformed slack identifier")
					continue
				}
				token, terr := slack.LoadToken(cfg.TokensDir(), teamID)
				if terr != nil {
					runErrors = append(runErrors, fmt.Sprintf("%s: %v", teamID, terr))
					continue
				}
				imp := slack.NewImporter(s, slack.NewClient("", token), teamID)
				sum, berr := imp.BackfillMedia(ctx, slackImportOptions(teamID, userID))
				if ctx.Err() != nil {
					break
				}
				if berr != nil {
					runErrors = append(runErrors, fmt.Sprintf("%s: %v", teamID, berr))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s done in %s: %d messages, %d downloaded, %d still pending\n",
					teamID, sum.Duration.Round(time.Second), sum.MessagesProcessed, sum.AttachmentsDownloaded, sum.AttachmentsPending)
			}
			if len(runErrors) > 0 {
				return fmt.Errorf("%d workspace(s) failed: %s", len(runErrors), strings.Join(runErrors, "; "))
			}
			if ctx.Err() != nil {
				return errors.New("interrupted")
			}
			return nil
		},
	}
	return cmd
}

func init() {
	rootCmd.AddCommand(newBackfillSlackMediaCmd())
}
