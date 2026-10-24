package cmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/textutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const loopsMinAPISchemaVersion = "3.25.0"

func init() {
	rootCmd.AddCommand(newLoopsCommand())
}

func newLoopsCommand() *cobra.Command {
	group := &cobra.Command{
		Use:   "loops",
		Short: "List open loops",
		Long: `Show who is waiting on a reply from you and who you are waiting on.

Each person gets a separate loop anchored on their oldest open question or
request. A reply you owe lists after 24 hours, and a reply you are waiting on
lists after 72 hours. Each list reads the archive as it is. Quiet conversations
stay eligible at any age; snooze reminders remain eligible after expiry.
The wording rules read English only.`,
	}
	group.AddCommand(newLoopsListCommand(), newLoopsDoneCommand(), newLoopsSnoozeCommand(), newLoopsReopenCommand())
	return group
}

func newLoopsListCommand() *cobra.Command {
	var opts daemonclient.LoopListOptions
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List conversations waiting on replies. Only a question mark or a request phrase opens a loop.",
		Args:  cobra.NoArgs,
		Example: `  msgvault loops list
  msgvault loops list --limit 10 --json
  msgvault loops list --person 42`,
	}
	command.Flags().IntVar(&opts.Limit, "limit", loops.DefaultLimit, "Maximum loops (1 through 200)")
	command.Flags().Int64Var(&opts.PersonID, "person", 0, "Show loops with this person ID")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	command.RunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("person") && opts.PersonID <= 0 {
			return usageErr(cmd, errors.New("--person must be a positive person ID"))
		}
		if opts.Limit < 1 || opts.Limit > loops.MaxLimit {
			return usageErr(cmd, fmt.Errorf("--limit must be from 1 through %d", loops.MaxLimit))
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			result, err := client.ListLoops(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSONOutput(cmd, result)
			}
			return writeLoops(cmd.OutOrStdout(), result, opts.Limit, time.Now())
		})
	}
	return command
}

func newLoopsDoneCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "done <message-id>",
		Short: "Mark a loop done",
		Long: `Mark a loop done. The message ID is the last column of 'msgvault loops list'.
A distinct question can list again, whatever its message date. Use 'reopen' to
clear this question's done decision.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(cmd, args[0], "message")
			if err != nil {
				return err
			}
			return withLoopsClient(cmd, func(client *daemonclient.Client) error {
				if _, err := client.MarkLoopDone(cmd.Context(), id); err != nil {
					return loopActionError(id, err)
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Marked %d done.\n", id); err != nil {
					return fmt.Errorf("write loop result: %w", err)
				}
				return nil
			})
		},
	}
}

func newLoopsSnoozeCommand() *cobra.Command {
	var until string
	command := &cobra.Command{
		Use:   "snooze <message-id> --until <when>",
		Short: "Hide a loop until a later time",
		Long: `Hide a loop until a later time. The loop returns once the time passes, or
