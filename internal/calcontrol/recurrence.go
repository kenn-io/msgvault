package calcontrol

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/gcal"
)

func (s *Service) selectScope(ctx context.Context, cal gcal.Calendar, event gcal.Event, r Request) (*gcal.Event, error) {
	scope := r.Scope
	if scope == "" {
		scope = "single"
	}
	recurring := event.RecurringEventID != "" || len(event.Recurrence) > 0
	if !recurring {
		if scope != "single" || r.OriginalStart != "" {
			return nil, invalid("recurrence scope requires a recurring event")
		}
		return &event, nil
	}
	if scope == "all" || scope == "future" {
		if event.RecurringEventID != "" {
			return s.Client.GetEvent(ctx, cal.ID, event.RecurringEventID)
		}
		return &event, nil
	}
	if event.RecurringEventID != "" {
		if r.OriginalStart != "" && !sameOriginal(event.OriginalStartTime, r.OriginalStart) {
			return nil, invalid("original_start does not match the selected instance")
		}
		return &event, nil
	}
	if r.OriginalStart == "" {
		return nil, invalid("single scope on a series requires original_start or an instance event_id")
	}
	original, err := parseOriginal(r.OriginalStart, event.Start)
	if err != nil {
		return nil, err
	}
	_, ok := original.Instant()
	if !ok {
		return nil, invalid("invalid original_start")
	}
	token := ""
	seen := map[string]bool{}
	for range 100 {
		page, err := s.Client.ListInstances(ctx, cal.ID, event.ID, gcal.EventsListParams{PageToken: token})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, errors.New("empty instance response")
		}
		for _, instance := range page.Items {
			if sameOriginal(instance.OriginalStartTime, r.OriginalStart) {
				return &instance, nil
			}
		}
		token = page.NextPageToken
		if token == "" {
			return nil, invalid("no occurrence with that original_start")
		}
		if seen[token] {
			return nil, errors.New("repeated instance page token")
		}
		seen[token] = true
	}
	return nil, invalid("instance lookup exceeds 100 pages")
}
func parseOriginal(value string, start gcal.EventDateTime) (gcal.EventDateTime, error) {
	if start.IsAllDay() {
		if _, err := time.Parse("2006-01-02", value); err != nil {
			return gcal.EventDateTime{}, invalid("all-day original_start must be YYYY-MM-DD")
		}
		return gcal.EventDateTime{Date: value, TimeZone: start.TimeZone}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return gcal.EventDateTime{}, invalid("original_start must be RFC3339")
	}
	return gcal.EventDateTime{DateTime: t, TimeZone: start.TimeZone}, nil
}
func sameOriginal(dt gcal.EventDateTime, value string) bool {
	if dt.IsAllDay() {
		return dt.Date == value
	}
	t, err := time.Parse(time.RFC3339, value)
	return err == nil && dt.DateTime.Equal(t)
}

