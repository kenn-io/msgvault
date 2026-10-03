package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/gcal"
)

func init() { rootCmd.AddCommand(newCalendarControlCmd(nil)) }

type calendarRunner func(context.Context, calcontrol.Request) (*calcontrol.Result, error)

func newCalendarControlCmd(run calendarRunner) *cobra.Command {
	var account, sendUpdates string
	var dryRun, readOnly, jsonOutput bool
	root := &cobra.Command{Use: "calendar", Short: "Control live calendar events and query availability", Long: "Create, update, delete, move, or respond to live Google Calendar events through the daemon.\nRequires an explicit --account; calendar IDs and configured aliases select the target calendar.\nWrites require add-calendar --write plus write_calendars in the source configuration.\nGuest changes also require invite_calendars. Notifications default to none."}
	root.PersistentFlags().StringVar(&account, "account", "", "configured calendar source name or OAuth account (required)")
	root.PersistentFlags().StringVar(&sendUpdates, "send-updates", "none", "guest notifications: none, all, or externalOnly")
	root.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "verify access and print the proposed changes without writing")
	root.PersistentFlags().BoolVar(&readOnly, "read-only", false, "forbid every event mutation")
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "print the complete result and archive receipts as JSON")
	for _, action := range []string{"create", "update", "delete", "move", "respond", "freebusy", "conflicts"} {
		var summary, description, location, from, to, tz, scope, original, response, destination string
		var allDay bool
		var attendees, addAttendees, rules, reminders, calendars []string
		use := action + " <calendar-id>"
		argCount := 1
		if action != "create" && !calcontrol.IsRead(action) {
			use += " <event-id>"
			argCount = 2
		}
		command := &cobra.Command{Use: use, Short: "Calendar " + action, Args: cobra.ExactArgs(argCount)}
		if action == "move" {
			command.Use += " [destination-calendar-id]"
			command.Args = cobra.RangeArgs(2, 3)
		}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			request := calcontrol.Request{Action: action, Account: account, CalendarID: args[0], DryRun: dryRun, ReadOnly: readOnly}
			if len(args) > 1 {
				request.EventID = args[1]
			}
			if !calcontrol.IsRead(action) {
				request.SendUpdates = sendUpdates
				request.Scope = scope
				request.OriginalStart = original
				request.Destination = destination
				if action == "move" && len(args) == 3 {
					if cmd.Flags().Changed("destination") {
						return usageErr(cmd, errors.New("use a positional destination or --destination, not both"))
					}
					request.Destination = args[2]
				}
				request.Response = response
			} else {
				if cmd.Flags().Changed("send-updates") {
					return usageErr(cmd, errors.New("--send-updates is only valid for event changes"))
				}
				request.CalendarIDs = calendars
				request.TimeZone = tz
			}
			if cmd.Flags().Changed("summary") {
				request.Event.Summary = &summary
			}
			if cmd.Flags().Changed("description") {
				request.Event.Description = &description
			}
			if cmd.Flags().Changed("location") {
				request.Event.Location = &location
			}
			for _, bound := range []struct {
				flag, value string
				target      **gcal.EventDateTime
			}{{"from", from, &request.Event.Start}, {"to", to, &request.Event.End}} {
				if !cmd.Flags().Changed(bound.flag) {
					continue
				}
				dt, err := parseCalendarBound(bound.value, tz, allDay)
				if err != nil {
					return usageErr(cmd, err)
				}
				if calcontrol.IsRead(action) {
					instant, ok := dt.Instant()
					if !ok {
						return usageErr(cmd, errors.New("invalid availability bound"))
					}
					if bound.flag == "from" {
						request.TimeMin = instant
					} else {
						request.TimeMax = instant
					}
				} else {
					*bound.target = &dt
				}
			}
			if cmd.Flags().Changed("attendees") {
				values := []gcal.Attendee{}
				for _, email := range attendees {
					if email != "" {
						values = append(values, gcal.Attendee{Email: strings.TrimSpace(email)})
					}
				}
				request.Event.Attendees = &values
			}
			request.AddAttendees = addAttendees
			if cmd.Flags().Changed("rrule") {
				values := []string{}
				for _, line := range rules {
					if line != "" {
						if !strings.HasPrefix(line, "RRULE:") {
							line = "RRULE:" + line
						}
						values = append(values, line)
					}
				}
				request.Event.Recurrence = &values
			}
			if cmd.Flags().Changed("reminder") {
				value, err := parseCalendarReminders(reminders)
				if err != nil {
					return usageErr(cmd, err)
				}
				request.Event.Reminders = value
			}
			if cmd.Flags().Changed("tz") && !calcontrol.IsRead(action) && request.Event.Start == nil && request.Event.End == nil {
				return usageErr(cmd, errors.New("--tz requires --from or --to"))
			}
			if err := request.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			runner := run
			if runner == nil {
				runner = func(ctx context.Context, r calcontrol.Request) (*calcontrol.Result, error) {
					client, _, err := OpenHTTPStore(ctx)
					if err != nil {
						return nil, err
					}
					defer func() { _ = client.Close() }()
					return client.ControlCalendar(ctx, r)
				}
			}
			result, err := runner(cmd.Context(), request)
			if err != nil {
				return err
			}
			if result == nil {
				return errors.New("calendar operation returned no result")
			}
			if jsonOutput || result.DryRun || calcontrol.IsRead(action) {
				if err := json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true)); err != nil {
					return err
				}
			} else {
				for _, receipt := range result.Writes {
					archiveStatus := fmt.Sprintf("archive message %d", receipt.MessageID)
					if !receipt.Archived {
						archiveStatus = "not archived: " + receipt.ArchiveError
					}
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s on %s (%s)\n", receipt.Action, receipt.Event.ID, receipt.CalendarID, archiveStatus); err != nil {
						return fmt.Errorf("print calendar receipt: %w", err)
					}
				}
			}
			if result.Error != "" {
				return errors.New(result.Error)
			}
			for _, receipt := range result.Writes {
				if !receipt.Archived {
					return fmt.Errorf("google change completed for %s; archive write failed: %s; sync-calendar to reconcile, do not repeat the mutation", receipt.Event.ID, receipt.ArchiveError)
				}
			}
			return nil
		}
		if action == "create" || action == "update" {
			command.Flags().StringVar(&summary, "summary", "", "event title (explicit empty clears on update)")
			command.Flags().StringVar(&description, "description", "", "event description")
			command.Flags().StringVar(&location, "location", "", "event location")
			command.Flags().BoolVar(&allDay, "all-day", false, "date-only event; --to is the exclusive end date")
			command.Flags().StringSliceVar(&attendees, "attendees", nil, "replace guest email list (comma-separated; empty clears)")
			command.Flags().StringArrayVar(&rules, "rrule", nil, "recurrence RRULE (repeatable; empty clears)")
			command.Flags().StringArrayVar(&reminders, "reminder", nil, "popup:minutes, email:minutes, default, or none (repeatable)")
		}
		if action == "create" || action == "update" || calcontrol.IsRead(action) {
			command.Flags().StringVar(&from, "from", "", "start: RFC3339, or local YYYY-MM-DDTHH:MM with --tz")
			command.Flags().StringVar(&to, "to", "", "end: RFC3339, or local YYYY-MM-DDTHH:MM with --tz")
			command.Flags().StringVar(&tz, "tz", "", "IANA time zone for local times and recurrence")
		}
		if action == "update" {
			command.Flags().StringSliceVar(&addAttendees, "add-attendee", nil, "add guests while preserving existing attendees and RSVP state")
		}
		if action == "update" || action == "delete" || action == "respond" {
			scopeHelp := "recurrence scope: single (default), or all"
			if action == "update" || action == "delete" {
				scopeHelp = "recurrence scope: single (default), future, or all"
			}
			command.Flags().StringVar(&scope, "scope", "", scopeHelp)
			command.Flags().StringVar(&original, "original-start", "", "original occurrence start (RFC3339 or all-day date)")
		}
		if action == "move" {
			command.Flags().StringVar(&destination, "destination", "", "destination calendar ID or configured alias")
		}
		if action == "respond" {
			command.Flags().StringVar(&response, "status", "", "self RSVP: accepted, declined, or tentative")
		}
		if calcontrol.IsRead(action) {
			command.Flags().StringSliceVar(&calendars, "calendars", nil, "calendar IDs or aliases to include (default: positional calendar)")
		}
		root.AddCommand(command)
	}
	return root
}
func parseCalendarBound(value, tz string, allDay bool) (gcal.EventDateTime, error) {
	dt := gcal.EventDateTime{TimeZone: tz}
	if allDay {
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return dt, fmt.Errorf("all-day bounds must be YYYY-MM-DD: %w", err)
		}
		dt.Date = value
		return dt, nil
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		dt.DateTime = parsed
		return dt, nil
	}
	if tz == "" {
		return dt, errors.New("timed bounds require RFC3339 with an offset, or --tz for local times")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return dt, fmt.Errorf("invalid IANA time zone: %w", err)
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			dt.DateTime = parsed
			return dt, nil
		}
	}
	return dt, errors.New("invalid calendar time; expected RFC3339 or YYYY-MM-DDTHH:MM")
}
func parseCalendarReminders(values []string) (*gcal.Reminders, error) {
	r := &gcal.Reminders{Overrides: []gcal.Reminder{}}
	for _, value := range values {
		if value == "default" || value == "none" {
			if len(values) != 1 {
				return nil, errors.New("default/none reminder cannot be combined with overrides")
			}
			r.UseDefault = value == "default"
			return r, nil
		}
		method, minutesText, ok := strings.Cut(value, ":")
		if !ok {
			return nil, errors.New("reminder must be popup:minutes, email:minutes, default, or none")
		}
		minutes, err := strconv.Atoi(minutesText)
		if err != nil {
			return nil, fmt.Errorf("invalid reminder minutes: %w", err)
		}
		r.Overrides = append(r.Overrides, gcal.Reminder{Method: method, Minutes: minutes})
	}
	return r, nil
}