sooner when a distinct question arrives. Decisions apply to this person's
currently open questions. Use the current message ID from loops list.`,
		Example: `  msgvault loops snooze 4521 --until 48h
  msgvault loops snooze 4521 --until 2026-10-25`,
		Args: cobra.ExactArgs(1),
	}
	command.Flags().StringVar(&until, "until", "", "A duration such as 48h, a date such as 2026-10-25 (local midnight), or an RFC3339 time")
	_ = command.MarkFlagRequired("until")
	command.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "message")
		if err != nil {
			return err
		}
		at, err := loops.ParseSnoozeUntil(until, time.Now())
		if err != nil {
			return usageErr(cmd, err)
		}
		return withLoopsClient(cmd, func(client *daemonclient.Client) error {
			decision, err := client.SnoozeLoop(cmd.Context(), id, at)
			if err != nil {
				return loopActionError(id, err)
			}
			result := fmt.Sprintf("Snoozed %d until %s.\n", id, at.Local().Format("Mon Jan 2 3:04 PM MST"))
			if decision.State == generated.Done {
				result = fmt.Sprintf("%d is done and stays done.\n", id)
			}
			if _, err := fmt.Fprint(cmd.OutOrStdout(), result); err != nil {
				return fmt.Errorf("write loop result: %w", err)
			}
			return nil
		})
	}
	return command
}

func newLoopsReopenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <message-id>",
		Short: "Clear a done or snooze decision",
		Long:  "Clear the done or snooze decision for one exact question. The message ID is the last column of 'msgvault loops list'.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(cmd, args[0], "message")
			if err != nil {
				return err
			}
			return withLoopsClient(cmd, func(client *daemonclient.Client) error {
				if _, err := client.ReopenLoop(cmd.Context(), id); err != nil {
					if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok && apiErr.Code == daemonclient.LoopDecisionNotFoundCode {
						return errors.New(strconv.FormatInt(id, 10) + " has no done or snooze decision.")
					}
					return err
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Reopened %d.\n", id); err != nil {
					return fmt.Errorf("write loop result: %w", err)
				}
				return nil
			})
		},
	}
}

func loopActionError(id int64, err error) error {
	if apiErr, ok := errors.AsType[*daemonclient.APIError](err); ok && apiErr.Code == daemonclient.LoopNotFoundCode {
		return errors.New(strconv.FormatInt(id, 10) + " isn't an open loop.")
	}
	return err
}

// withLoopsClient opens the daemon and refuses one that predates the loop routes.
func withLoopsClient(cmd *cobra.Command, run func(*daemonclient.Client) error) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	version, err := client.APISchemaVersion(cmd.Context())
	if err != nil {
		return fmt.Errorf("check daemon open loops capability: %w", err)
	}
	if !daemonclient.APISchemaVersionAtLeast(version, loopsMinAPISchemaVersion) {
		return fmt.Errorf("open loops require daemon API schema %s or newer (daemon reports %q); upgrade and restart the daemon",
			loopsMinAPISchemaVersion, version)
	}
	return run(client)
}

type loopRow struct{ age, with, subject, id string }

func writeLoops(w io.Writer, result generated.LoopListResponse, limit int, now time.Time) error {
	sections := []struct {
		title, kind string
		rows        []loopRow
	}{{title: "Waiting on you", kind: loops.ReplyOwed}, {title: "Waiting on them", kind: loops.AwaitingReply}}
	if len(result.Items) == 0 {
		var sb strings.Builder
		sb.WriteString("No open loops.\n")
		writeLoopWarnings(&sb, result.SkippedUnparseable, result.CoverageIncomplete)
		if _, err := io.WriteString(w, sb.String()); err != nil {
			return fmt.Errorf("write loops: %w", err)
		}
		return nil
	}
	var ageWidth, withWidth, subjectWidth int
	for _, item := range result.Items {
		row := loopRow{
			age:     loopAge(now.Sub(item.OwedSinceAt)),
			with:    loopWith(item.Counterparty),
			subject: loopSubject(item),
			id:      strconv.FormatInt(item.AnchorMessageID, 10),
		}
		ageWidth, withWidth = max(ageWidth, utf8.RuneCountInString(row.age)), max(withWidth, utf8.RuneCountInString(row.with))
		subjectWidth = max(subjectWidth, utf8.RuneCountInString(row.subject))
		for i := range sections {
			if string(item.Kind) == sections[i].kind {
				sections[i].rows = append(sections[i].rows, row)
			}
		}
	}
	var sb strings.Builder
	pad := func(s string, width int) string {
		return s + strings.Repeat(" ", width-utf8.RuneCountInString(s))
	}
	counts := make([]int, len(sections))
	for i, section := range sections {
		counts[i] = len(section.rows)
		if len(section.rows) == 0 {
			continue
		}
		sb.WriteString(section.title + "\n")
		for _, row := range section.rows {
			fmt.Fprintf(&sb, "  %s   %s   %s   %s\n", pad(row.age, ageWidth), pad(row.with, withWidth), pad(row.subject, subjectWidth), row.id)
		}
	}
	if result.Truncated {
		// The page holds the oldest loops, so its counts are not the totals.
		if limit < loops.MaxLimit {
			fmt.Fprintf(&sb, "\nShowing the oldest %d. Use --limit up to %d to see more.\n", limit, loops.MaxLimit)
		} else {
			fmt.Fprintf(&sb, "\nShowing the oldest %d. Results are capped at %d.\n", limit, loops.MaxLimit)
		}
	} else {
		fmt.Fprintf(&sb, "\n%d waiting on you, %d waiting on them.\n", counts[0], counts[1])
	}
	writeLoopWarnings(&sb, result.SkippedUnparseable, result.CoverageIncomplete)
	if _, err := io.WriteString(w, sb.String()); err != nil {
		return fmt.Errorf("write loops: %w", err)
	}
	return nil
}

// writeLoopWarnings reports unreadable originals and incomplete archive scans.
func writeLoopWarnings(sb *strings.Builder, skipped int64, incomplete bool) {
	switch {
	case skipped == 1:
		sb.WriteString("1 stored message could not be read, so a reply in it may have settled a loop.\n")
	case skipped > 1:
		fmt.Fprintf(sb, "%d stored messages could not be read, so a reply in one may have settled a loop.\n", skipped)
	}
	if incomplete {
		fmt.Fprint(sb, "More history remains. Run the list again to continue checking; these results cover checked conversations.\n")
	}
}

func loopAge(d time.Duration) string {
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", max(int64(d/time.Hour), 0))
	}
	return fmt.Sprintf("%dd", int64(d/(24*time.Hour)))
}

func loopWith(c generated.LoopCounterparty) string {
	name, address := textutil.SanitizeTerminal(c.DisplayName), textutil.SanitizeTerminal(c.Address)
	switch {
	case name != "" && address != "" && name != address:
		return name + " <" + address + ">"
	case name != "":
		return name
	case address != "":
		return address
	}
	return "(unknown)"
}

func loopSubject(item generated.LoopItem) string {
	subject := strings.Join(strings.Fields(textutil.SanitizeTerminal(item.Summary)), " ")
	if string(item.Channel) == "chat" {
		subject = textutil.TruncateRunes(subject, 60)
	}
	if subject == "" {
		return "(no subject)"
	}
	return subject
}
