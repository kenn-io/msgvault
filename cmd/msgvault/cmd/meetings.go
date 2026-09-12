package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const meetingsMinAPISchemaVersion = "2.25.0"

type meetingCommandClient interface {
	mcpserver.MeetingBackend
	APISchemaVersion(ctx context.Context) (string, error)
}

type meetingCommandDeps struct {
	open func(context.Context) (meetingCommandClient, func(), error)
}

func defaultMeetingCommandDeps() meetingCommandDeps {
	return meetingCommandDeps{open: func(ctx context.Context) (meetingCommandClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open daemon: %w", err)
		}
		return client, func() { _ = client.Close() }, nil
	}}
}

func newMeetingsCommand(deps meetingCommandDeps) *cobra.Command {
	command := &cobra.Command{
		Use:   "meetings",
		Short: "Read archived meeting context, actions, and metrics",
	}
	command.AddCommand(
		newMeetingContextCommand(deps),
		newMeetingActionsCommand(deps),
		newMeetingMetricsCommand(deps),
	)
	return command
}

func openMeetingCommandClient(
	ctx context.Context,
	deps meetingCommandDeps,
) (meetingCommandClient, func(), error) {
	if deps.open == nil {
		return nil, func() {}, errors.New("meeting daemon client is unavailable")
	}
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
		return nil, func() {}, fmt.Errorf("check daemon meeting capability: %w", err)
	}
	if !daemonclient.APISchemaVersionAtLeast(version, meetingsMinAPISchemaVersion) {
		cleanup()
		return nil, func() {}, fmt.Errorf(
			"meeting commands require daemon API schema %s or newer (daemon reports %q); upgrade the daemon",
			meetingsMinAPISchemaVersion, version,
		)
	}
	return client, cleanup, nil
}

func newMeetingContextCommand(deps meetingCommandDeps) *cobra.Command {
	var messageIDs []int64
	var format string
	var includeTranscript bool
	var maxBytes int64
	var output string
	command := &cobra.Command{
		Use:   "context",
		Short: "Export deterministic context for selected meetings",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := validateMeetingIDs("--id", messageIDs, true); err != nil {
				return usageErr(command, err)
			}
			format = strings.ToLower(strings.TrimSpace(format))
			if format != "json" && format != "markdown" {
				return usageErr(command, errors.New("--format must be json or markdown"))
			}
			if maxBytes < 4096 || maxBytes > 1048576 {
				return usageErr(command, errors.New("--max-bytes must be between 4096 and 1048576"))
			}
			client, cleanup, err := openMeetingCommandClient(command.Context(), deps)
			if err != nil {
				return err
			}
			defer cleanup()
			requestFormat := generated.MeetingContextRequestFormat(format)
			result, err := client.GetMeetingContext(command.Context(), generated.GetMeetingContextBody{
				MessageIds: &messageIDs, Format: &requestFormat,
				IncludeTranscript: &includeTranscript, MaxBytes: &maxBytes,
			})
			if err != nil {
				return meetingCommandError(err)
			}
			if result == nil {
				return errors.New("meeting context response was empty")
			}
			return writeMeetingContext(command, output, result.Content)
		},
	}
	command.Flags().Int64SliceVar(&messageIDs, "id", nil, "Meeting message ID (repeatable)")
	command.Flags().StringVar(&format, "format", "markdown", "Context format: json or markdown")
	command.Flags().BoolVar(&includeTranscript, "include-transcript", false, "Include archived transcript evidence")
	command.Flags().Int64Var(&maxBytes, "max-bytes", 131072, "Maximum UTF-8 content bytes")
	command.Flags().StringVarP(&output, "output", "o", "", "Output file (default stdout, use - for stdout)")
	return command
}

func writeMeetingContext(command *cobra.Command, output, content string) error {
	if output == "" || output == "-" {
		if _, err := io.WriteString(command.OutOrStdout(), content); err != nil {
			return fmt.Errorf("write meeting context: %w", err)
		}
		return nil
	}
	if _, err := writeAttachmentStreamToFile(output, strings.NewReader(content)); err != nil {
		return fmt.Errorf("write meeting context: %w", err)
	}
	if _, err := fmt.Fprintf(command.ErrOrStderr(), "Wrote meeting context to %s\n", output); err != nil {
		return fmt.Errorf("write meeting context notice: %w", err)
	}
	return nil
}

