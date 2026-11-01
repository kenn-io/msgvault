package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type personTiersCommandStore interface {
	PreviewPersonTiersContext(ctx context.Context, options store.PersonTierOptions) (*store.PersonTierPreview, error)
	ExplainPersonTierContext(ctx context.Context, personID int64, options store.PersonTierOptions) (*store.PersonTierExplanation, error)
}

type personTiersCommandDeps struct {
	bind               func(context.Context) personTiersCommandDeps
	options            func() (store.PersonTierOptions, error)
	openStore          func() (personTiersCommandStore, func(), error)
	isDaemonSubprocess func() bool
	proxy              func(cmd *cobra.Command, runArgs []string) error
}

func defaultPersonTiersCommandDeps() personTiersCommandDeps {
	return personTiersCommandDeps{
		bind: func(ctx context.Context) personTiersCommandDeps {
			deps := defaultPersonTiersCommandDeps()
			state := invocationFromContext(ctx)
			var currentCfg *config.Config
			if state != nil {
				currentCfg = state.cfg
			}
			deps.options = func() (store.PersonTierOptions, error) {
				if currentCfg == nil {
					return store.PersonTierOptions{}, errors.New("configuration is unavailable")
				}
				if err := currentCfg.PeopleTiersError(); err != nil {
					return store.PersonTierOptions{}, err
				}
				return store.PersonTierOptions{
					Config:            currentCfg.People.Tiers,
					ActivityTimezone:  currentCfg.Activity.Timezone,
					ActivityMaxDirect: currentCfg.Activity.MaxDirectCounterparts,
				}, nil
			}
			deps.openStore = func() (personTiersCommandStore, func(), error) {
				return openInitializedStoreForInvocation(ctx, state)
			}
			return deps
		},
		options: func() (store.PersonTierOptions, error) {
			return store.PersonTierOptions{}, errors.New("configuration is unavailable")
		},
		openStore: func() (personTiersCommandStore, func(), error) {
			return nil, nil, errors.New("configuration is unavailable")
		},
		isDaemonSubprocess: isDaemonCLISubprocess,
		proxy: func(cmd *cobra.Command, runArgs []string) error {
			return runDaemonCLICommandHTTPWithEnv(cmd, runArgs, nil, false, false)
		},
	}
}

func newPersonTiersCommand(deps personTiersCommandDeps) *cobra.Command {
	command := &cobra.Command{Use: "tiers", Short: "Find saved people worth tracking from their archived contact"}
	command.AddCommand(newPersonTiersPreviewCommand(deps), newPersonTiersExplainCommand(deps))
	return command
}

func newPersonTiersPreviewCommand(deps personTiersCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "preview",
		Short: "List saved people in tier 1 or 2",
		Long: `List saved people in tier 1 (sustained two-way contact in the short window,
90 days by default) or tier 2 (two-way contact or meetings in the long window,
a year by default), with the rule that put each there, whether they are
tracked, and the latest day that counted toward the tier. Windows and
thresholds come from [people.tiers]. Counts follow the timezone and direct
limit the activity projection was built under. Nothing is written; pass the
IDs from --json to "person track".`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return runPersonTiers(command, deps, args, func(st personTiersCommandStore, options store.PersonTierOptions) error {
				preview, err := st.PreviewPersonTiersContext(command.Context(), options)
				if err != nil {
					return err
				}
				writePersonTiersWarning(command.ErrOrStderr(), preview.Readiness)
				if jsonOutput {
					return printJSONTo(command.OutOrStdout(), preview)
				}
				return writePersonTiersPreview(command.OutOrStdout(), preview)
			})
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonTiersExplainCommand(deps personTiersCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "explain <person-id>",
		Short: "Show every count behind one person's tier",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(command, args[0], personValue)
			if err != nil {
				return err
			}
			return runPersonTiers(command, deps, args, func(st personTiersCommandStore, options store.PersonTierOptions) error {
				explanation, err := st.ExplainPersonTierContext(command.Context(), personID, options)
				if err != nil {
					return err
				}
				writePersonTiersWarning(command.ErrOrStderr(), explanation.Readiness)
				if jsonOutput {
					return printJSONTo(command.OutOrStdout(), explanation)
				}
				return writePersonTierExplanation(command.OutOrStdout(), explanation)
			})
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

// runPersonTiers sends the command to the daemon from the CLI. In the daemon
// it opens the store and runs the subcommand against it.
func runPersonTiers(command *cobra.Command, deps personTiersCommandDeps, args []string, run func(personTiersCommandStore, store.PersonTierOptions) error) error {
	if invocationFromContext(command.Context()) != nil && deps.bind != nil {
		deps = deps.bind(command.Context())
	}
	if !deps.isDaemonSubprocess() {
		runArgs, err := daemonCLIArgsFromCobra(command, args)
		if err != nil {
			return err
		}
		return deps.proxy(command, runArgs)
	}
	options, err := deps.options()
	if err != nil {
		return err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	return run(st, options)
}

func writePersonTiersWarning(w io.Writer, readiness store.PersonTierReadiness) {
	if !readiness.Ready {
		_, _ = fmt.Fprintf(w, "Warning: the activity projection is behind or built under other settings (%s); counts may be too low or follow the stored settings until \"msgvault activity build\" catches up.\n",
			strings.Join(readiness.Reasons, ", "))
	}
}

func personTiersProjectionLine(p store.PersonTierProjection) string {
	return fmt.Sprintf("counts built with timezone %s, direct limit %d", p.Timezone, p.MaxDirect)
}

func writePersonTiersPreview(w io.Writer, preview *store.PersonTierPreview) error {
	if _, err := fmt.Fprintln(w, personTiersProjectionLine(preview.Projection)); err != nil {
		return fmt.Errorf("write person tiers preview: %w", err)
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "TIER\tPERSON\tNAME\tRULE\tTRACKED\tLAST COUNTED")
	for _, row := range preview.People {
		_, _ = fmt.Fprintf(table, "%d\t%d\t%s\t%s\t%t\t%s\n", row.Tier, row.PersonID,
			personDisplayName(row.DisplayName), row.Rule, row.Tracked, optionalCLIString(row.LastContactDate))
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write person tiers preview: %w", err)
	}
	return nil
}

func writePersonTierExplanation(w io.Writer, e *store.PersonTierExplanation) error {
	exclusion := e.Exclusion
	if exclusion == "" {
		exclusion = "-"
	}
	_, err := fmt.Fprintf(w, "Person: %d\nName: %s\nTier: %d\nRule: %s\nExclusion: %s\nTracked: %t\nLast counted: %s\nAs of: %s\nProjection: timezone %s, direct limit %d\n"+
		"Excluded list messages: %d\nLarge-audience inbound messages: %d\n",
		e.PersonID, personDisplayName(e.DisplayName), e.Tier, e.Rule, exclusion, e.Tracked,
		optionalCLIString(e.LastContactDate), e.AsOf, e.Projection.Timezone, e.Projection.MaxDirect,
		e.Signals.ExcludedListEvents, e.Signals.LargeAudienceInboundEvents)
	if err != nil {
		return fmt.Errorf("write person tier explanation: %w", err)
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "THRESHOLD\tCOUNT\tREQUIRED\tMET")
	for _, check := range e.Thresholds {
		required := strconv.Itoa(check.Required)
		if !check.Enabled {
			required = "off"
		}
		_, _ = fmt.Fprintf(table, "%s\t%d\t%s\t%t\n", check.Name, check.Observed, required, check.Met)
	}
	if err = table.Flush(); err != nil {
		return fmt.Errorf("write person tier thresholds: %w", err)
	}
	return nil
}
