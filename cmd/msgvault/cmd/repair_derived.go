package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
)

var (
	repairDerivedSourceTypes []string
	repairDerivedIdentifiers []string
)

func newRepairDerivedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair-derived",
		Short: "Re-derive stored message text and metadata from archived payloads",
		Long: `Re-derive stored message columns from the payloads archived with them.

Message bodies, snippets, the search index, and attachment metadata are computed
from a provider's payload when a message is imported, so improving how they are
derived leaves already-archived rows stale. This command recomputes them from
the verbatim payload stored alongside every message, for source types with a
re-derivation pass. For every source it also derives account attribution (the
address searched by account: and received:) for email and calendar rows still
waiting for it.

Syncing already heals an archive on its own — each source re-derives once, on its
next sync, and every sync or import derives pending account attribution — so
this is for repairing on demand instead of waiting, for file imports that will
not run again, or for re-running after an interrupted pass. It works entirely
against the local archive: no provider connection is needed, and messages the
provider no longer holds are repaired too. Only derived columns are rewritten;
raw payloads, downloaded media, and sync cursors are untouched, so it is
idempotent.

Examples:
  msgvault repair-derived
  msgvault repair-derived --source-type beeper
  msgvault repair-derived --source-type beeper --identifier instagramgo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			cfg := state.cfg
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Stopping...")
			defer stop()

			sources, err := repairDerivedTargets(s)
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No matching sources")
				return nil
			}

			for _, src := range sources {
				label := src.SourceType + "/" + src.Identifier
				rerr := repairDerivedSource(ctx, cmd.OutOrStdout(), s, src)
				if ctx.Err() != nil {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run repair-derived to finish (idempotent).")
					return rebuildCacheAfterWrite(cfg.DatabaseDSN(), state)
				}
				if rerr != nil {
					return errors.Join(
						fmt.Errorf("repair failed for %s: %w", label, rerr),
						rebuildCacheAfterWrite(cfg.DatabaseDSN(), state),
					)
				}
			}

			return rebuildCacheAfterWrite(cfg.DatabaseDSN(), state)
		},
	}
	cmd.Flags().StringArrayVar(&repairDerivedSourceTypes, "source-type", nil,
		"source type to repair (repeatable; default: every source)")
	cmd.Flags().StringArrayVar(&repairDerivedIdentifiers, "identifier", nil,
		"source identifier to repair (repeatable; default: all matching sources)")
	return cmd
}

// repairDerivedSource runs a source's typed re-derivation pass, if any, then
// derives its pending account attribution, printing a summary of each.
func repairDerivedSource(ctx context.Context, out io.Writer, s *store.Store, src *store.Source) error {
	label := src.SourceType + "/" + src.Identifier
	progress := func(msg string) { _, _ = fmt.Fprintf(out, "  %s: %s\n", label, msg) }
	if _, _, ok := rederive.Lookup(src.SourceType); ok {
		sum, err := rederive.Run(ctx, s, src.SourceType, src.Identifier, src.ID, progress)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(out, formatRepairDerivedSummary(label, sum))
		if sum.Undecodable > 0 {
			_, _ = fmt.Fprintf(out, "  %d archived payloads could not be decoded — left unchanged\n", sum.Undecodable)
		}
		if sum.Errors > 0 {
			_, _ = fmt.Fprintf(out, "  %d errors — re-run to retry\n", sum.Errors)
		}
	}
	report := func(sum store.AccountAttributionRepairSummary) {
		progress(fmt.Sprintf("account attribution: %d messages", sum.Scanned))
	}
	sum, err := s.RepairAccountAttributionContext(ctx, src.ID, report)
	if err != nil {
		return err
	}
	if sum.Scanned > 0 {
		_, _ = fmt.Fprint(out, formatAccountAttributionSummary(label, sum))
	}
	return nil
}

func formatRepairDerivedSummary(label string, sum *rederive.Summary) string {
	return fmt.Sprintf(
		"%s: %d messages scanned, %d message metadata rewritten, %d bodies rewritten, %d attachments tagged (%s)\n",
		label, sum.MessagesScanned, sum.MessageMetadataRewritten, sum.BodiesRewritten,
		sum.AttachmentsTagged, sum.Duration.Round(time.Second),
	)
}

func formatAccountAttributionSummary(label string, sum store.AccountAttributionRepairSummary) string {
	out := fmt.Sprintf("%s: account attribution derived for %d messages\n", label, sum.Scanned)
	if sum.Undecodable > 0 {
		out += fmt.Sprintf("  %d had unreadable delivery headers — attributed from their recipients and sender\n",
			sum.Undecodable)
	}
	return out
}

// repairDerivedTargets resolves the sources this run should repair, narrowed
// by the --source-type and --identifier flags. Every source can hold pending
// account attribution, so a flag value is known when it has a typed pass or
// names a type this archive holds. An unknown source type is an error rather
// than a silent no-op, so a typo does not look like a clean run.
func repairDerivedTargets(s *store.Store) ([]*store.Source, error) {
	all, err := s.ListSources("")
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, t := range rederive.SourceTypes() {
		known[t] = true
	}
	for _, src := range all {
		known[src.SourceType] = true
	}
	wantType := map[string]bool{}
	for _, t := range repairDerivedSourceTypes {
		if !known[t] {
			return nil, fmt.Errorf("no source of type %q to repair (available: %s)",
				t, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
		wantType[t] = true
	}
	wantID := map[string]bool{}
	for _, id := range repairDerivedIdentifiers {
		wantID[id] = true
	}

	var out []*store.Source
	for _, src := range all {
		if len(wantType) > 0 && !wantType[src.SourceType] {
			continue
		}
		if len(wantID) > 0 && !wantID[src.Identifier] {
			continue
		}
		out = append(out, src)
	}
	return out, nil
}

func init() {
	rootCmd.AddCommand(newRepairDerivedCmd())
}
