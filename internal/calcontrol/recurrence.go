package calcontrol

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
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
	if scope == "all" || scope == scopeFuture {
		if event.RecurringEventID != "" {
			if scope == scopeFuture && r.OriginalStart != "" && !sameOriginal(event.OriginalStartTime, r.OriginalStart) {
				return nil, invalid("original_start does not match the selected instance")
			}
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
	exceptions, err := s.checkFutureExceptions(ctx, cal.ID, master.ID, r.OriginalStart)
	if err != nil {
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
	var retained []time.Time
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
		retained = append(retained, occurrence)
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
		mergedStart, mergedEnd := master.Start, master.End
		if patch.Start != nil {
			mergedStart = *patch.Start
		}
		if patch.End != nil {
			mergedEnd = *patch.End
		}
		if patch.Start != nil || patch.End != nil {
			if err := validateRange(&mergedStart, &mergedEnd); err != nil {
				return nil, err
			}
		}
		if patch.Start != nil || patch.Recurrence != nil {
			recurrence := master.Recurrence
			if patch.Recurrence != nil {
				recurrence = *patch.Recurrence
			}
			normalizedStart, err := recurrenceTimeZone(&mergedStart, master.Start.TimeZone, cal.TimeZone)
			if err != nil {
				return nil, err
			}
			if err := validateFutureRecurrence(*normalizedStart, recurrence); err != nil {
				return nil, err
			}
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
	if err := validateFutureRecurrence(*input.Start, *input.Recurrence); err != nil {
		return nil, err
	}
	newStart, _ := input.Start.Instant()
	instanceStart, _ := instance.Start.Instant()
	newEnd, _ := input.End.Instant()
	instanceEnd, _ := instance.End.Instant()
	instanceTimeZone := instance.Start.TimeZone
	if instanceTimeZone == "" {
		instanceTimeZone = master.Start.TimeZone
	}
	if instanceTimeZone == "" {
		instanceTimeZone = cal.TimeZone
	}
	changedZone := !input.Start.IsAllDay() && input.Start.TimeZone != instanceTimeZone
	changedSchedule := changedZone || !newStart.Equal(instanceStart) || !newEnd.Equal(instanceEnd) || input.Start.IsAllDay() != instance.Start.IsAllDay() || (patch.Recurrence != nil && !slices.Equal(*patch.Recurrence, recurrence))
	if changedSchedule {
		if err := rejectFutureOverlap(ctx, master, retained, exceptions, input, cal.TimeZone); err != nil {
			return nil, err
		}
	}
	steps = append(steps, PlannedWrite{Action: actionCreate, CalendarID: cal.ID, Event: input})
	if err := validateFuturePlan(steps); err != nil {
		return nil, err
	}
	// Truncation and insertion are separate Google requests. Completed writes
	// are archived and returned. A definite insertion failure triggers guarded
	// restoration of the original recurrence; never replay completed writes.
	return steps, nil
}

// rejectFutureOverlap checks changed schedules against the events that remain in
// the original series. Exceptions replace their nominal instances, and end
// times are exclusive, so a replacement may fit between retained events.
func rejectFutureOverlap(ctx context.Context, master gcal.Event, retained []time.Time, exceptions map[time.Time]gcal.Event, input gcal.EventInput, calendarTimeZone string) error {
	start, err := overlapInstant(master.Start, calendarTimeZone)
	if err != nil {
		return err
	}
	end, err := overlapInstant(master.End, calendarTimeZone)
	if err != nil {
		return err
	}
	nominalStart, _ := master.Start.Instant()
	nominalEnd, _ := master.End.Instant()
	intervals := make([]gcal.BusyPeriod, 0, len(retained)+len(exceptions))
	for _, occurrence := range retained {
		if _, replaced := exceptions[occurrence.UTC()]; !replaced {
			occurrenceStart, occurrenceEnd := occurrence, occurrence.Add(end.Sub(start))
			if master.Start.IsAllDay() {
				occurrenceStart, err = overlapInstant(gcal.EventDateTime{Date: occurrence.Format("2006-01-02")}, calendarTimeZone)
				if err != nil {
					return err
				}
				occurrenceEnd, err = overlapInstant(gcal.EventDateTime{Date: occurrence.AddDate(0, 0, int(nominalEnd.Sub(nominalStart)/(24*time.Hour))).Format("2006-01-02")}, calendarTimeZone)
				if err != nil {
					return err
				}
			}
			intervals = append(intervals, gcal.BusyPeriod{Start: occurrenceStart, End: occurrenceEnd})
		}
	}
	for _, event := range exceptions {
		if event.Status == gcal.StatusCancelled {
			continue
		}
		start, startErr := overlapInstant(event.Start, calendarTimeZone)
		end, endErr := overlapInstant(event.End, calendarTimeZone)
		if startErr != nil || endErr != nil || !end.After(start) {
			return invalid("cannot determine retained exception bounds")
		}
		intervals = append(intervals, gcal.BusyPeriod{Start: start, End: end})
	}
	slices.SortFunc(intervals, func(a, b gcal.BusyPeriod) int { return a.Start.Compare(b.Start) })
	newStart, err := overlapInstant(*input.Start, calendarTimeZone)
	if err != nil {
		return err
	}
	newEnd, err := overlapInstant(*input.End, calendarTimeZone)
	if err != nil {
		return err
	}
	duration := newEnd.Sub(newStart)
	index := 0
	check := func(occurrence time.Time) error {
		occurrenceEnd := occurrence.Add(duration)
		if input.Start.IsAllDay() {
			startDate, _ := input.Start.Instant()
			endDate, _ := input.End.Instant()
			localStart, err := overlapInstant(gcal.EventDateTime{Date: occurrence.Format("2006-01-02")}, calendarTimeZone)
			if err != nil {
				return err
			}
			localEnd, err := overlapInstant(gcal.EventDateTime{Date: occurrence.AddDate(0, 0, int(endDate.Sub(startDate)/(24*time.Hour))).Format("2006-01-02")}, calendarTimeZone)
			if err != nil {
				return err
			}
			occurrence, occurrenceEnd = localStart, localEnd
		}
		for index < len(intervals) && !intervals[index].End.After(occurrence) {
			index++
		}
		if index < len(intervals) && occurrenceEnd.After(intervals[index].Start) {
			return invalid("future scope replacement overlaps a retained occurrence")
		}
		return nil
	}
	initialStart, _ := input.Start.Instant()
	if err := check(initialStart); err != nil || index == len(intervals) {
		return err
	}
	loc := time.UTC
	if !input.Start.IsAllDay() {
		var err error
		loc, err = time.LoadLocation(input.Start.TimeZone)
		if err != nil {
			return invalid("invalid replacement time zone")
		}
	}
	for _, line := range *input.Recurrence {
		option, err := rrule.StrToROption(strings.TrimPrefix(line, "RRULE:"))
		if err != nil {
			return invalid("invalid replacement recurrence: %v", err)
		}
		option.Dtstart = initialStart.In(loc)
		rule, err := rrule.NewRRule(*option)
		if err != nil {
			return invalid("invalid replacement recurrence: %v", err)
		}
		iterator := rule.Iterator()
		index = 0
		for count := 0; ; count++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			occurrence, ok := iterator()
			if !ok {
				break
			}
			if count == 10000 {
				return invalid("future overlap check exceeds 10000 replacement instances")
			}
			if err := check(occurrence); err != nil {
				return err
			}
			if index == len(intervals) {
				break
			}
		}
	}
	return nil
}

// All-day dates occupy calendar-local days, including 23/25-hour DST days.
func overlapInstant(bound gcal.EventDateTime, calendarTimeZone string) (time.Time, error) {
	if !bound.IsAllDay() {
		if instant, ok := bound.Instant(); ok {
			return instant, nil
		}
		return time.Time{}, invalid("cannot determine overlap bounds")
	}
	loc, err := time.LoadLocation(calendarTimeZone)
	if err != nil || calendarTimeZone == "" {
		return time.Time{}, invalid("all-day overlap checks require the calendar time zone")
	}
	instant, err := time.ParseInLocation("2006-01-02", bound.Date, loc)
	if err != nil {
		return time.Time{}, invalid("cannot determine all-day overlap bounds")
	}
	return instant, nil
}

func validateFutureRecurrence(start gcal.EventDateTime, recurrence []string) error {
	instant, _ := start.Instant()
	loc := time.UTC
	if !start.IsAllDay() {
		var err error
		loc, err = time.LoadLocation(start.TimeZone)
		if err != nil {
			return invalid("invalid replacement time zone")
		}
	}
	for _, line := range recurrence {
		for part := range strings.SplitSeq(strings.TrimPrefix(line, "RRULE:"), ";") {
			if until, ok := strings.CutPrefix(part, "UNTIL="); ok && (len(until) == 8) != start.IsAllDay() {
				return invalid("future recurrence UNTIL must match the replacement start type; supply a compatible recurrence rule")
			}
		}
		option, err := rrule.StrToROption(strings.TrimPrefix(line, "RRULE:"))
		if err != nil {
			return invalid("invalid replacement recurrence: %v", err)
		}
		option.Dtstart = instant.In(loc)
		rule, err := rrule.NewRRule(*option)
		if err != nil {
			return invalid("invalid replacement recurrence: %v", err)
		}
		first, ok := rule.Iterator()()
		if !ok || !first.Equal(instant) {
			return invalid("future recurrence does not include the replacement start; supply a compatible recurrence rule")
		}
	}
	return nil
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

func (s *Service) checkFutureExceptions(ctx context.Context, calendarID, seriesID, original string) (map[time.Time]gcal.Event, error) {
	token := ""
	seen := map[string]bool{}
	retained := map[time.Time]gcal.Event{}
	for range 100 {
		page, err := s.Client.ListEvents(ctx, calendarID, gcal.EventsListParams{PageToken: token, SingleEvents: false, ShowDeleted: true, MaxResults: 2500})
		if err != nil {
			return nil, err
		}
		if page == nil {
			return nil, invalid("empty events response while checking series exceptions")
		}
		for _, event := range page.Items {
			if event.RecurringEventID != seriesID {
				continue
			}
			occurrence, ok := event.OriginalStartTime.Instant()
			if !ok {
				return nil, invalid("cannot migrate a recurring exception without originalStartTime")
			}
			cutoff, err := parseOriginal(original, event.OriginalStartTime)
			if err != nil {
				return nil, err
			}
			instant, _ := cutoff.Instant()
			if !occurrence.Before(instant) {
				return nil, invalid("future scope cannot migrate detached exceptions; edit the series manually")
			}
			retained[occurrence.UTC()] = event
		}
		token = page.NextPageToken
		if token == "" {
			return retained, nil
		}
		if seen[token] {
			return nil, invalid("repeated events page token")
		}
		seen[token] = true
	}
	return nil, invalid("series exception check exceeds 100 pages; edit the series manually")
}