type meetingScopeFlags struct {
	messageIDs     []int64
	sourceIDs      []int64
	domains        []string
	participantIDs []int64
	personID       int64
	after          string
	before         string
	deletion       string
}

func addMeetingScopeFlags(command *cobra.Command, scope *meetingScopeFlags) {
	command.Flags().Int64SliceVar(&scope.messageIDs, "id", nil, "Meeting message ID (repeatable)")
	command.Flags().Int64SliceVar(&scope.sourceIDs, "source-id", nil, "Source ID (repeatable)")
	command.Flags().StringSliceVar(&scope.domains, "domain", nil, "Exact participant domain (repeatable)")
	command.Flags().Int64SliceVar(&scope.participantIDs, "participant-id", nil, "Exact participant ID (repeatable)")
	command.Flags().Int64Var(&scope.personID, "person-id", 0, "Durable person ID")
	command.Flags().StringVar(&scope.after, "after", "", "Meetings on or after YYYY-MM-DD")
	command.Flags().StringVar(&scope.before, "before", "", "Meetings before YYYY-MM-DD")
	command.Flags().StringVar(&scope.deletion, "deletion", "any", "Source deletion state: any, active, or deleted")
	command.MarkFlagsMutuallyExclusive("person-id", "participant-id")
}

func (flags *meetingScopeFlags) request(command *cobra.Command) (*generated.MeetingScopeRequest, error) {
	if err := validateMeetingIDs("--id", flags.messageIDs, false); err != nil {
		return nil, err
	}
	if err := validateMeetingIDs("--source-id", flags.sourceIDs, false); err != nil {
		return nil, err
	}
	if err := validateMeetingIDs("--participant-id", flags.participantIDs, false); err != nil {
		return nil, err
	}
	if flags.personID < 0 || flags.personID > int64(maxJSONSafeInteger) {
		return nil, errors.New("--person-id must be a positive JavaScript-safe integer")
	}
	if command.Flags().Changed("person-id") && flags.personID == 0 {
		return nil, errors.New("--person-id must be positive")
	}
	if len(flags.domains) > 100 {
		return nil, errors.New("--domain accepts at most 100 values")
	}
	for i := range flags.domains {
		flags.domains[i] = strings.TrimSpace(flags.domains[i])
		if flags.domains[i] == "" {
			return nil, errors.New("--domain values must be nonempty")
		}
	}
	after, err := parseMeetingCLIDate(flags.after)
	if err != nil {
		return nil, fmt.Errorf("invalid --after: %w", err)
	}
	before, err := parseMeetingCLIDate(flags.before)
	if err != nil {
		return nil, fmt.Errorf("invalid --before: %w", err)
	}
	if after != nil && before != nil && !after.Before(*before) {
		return nil, errors.New("--after must be before --before")
	}
	flags.deletion = strings.ToLower(strings.TrimSpace(flags.deletion))
	if flags.deletion != "any" && flags.deletion != "active" && flags.deletion != "deleted" {
		return nil, errors.New("--deletion must be any, active, or deleted")
	}
	deletion := generated.MeetingScopeRequestDeletion(flags.deletion)
	scope := &generated.MeetingScopeRequest{
		SourceIds: flags.sourceIDs, ParticipantIds: flags.participantIDs,
		Domains: flags.domains, After: after, Before: before, Deletion: &deletion,
	}
	if len(flags.messageIDs) > 0 {
		scope.MessageIds = &flags.messageIDs
	}
	if command.Flags().Changed("person-id") {
		scope.PersonID = &flags.personID
	}
	return scope, nil
}

const maxJSONSafeInteger = 9007199254740991

func validateMeetingIDs(name string, ids []int64, required bool) error {
	if required && len(ids) == 0 {
		return fmt.Errorf("at least one %s is required", name)
	}
	if len(ids) > 100 {
		return fmt.Errorf("%s accepts at most 100 values", name)
	}
	for _, id := range ids {
		if id < 1 || id > maxJSONSafeInteger {
			return fmt.Errorf("%s values must be positive JavaScript-safe integers", name)
		}
	}
	return nil
}

