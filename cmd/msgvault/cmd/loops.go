package cmd

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/textutil"
)

func newLoopsCommand() *cobra.Command {
	group := &cobra.Command{Use: "loops", Short: "Find unanswered turns and manage follow-ups"}
	group.AddCommand(newLoopsWaitingCommand(), newLoopsListCommand(), newLoopsShowCommand(), newLoopsTrackCommand(), newLoopsUntrackCommand(), newLoopsActionCommand("done"), newLoopsActionCommand("snooze"), newLoopsActionCommand("unsnooze"), newLoopsMuteCommand(false), newLoopsMuteCommand(true), newLoopsMutesCommand(), newLoopsStatusCommand())
	for _, name := range []string{"run", "rebuild"} {
		command := &cobra.Command{Use: name, Short: map[string]string{"run": "Request one derivation pass now", "rebuild": "Request a full lookback rescan, preserving owner actions"}[name], Args: cobra.NoArgs}
		command.RunE = func(cmd *cobra.Command, _ []string) error {
			return withLoopsClient(cmd, func(client *daemonclient.Client) error {
				result, err := client.RunLoops(cmd.Context(), name == "rebuild")
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), result.Disposition)
				if err != nil {
					return fmt.Errorf("write loops job disposition: %w", err)
				}
				return nil
			})
		}
		group.AddCommand(command)
	}
	return group
}

func withLoopsClient(cmd *cobra.Command, action func(*daemonclient.Client) error) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return action(client)
}

func newLoopsWaitingCommand() *cobra.Command {
	var opts loops.WaitingOptions
	var asOf string
	var jsonOutput bool
	command := &cobra.Command{Use: "waiting", Short: "Rank people waiting for your reply", Args: cobra.NoArgs}
	command.Flags().IntVar(&opts.Top, "top", 10, "Maximum counterpart groups (1 through 50)")
	command.Flags().BoolVar(&opts.AllowStale, "allow-stale", false, "Show the ranking even when the archive may be stale")
	command.Flags().StringVar(&asOf, "as-of", "", "Evaluate age and freshness at RFC3339 time (current projection; snoozes use current time)")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		opts.AsOf = time.Time{}
		if opts.Top < 1 || opts.Top > 50 {
			return usageErr(cmd, errors.New("--top must be from 1 through 50"))
		}
		if asOf != "" {
			parsed, err := time.Parse(time.RFC3339Nano, asOf)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("--as-of must be RFC3339: %w", err))
			}
			opts.AsOf = parsed
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			result, err := client.GetWaitingLoops(cmd.Context(), opts)
			if err != nil {
				if stale, ok := errors.AsType[*loops.StaleError](err); ok {
					writeLoopsFreshness(cmd, stale.Response.Freshness)
					if jsonOutput {
						if writeErr := writePersonAgendaJSON(cmd, stale.Response); writeErr != nil {
							return writeErr
						}
					}
				}
				return err
			}
			writeLoopsFreshness(cmd, result.Freshness)
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			for _, group := range result.Groups {
				age := int64(max(time.Duration(0), result.Freshness.AsOf.Sub(group.OldestOwedSinceAt)) / time.Second)
				_, _ = fmt.Fprintf(writer, "%s\t%d loop(s)\toldest %s\n", loopCounterpartyName(group.Counterparty), group.LoopCount, loopAge(age))
				_, _ = fmt.Fprintln(writer, "  ID\tSUMMARY\tOWED SINCE\tCHANNEL\tKATA")
				for _, item := range group.Loops {
					_, _ = fmt.Fprintf(writer, "  %d\t%s\t%s\t%s\t%s\n", item.ID, cleanLoopText(item.Summary), item.OwedSinceAt.Format(time.RFC3339), cleanLoopText(item.Channel), loopKataColumn(item))
				}
			}
			if len(result.Groups) == 0 {
				_, _ = fmt.Fprintln(writer, "No people are waiting for your reply.")
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush waiting loops output: %w", err)
			}
			return nil
		})
	}
	return command
}