// futurePlan splits a single RRULE series. RDATE/EXDATE and detached exceptions
// need a more extensive migration; reject them before any remote write.
func (s *Service) futurePlan(ctx context.Context, cal gcal.Calendar, master gcal.Event, r Request, patch gcal.EventInput) ([]PlannedWrite, error) {
	if r.OriginalStart == "" {
		return nil, invalid("future scope requires original_start")
	}
	if len(master.Recurrence) != 1 || !strings.HasPrefix(master.Recurrence[0], "RRULE:") {
		return nil, invalid("future scope supports one RRULE; RDATE/EXDATE and multiple rules require a manual series edit")
	}
	if master.EventType != "" && master.EventType != "default" {
		return nil, invalid("future scope supports default event types only")
	}
	if err := s.rejectFutureExceptions(ctx, cal.ID, master.ID, r.OriginalStart); err != nil {
		return nil, err
	}
	original, err := parseOriginal(r.OriginalStart, master.Start)
	if err != nil {
		return nil, err
	}
	cutoff, _ := original.Instant()
	start, _ := master.Start.Instant()
	if cutoff.Before(start) {
		return nil, invalid("original_start precedes series start")
	}
	// Find the actual instance before planning the split. Its current bounds
	// account for a rescheduled occurrence and establish the new series start.
	instance, err := s.selectScope(ctx, cal, master, Request{Scope: "single", OriginalStart: r.OriginalStart})
	if err != nil {
		return nil, err
	}
	opt, err := rrule.StrToROption(strings.TrimPrefix(master.Recurrence[0], "RRULE:"))
	if err != nil {
		return nil, invalid("invalid existing recurrence: %v", err)
	}
	loc := time.UTC
	if !master.Start.IsAllDay() {
		timeZone := master.Start.TimeZone
		if timeZone == "" {
			timeZone = cal.TimeZone
		}
		if timeZone == "" {
			return nil, invalid("timed recurrence requires an event or calendar time zone")
		}
		loc, err = time.LoadLocation(timeZone)
		if err != nil {
			return nil, invalid("invalid series time zone")
		}
	}
	opt.Dtstart = start.In(loc)
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, invalid("invalid series rule: %v", err)
	}
	iterator := rule.Iterator()
	before := 0
	found := false
	for range 10000 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		occurrence, ok := iterator()
		if !ok || occurrence.After(cutoff) {
			break
		}
		if occurrence.Equal(cutoff) {
			found = true
			break
		}
		before++
	}
	if !found {
		return nil, invalid("original_start is not an occurrence within the first 10000 series instances")
	}
	if before == 0 {
		action := r.Action
		if action == "delete" {
			return []PlannedWrite{{Action: "delete", CalendarID: cal.ID, EventID: master.ID}}, nil
		}
		patch.Start, err = recurrenceTimeZone(patch.Start, master.Start.TimeZone, cal.TimeZone)
		if err != nil {
			return nil, err
		}
		patch.End, err = recurrenceTimeZone(patch.End, master.Start.TimeZone, cal.TimeZone)
		if err != nil {
			return nil, err
		}
		return []PlannedWrite{{Action: actionUpdate, CalendarID: cal.ID, EventID: master.ID, Event: patch}}, nil
	}
	oldOpt := *opt
	oldOpt.Count = 0
	oldOpt.Until = cutoff.Add(-time.Second)
	oldRecurrence := []string{seriesRule(oldOpt, master.Start.IsAllDay())}
	steps := []PlannedWrite{{Action: actionUpdate, CalendarID: cal.ID, EventID: master.ID, Event: gcal.EventInput{Recurrence: &oldRecurrence}}}
	if r.Action == "delete" {
		if err := validateFuturePlan(steps); err != nil {
			return nil, err
		}
		return steps, nil
	}
	if hasGuestRSVP(master.Attendees) {
		return nil, invalid("future scope cannot preserve attendee RSVP state when splitting this series")
	}
	if err := validateSeriesClone(master); err != nil {
		return nil, err
	}
	summary, description, location := master.Summary, master.Description, master.Location
	attendees := writableAttendees(master.Attendees)
	newOpt := *opt
	if newOpt.Count > 0 {
		newOpt.Count -= before
	}
	recurrence := []string{seriesRule(newOpt, master.Start.IsAllDay())}
	input := gcal.EventInput{Summary: &summary, Description: &description, Location: &location, Start: &instance.Start, End: &instance.End, Attendees: &attendees, Recurrence: &recurrence}
	if patch.Summary != nil {
		input.Summary = patch.Summary
	}
	if patch.Description != nil {
		input.Description = patch.Description
	}
	if patch.Location != nil {
		input.Location = patch.Location
	}
	if patch.Start != nil {
		input.Start = patch.Start
		if patch.Recurrence == nil && !newOpt.Until.IsZero() {
			newStart, _ := patch.Start.Instant()
			if newStart.After(newOpt.Until) {
				return nil, invalid("future scope cannot move the replacement start after the existing recurrence UNTIL without a new recurrence rule")
			}
		}
	}
	if patch.End != nil {
		input.End = patch.End
	}
	if patch.Attendees != nil {
		attendees = writableAttendees(*patch.Attendees)
		input.Attendees = &attendees
	}
	if patch.Recurrence != nil {
		input.Recurrence = patch.Recurrence
	}
	input.Reminders = master.Reminders
	if patch.Reminders != nil {
		input.Reminders = patch.Reminders
	}
	input.Start, err = recurrenceTimeZone(input.Start, master.Start.TimeZone, cal.TimeZone)
	if err != nil {
		return nil, err
	}
	input.End, err = recurrenceTimeZone(input.End, master.Start.TimeZone, cal.TimeZone)
	if err != nil {
		return nil, err
	}
	if err := validateRange(input.Start, input.End); err != nil {
		return nil, err
	}
	steps = append(steps, PlannedWrite{Action: "create", CalendarID: cal.ID, Event: input})
	if err := validateFuturePlan(steps); err != nil {
		return nil, err
	}
	// Truncation and insertion are separate Google requests. Completed writes
	// are archived and returned if the second request fails; never replay them.
	return steps, nil
}

