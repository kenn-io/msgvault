package cmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func newRepairSendersCmd() *cobra.Command {
	apply := false
	cmd := &cobra.Command{
		Use:   "repair-senders",
		Short: "Restore missing email senders from archived MIME",
		Long: `Scan archived email MIME for messages that have neither sender_id nor a
From-recipient snapshot. The default is a read-only report. Pass --apply to
restore both sender representations atomically and refresh derived analytics.

The command reads only the local archive. It never contacts a provider and
never overwrites sender evidence that already exists.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runRepairSendersLocal(cmd, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write recovered senders")
	return cmd
}

type plannedSenderRepair struct {
	messageID          int64
	rawMIMEFingerprint [sha256.Size]byte
	sender             mime.Address
}

type senderRepairPlan struct {
	candidateCount int
	repairs        []plannedSenderRepair
	unresolved     int
}

const senderRepairScanBatchSize = 100

func runRepairSendersLocal(cmd *cobra.Command, apply bool) error {
	ctx := cmd.Context()
	var (
		st      *store.Store
		cleanup func()
		err     error
	)
	if apply {
		st, cleanup, err = openWritableStoreAndInit()
	} else {
		st, err = store.OpenReadOnly(cfg.DatabaseDSN())
		cleanup = func() { _ = st.Close() }
	}
	if err != nil {
		return fmt.Errorf("open archive for sender repair: %w", err)
	}
	defer cleanup()

	plan, err := scanAndPlanSenderRepairs(ctx, st)
	if err != nil {
		return err
	}
	if err := printSenderRepairPlan(cmd, plan); err != nil {
		return err
	}
	if !apply {
		if _, err := fmt.Fprintln(
			cmd.OutOrStdout(),
			"Dry run: no rows were modified. Re-run with --apply to write repairs.",
		); err != nil {
			return fmt.Errorf("write sender repair dry-run summary: %w", err)
		}
		return nil
	}

	repaired := 0
	var failures []error
	for _, repair := range plan.repairs {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := st.ApplySenderRepairContext(
			ctx, repair.messageID, repair.rawMIMEFingerprint, repair.sender,
		); err != nil {
			failures = append(failures, err)
			continue
		}
		repaired++
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Repaired: %d\n", repaired); err != nil {
		failures = append(failures, fmt.Errorf("write sender repair result: %w", err))
	}
	if repaired > 0 {
		if err := rebuildCacheAfterWrite(cfg.DatabaseDSN()); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func scanAndPlanSenderRepairs(
	ctx context.Context,
	st *store.Store,
) (*senderRepairPlan, error) {
	plan := &senderRepairPlan{}
	var afterMessageID int64
	for {
		candidates, err := st.ListMissingMIMESendersPageContext(
			ctx, afterMessageID, senderRepairScanBatchSize,
		)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			break
		}
		plan.candidateCount += len(candidates)
		for _, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if candidate.DecodeError != nil {
				logger.Warn("cannot decode archived MIME for sender repair",
					"error", candidate.DecodeError)
				plan.unresolved++
				continue
			}
			parsed, _ := mime.ParseWithRecovery(candidate.RawMIME, "")
			if parsed == nil || len(parsed.From) == 0 || parsed.From[0].Email == "" {
				plan.unresolved++
				continue
			}
			if err := store.ValidateRepairSender(parsed.From[0]); err != nil {
				logger.Warn("recovered sender is not an installable address",
					"message_id", candidate.MessageID, "error", err)
				plan.unresolved++
				continue
			}
			plan.repairs = append(plan.repairs, plannedSenderRepair{
				messageID:          candidate.MessageID,
				rawMIMEFingerprint: candidate.RawMIMEFingerprint,
				sender:             parsed.From[0],
			})
		}
		afterMessageID = candidates[len(candidates)-1].MessageID
	}
	return plan, nil
}

func printSenderRepairPlan(cmd *cobra.Command, plan *senderRepairPlan) error {
	if _, err := fmt.Fprintf(
		cmd.OutOrStdout(),
		"Candidates: %d\nRepairable: %d\nUnresolved: %d\n",
		plan.candidateCount,
		len(plan.repairs),
		plan.unresolved,
	); err != nil {
		return fmt.Errorf("write sender repair plan: %w", err)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(newRepairSendersCmd())
}
