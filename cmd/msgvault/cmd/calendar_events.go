package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const calendarEventsMinAPISchemaVersion = "3.26.0"
const calendarEventDeletedStatus = "deleted"

type calendarEventsClient interface {
	ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error)
	GetCalendarOccurrenceContext(ctx context.Context, ref string) (store.CalendarOccurrenceDetail, error)
	APISchemaVersion(ctx context.Context) (string, error)
}
type calendarEventsOpen func(context.Context) (calendarEventsClient, func(), error)

func openCalendarEvents(ctx context.Context) (calendarEventsClient, func(), error) {
	client, _, err := OpenHTTPStore(ctx)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open daemon: %w", err)
	}
	return client, func() { _ = client.Close() }, nil
}
func newCalendarEventsCommand(open calendarEventsOpen) *cobra.Command {
	var from, to, cursor string
	var limit int
	var jsonOutput bool
	command := &cobra.Command{Use: "events --from <date> --to <date>", Short: "List archived calendar events", Args: cobra.NoArgs}
	command.Flags().StringVar(&from, "from", "", "window start: YYYY-MM-DD or RFC3339")
	command.Flags().StringVar(&to, "to", "", "exclusive window end: YYYY-MM-DD or RFC3339")
	command.Flags().BoolVar(&jsonOutput, "json", false, "print occurrences and attendees as JSON")
	command.Flags().IntVar(&limit, "limit", 100, "maximum occurrences, 1 to 500")
	command.Flags().StringVar(&cursor, "cursor", "", "next cursor from the same window")
	_ = command.MarkFlagRequired("from")
	_ = command.MarkFlagRequired("to")
	bounds := make([]time.Time, 2)
	prepare := func(cmd *cobra.Command) error {
		for i, value := range []string{from, to} {
			var err error
			bounds[i], err = parseCalendarEventsBound(value)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("--%s: use YYYY-MM-DD or RFC3339 with an offset", []string{"from", "to"}[i]))
			}
		}
		if bounds[1].Before(bounds[0]) || bounds[1].Equal(bounds[0]) && bounds[1].Format(time.DateOnly) <= bounds[0].Format(time.DateOnly) {
			return usageErr(cmd, errors.New("--to must be after --from"))
		}
		return nil
	}
	command.RunE = archiveCalendarRun(open, prepare, func(cmd *cobra.Command, _ []string, client calendarEventsClient) error {
		query := store.CalendarOccurrenceQuery{From: bounds[0], To: bounds[1], Cursor: cursor}
		if cmd.Flags().Changed("limit") {
			query.Limit = &limit
		}
		page, err := client.ListCalendarOccurrencesContext(cmd.Context(), query)
		if err != nil {
			var apiError *daemonclient.APIError
			if errors.As(err, &apiError) && apiError.Code == "query_too_broad" {
				return fmt.Errorf("%w%s", store.ErrQueryTooBroad, strings.TrimPrefix(apiError.Message, store.ErrQueryTooBroad.Error()))
			}
			return err
		}
		if jsonOutput {
			data, err := json.Marshal(page)
			if err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			if err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			return nil
		}

		if len(page.Occurrences) == 0 {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "No archived calendar events from %s to %s.\n", from, to); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
		} else {
			if err := printCalendarOccurrences(cmd, page.Occurrences); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next cursor: %s\n", page.NextCursor); err != nil {
				return fmt.Errorf("write calendar cursor: %w", err)
			}
		}
		if err := printCalendarCoverageWarning(cmd, page.Coverage); err != nil {
			return err
		}

		if len(page.Unexpandable) > 0 {
			noun := "events"
			if len(page.Unexpandable) == 1 {
				noun = "event"
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%d %s could not be expanded: ", len(page.Unexpandable), noun); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
			for i, event := range page.Unexpandable {
				if i > 0 {
					if _, err := fmt.Fprint(cmd.ErrOrStderr(), ", "); err != nil {
						return fmt.Errorf("write calendar events: %w", err)
					}
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "message %d (%s)", event.MessageID, event.Reason); err != nil {
					return fmt.Errorf("write calendar events: %w", err)
				}
			}
			if _, err := fmt.Fprintln(cmd.ErrOrStderr()); err != nil {
				return fmt.Errorf("write calendar events: %w", err)
			}
		}
		return nil
	})
	return command
}