func recurrenceTimeZone(bound *gcal.EventDateTime, eventTimeZone, calendarTimeZone string) (*gcal.EventDateTime, error) {
	if bound == nil || bound.IsAllDay() || bound.TimeZone != "" {
		return bound, nil
	}
	updated := *bound
	updated.TimeZone = eventTimeZone
	if updated.TimeZone == "" {
		updated.TimeZone = calendarTimeZone
	}
	if updated.TimeZone == "" {
		return nil, invalid("timed recurrence requires an event or calendar time zone")
	}
	return &updated, nil
}

func writableAttendees(attendees []gcal.Attendee) []gcal.Attendee {
	result := make([]gcal.Attendee, 0, len(attendees))
	for _, attendee := range attendees {
		result = append(result, gcal.Attendee{
			Email: attendee.Email, DisplayName: attendee.DisplayName, Resource: attendee.Resource,
			Optional: attendee.Optional, AdditionalGuests: attendee.AdditionalGuests, Comment: attendee.Comment,
		})
	}
	return result
}

func hasGuestRSVP(attendees []gcal.Attendee) bool {
	for _, attendee := range attendees {
		if attendee.Organizer {
			continue
		}
		switch attendee.ResponseStatus {
		case "", "needsAction":
			continue
		default:
			return true
		}
	}
	return false
}

func validateFuturePlan(plan []PlannedWrite) error {
	for _, write := range plan {
		if err := validateEventInput(write.Event); err != nil {
			return invalid("future scope generated invalid %s input: %v", write.Action, err)
		}
	}
	return nil
}

func seriesRule(option rrule.ROption, allDay bool) string {
	value := option.RRuleString()
	if allDay && !option.Until.IsZero() {
		// RFC 5545 section 3.3.10 requires UNTIL to match DTSTART's value
		// type. rrule-go emits date-times, so preserve date-only series here.
		value = strings.Replace(value, "UNTIL="+option.Until.UTC().Format("20060102T150405Z"), "UNTIL="+option.Until.UTC().Format("20060102"), 1)
	}
	return "RRULE:" + value
}

// A split creates a new event. Reject fields this client cannot copy rather
// than silently stripping them after truncating the original series.
func validateSeriesClone(event gcal.Event) error {
	if event.HangoutLink != "" || (event.Visibility != "" && event.Visibility != "default") || (event.Transparency != "" && event.Transparency != "opaque") {
		return invalid("future scope cannot preserve meeting details or custom visibility; use a manual series edit")
	}
	if len(event.Raw) == 0 {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(event.Raw, &fields); err != nil {
		return invalid("cannot inspect existing series metadata; use a manual series edit")
	}
	for _, key := range []string{"conferenceData", "attachments", "extendedProperties", "colorId", "eventLabelId", "source"} {
		value := fields[key]
		switch v := value.(type) {
		case nil:
			continue
		case string:
			if v == "" {
				continue
			}
		case []any:
			if len(v) == 0 {
				continue
			}
		case map[string]any:
			if len(v) == 0 {
				continue
			}
		}
		return invalid("future scope cannot preserve %s; use a manual series edit", key)
	}
	if value, exists := fields["privateCopy"]; exists && value != false {
		return invalid("future scope cannot preserve privateCopy event propagation; use a manual series edit")
	}
	for key, defaultValue := range map[string]bool{"guestsCanModify": false, "guestsCanInviteOthers": true, "guestsCanSeeOtherGuests": true, "anyoneCanAddSelf": false} {
		if value, exists := fields[key]; exists && value != defaultValue {
			return invalid("future scope cannot preserve custom %s; use a manual series edit", key)
		}
	}
	return nil
}

func (s *Service) rejectFutureExceptions(ctx context.Context, calendarID, seriesID, original string) error {
	token := ""
	seen := map[string]bool{}
	for range 100 {
		page, err := s.Client.ListEvents(ctx, calendarID, gcal.EventsListParams{PageToken: token, SingleEvents: false, ShowDeleted: true, MaxResults: 2500})
		if err != nil {
			return err
		}
		if page == nil {
			return invalid("empty events response while checking series exceptions")
		}
		for _, event := range page.Items {
			if event.RecurringEventID != seriesID {
				continue
			}
			occurrence, ok := event.OriginalStartTime.Instant()
			if !ok {
				return invalid("cannot migrate a recurring exception without originalStartTime")
			}
			cutoff, err := parseOriginal(original, event.OriginalStartTime)
			if err != nil {
				return err
			}
			instant, _ := cutoff.Instant()
			if !occurrence.Before(instant) {
				return invalid("future scope cannot migrate detached exceptions; edit the series manually")
			}
		}
		token = page.NextPageToken
		if token == "" {
			return nil
		}
		if seen[token] {
			return invalid("repeated events page token")
		}
		seen[token] = true
	}
	return invalid("series exception check exceeds 100 pages; edit the series manually")
}
