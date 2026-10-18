package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
)

const archiveCalendarEventsMinAPISchemaVersion = "3.24.0"

type calendarEventsCommandClient interface {
	mcpserver.CalendarEventsBackend
	APISchemaVersion(ctx context.Context) (string, error)
}
type calendarEventsCommandDeps struct {
	open func(context.Context) (calendarEventsCommandClient, func(), error)
}

func defaultCalendarEventsCommandDeps() calendarEventsCommandDeps {
	return calendarEventsCommandDeps{open: func(ctx context.Context) (calendarEventsCommandClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return client, func() { _ = client.Close() }, nil
	}}
}

func openCalendarEventsClient(ctx context.Context, deps calendarEventsCommandDeps) (calendarEventsCommandClient, func(), error) {
	client, cleanup, err := deps.open(ctx)
	if err != nil {
		return nil, func() {}, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	version, err := client.APISchemaVersion(ctx)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if !daemonclient.APISchemaVersionAtLeast(version, archiveCalendarEventsMinAPISchemaVersion) {
		cleanup()
		return nil, func() {}, fmt.Errorf("archived calendar reads require daemon API schema %s or newer (daemon reports %q); upgrade the daemon", archiveCalendarEventsMinAPISchemaVersion, version)
	}
	return client, cleanup, nil
}

func newCalendarEventsCommand(deps calendarEventsCommandDeps) *cobra.Command {
	var from, to, attendee, cursor string
	var sources []int64
	var includeCancelled, jsonOutput bool
	var limit int
	command := &cobra.Command{Use: "events", Short: "List archived calendar occurrences", Long: "Expand archived calendar series in a window of at most 400 days. Reads use message.read on the calendar sources. --source further narrows the granted sources. Dates and times come from the archive.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start, err := time.Parse(time.RFC3339, from)
			if err != nil {
				return usageErr(cmd, errors.New("--from must be an RFC3339 timestamp"))
			}
			end, err := time.Parse(time.RFC3339, to)
			if err != nil {
				return usageErr(cmd, errors.New("--to must be an RFC3339 timestamp"))
			}
			if !end.After(start) || end.Sub(start) > 400*24*time.Hour {
				return usageErr(cmd, errors.New("window must be positive and at most 400 days"))
			}
			if limit < 1 || limit > 500 {
				return usageErr(cmd, errors.New("--limit must be between 1 and 500"))
			}
			if err := validateMeetingIDs("--source", sources, false); err != nil {
				return usageErr(cmd, err)
			}
			client, cleanup, err := openCalendarEventsClient(cmd.Context(), deps)
			if err != nil {
				return err
			}
			defer cleanup()
			page, err := client.ListCalendarEvents(cmd.Context(), store.CalendarOccurrenceFilter{From: start, To: end, SourceIDs: sources, AttendeeEmail: attendee, IncludeCancelled: includeCancelled, Limit: limit, Cursor: cursor})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeMeetingJSON(cmd, page)
			}
			for _, o := range page.Items {
				if err := writeCalendarOccurrence(cmd, o); err != nil {
					return err
				}
			}
			for _, u := range page.Unexpandable {
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Unexpandable %s: %s\n", u.Ref, u.Reason); err != nil {
					return fmt.Errorf("write unexpandable calendar series: %w", err)
				}
			}
			if page.UnexpandableMore {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "More unexpandable series; narrow the window or sources"); err != nil {
					return fmt.Errorf("write unexpandable calendar series warning: %w", err)
				}
			}
			if page.NextCursor != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Next cursor: %s\n", page.NextCursor)
				if err != nil {
					return fmt.Errorf("write calendar cursor: %w", err)
				}
			}
			return nil
		}}
	command.Flags().StringVar(&from, "from", "", "Inclusive RFC3339 window start (required)")
	command.Flags().StringVar(&to, "to", "", "Exclusive RFC3339 window end (required)")
	command.Flags().Int64SliceVar(&sources, "source", nil, "Archive source ID (repeatable)")
	command.Flags().StringVar(&attendee, "attendee", "", "Exact effective attendee email")
	command.Flags().BoolVar(&includeCancelled, "include-cancelled", false, "Include cancelled occurrences")
	command.Flags().IntVar(&limit, "limit", 100, "Maximum occurrences (1-500)")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque continuation cursor from the same query")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the complete occurrence page as JSON")
	return command
}

func newCalendarEventCommand(deps calendarEventsCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "event <ref>", Short: "Read one archived calendar occurrence", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		kind, _, _, err := store.ParseMeetingRef(args[0])
		if err != nil || kind != "evt" {
			return usageErr(cmd, errors.New("ref must identify a calendar occurrence"))
		}
		client, cleanup, err := openCalendarEventsClient(cmd.Context(), deps)
		if err != nil {
			return err
		}
		defer cleanup()
		occurrence, err := client.GetCalendarEvent(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeMeetingJSON(cmd, occurrence)
		}
		return writeCalendarOccurrence(cmd, occurrence)
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print the archived occurrence as JSON")
	return command
}

func writeCalendarOccurrence(cmd *cobra.Command, o store.CalendarOccurrence) error {
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", o.Ref, o.Start.Format(time.RFC3339), o.Status, o.Subject)
	if err != nil {
		return fmt.Errorf("write calendar occurrence: %w", err)
	}
	return nil
}