func parseCalendarEventsBound(value string) (time.Time, error) {
	layout := time.RFC3339
	if len(value) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	parsed, err := time.Parse(layout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse calendar bound: %w", err)
	}
	if layout == time.RFC3339 {
		return parsed, nil
	}
	boundary, exists, err := calrecur.CivilDateBoundary(value, time.Local)
	if err != nil || exists && boundary.Format(time.TimeOnly) == "00:00:00" {
		return boundary, err
	}
	// Send midnight at the resolved instant so all-day filtering keeps the
	// requested civil date even when the local clock skips midnight or a day.
	return boundary.In(time.FixedZone("", int(parsed.Sub(boundary)/time.Second))), nil
}

func calendarOccurrenceStatus(o store.CalendarOccurrence) string {
	if o.DeletedFromSource {
		return calendarEventDeletedStatus
	}
	return o.Status
}

func printCalendarCoverageWarning(cmd *cobra.Command, coverage store.CalendarCoverage) error {
	if coverage.Complete || coverage.Notice == "" {
		return nil
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Coverage: %s\n", textutil.SanitizeTerminal(coverage.Notice)); err != nil {
		return fmt.Errorf("write calendar coverage warning: %w", err)
	}
	for _, source := range coverage.Sources {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "coverage source %d (%s): %s\n", source.SourceID,
			textutil.SanitizeTerminal(source.SourceType), textutil.SanitizeTerminal(source.Status)); err != nil {
			return fmt.Errorf("write calendar coverage source: %w", err)
		}
	}
	return nil
}

func archiveCalendarRun(open calendarEventsOpen, prepare func(*cobra.Command) error, run func(*cobra.Command, []string, calendarEventsClient) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		for _, name := range []string{"account", "dry-run", "read-only", "send-updates"} {
			if cmd.Flags().Changed(name) {
				return usageErr(cmd, fmt.Errorf("--%s is only valid for live calendar commands", name))
			}
		}
		if prepare != nil {
			if err := prepare(cmd); err != nil {
				return err
			}
		}
		client, cleanup, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer cleanup()
		version, err := client.APISchemaVersion(cmd.Context())
		if err != nil {
			return err
		}
		if !daemonclient.APISchemaVersionAtLeast(version, calendarEventsMinAPISchemaVersion) {
			return fmt.Errorf("calendar events require daemon API schema %s or newer", calendarEventsMinAPISchemaVersion)
		}
		return run(cmd, args, client)
	}
}

func printCalendarOccurrences(cmd *cobra.Command, occurrences []store.CalendarOccurrence) error {
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "START\tEND (EXCL)\tSTATUS\tID\tREF\tTITLE"); err != nil {
		return fmt.Errorf("write calendar heading: %w", err)
	}
	for _, o := range occurrences {
		start, end := o.Start.In(time.Local).Format("2006-01-02 15:04"), o.End.In(time.Local).Format("2006-01-02 15:04")
		if o.AllDay {
			start, end = o.StartDate, o.EndDate
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\t%s\n", start, end, calendarOccurrenceStatus(o), o.MessageID, o.Ref, normalizeSearchTableText(o.Subject)); err != nil {
			return fmt.Errorf("write calendar occurrence: %w", err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write calendar occurrences: %w", err)
	}
	return nil
}

func newCalendarEventCommand(open calendarEventsOpen) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "event <ref>", Short: "Read an archived calendar occurrence", Args: cobra.ExactArgs(1)}
	command.Flags().BoolVar(&jsonOutput, "json", false, "print the occurrence as JSON")
	command.RunE = archiveCalendarRun(open, nil, func(cmd *cobra.Command, args []string, client calendarEventsClient) error {
		o, err := client.GetCalendarOccurrenceContext(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if jsonOutput {
			data, err := json.Marshal(o)
			if err != nil {
				return fmt.Errorf("write calendar event: %w", err)
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(data)); err != nil {
				return fmt.Errorf("write calendar event: %w", err)
			}
			return nil
		}

		if err := printCalendarOccurrences(cmd, []store.CalendarOccurrence{o.CalendarOccurrence}); err != nil {
			return err
		}
		for _, field := range []struct{ label, value string }{{"location", textutil.SanitizeTerminal(o.Location)}, {"description", textutil.SanitizeTerminalMultiline(o.Description)}, {"join URL", textutil.SanitizeTerminal(o.JoinURL)}} {
			if field.value != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", field.label, field.value); err != nil {
					return fmt.Errorf("write calendar content: %w", err)
				}
			}
		}
		for _, a := range o.Attendees {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %s\n", textutil.SanitizeTerminal(a.Email), textutil.SanitizeTerminal(a.DisplayName), textutil.SanitizeTerminal(a.Role), textutil.SanitizeTerminal(a.RSVP)); err != nil {
				return fmt.Errorf("write calendar attendee: %w", err)
			}
		}
		return nil
	})
	return command
}