func newLoopsListCommand() *cobra.Command {
	var opts loops.ListOptions
	var jsonOutput bool
	command := &cobra.Command{Use: "list", Short: "List unanswered turns and their history", Args: cobra.NoArgs}
	command.Flags().StringVar(&opts.Kind, "kind", "", "Loop kind: reply_owed, awaiting_reply, commitment_owed_by_me, commitment_owed_to_me")
	command.Flags().StringVar(&opts.Status, "status", "open", "Loop status: open, held, closed, all")
	command.Flags().Int64Var(&opts.PersonID, "person", 0, "Filter by a positive person ID")
	command.Flags().Int64Var(&opts.SourceID, "source-id", 0, "Filter by a positive source ID")
	command.Flags().BoolVar(&opts.IncludeSnoozed, "include-snoozed", false, "Include snoozed loops")
	command.Flags().IntVar(&opts.Limit, "limit", 50, "Maximum loops (1 through 200)")
	command.Flags().StringVar(&opts.Cursor, "cursor", "", "Opaque cursor from the previous page")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		if opts.Limit < 1 || opts.Limit > 200 {
			return usageErr(cmd, errors.New("--limit must be from 1 through 200"))
		}
		for _, flag := range []string{"person", "source-id"} {
			value := opts.PersonID
			if flag == "source-id" {
				value = opts.SourceID
			}
			if cmd.Flags().Changed(flag) && value <= 0 {
				return usageErr(cmd, fmt.Errorf("--%s must be a positive integer", flag))
			}
		}
		if !slices.Contains([]string{"open", "held", "closed", "all"}, opts.Status) {
			return usageErr(cmd, errors.New("--status must be open, held, closed or all"))
		}
		if !slices.Contains([]string{"", loops.ReplyOwed, loops.AwaitingReply, "commitment_owed_by_me", "commitment_owed_to_me"}, opts.Kind) {
			return usageErr(cmd, errors.New("invalid --kind"))
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			result, err := client.ListOpenLoops(cmd.Context(), opts)
			if err != nil {
				return err
			}
			writeLoopsFreshness(cmd, result.Freshness)
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tPERSON\tKIND\tSTATUS\tOWED SINCE\tCHANNEL\tKATA\tSUMMARY")
			for _, item := range result.Items {
				_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, loopCounterpartyName(item.Counterparty), cleanLoopText(item.Kind), cleanLoopText(item.Status), item.OwedSinceAt.Format(time.RFC3339), cleanLoopText(item.Channel), loopKataColumn(item), cleanLoopText(item.Summary))
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush loops list output: %w", err)
			}
			if result.NextCursor != "" {
				_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Next cursor: %s\n", cleanLoopText(result.NextCursor))
				if err != nil {
					return fmt.Errorf("write loops list cursor: %w", err)
				}
			}
			return nil
		})
	}
	return command
}

func newLoopsShowCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "show <id>", Short: "Show one loop and its archived evidence dates", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := positiveLoopID(cmd, args[0])
		if err != nil {
			return err
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			result, err := client.GetOpenLoop(cmd.Context(), id)
			if err != nil {
				return err
			}
			writeLoopsFreshness(cmd, result.Freshness)
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			return writeLoopDetail(cmd, result.Loop)
		})
	}
	return command
}

// loopsKataMinAPISchemaVersion adds persistent loop issue links.
const loopsKataMinAPISchemaVersion = "3.26.0"

func newLoopsTrackCommand() *cobra.Command {
	var request loops.TrackRequest
	var jsonOutput bool
	command := &cobra.Command{Use: "track <id>", Short: "Track a loop with a new or existing Kata issue", Args: cobra.ExactArgs(1)}
	command.Flags().StringVar(&request.IssueRef, "issue", "", "Existing issue ref; first link adds evidence, replays reuse the cached receipt")
	command.Flags().StringVar(&request.Title, "title", "", "Issue title (default derived from the loop; at most 512 characters)")
	command.Flags().StringVar(&request.List, "list", "", "Person agenda list (default agenda; requires a person)")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := positiveLoopID(cmd, args[0])
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("issue") && (cmd.Flags().Changed("title") || cmd.Flags().Changed("list")) {
			return usageErr(cmd, errors.New("--issue cannot be combined with --title or --list"))
		}
		client, closeClient, err := openKataClient(cmd, loopsKataMinAPISchemaVersion)
		if err != nil {
			return err
		}
		defer closeClient()
		result, err := client.TrackOpenLoop(cmd.Context(), id, request)
		if err != nil {
			if conflict, ok := errors.AsType[*daemonclient.LoopAlreadyTrackedError](err); ok {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Already tracked: %s %s\n", cleanLoopText(conflict.Kata.QualifiedRef), cleanLoopText(conflict.Kata.Status))
			}
			return err
		}
		writeLoopsFreshness(cmd, result.Freshness)
		if result.Note != "" {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), cleanLoopText(result.Note))
		}
		if jsonOutput {
			return writePersonAgendaJSON(cmd, result)
		}
		replay := ""
		if result.Replayed {
			replay = " (already tracked)"
		}
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s%s\n", cleanLoopText(result.Issue.QualifiedRef), cleanLoopText(result.Issue.Status), cleanLoopText(result.Issue.Title), replay); err != nil {
			return fmt.Errorf("write tracked loop issue: %w", err)
		}
		return writeLoopDetail(cmd, result.Loop)
	}
	return command
}
func newLoopsUntrackCommand() *cobra.Command {
	command := &cobra.Command{Use: "untrack <id>", Short: "Remove a loop's Kata issue link", Args: cobra.ExactArgs(1)}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := positiveLoopID(cmd, args[0])
		if err != nil {
			return err
		}
		client, closeClient, err := openKataClient(cmd, loopsKataMinAPISchemaVersion)
		if err != nil {
			return err
		}
		defer closeClient()
		if err = client.UntrackOpenLoop(cmd.Context(), id); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Untracked loop %d.\n", id)
		if err != nil {
			return fmt.Errorf("write untracked loop result: %w", err)
		}
		return nil
	}
	return command
}