func parseMeetingCLIDate(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil //nolint:nilnil // an omitted date is intentionally unbounded
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return nil, errors.New("expected YYYY-MM-DD")
	}
	return &parsed, nil
}

func newMeetingActionsCommand(deps meetingCommandDeps) *cobra.Command {
	var scopeFlags meetingScopeFlags
	var assignee, status, queryText, cursor string
	var limit int64
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "actions",
		Short: "List archived meeting action evidence",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			scope, err := scopeFlags.request(command)
			if err != nil {
				return usageErr(command, err)
			}
			if limit < 1 || limit > 200 {
				return usageErr(command, errors.New("--limit must be between 1 and 200"))
			}
			status = strings.ToLower(strings.TrimSpace(status))
			if status != "" && status != "pending" && status != "completed" && status != "cancelled" && status != "unknown" {
				return usageErr(command, errors.New("--status must be pending, completed, cancelled, or unknown"))
			}
			queryText = strings.TrimSpace(queryText)
			if utf8.RuneCountInString(queryText) > 256 {
				return usageErr(command, errors.New("--query must be at most 256 characters"))
			}
			client, cleanup, err := openMeetingCommandClient(command.Context(), deps)
			if err != nil {
				return err
			}
			defer cleanup()
			body := generated.ListMeetingActionItemsBody{Scope: scope, Limit: &limit}
			if assignee = strings.TrimSpace(assignee); assignee != "" {
				body.AssigneeEmail = &assignee
			}
			if status != "" {
				value := generated.MeetingActionsRequestStatus(status)
				body.Status = &value
			}
			if queryText != "" {
				body.Query = &queryText
			}
			if cursor = strings.TrimSpace(cursor); cursor != "" {
				body.Cursor = &cursor
			}
			result, err := client.ListMeetingActionItems(command.Context(), body)
			if err != nil {
				return meetingCommandError(err)
			}
			if result == nil {
				return errors.New("meeting actions response was empty")
			}
			if jsonOutput {
				return writeMeetingJSON(command, result)
			}
			return writeMeetingActions(command, result)
		},
	}
	addMeetingScopeFlags(command, &scopeFlags)
	command.Flags().StringVar(&assignee, "assignee", "", "Exact assignee email")
	command.Flags().StringVar(&status, "status", "", "Normalized source status")
	command.Flags().StringVar(&queryText, "query", "", "Literal title or description substring")
	command.Flags().Int64Var(&limit, "limit", 50, "Maximum action rows")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque continuation cursor")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func writeMeetingActions(command *cobra.Command, page *meetingcontent.ActionsPage) error {
	out := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintf(out, "MEETING\tDATE\tSTATUS\tASSIGNEE\tACTION\n"); err != nil {
		return fmt.Errorf("write meeting actions: %w", err)
	}
	for _, row := range page.Rows {
		date := "-"
		if row.Meeting.OccurredAt != nil {
			date = row.Meeting.OccurredAt.UTC().Format("2006-01-02")
		}
		assignee := row.Action.AssigneeEmail
		if assignee == "" {
			assignee = row.Action.AssigneeName
		}
		if assignee == "" {
			assignee = "-"
		}
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n",
			row.Meeting.Title, date, row.Action.Status, assignee, row.Action.Title); err != nil {
			return fmt.Errorf("write meeting actions: %w", err)
		}
	}
	if _, err := fmt.Fprintf(out,
		"\nTotal actions: %d\tMeetings: %d\tCoverage: %d available, %d partial, %d unsupported, %d unavailable\n",
		page.TotalCount, page.Coverage.MeetingCount, page.Coverage.Available,
		page.Coverage.Partial, page.Coverage.Unsupported, page.Coverage.Unavailable,
	); err != nil {
		return fmt.Errorf("write meeting action totals: %w", err)
	}
	if page.NextCursor != "" {
		if _, err := fmt.Fprintf(out, "Next cursor: %s\n", page.NextCursor); err != nil {
			return fmt.Errorf("write meeting action cursor: %w", err)
		}
	}
	if err := out.Flush(); err != nil {
		return fmt.Errorf("flush meeting actions: %w", err)
	}
	return nil
}

