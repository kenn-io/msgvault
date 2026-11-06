package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/keepintouch"
	"go.kenn.io/msgvault/internal/textutil"
)

type keepInTouchCommandDeps struct {
	isDaemon func() bool
	proxy    func(*cobra.Command, []string) error
	run      func(context.Context, bool) (keepintouch.Result, error)
}

func defaultKeepInTouchCommandDeps() keepInTouchCommandDeps {
	return keepInTouchCommandDeps{
		isDaemon: isDaemonCLISubprocess,
		proxy: func(command *cobra.Command, args []string) error {
			preview, _ := command.Flags().GetBool("dry-run")
			state := invocationFromContext(command.Context())
			cfg, _ := invocationConfigLogger(state)
			if preview || cfg == nil {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(command, args, personSweepForwardEnv(cfg.People.Sweep, os.LookupEnv))
		},
		run: func(ctx context.Context, preview bool) (keepintouch.Result, error) {
			state := invocationFromContext(ctx)
			cfg, _ := invocationConfigLogger(state)
			if cfg == nil {
				return keepintouch.Result{}, errors.New("configuration is unavailable")
			}
			open := openInitializedStoreForInvocation
			if preview {
				open = openReadOnlyStoreForInvocation
			}
			st, cleanup, err := open(ctx, state)
			if err != nil {
				return keepintouch.Result{}, err
			}
			defer cleanup()
			service, err := newKeepInTouchService(ctx, cfg, st, preview)
			if err != nil {
				return keepintouch.Result{}, err
			}
			return service.Check(ctx, preview)
		},
	}
}
func newPersonKeepInTouchCommand(deps keepInTouchCommandDeps) *cobra.Command {
	root := &cobra.Command{Use: "keep-in-touch", Short: "Prepare recurring person-linked outreach reminders"}
	var preview, jsonOutput bool
	check := &cobra.Command{Use: "check", Short: "Prepare Kata inbox drafts for contacts due a check-in", Args: cobra.NoArgs, RunE: func(command *cobra.Command, args []string) error {
		if !deps.isDaemon() {
			return deps.proxy(command, args)
		}
		result, runErr := deps.run(command.Context(), preview)
		if jsonOutput {
			if err := writeJSONOutput(command, result); err != nil {
				return err
			}
		} else {
			for _, item := range result.Items {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%d\t%s\t%s\t%s\n", item.PersonID, item.Status, item.DueAt.Format("2006-01-02"), textutil.SanitizeTerminal(item.Issue.Ref)); err != nil {
					return fmt.Errorf("write keep-in-touch item: %w", err)
				}
			}
			for _, failure := range result.Failures {
				if _, err := fmt.Fprintf(command.ErrOrStderr(), "person %d: %s\n", failure.PersonID, textutil.SanitizeTerminal(failure.Error)); err != nil {
					return fmt.Errorf("write keep-in-touch failure: %w", err)
				}
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "%d actionable; %d draft attempts; Skipped: %s\n", len(result.Items), result.DraftAttempts, formatKeepInTouchSkipped(result.Skipped)); err != nil {
				return fmt.Errorf("write keep-in-touch summary: %w", err)
			}
		}
		return runErr
	}}
	check.Flags().BoolVar(&preview, "dry-run", false, "Preview cadence without provider calls, Kata access, or writes")
	check.Flags().BoolVar(&jsonOutput, "json", false, "Output the check result as JSON")
	root.AddCommand(check)
	return root
}

func formatKeepInTouchSkipped(skipped map[string]int) string {
	counts := make(map[string]int, len(skipped))
	for reason, count := range skipped {
		if count <= 0 {
			continue
		}
		label := "other"
		switch reason {
		case "not_due":
			label = "not due"
		case "stale":
			label = "outdated"
		case "unknown_cadence":
			label = "no saved cadence"
		case "open_reminder":
			label = "open reminder exists"
		case "draft_limit":
			label = "draft limit reached"
		case "evidence_changed":
			label = "evidence changed"
		case "changed":
			label = "contact changed"
		case "closed_cycle":
			label = "reminder already closed"
		case "already_delivered":
			label = "already notified"
		}
		counts[label] += count
	}
	if len(counts) == 0 {
		return "none"
	}
	labels := make([]string, 0, len(counts))
	for label := range counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, label := range labels {
		parts = append(parts, fmt.Sprintf("%d %s", counts[label], label))
	}
	return strings.Join(parts, "; ")
}
