package cmd

import (
	"errors"
	"fmt"
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
the verbatim payload stored alongside every message.

Syncing already heals an archive on its own — each source re-derives once, on its
next sync — so this is for repairing on demand instead of waiting, or for
re-running after an interrupted pass. It works entirely against the local
archive: no provider connection is needed, and messages the provider no longer
holds are repaired too. Only derived columns are rewritten; raw payloads,
downloaded media, and sync cursors are untouched, so it is idempotent.

Examples:
  msgvault repair-derived
  msgvault repair-derived --source-type beeper
  msgvault repair-derived --source-type beeper --identifier instagramgo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}

			s, cleanup, err := openWritableStoreAndInitForIngest()
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
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"No matching sources with a re-derivation pass (available: %s)\n",
					strings.Join(rederive.SourceTypes(), ", "))
				return nil
			}

			for _, src := range sources {
				label := src.SourceType + "/" + src.Identifier
				progress := func(msg string) { _, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", label, msg) }
				sum, rerr := rederive.Run(ctx, s, src.SourceType, src.Identifier, src.ID, progress)
				if ctx.Err() != nil {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "\nInterrupted — re-run repair-derived to finish (idempotent).")
					return rebuildCacheAfterWrite(cfg.DatabaseDSN())
				}
				if rerr != nil {
					return errors.Join(
						fmt.Errorf("repair failed for %s: %w", label, rerr),
						rebuildCacheAfterWrite(cfg.DatabaseDSN()),
					)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"%s: %d messages scanned, %d bodies rewritten, %d attachments tagged (%s)\n",
					label, sum.MessagesScanned, sum.BodiesRewritten, sum.AttachmentsTagged,
					sum.Duration.Round(time.Second))
				if sum.Undecodable > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %d archived payloads could not be decoded — left unchanged\n", sum.Undecodable)
				}
				if sum.Errors > 0 {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %d errors — re-run to retry\n", sum.Errors)
				}
			}

			return rebuildCacheAfterWrite(cfg.DatabaseDSN())
		},
	}
	cmd.Flags().StringArrayVar(&repairDerivedSourceTypes, "source-type", nil,
		"source type to repair (repeatable; default: every type with a re-derivation pass)")
	cmd.Flags().StringArrayVar(&repairDerivedIdentifiers, "identifier", nil,
		"source identifier to repair (repeatable; default: all matching sources)")
	return cmd
}

// repairDerivedTargets resolves the sources this run should repair: those whose
// type has a registered pass, narrowed by the --source-type and --identifier
// flags. An unknown source type is an error rather than a silent no-op, so a
// typo does not look like a clean run.
func repairDerivedTargets(s *store.Store) ([]*store.Source, error) {
	wantType := map[string]bool{}
	for _, t := range repairDerivedSourceTypes {
		if _, _, ok := rederive.Lookup(t); !ok {
			return nil, fmt.Errorf("no re-derivation pass for source type %q (available: %s)",
				t, strings.Join(rederive.SourceTypes(), ", "))
		}
		wantType[t] = true
	}
	wantID := map[string]bool{}
	for _, id := range repairDerivedIdentifiers {
		wantID[id] = true
	}

	all, err := s.ListSources("")
	if err != nil {
		return nil, err
	}
	var out []*store.Source
	for _, src := range all {
		if _, _, ok := rederive.Lookup(src.SourceType); !ok {
			continue
		}
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
