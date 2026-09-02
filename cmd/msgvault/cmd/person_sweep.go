package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

const maxPersonSweepHistoryLimit = 200

type personSweepRunner interface {
	Run(ctx context.Context, request peoplesweep.RunRequest) (peoplesweep.RunResult, error)
}

type personSweepCommandStore interface {
	GetPersonTrackingContext(ctx context.Context, personID int64) (*store.PersonTracking, error)
	ListPersonSweepRuns(ctx context.Context, filter peoplesweep.RunFilter) ([]peoplesweep.RunSummary, error)
	ListPersonSweepAttempts(ctx context.Context, filter peoplesweep.AttemptFilter) ([]peoplesweep.AttemptSummary, error)
	PersonSweepOperationalStatus(ctx context.Context) (peoplesweep.OperationalStatus, error)
	BuildPersonFactCatalogContext(ctx context.Context, includeSensitive bool) (personfacts.Catalog, error)
}

type personSweepCommandDeps struct {
	config             func() peoplesweep.Config
	openStore          func() (personSweepCommandStore, func(), error)
	newRunner          func(peoplesweep.Config, personSweepCommandStore) (personSweepRunner, error)
	isDaemonSubprocess func() bool
	lookupEnv          peoplesweep.CredentialLookup
	proxy              func(*cobra.Command, []string, map[string]string) error
}

type personSweepRunOutput struct {
	RunID           string                 `json:"run_id"`
	PeopleAttempted int                    `json:"people_attempted"`
	PeopleSucceeded int                    `json:"people_succeeded"`
	ProjectedWrites int                    `json:"projected_writes"`
	Usage           personSweepUsageOutput `json:"usage"`
}

