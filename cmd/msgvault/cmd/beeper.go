package cmd

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/beeper"
)

const (
	beeperTrueValue        = "true"
	beeperArchiveOperation = "archive"
	beeperLabelsOperation  = "labels"
	beeperTriageOperation  = "triage"
)

// beeperBool accepts literal true/false only; pflag aliases would widen explicit write controls.
type beeperBool bool

func (v *beeperBool) String() string {
	if *v {
		return beeperTrueValue
	}
	return "false"
}
func (v *beeperBool) Type() string { return "bool" }
func (v *beeperBool) Set(s string) error {
	if s != beeperTrueValue && s != "false" {
		return errors.New("expected true or false")
	}
	*v = s == beeperTrueValue
	return nil
}
func beeperBoolFlag(f *pflag.FlagSet, target *beeperBool, name, help string) {
	f.Var(target, name, help)
	f.Lookup(name).NoOptDefVal = beeperTrueValue
}

type beeperIntent struct {
	Operation                                                                                  string
	SourceID, ConversationID, After, Revision                                                  int64
	Limit                                                                                      int
	Category, Observed, Stamp, Head, Lower, Policy, CatalogCursor                              string
	Apply, Clear, NeedsAnswer, Refresh, AsJSON, ResolveHold, Reconcile, InitialReview, Catalog beeperBool
	RequiredDraftID                                                                            string
	RequiredDraftRevision                                                                      int64
	Bounds                                                                                     beeper.ContextBounds
	Flags                                                                                      *pflag.FlagSet
}

func beeperFlagSet(op string, intent *beeperIntent) *pflag.FlagSet {
	f := pflag.NewFlagSet("beeper "+op, pflag.ContinueOnError)
	f.SetOutput(io.Discard)
	intent.Operation = op
	intent.Flags = f
	intent.Bounds = beeper.DefaultContextBounds()
	intent.Policy = "retain-actionable"
	beeperBoolFlag(f, &intent.AsJSON, "json", "Output is always JSON")
	switch op {
	case beeperLabelsOperation, beeperTriageOperation:
		f.Int64Var(&intent.SourceID, "source-id", 0, "Exact Beeper source ID")
	default:
		f.Int64Var(&intent.ConversationID, "conversation", 0, "Exact archived conversation ID")
	}
	switch op {
	case beeperTriageOperation:
		f.Int64Var(&intent.After, "after", 0, "Continue after this archive conversation ID")
		f.IntVar(&intent.Limit, "limit", 50, "Local conversation page size (1–200); unavailable with --catalog")
		beeperBoolFlag(f, &intent.NeedsAnswer, "needs-answer", "Show answer-needed across all categories")
		beeperBoolFlag(f, &intent.Refresh, "refresh", "Read native state per row")
		beeperBoolFlag(f, &intent.Catalog, "catalog", "One native chat catalog page")
		f.StringVar(&intent.CatalogCursor, "catalog-cursor", "", "Opaque native catalog continuation")
	case "classify":
		f.StringVar(&intent.Category, "category", "", "msgvault category: todo/waiting/delegated/reference")
		beeperBoolFlag(f, &intent.NeedsAnswer, "needs-answer", "Independent answer-needed state")
		beeperBoolFlag(f, &intent.Clear, "clear", "Reset local classification while preserving unknown archive hold")
		f.StringVar(&intent.Stamp, "context-stamp", "", "Stamp of the reviewed live packet")
		f.StringVar(&intent.Head, "reviewed-head-json", "", "Reviewed native head metadata")
		fallthrough
	case beeperArchiveOperation:
		f.Int64Var(&intent.Revision, "revision", 0, "Expected classification revision (0 means absent)")
		f.StringVar(&intent.Observed, "observed-activity", "", "Reviewed native activity, RFC3339Nano")
		beeperBoolFlag(f, &intent.Apply, "apply", "")
		if op == beeperArchiveOperation {
			f.StringVar(&intent.RequiredDraftID, "required-draft-id", "", "Managed draft that must still occupy the native composer")
			f.Int64Var(&intent.RequiredDraftRevision, "required-draft-revision", 0, "Exact required managed draft revision")
			f.StringVar(&intent.Policy, "policy", "retain-actionable", "retain-actionable or archive-selected")
			beeperBoolFlag(f, &intent.ResolveHold, "resolve-hold", "Acknowledge uncertainty only; never sends archive")
			beeperBoolFlag(f, &intent.Reconcile, "reconcile", "Confirm current state only; can never send archive")
		}
	}
	if op == "context" || op == beeperArchiveOperation {
		f.IntVar(&intent.Bounds.InitialMessages, "initial-context-messages", 50, "Initial authorized recent-event window")
		f.IntVar(&intent.Bounds.MaxEvents, "max-events", 200, "Maximum selected events plus reply ancestors")
		f.IntVar(&intent.Bounds.MaxBytes, "max-bytes", 128<<10, "Maximum context/response bytes")
		f.IntVar(&intent.Bounds.MaxRequests, "max-requests", 64, "Maximum context requests")
		f.DurationVar(&intent.Bounds.Timeout, "timeout", 30*time.Second, "")
		f.StringVar(&intent.Lower, "lower-anchor-json", "", "Original lower anchor for same-scope verification")
		beeperBoolFlag(f, &intent.InitialReview, "initial-review", "Repeat initial review scope")
	}
	return f
}

