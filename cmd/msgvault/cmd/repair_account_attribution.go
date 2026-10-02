package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairAccountAttributionCmd() *cobra.Command {
	var sourceID int64
	var pageSize int
	cmd := &cobra.Command{
		Use:   "repair-account-attribution",
		Short: "Recompute archived email and calendar account attribution",
		Long: `Derive one account per message from confirmed source identities and outer
email headers or calendar mappings. Ambiguous messages remain unattributed.
Progress commits with each page. Interrupted runs resume from the committed
cursor, and completed runs do no work. New syncs maintain the projection.

Examples:
  msgvault repair-account-attribution
  msgvault repair-account-attribution --source-id 3 --page-size 100`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			if sourceID < 0 {
				return errors.New("source-id must be positive")
			}
			if pageSize < 1 || pageSize > 500 {
				return errors.New("page-size must be between 1 and 500")
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			sources, err := st.ListSourcesContext(cmd.Context(), "")
			if err != nil {
				return err
			}
			found := sourceID == 0
			for _, src := range sources {
				if sourceID != 0 && src.ID != sourceID {
					continue
				}
				found = true
				progress, repairErr := st.BackfillAccountAttributionContext(cmd.Context(), src.ID, pageSize, func(p store.AccountAttributionProgress) error {
					_, e := fmt.Fprintf(cmd.OutOrStdout(), "Source %d: committed through message %d / %d; complete=%t\n", p.SourceID, p.LastMessageID, p.HighWaterID, p.Completed)
					if e != nil {
						return fmt.Errorf("write account repair checkpoint: %w", e)
					}
					return nil
				})
				if repairErr != nil {
					return repairErr
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Source %d: %d messages processed.\n", src.ID, progress.Scanned)
			}
			if !found {
				return fmt.Errorf("source %d not found", sourceID)
			}
			return rebuildCacheAfterWrite(state.cfg.DatabaseDSN(), state)
		},
	}
	cmd.Flags().Int64Var(&sourceID, "source-id", 0, "Repair one physical source; zero repairs all sources")
	cmd.Flags().IntVar(&pageSize, "page-size", 100, "Messages per committed page (1–500)")
	return cmd
}

func init() { rootCmd.AddCommand(newRepairAccountAttributionCmd()) }