type personSweepUsageOutput struct {
	Requests              int   `json:"requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	EstimatedCostMicroUSD int64 `json:"estimated_cost_microusd"`
}

type personSweepStatusOutput struct {
	Enabled             bool                     `json:"enabled"`
	Schedule            string                   `json:"schedule"`
	DirtyCount          int                      `json:"dirty_count"`
	LeasedCount         int                      `json:"leased_count"`
	RetryCount          int                      `json:"retry_count"`
	OldestDirtyAt       *time.Time               `json:"oldest_dirty_at,omitempty"`
	JournalHighWater    int64                    `json:"journal_high_water"`
	CursorHighWater     int64                    `json:"cursor_high_water"`
	ProgramFingerprint  string                   `json:"program_fingerprint,omitempty"`
	CatalogFingerprint  string                   `json:"catalog_fingerprint,omitempty"`
	ProviderFingerprint string                   `json:"provider_fingerprint,omitempty"`
	LastFailure         peoplesweep.FailureClass `json:"last_failure,omitempty"`
}

type personSweepHistoryRunOutput struct {
	Kind            peoplesweep.RunKind    `json:"kind"`
	Mode            peoplesweep.RunMode    `json:"mode"`
	Status          peoplesweep.RunStatus  `json:"status"`
	Attempts        int                    `json:"attempts"`
	Successes       int                    `json:"successes"`
	Failures        int                    `json:"failures"`
	ProjectedWrites int                    `json:"projected_writes"`
	Usage           personSweepUsageOutput `json:"usage"`
}

type personSweepHistoryAttemptOutput struct {
	PersonID        int64                     `json:"person_id"`
	Status          peoplesweep.AttemptStatus `json:"status"`
	FailureClass    peoplesweep.FailureClass  `json:"failure_class,omitempty"`
	SeedCount       int                       `json:"seed_count"`
	ContextCount    int                       `json:"context_count"`
	ClaimCount      int                       `json:"claim_count"`
	DecisionCount   int                       `json:"decision_count"`
	ProjectedWrites int                       `json:"projected_writes"`
	Usage           personSweepUsageOutput    `json:"usage"`
	LatencyMS       int64                     `json:"latency_ms"`
}

type personSweepHistoryOutput struct {
	Runs     []personSweepHistoryRunOutput     `json:"runs"`
	Attempts []personSweepHistoryAttemptOutput `json:"attempts"`
}

func defaultPersonSweepCommandDeps() personSweepCommandDeps {
	return personSweepCommandDeps{
		config: func() peoplesweep.Config { return cfg.People.Sweep },
		openStore: func() (personSweepCommandStore, func(), error) {
			return openWritableStoreAndInit()
		},
		newRunner: func(config peoplesweep.Config, commandStore personSweepCommandStore) (personSweepRunner, error) {
			st, ok := commandStore.(*store.Store)
			if !ok {
				return nil, errors.New("people sweep production store is unavailable")
			}
			productionConfig := *cfg
			productionConfig.People.Sweep = config
			return newProductionPersonSweepWorker(&productionConfig, st)
		},
		isDaemonSubprocess: isDaemonCLISubprocess,
		lookupEnv:          os.LookupEnv,
		proxy: func(command *cobra.Command, args []string, env map[string]string) error {
			if len(env) == 0 {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(command, args, env)
		},
	}
}

func newPersonSweepCommand(deps personSweepCommandDeps) *cobra.Command {
	command := &cobra.Command{Use: "sweep", Short: "Run and inspect incremental person maintenance"}
	command.AddCommand(newPersonSweepRunCommand(deps), newPersonSweepStatusCommand(deps),
		newPersonSweepHistoryCommand(deps))
	return command
}

func newPersonSweepRunCommand(deps personSweepCommandDeps) *cobra.Command {
	var personID int64
	var backstop, jsonOutput bool
	var limit int
	command := &cobra.Command{
		Use: "run", Short: "Run bounded person maintenance", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			config := deps.config()
			personSet := command.Flags().Changed("person")
			if personSet && personID <= 0 {
				return usageErr(command, errors.New("person ID must be a positive integer"))
			}
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, personSweepForwardEnv(config, deps.lookupEnv))
			}
			if !config.Enabled {
				return errors.New("people sweep is disabled")
			}
			resolvedLimit := limit
			if !command.Flags().Changed("limit") && resolvedLimit > config.WorkBatchSize {
				resolvedLimit = config.WorkBatchSize
			}
			if resolvedLimit < 1 || resolvedLimit > config.WorkBatchSize {
				return fmt.Errorf("--limit must be between 1 and %d", config.WorkBatchSize)
			}
			commandStore, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			if personSet {
				tracking, trackingErr := commandStore.GetPersonTrackingContext(command.Context(), personID)
				if trackingErr != nil {
					return trackingErr
				}
				if !tracking.Tracked {
					return fmt.Errorf("person %d is not tracked", personID)
				}
			}
			runner, err := deps.newRunner(config, commandStore)
			if err != nil {
				return err
			}
			mode := peoplesweep.RunIncremental
			if backstop {
				mode = peoplesweep.RunBackstop
			}
			result, err := runner.Run(command.Context(), peoplesweep.RunRequest{
				Kind: peoplesweep.RunManual, Mode: mode, PersonID: personID, Limit: resolvedLimit,
			})
			if err != nil {
				return err
			}
			return writePersonSweepRun(command.OutOrStdout(), result, jsonOutput)
		},
	}
	command.Flags().Int64Var(&personID, "person", 0, "Run only the tracked durable person ID")
	command.Flags().BoolVar(&backstop, "backstop", false, "Force the bounded backstop path")
	command.Flags().IntVar(&limit, "limit", 25, "Maximum people to process")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonSweepStatusCommand(deps personSweepCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use: statusValue, Short: "Show redacted person maintenance state", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			config := deps.config()
			commandStore, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			state, err := commandStore.PersonSweepOperationalStatus(command.Context())
			if err != nil {
				return err
			}
			output := personSweepStatusOutput{
				Enabled: config.Enabled, Schedule: config.Schedule, DirtyCount: state.DirtyCount,
				LeasedCount: state.LeasedCount, RetryCount: state.RetryCount,
				OldestDirtyAt: state.OldestDirtyAt, JournalHighWater: state.JournalHighWater,
				CursorHighWater: state.CursorHighWater, LastFailure: state.LastFailure,
			}
			if config.Enabled {
				profile, profileErr := config.Profile()
				if profileErr != nil {
					return profileErr
				}
				catalog, catalogErr := commandStore.BuildPersonFactCatalogContext(command.Context(), profile.AllowSensitive)
				if catalogErr != nil {
					return catalogErr
				}
				output.ProgramFingerprint = peoplesweep.ProgramFingerprint()
				output.CatalogFingerprint = catalog.Fingerprint
				output.ProviderFingerprint = profile.Fingerprint
			}
			return writePersonSweepStatus(command.OutOrStdout(), output, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonSweepHistoryCommand(deps personSweepCommandDeps) *cobra.Command {
	var personID int64
	var limit int
	var jsonOutput bool
	command := &cobra.Command{
		Use: "history", Short: "Show redacted person maintenance history", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			if limit < 1 || limit > maxPersonSweepHistoryLimit {
				return fmt.Errorf("--limit must be between 1 and %d", maxPersonSweepHistoryLimit)
			}
			commandStore, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			runs, err := commandStore.ListPersonSweepRuns(command.Context(), peoplesweep.RunFilter{PersonID: personID, Limit: limit})
			if err != nil {
				return err
			}
			attempts, err := commandStore.ListPersonSweepAttempts(command.Context(), peoplesweep.AttemptFilter{PersonID: personID, Limit: limit})
			if err != nil {
				return err
			}
			return writePersonSweepHistory(command.OutOrStdout(), safePersonSweepHistory(runs, attempts), jsonOutput)
		},
	}
	command.Flags().Int64Var(&personID, "person", 0, "Filter by durable person ID")
	command.Flags().IntVar(&limit, "limit", 20, "Maximum runs and attempts")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func personSweepForwardEnv(
	config peoplesweep.Config, lookup peoplesweep.CredentialLookup,
) map[string]string {
	_, provider, err := config.ActiveProviderConfig()
	if err != nil || provider.Credential != peoplesweep.CredentialEnv || lookup == nil {
		return nil
	}
	value, ok := lookup(provider.CredentialEnv)
	if !ok || value == "" {
		return nil
	}
	return map[string]string{provider.CredentialEnv: value}
}

func writePersonSweepRun(w io.Writer, result peoplesweep.RunResult, jsonOutput bool) error {
	output := personSweepRunOutput{RunID: result.RunID, PeopleAttempted: result.PeopleAttempted,
		PeopleSucceeded: result.PeopleSucceeded, ProjectedWrites: result.ProjectedWrites,
		Usage: personSweepUsage(result.Usage)}
	if jsonOutput {
		return json.NewEncoder(w).Encode(output)
	}
	_, err := fmt.Fprintf(w, "Run %s: attempted=%d succeeded=%d projected_writes=%d\n",
		output.RunID, output.PeopleAttempted, output.PeopleSucceeded, output.ProjectedWrites)
	if err != nil {
		return fmt.Errorf("write person sweep run: %w", err)
	}
	return nil
}

func writePersonSweepStatus(w io.Writer, output personSweepStatusOutput, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(output)
	}
	oldestDirty := "-"
	if output.OldestDirtyAt != nil {
		oldestDirty = output.OldestDirtyAt.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(w,
		"Enabled: %t\nSchedule: %s\nDirty: %d\nLeased: %d\nRetry: %d\nOldest dirty: %s\nJournal high water: %d\nCursor high water: %d\nProgram: %s\nCatalog: %s\nProvider: %s\nLast failure: %s\n",
		output.Enabled, output.Schedule, output.DirtyCount, output.LeasedCount,
		output.RetryCount, oldestDirty, output.JournalHighWater, output.CursorHighWater,
		output.ProgramFingerprint, output.CatalogFingerprint, output.ProviderFingerprint,
		output.LastFailure)
	if err != nil {
		return fmt.Errorf("write person sweep status: %w", err)
	}
	return nil
}

func safePersonSweepHistory(
	runs []peoplesweep.RunSummary, attempts []peoplesweep.AttemptSummary,
) personSweepHistoryOutput {
	output := personSweepHistoryOutput{
		Runs:     make([]personSweepHistoryRunOutput, 0, len(runs)),
		Attempts: make([]personSweepHistoryAttemptOutput, 0, len(attempts)),
	}
	for _, run := range runs {
		output.Runs = append(output.Runs, personSweepHistoryRunOutput{
			Kind: run.Kind, Mode: run.Mode, Status: run.Status, Attempts: run.Attempts,
			Successes: run.Successes, Failures: run.Failures, ProjectedWrites: run.ProjectedWrites,
			Usage: personSweepUsage(run.Usage),
		})
	}
	for _, attempt := range attempts {
		output.Attempts = append(output.Attempts, personSweepHistoryAttemptOutput{
			PersonID: attempt.PersonID, Status: attempt.Status,
			FailureClass: attempt.FailureClass, SeedCount: attempt.SeedCount,
			ContextCount: attempt.ContextCount, ClaimCount: attempt.ClaimCount,
			DecisionCount: attempt.DecisionCount, ProjectedWrites: attempt.ProjectedWrites,
			Usage: personSweepUsage(attempt.Usage), LatencyMS: attempt.Latency.Milliseconds(),
		})
	}
	return output
}

func personSweepUsage(usage peoplesweep.Usage) personSweepUsageOutput {
	return personSweepUsageOutput{
		Requests: usage.Requests, InputTokens: usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		EstimatedCostMicroUSD: usage.EstimatedCostMicroUSD,
	}
}

func writePersonSweepHistory(w io.Writer, output personSweepHistoryOutput, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(output)
	}
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "TYPE\tSTATUS\tMODE\tPERSON\tFAILURE\tATTEMPTS\tWRITES")
	for _, run := range output.Runs {
		_, _ = fmt.Fprintf(table, "run\t%s\t%s\t-\t-\t%d\t%d\n",
			run.Status, run.Mode, run.Attempts, run.ProjectedWrites)
	}
	for _, attempt := range output.Attempts {
		_, _ = fmt.Fprintf(table, "attempt\t%s\t-\t%d\t%s\t-\t%d\n",
			attempt.Status, attempt.PersonID, attempt.FailureClass, attempt.ProjectedWrites)
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write person sweep history: %w", err)
	}
	return nil
}