func newBeeperCommand() *cobra.Command {
	group := &cobra.Command{Use: "beeper", Short: "Review msgvault GTD and native Beeper state"}
	group.Long = "Output is always JSON, including with --json=false."
	for _, op := range []string{beeperLabelsOperation, beeperTriageOperation, "context", "classify", beeperArchiveOperation} {
		intent := new(beeperIntent)
		command := &cobra.Command{Use: op, Short: map[string]string{beeperLabelsOperation: "List native labels", beeperTriageOperation: "List persistent local categories and answer-needed chats", "context": "Capture bounded live context for review", "classify": "Set msgvault-owned classification (preview by default)", beeperArchiveOperation: "Archive one reviewed chat (preview by default)"}[op], Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return runDaemonCLICommandHTTPFromCobra(cmd, nil) }}
		if op == beeperArchiveOperation {
			command.Long = "Preview by default. A message arriving between the final check and write may also be archived."
		}
		command.Flags().AddFlagSet(beeperFlagSet(op, intent))
		group.AddCommand(command)
	}
	return group
}
func init() { rootCmd.AddCommand(newBeeperCommand()) }

func parseBeeperArgs(args []string) (beeperIntent, error) {
	var i beeperIntent
	if len(args) < 2 || args[0] != "beeper" {
		return i, errors.New("expected beeper command")
	}
	switch args[1] {
	case beeperLabelsOperation, beeperTriageOperation, "context", "classify", beeperArchiveOperation:
	default:
		return i, errors.New("unknown Beeper command")
	}
	flags := beeperFlagSet(args[1], &i)
	// The client forwards root logging intent. This in-process route already
	// uses the daemon logger, as do the existing draft command parsers.
	flags.String("log-level", "", "")
	flags.Bool("verbose", false, "")
	flags.Bool("log-sql", false, "")
	flags.Int64("log-sql-slow-ms", 0, "")
	if err := flags.Parse(args[2:]); err != nil {
		return i, fmt.Errorf("parse beeper flags: %w", err)
	}
	if flags.NArg() != 0 {
		return i, errors.New("unexpected Beeper positional arguments")
	}
	if (i.Operation == beeperLabelsOperation || i.Operation == beeperTriageOperation) && i.SourceID <= 0 {
		return i, errors.New("--source-id must be positive")
	}
	if i.Operation != beeperLabelsOperation && i.Operation != beeperTriageOperation && i.ConversationID <= 0 {
		return i, errors.New("--conversation must be positive")
	}
	if i.Revision < 0 || i.After < 0 {
		return i, errors.New("revision/after must not be negative")
	}
	if i.Operation == beeperTriageOperation && bool(i.Catalog) {
		for _, name := range []string{"limit", "refresh", "after", "needs-answer"} {
			if flags.Changed(name) {
				return i, fmt.Errorf("native catalog does not accept the local --%s control", name)
			}
		}
	}
	if i.Operation == beeperTriageOperation && (i.Limit < 1 || i.Limit > 200) {
		return i, errors.New("limit must be 1–200")
	}
	if (i.Operation == "context" || i.Operation == beeperArchiveOperation) && i.InitialReview && i.Lower != "" {
		return i, errors.New("initial-review and lower anchor are exclusive")
	}
	if (i.Operation == "context" || i.Operation == beeperArchiveOperation) && !bool(i.Reconcile) && !bool(i.ResolveHold) {
		if err := i.Bounds.Validate(); err != nil {
			return i, err
		}
	}
	if i.Observed != "" {
		observed, err := time.Parse(time.RFC3339Nano, i.Observed)
		if err != nil {
			return i, errors.New("invalid observed activity")
		}
		i.Observed = observed.UTC().Format(time.RFC3339Nano)
	}
	if i.Operation == "classify" {
		if i.Clear {
			for _, name := range []string{"category", "needs-answer", "observed-activity", "context-stamp", "reviewed-head-json"} {
				if flags.Changed(name) {
					return i, fmt.Errorf("clear cannot combine with %s", name)
				}
			}
		} else if i.Category != "todo" && i.Category != "waiting" && i.Category != "delegated" && i.Category != "reference" {
			return i, errors.New("invalid category")
		}
		if i.Head != "" || i.Stamp != "" {
			if i.Head == "" || len(i.Stamp) != 64 || !flags.Changed("revision") || i.Observed == "" {
				return i, errors.New("review metadata requires head, stamp, revision and observed activity")
			}
		}
	}
	if i.Operation == beeperArchiveOperation {
		if i.Reconcile || i.ResolveHold {
			if slices.ContainsFunc([]string{"initial-context-messages", "max-events", "max-bytes", "max-requests", "timeout", "lower-anchor-json", "initial-review", "required-draft-id", "required-draft-revision"}, flags.Changed) {
				return i, errors.New("reconciliation/hold acknowledgment accepts no review or draft controls")
			}
		}
		if flags.Changed("required-draft-id") != flags.Changed("required-draft-revision") || (flags.Changed("required-draft-id") && (i.RequiredDraftID == "" || i.RequiredDraftRevision < 1)) {
			return i, errors.New("required draft needs ID and positive revision")
		}
		if i.Reconcile {
			if bool(i.ResolveHold) || flags.Changed("policy") || flags.Changed("observed-activity") || !flags.Changed("revision") {
				return i, errors.New("reconcile requires revision and no archive/resolve policy")
			}
			return i, nil
		}
		if i.ResolveHold {
			if !bool(i.Apply) || flags.Changed("policy") || flags.Changed("revision") || flags.Changed("observed-activity") {
				return i, errors.New("resolve-hold requires apply alone")
			}
		} else if !flags.Changed("revision") || i.Observed == "" {
			return i, errors.New("archive requires reviewed revision and observed activity")
		}
		if i.Policy != "retain-actionable" && i.Policy != "archive-selected" {
			return i, errors.New("invalid archive policy")
		}
	}
	return i, nil
}