func newLoopsActionCommand(action string) *cobra.Command {
	var note, until string
	command := &cobra.Command{Use: action + " <id>", Short: map[string]string{"done": "Mark a loop done", "snooze": "Hide a loop until a future instant", "unsnooze": "Remove a loop's snooze"}[action], Args: cobra.ExactArgs(1)}
	if action == "done" {
		command.Flags().StringVar(&note, "note", "", "Owner note recorded with the completion")
	}
	if action == "snooze" {
		command.Flags().StringVar(&until, "until", "", "Future YYYY-MM-DD (local midnight), RFC3339 timestamp or positive duration")
		_ = command.MarkFlagRequired("until")
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := positiveLoopID(cmd, args[0])
		if err != nil {
			return err
		}
		var at time.Time
		if action == "snooze" {
			at, err = loops.ParseSnoozeUntil(until, time.Now())
			if err != nil {
				return usageErr(cmd, err)
			}
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			var result loops.DetailResponse
			switch action {
			case "done":
				result, err = client.CloseOpenLoop(cmd.Context(), id, note)
			case "snooze":
				result, err = client.SnoozeOpenLoop(cmd.Context(), id, at)
			case "unsnooze":
				result, err = client.UnsnoozeOpenLoop(cmd.Context(), id)
			}
			if err != nil {
				return err
			}
			return writeLoopDetail(cmd, result.Loop)
		})
	}
	return command
}

func newLoopsMuteCommand(unmute bool) *cobra.Command {
	var note string
	var dryRun bool
	name, short := "mute", "Stop deriving loops for a target"
	if unmute {
		name, short = "unmute", "Resume deriving loops for a muted target"
	}
	command := &cobra.Command{Use: name, Short: short}
	if !unmute {
		command.PersistentFlags().StringVar(&note, "note", "", "Reason for muting this target")
		command.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "Preview the number of loops that would close")
	}
	for _, kind := range []string{"person", "participant", "conversation"} {
		child := &cobra.Command{Use: kind + " <id>", Short: short + " " + kind, Args: cobra.ExactArgs(1)}
		child.RunE = func(cmd *cobra.Command, args []string) error {
			id, err := positiveLoopID(cmd, args[0])
			if err != nil {
				return err
			}
			return withLoopsClient(cmd, func(client *daemonclient.Client) error {
				if unmute {
					items, err := client.ListLoopSuppressions(cmd.Context())
					if err != nil {
						return err
					}
					for _, item := range items {
						target := suppressionTarget(item)
						if item.TargetKind == kind && target != nil && *target == id {
							if err := client.DeleteLoopSuppression(cmd.Context(), item.ID); err != nil {
								return err
							}
							_, err = fmt.Fprintf(cmd.OutOrStdout(), "Unmuted %s %d.\n", kind, id)
							if err != nil {
								return fmt.Errorf("write loops unmute result: %w", err)
							}
							return nil
						}
					}
					return fmt.Errorf("no mute found for %s %d", kind, id)
				}
				request := loops.SuppressionRequest{TargetKind: kind, Note: note, DryRun: dryRun}
				switch kind {
				case "person":
					request.PersonID = &id
				case "participant":
					request.ParticipantID = &id
				case "conversation":
					request.ConversationID = &id
				}
				result, err := client.CreateLoopSuppression(cmd.Context(), request)
				if err != nil {
					return err
				}
				if dryRun {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "Would close %d loop(s).\n", result.WouldClose)
				} else {
					_, err = fmt.Fprintf(cmd.OutOrStdout(), "Muted %s %d; closed %d loop(s).\n", kind, id, result.WouldClose)
				}
				if err != nil {
					return fmt.Errorf("write loops mute result: %w", err)
				}
				return nil
			})
		}
		command.AddCommand(child)
	}
	return command
}

func newLoopsMutesCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "mutes", Short: "List muted people, participants and conversations", Args: cobra.NoArgs}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			items, err := client.ListLoopSuppressions(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonAgendaJSON(cmd, loops.SuppressionListResponse{Items: items})
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tTARGET\tTARGET ID\tCREATED\tNOTE")
			for _, item := range items {
				id := "-"
				if target := suppressionTarget(item); target != nil {
					id = strconv.FormatInt(*target, 10)
				}
				_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\n", item.ID, cleanLoopText(item.TargetKind), id, item.CreatedAt.Format(time.RFC3339), cleanLoopText(item.Note))
			}
			if err := writer.Flush(); err != nil {
				return fmt.Errorf("flush loops mutes output: %w", err)
			}
			return nil
		})
	}
	return command
}

func newLoopsStatusCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "status", Short: "Show derivation progress and archive freshness", Args: cobra.NoArgs}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			result, err := client.GetLoopsStatus(cmd.Context())
			if err != nil {
				return err
			}
			writeLoopsFreshness(cmd, result.Freshness)
			if jsonOutput {
				return writePersonAgendaJSON(cmd, result)
			}
			state := result.State
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Applied through: %s\nFeed frontier: %s\nQueue: %d\nSignals backfill complete: %t\nBackfill message: %d\nRescan: %s\nRescan conversation: %d\nLast run: %s\nLast success: %s\nLast error: %s\n", loopTime(state.AppliedThrough), loopTime(state.FeedFrontier), state.QueueDepth, state.SignalsBackfillComplete, state.SignalsBackfillLastMessageID, cleanLoopText(state.RescanReason), state.RescanLastConversationID, loopTime(state.LastRunAt), loopTime(state.LastSuccessAt), cleanLoopText(state.LastError))
			if err != nil {
				return fmt.Errorf("write loops status: %w", err)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Kata last run: %s\nKata last success: %s\nKata last error: %s\n", loopTime(state.KataLastRunAt), loopTime(state.KataLastSuccessAt), cleanLoopText(state.KataLastError)); err != nil {
				return fmt.Errorf("write Kata loop status: %w", err)
			}
			return nil
		})
	}
	return command
}

func positiveLoopID(cmd *cobra.Command, value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageErr(cmd, errors.New("id must be a positive integer"))
	}
	return id, nil
}

func suppressionTarget(item loops.Suppression) *int64 {
	switch item.TargetKind {
	case "person":
		return item.PersonID
	case "participant":
		return item.ParticipantID
	case "conversation":
		return item.ConversationID
	}
	return nil
}

