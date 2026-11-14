package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairAttachmentRolesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "repair-attachment-roles",
		Short: "Classify older email attachments",
		Long: `Classify email attachments archived before msgvault recorded attachment roles.

Unknown roles prevent inline/standalone filtering. Uses each message's stored
MIME to classify attachments only when exactly one part matches; missing MIME
or ambiguous matches leave roles unknown.

Changes are written immediately. Interrupted runs resume from saved progress;
completed runs return at once. The command works entirely offline.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runRepairAttachmentRolesLocal(cmd)
		},
	}
}

func runRepairAttachmentRolesLocal(cmd *cobra.Command) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
	if err != nil {
		return fmt.Errorf("open archive for attachment role repair: %w", err)
	}
	defer cleanup()

	summary, err := repairHistoricalAttachmentRoles(cmd.Context(), st, repairLogger(state.logger))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(),
		"Attachment role repair complete: messages scanned=%d attachments updated=%d\n",
		summary.MessagesScanned, summary.AttachmentsUpdated); err != nil {
		return fmt.Errorf("write attachment role repair summary: %w", err)
	}
	return nil
}

// attachmentRoleRepairSummary totals the batches of one repair run.
type attachmentRoleRepairSummary struct {
	MessagesScanned    int
	AttachmentsUpdated int
}

// repairHistoricalAttachmentRoles runs role repair batches until the durable
// cursor reports completion. A nil logger disables per-batch progress.
func repairHistoricalAttachmentRoles(
	ctx context.Context,
	st *store.Store,
	logger *slog.Logger,
) (attachmentRoleRepairSummary, error) {
	const batchSize = 1000
	var summary attachmentRoleRepairSummary
	for {
		progress, err := st.RepairHistoricalAttachmentRolesBatch(ctx, batchSize)
		if err != nil {
			return summary, fmt.Errorf("repair historical attachment roles: %w", err)
		}
		summary.MessagesScanned += progress.MessagesScanned
		summary.AttachmentsUpdated += progress.AttachmentsUpdated
		if logger != nil && progress.MessagesScanned > 0 {
			logger.Info("attachment role repair batch",
				"last_message_id", progress.LastMessageID,
				"messages_scanned", summary.MessagesScanned,
				"attachments_updated", summary.AttachmentsUpdated)
		}
		if progress.Completed {
			return summary, nil
		}
	}
}

func init() {
	rootCmd.AddCommand(newRepairAttachmentRolesCmd())
}