func newMeetingMetricsCommand(deps meetingCommandDeps) *cobra.Command {
	var scopeFlags meetingScopeFlags
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "metrics",
		Short: "Show archived meeting duration metrics",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			scope, err := scopeFlags.request(command)
			if err != nil {
				return usageErr(command, err)
			}
			client, cleanup, err := openMeetingCommandClient(command.Context(), deps)
			if err != nil {
				return err
			}
			defer cleanup()
			result, err := client.GetMeetingMetrics(command.Context(), generated.GetMeetingMetricsBody{Scope: scope})
			if err != nil {
				return meetingCommandError(err)
			}
			if result == nil {
				return errors.New("meeting metrics response was empty")
			}
			if jsonOutput {
				return writeMeetingJSON(command, result)
			}
			return writeMeetingMetrics(command, result)
		},
	}
	addMeetingScopeFlags(command, &scopeFlags)
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func writeMeetingMetrics(command *cobra.Command, metrics *meetingcontent.Metrics) error {
	out := command.OutOrStdout()
	if _, err := fmt.Fprintf(out,
		"Meetings: %d\nKnown duration: %d\nUnknown duration: %d\nTotal known seconds: %.0f\n",
		metrics.Totals.MeetingCount, metrics.Totals.KnownDurationCount,
		metrics.Totals.UnknownDurationCount, metrics.Totals.TotalKnownSeconds,
	); err != nil {
		return fmt.Errorf("write meeting metric totals: %w", err)
	}
	if metrics.Totals.AverageKnownSeconds == nil {
		if _, err := fmt.Fprintln(out, "Average known seconds: unavailable"); err != nil {
			return fmt.Errorf("write meeting metric average: %w", err)
		}
	} else if _, err := fmt.Fprintf(out, "Average known seconds: %.0f\n", *metrics.Totals.AverageKnownSeconds); err != nil {
		return fmt.Errorf("write meeting metric average: %w", err)
	}
	if _, err := fmt.Fprintln(out, "\nDuration by basis:"); err != nil {
		return fmt.Errorf("write meeting duration heading: %w", err)
	}
	for _, basis := range metrics.DurationByBasis {
		if _, err := fmt.Fprintf(out, "  %s: %d meetings, %.0f seconds\n", basis.Basis, basis.Count, basis.TotalSeconds); err != nil {
			return fmt.Errorf("write meeting duration basis: %w", err)
		}
	}
	if _, err := fmt.Fprintln(out, "\nMonths:"); err != nil {
		return fmt.Errorf("write meeting month heading: %w", err)
	}
	for _, month := range metrics.Months {
		average := "unavailable"
		if month.Totals.AverageKnownSeconds != nil {
			average = fmt.Sprintf("%.0f", *month.Totals.AverageKnownSeconds)
		}
		if _, err := fmt.Fprintf(out,
			"  %s: %d meetings, %d known, %d unknown, %.0f total seconds, %s average seconds\n",
			month.Month, month.Totals.MeetingCount, month.Totals.KnownDurationCount,
			month.Totals.UnknownDurationCount, month.Totals.TotalKnownSeconds, average,
		); err != nil {
			return fmt.Errorf("write meeting month: %w", err)
		}
	}
	return nil
}

func writeMeetingJSON(command *cobra.Command, value any) error {
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write meeting JSON: %w", err)
	}
	return nil
}

func meetingCommandError(err error) error {
	var apiErr *daemonclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Status {
	case http.StatusNotFound:
		if apiErr.APIErrorCode() != "not_found" {
			return err
		}
		return fmt.Errorf(
			"meeting intelligence is unavailable from this daemon; upgrade it to API schema %s or newer: %w",
			meetingsMinAPISchemaVersion, err,
		)
	case http.StatusServiceUnavailable:
		return fmt.Errorf("meeting intelligence is unavailable: %w", err)
	default:
		return err
	}
}

func init() {
	rootCmd.AddCommand(newMeetingsCommand(defaultMeetingCommandDeps()))
}