func writeLoopDetail(cmd *cobra.Command, item loops.Loop) error {
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(writer, "Loop %d\t%s\t%s\n%s\nPerson: %s\nOwed since: %s\nLast activity: %s\nChannel: %s\nConversation: %d\nSource: %d\nSnoozed until: %s\n", item.ID, cleanLoopText(item.Kind), cleanLoopText(item.Status), cleanLoopText(item.Summary), loopCounterpartyName(item.Counterparty), item.OwedSinceAt.Format(time.RFC3339), item.LastActivityAt.Format(time.RFC3339), cleanLoopText(item.Channel), item.ConversationID, item.SourceID, loopTime(item.SnoozedUntil))
	if item.ClosedBy != nil {
		suffix := ""
		if *item.ClosedBy == "system" && item.Kata != nil && item.Kata.ClosedLoopAt != nil {
			ref := ""
			ref = item.Kata.QualifiedRef
			if ref == "" {
				for _, evidence := range item.Evidence {
					if evidence.KataClosedRef != "" {
						ref = evidence.KataClosedRef
						break
					}
				}
			}
			if ref != "" {
				suffix = " (Kata " + cleanLoopText(ref) + " closed)"
			}
		}
		_, _ = fmt.Fprintf(writer, "Closed by: %s%s\n", cleanLoopText(*item.ClosedBy), suffix)
	}
	if kata := item.Kata; kata != nil {
		checked := "never checked"
		if kata.CheckedAt != nil {
			checked = "checked " + loopTime(kata.CheckedAt)
		}
		_, _ = fmt.Fprintf(writer, "Kata: %s  %s  %s  %s\nKata live: %t\nKata linked via: %s\nKata linked at: %s\nKata attempts: %d\nKata next attempt: %s\nKata last error: %s\nKata closure notified: %s\nKata closed loop at: %s\n", cleanLoopText(kata.QualifiedRef), cleanLoopText(kata.Status), checked, cleanLoopText(kata.Title), kata.Live, cleanLoopText(kata.LinkedVia), kata.LinkedAt.Format(time.RFC3339), kata.Attempts, loopTime(kata.NextAttemptAt), cleanLoopText(kata.LastError), loopTime(kata.ClosedNotifiedAt), loopTime(kata.ClosedLoopAt))
	}
	for _, evidence := range item.Evidence {
		if evidence.KataReopenBlocked != "" {
			_, _ = fmt.Fprintf(writer, "Kata reopen blocked by loop: %s\n", cleanLoopText(evidence.KataReopenBlocked))
		}
		if evidence.MessageID != 0 {
			_, _ = fmt.Fprintf(writer, "Evidence: message %d\t%s\t%s\n", evidence.MessageID, evidence.OccurredAt.Format(time.RFC3339), cleanLoopText(evidence.Subject))
		}
		if evidence.Quote != "" {
			_, _ = fmt.Fprintf(writer, "  Quote: %s\n", cleanLoopText(evidence.Quote))
		}
		if evidence.OwnerNote != "" {
			_, _ = fmt.Fprintf(writer, "  Owner note: %s\n", cleanLoopText(evidence.OwnerNote))
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush loop detail output: %w", err)
	}
	return nil
}

func writeLoopsFreshness(cmd *cobra.Command, report freshness.Report) {
	if report.Stale {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: archive may be stale (%s)\n", cleanLoopText(strings.Join(report.StaleReasons, ", ")))
		for _, source := range report.Sources {
			if !source.Stale {
				continue
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s: last successful sync %s\n", cleanLoopText(source.Identifier), loopTime(source.LastSuccessfulSyncAt))
			if source.SyncCommand != "" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  Refresh: %s\n", cleanLoopText(source.SyncCommand))
			}
		}
		for name, state := range report.Projections {
			if state != nil && state.Stale {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s: computed %s; refresh: msgvault loops run\n", cleanLoopText(name), loopTime(state.ComputedAt))
			}
		}
	}
	for _, warning := range report.Warnings {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %s\n", cleanLoopText(warning))
	}
}

func loopKataColumn(item loops.Loop) string {
	if item.Kata == nil {
		return "-"
	}
	return cleanLoopText(item.Kata.QualifiedRef) + " " + cleanLoopText(item.Kata.Status)
}

func cleanLoopText(value string) string { return textutil.SanitizeTerminal(value) }
func loopCounterpartyName(counterparty loops.Counterparty) string {
	if counterparty.DisplayName != "" {
		return cleanLoopText(counterparty.DisplayName)
	}
	if counterparty.PersonID != nil {
		return fmt.Sprintf("Person %d", *counterparty.PersonID)
	}
	return fmt.Sprintf("Participant %d", counterparty.ParticipantID)
}
func loopTime(value *time.Time) string {
	if value == nil {
		return "never"
	}
	return value.Format(time.RFC3339)
}
func loopAge(seconds int64) string {
	if seconds >= 86400 {
		return fmt.Sprintf("%dd", seconds/86400)
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}

func init() { rootCmd.AddCommand(newLoopsCommand()) }

// The process may execute the registered tree repeatedly. Reset only loop
// flags during root cleanup, which also runs on parse and argument failures.
func resetLoopsInvocationFlags(root *cobra.Command) {
	var group *cobra.Command
	if root.Name() == "loops" {
		group = root
	} else {
		for _, child := range root.Commands() {
			if child.Name() == "loops" {
				group = child
				break
			}
		}
	}
	if group == nil {
		return
	}
	var reset func(*cobra.Command)
	reset = func(command *cobra.Command) {
		for _, flags := range []*pflag.FlagSet{command.LocalNonPersistentFlags(), command.PersistentFlags()} {
			flags.VisitAll(func(flag *pflag.Flag) {
				_ = flag.Value.Set(flag.DefValue)
				flag.Changed = false
			})
		}
		for _, child := range command.Commands() {
			reset(child)
		}
	}
	reset(group)
}