var _ api.CalendarController = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) ControlCalendar(ctx context.Context, request calcontrol.Request, grant *agentgrant.Grant) (*calcontrol.Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if a.config == nil {
		return nil, fmt.Errorf("%w: calendar configuration is unavailable", calcontrol.ErrDenied)
	}
	source := a.config.GetGCalSource(request.Account)
	if source == nil || !source.Enabled {
		return nil, fmt.Errorf("%w: enabled calendar source is required", calcontrol.ErrDenied)
	}
	source.Email = normalizeCalendarAccountEmail(source.Email)
	existing, err := a.store.GetSourcesByTypeAndAccount(sourceTypeCalendar, source.Email)
	if err != nil {
		return nil, fmt.Errorf("%w: load calendar OAuth binding: %w", calcontrol.ErrInternal, err)
	}
	appDecision, err := calendarSyncOAuthAppDecision(a.store, source.Email, existing, source.OAuthApp, source.OAuthApp != "")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve calendar OAuth binding: %w", calcontrol.ErrInternal, err)
	}
	source.OAuthApp = appDecision.OAuthApp
	ctx = a.invocationContext(ctx)
	var client gcal.ControlAPI
	if a.calendarClientFactory != nil {
		client, err = a.calendarClientFactory(ctx, *source, !calcontrol.IsRead(request.Action))
	} else {
		var reader gcal.API
		reader, err = buildCalendarClient(ctx, source.Email, source.OAuthApp, false, !calcontrol.IsRead(request.Action))
		if err == nil {
			var ok bool
			client, ok = reader.(gcal.ControlAPI)
			if !ok {
				_ = reader.Close()
				return nil, fmt.Errorf("%w: calendar client does not support event control", calcontrol.ErrInternal)
			}
		}
	}
	if err != nil {
		if errors.Is(err, calcontrol.ErrDenied) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: create calendar client: %w", calcontrol.ErrInternal, err)
	}
	defer func() { _ = client.Close() }()
	syncer := calsync.New(client, a.store, calsync.Options{AccountEmail: source.Email, OAuthApp: source.OAuthApp, OAuthAppSet: appDecision.OAuthAppSet})
	if a.logger != nil {
		syncer.WithLogger(a.logger)
	}
	service := calcontrol.Service{Source: *source, Client: client, Persist: syncer.PersistEvent}
	result, err := service.Execute(ctx, request, grant)
	if err == nil && result != nil && len(result.Writes) > 0 && a.draftCacheRefresh != nil {
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		refreshErr := a.draftCacheRefresh(refreshCtx, "calendar event changed")
		cancel()
		if refreshErr != nil && a.logger != nil {
			a.logger.Warn("calendar analytics refresh failed", "error", refreshErr)
		}
	}
	return result, err
}
