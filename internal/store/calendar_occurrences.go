package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

var (
	ErrInvalidArgument = errors.New("invalid argument")
	ErrNotFound        = errors.New("calendar occurrence not found")
)

// CalendarOccurrence is an archived event after recurrence and exception resolution.
type CalendarOccurrence struct {
	Ref               string              `json:"ref"`
	MessageID         int64               `json:"message_id"`
	SeriesMessageID   int64               `json:"series_message_id"`
	SourceID          int64               `json:"source_id"`
	Subject           string              `json:"subject"`
	Original          calrecur.Identity   `json:"original"`
	Start             time.Time           `json:"start"`
	End               time.Time           `json:"end"`
	BoundsKnown       bool                `json:"bounds_known"`
	AllDay            bool                `json:"all_day"`
	StartDate         string              `json:"start_date,omitempty"`
	EndDate           string              `json:"end_date,omitempty"`
	Status            string              `json:"status"`
	Cancelled         bool                `json:"cancelled"`
	DeletedFromSource bool                `json:"deleted_from_source"`
	ICalUID           string              `json:"ical_uid,omitempty"`
	JoinURL           string              `json:"join_url,omitempty"`
	OrganizerEmail    string              `json:"organizer_email,omitempty"`
	Location          string              `json:"location,omitempty"`
	Attendees         []calevent.Attendee `json:"attendees"`
}

type UnexpandableSeries struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason" enum:"budget,recurrence_error,unsupported"`
}

type CalendarOccurrenceFilter struct {
	From             time.Time
	To               time.Time
	SourceIDs        []int64
	AttendeeEmail    string
	IncludeCancelled bool
	Limit            int
	Cursor           string
}

type CalendarOccurrencePage struct {
	Items            []CalendarOccurrence `json:"items"`
	Unexpandable     []UnexpandableSeries `json:"unexpandable"`
	UnexpandableMore bool                 `json:"unexpandable_more"`
	NextCursor       string               `json:"next_cursor,omitempty"`
}

type occurrenceCursor struct {
	Start time.Time `json:"start"`
	Ref   string    `json:"ref"`
	Query string    `json:"query"`
}

func occurrenceQueryKey(scope SourceScope, f CalendarOccurrenceFilter) string {
	scope.IDs = slices.Clone(scope.IDs)
	slices.Sort(scope.IDs)
	scope.IDs = slices.Compact(scope.IDs)
	f.SourceIDs = slices.Clone(f.SourceIDs)
	slices.Sort(f.SourceIDs)
	f.SourceIDs = slices.Compact(f.SourceIDs)
	data, _ := json.Marshal(struct {
		Scope     SourceScope
		From, To  time.Time
		Sources   []int64
		Attendee  string
		Cancelled bool
	}{scope, f.From.UTC(), f.To.UTC(), f.SourceIDs, strings.ToLower(strings.TrimSpace(f.AttendeeEmail)), f.IncludeCancelled})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Store) ListCalendarOccurrencesContext(ctx context.Context, scope SourceScope, f CalendarOccurrenceFilter) (CalendarOccurrencePage, error) {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) || f.To.Sub(f.From) > 400*24*time.Hour {
		return CalendarOccurrencePage{}, fmt.Errorf("%w: window must be positive and at most 400 days", ErrInvalidArgument)
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > 500 {
		return CalendarOccurrencePage{}, fmt.Errorf("%w: limit must be between 1 and 500", ErrInvalidArgument)
	}
	for _, id := range f.SourceIDs {
		if id <= 0 {
			return CalendarOccurrencePage{}, fmt.Errorf("%w: source IDs must be positive", ErrInvalidArgument)
		}
	}
	queryKey := occurrenceQueryKey(scope, f)
	var cursor occurrenceCursor
	if f.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(f.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Query != queryKey || cursor.Start.IsZero() || cursor.Ref == "" {
			return CalendarOccurrencePage{}, fmt.Errorf("%w: cursor does not match this query", ErrInvalidArgument)
		}
	}
	if len(f.SourceIDs) > 0 {
		ids := make([]int64, 0, len(f.SourceIDs))
		for _, id := range f.SourceIDs {
			if scope.Allows(id) {
				ids = append(ids, id)
			}
		}
		scope = SourceScope{IDs: ids}
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return CalendarOccurrencePage{}, err
	}
	defer release()
	r := s.snapshotReader(ctx)
	rows, err := calendarEventRowsInWindow(ctx, r, scope, f.From, f.To)
	if err != nil {
		return CalendarOccurrencePage{}, err
	}
	budget := calrecur.RequestReadBudget
	items, unexpandable, err := expandRowsToOccurrences(ctx, r, rows, f.From, f.To, &budget)
	if err != nil {
		return CalendarOccurrencePage{}, err
	}
	page := CalendarOccurrencePage{Items: []CalendarOccurrence{}, Unexpandable: []UnexpandableSeries{}}
	slices.SortFunc(items, func(a, b CalendarOccurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return strings.Compare(a.Ref, b.Ref)
	})
	attendee := strings.ToLower(strings.TrimSpace(f.AttendeeEmail))
	for _, o := range items {
		if o.Cancelled && !f.IncludeCancelled {
			continue
		}
		if attendee != "" && !slices.ContainsFunc(o.Attendees, func(a calevent.Attendee) bool { return strings.EqualFold(a.Email, attendee) }) {
			continue
		}
		if f.Cursor != "" && (o.Start.Before(cursor.Start) || (o.Start.Equal(cursor.Start) && o.Ref <= cursor.Ref)) {
			continue
		}
		if len(page.Items) == f.Limit {
			last := page.Items[len(page.Items)-1]
			encoded, err := json.Marshal(occurrenceCursor{Start: last.Start, Ref: last.Ref, Query: queryKey})
			if err != nil {
				return CalendarOccurrencePage{}, err
			}
			page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
			break
		}
		page.Items = append(page.Items, o)
	}
	slices.SortFunc(unexpandable, func(a, b UnexpandableSeries) int { return strings.Compare(a.Ref, b.Ref) })
	page.UnexpandableMore = len(unexpandable) > 50
	page.Unexpandable = append(page.Unexpandable, unexpandable[:min(50, len(unexpandable))]...)
	return page, nil
}

func calendarSeriesMatches(master, exception CalendarEventRow) bool {
	return master.SourceID == exception.SourceID && master.Event.Provider == exception.Event.Provider && master.Event.Namespace == exception.Event.Namespace && master.Event.ProviderEventID == exception.Event.SeriesProviderID
}

type calendarSeriesIdentity struct {
	sourceID                int64
	provider, namespace, id string
}

func calendarSeriesGroup(row CalendarEventRow, id string) calendarSeriesIdentity {
	return calendarSeriesIdentity{row.SourceID, row.Event.Provider, row.Event.Namespace, id}
}

func calendarRowSet(ctx context.Context, r rowReader, row CalendarEventRow) (calrecur.Set, time.Duration, int, error) {
	e := row.Event
	if !e.HasBounds || e.Start == nil || e.End == nil {
		return calrecur.Set{}, 0, 0, fmt.Errorf("%w: series has no bounds", ErrInvalidArgument)
	}
	zone, err := currentCalendarZone(ctx, r, row.SourceID)
	if err != nil {
		return calrecur.Set{}, 0, 0, err
	}
	if !e.AllDay && e.EventTimeZone != "" {
		zone, err = time.LoadLocation(e.EventTimeZone)
		if err != nil {
			return calrecur.Set{}, 0, 0, fmt.Errorf("%w: invalid event zone", calrecur.ErrUnsupportedRecurrence)
		}
	}
	set, err := calrecur.ParseSet(*e.Start, e.AllDay, e.StartDate, zone, e.Recurrence)
	days := 0
	if e.AllDay {
		start, err1 := time.Parse("2006-01-02", e.StartDate)
		end, err2 := time.Parse("2006-01-02", e.EndDate)
		if err1 != nil || err2 != nil {
			return calrecur.Set{}, 0, 0, fmt.Errorf("%w: invalid series dates", ErrInvalidArgument)
		}
		days = int(end.Sub(start) / (24 * time.Hour))
	}
	return set, e.End.Sub(*e.Start), days, err
}

// recurrenceWork counts candidates before expanding. Iter does not expose its
// pull count, so inspect each rule separately without exclusions and reserve
// an extra DTSTART pull for its possible duplicate. Both scans are charged to
// the request budget; expansion itself retains the per-series pull limit.
func recurrenceWork(ctx context.Context, set calrecur.Set, to time.Time, budget *int) error {
	candidateBudget := min(calrecur.SeriesReadBudget, *budget/2)
	if candidateBudget < 1 {
		return ErrQueryTooBroad
	}
	work := 1 + len(set.RDates)
	for _, rule := range set.RRules {
		one := set
		one.RRules = []string{rule}
		one.RDates = nil
		one.ExDates = nil
		count := 0
		err := calrecur.Iter(ctx, one, to, candidateBudget-work+1, func(calrecur.Instance) bool { count++; return true })
		if err != nil {
			if errors.Is(err, calrecur.ErrBudgetExceeded) {
				*budget -= min(candidateBudget, *budget)
				if candidateBudget < calrecur.SeriesReadBudget {
					return ErrQueryTooBroad
				}
			}
			return err
		}
		work += count + 1
		if work > candidateBudget {
			*budget -= min(candidateBudget, *budget)
			if candidateBudget < calrecur.SeriesReadBudget {
				return ErrQueryTooBroad
			}
			return calrecur.ErrBudgetExceeded
		}
	}
	if work > candidateBudget {
		*budget -= min(candidateBudget, *budget)
		if candidateBudget < calrecur.SeriesReadBudget {
			return ErrQueryTooBroad
		}
		return calrecur.ErrBudgetExceeded
	}
	*budget -= 2 * work
	return nil
}

func calendarOccurrenceFields(row CalendarEventRow) CalendarOccurrence {
	e := row.Event
	o := CalendarOccurrence{Ref: FormatOccurrenceRef(OccurrenceRef{MessageID: row.MessageID}), MessageID: row.MessageID, SourceID: row.SourceID, Subject: row.Subject, BoundsKnown: e.HasBounds, AllDay: e.AllDay, StartDate: e.StartDate, EndDate: e.EndDate, Status: e.Status, Cancelled: e.Status == "cancelled", DeletedFromSource: row.DeletedFromSource, ICalUID: e.ICalUID, JoinURL: e.JoinURL, OrganizerEmail: e.OrganizerEmail, Location: e.Location, Attendees: slices.Clone(e.Attendees)}
	if o.Attendees == nil {
		o.Attendees = []calevent.Attendee{}
	}
	return o
}

func calendarAllDaySearchWindow(date string) (time.Time, time.Time, error) {
	civil, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse calendar occurrence date: %w", err)
	}
	// Allow for UTC offsets and skipped local midnights. The recurrence engine
	// resolves the actual local boundary and keeps the date identity civil.
	return civil.Add(-72 * time.Hour), civil.Add(24 * time.Hour), nil
}

func calendarAllDayDurationDays(startDate, endDate string) (int, error) {
	start, err := time.Parse(time.DateOnly, startDate)
	if err != nil {
		return 0, fmt.Errorf("parse calendar occurrence start date: %w", err)
	}
	end, err := time.Parse(time.DateOnly, endDate)
	if err != nil {
		return 0, fmt.Errorf("parse calendar occurrence end date: %w", err)
	}
	if end.Before(start) {
		return 0, errors.New("calendar occurrence end date precedes start date")
	}
	return int(end.Sub(start) / (24 * time.Hour)), nil
}

func resolveCalendarAllDayOccurrence(ctx context.Context, date, endDate string, zone *time.Location) (calrecur.Occurrence, error) {
	days := 0
	if endDate != "" {
		var err error
		days, err = calendarAllDayDurationDays(date, endDate)
		if err != nil {
			return calrecur.Occurrence{}, err
		}
	}
	from, to, err := calendarAllDaySearchWindow(date)
	if err != nil {
		return calrecur.Occurrence{}, err
	}
	occurrences, err := calrecur.Expand(ctx, calrecur.Set{AllDay: true, StartDate: date, Zone: zone}, 0, days, nil, from, to, calrecur.SeriesReadBudget)
	if err != nil {
		return calrecur.Occurrence{}, err
	}
	for _, occurrence := range occurrences {
		if occurrence.Original.AllDay && occurrence.Original.Date == date {
			return occurrence, nil
		}
	}
	return calrecur.Occurrence{}, errors.New("calendar occurrence date did not expand")
}

func calendarRowOccurrence(ctx context.Context, r rowReader, row CalendarEventRow) (CalendarOccurrence, error) {
	e := row.Event
	o := calendarOccurrenceFields(row)
	if e.Start != nil {
		o.Start = *e.Start
	}
	if e.End != nil {
		o.End = *e.End
	}
	if e.AllDay {
		zone, err := currentCalendarZone(ctx, r, row.SourceID)
		if err != nil {
			return CalendarOccurrence{}, err
		}
		date := e.StartDate
		if !e.HasBounds && e.Original != nil {
			date = e.Original.Date
		}
		endDate := ""
		if e.HasBounds {
			endDate = e.EndDate
		}
		resolved, err := resolveCalendarAllDayOccurrence(ctx, date, endDate, zone)
		if err != nil {
			return CalendarOccurrence{}, fmt.Errorf("resolve calendar occurrence date: %w", err)
		}
		o.Start = resolved.Start
		if e.HasBounds {
			o.End = resolved.End
		}
		o.Original = calrecur.Identity{AllDay: true, Date: date}
	} else {
		o.Original = calrecur.Identity{Instant: o.Start.UTC()}
	}
	if e.Original != nil {
		o.Original = *e.Original
		if !e.HasBounds && !e.AllDay {
			o.Start = e.Original.Instant
		}
	}
	if !e.HasBounds {
		o.End = o.Start
	}
	return o, nil
}

func occurrenceOverlaps(o CalendarOccurrence, from, to time.Time) bool {
	return o.Start.Before(to) && (o.End.After(from) || (o.End.Equal(o.Start) && !o.Start.Before(from)))
}

func calendarExceptions(ctx context.Context, r rowReader, rows []CalendarEventRow, master CalendarEventRow) ([]calrecur.Exception, map[string]CalendarEventRow, error) {
	var exceptions []calrecur.Exception
	var zone *time.Location
	// Key by recurrence identity: ExceptionRef's zero value means no override,
	// but message ID zero is legal in the archive.
	byOriginal := map[string]CalendarEventRow{}
	for _, row := range rows {
		if row.Event.Kind != calendarExceptionKind || row.Event.Original == nil || !calendarSeriesMatches(master, row) {
			continue
		}
		e := row.Event
		exception := calrecur.Exception{Original: *e.Original, Start: e.Start, End: e.End, StartDate: e.StartDate, EndDate: e.EndDate, Cancelled: e.Status == "cancelled", Ref: row.MessageID}
		if e.StartDate != "" || e.EndDate != "" {
			if zone == nil {
				var err error
				zone, err = currentCalendarZone(ctx, r, master.SourceID)
				if err != nil {
					return nil, nil, err
				}
			}
			exception.Zone = zone
		}
		exceptions = append(exceptions, exception)
		byOriginal[e.Original.Key()] = row
	}
	return exceptions, byOriginal, nil
}

func expandedCalendarOccurrence(master CalendarEventRow, byOriginal map[string]CalendarEventRow, expanded calrecur.Occurrence) CalendarOccurrence {
	effective := master
	if exception, ok := byOriginal[expanded.Original.Key()]; ok {
		if exception.Event.HasBounds {
			effective = exception
		} else {
			effective.MessageID = exception.MessageID
			effective.DeletedFromSource = exception.DeletedFromSource
			effective.Event.Status = exception.Event.Status
		}
	}
	o := calendarOccurrenceFields(effective)
	o.Ref = FormatOccurrenceRef(OccurrenceRef{MessageID: master.MessageID, Key: expanded.Original.Key()})
	o.SeriesMessageID = master.MessageID
	o.Original = expanded.Original
	o.Start = expanded.Start
	o.End = expanded.End
	o.StartDate = expanded.StartDate
	o.EndDate = expanded.EndDate
	o.AllDay = expanded.AllDay
	o.BoundsKnown = expanded.BoundsKnown
	o.Cancelled = master.Event.Status == "cancelled" || effective.Event.Status == "cancelled" || expanded.Cancelled
	if o.Cancelled {
		o.Status = "cancelled"
	}
	if o.ICalUID == "" {
		o.ICalUID = master.Event.ICalUID
	}
	return o
}

// expandRowsToOccurrences is shared with meeting-record reads. Its caller has
// already applied SourceScope and obtained all candidates inside one snapshot.
func expandRowsToOccurrences(ctx context.Context, r rowReader, rows []CalendarEventRow, from, to time.Time, budget *int) ([]CalendarOccurrence, []UnexpandableSeries, error) {
	var items []CalendarOccurrence
	var unexpandable []UnexpandableSeries
	masters := map[calendarSeriesIdentity]bool{}
	exceptionRows := map[calendarSeriesIdentity][]CalendarEventRow{}
	for _, row := range rows {
		if row.Event.Kind == "series" {
			masters[calendarSeriesGroup(row, row.Event.ProviderEventID)] = true
		}
		if row.Event.Kind == calendarExceptionKind {
			key := calendarSeriesGroup(row, row.Event.SeriesProviderID)
			exceptionRows[key] = append(exceptionRows[key], row)
		}
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if row.Event.Kind == calendarExceptionKind && masters[calendarSeriesGroup(row, row.Event.SeriesProviderID)] {
			continue
		}
		if row.Event.Kind != "series" {
			if *budget <= 0 {
				return nil, nil, ErrQueryTooBroad
			}
			*budget--
			o, err := calendarRowOccurrence(ctx, r, row)
			if err != nil {
				return nil, nil, err
			}
			if occurrenceOverlaps(o, from, to) {
				items = append(items, o)
			}
			continue
		}
		ref := FormatOccurrenceRef(OccurrenceRef{MessageID: row.MessageID})
		set, duration, days, err := calendarRowSet(ctx, r, row)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			if !errors.Is(err, ErrInvalidArgument) && !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, nil, fmt.Errorf("read calendar series: %w", err)
			}
			reason := "recurrence_error"
			if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				reason = "unsupported"
			}
			unexpandable = append(unexpandable, UnexpandableSeries{Ref: ref, Reason: reason})
			continue
		}
		exceptions, byOriginal, err := calendarExceptions(ctx, r, exceptionRows[calendarSeriesGroup(row, row.Event.ProviderEventID)], row)
		if err != nil {
			return nil, nil, err
		}
		err = recurrenceWork(ctx, set, to, budget)
		if errors.Is(err, ErrQueryTooBroad) {
			return nil, nil, ErrQueryTooBroad
		}
		if err == nil {
			var expanded []calrecur.Occurrence
			// Expand uses strict interval overlap. Include point events at the
			// lower boundary, then apply the archive's exact overlap predicate.
			expanded, err = calrecur.Expand(ctx, set, duration, days, exceptions, from.Add(-time.Nanosecond), to, calrecur.SeriesReadBudget)
			if err == nil {
				for _, e := range expanded {
					o := expandedCalendarOccurrence(row, byOriginal, e)
					if occurrenceOverlaps(o, from, to) {
						items = append(items, o)
					}
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		reason := "recurrence_error"
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			reason = "budget"
		} else if errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
			reason = "unsupported"
		}
		unexpandable = append(unexpandable, UnexpandableSeries{Ref: ref, Reason: reason})
	}
	return items, unexpandable, nil
}

func (s *Store) GetCalendarOccurrenceContext(ctx context.Context, scope SourceScope, ref string) (CalendarOccurrence, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return CalendarOccurrence{}, err
	}
	defer release()
	budget := calrecur.RequestReadBudget
	return calendarOccurrenceWithReader(ctx, s.snapshotReader(ctx), scope, ref, &budget)
}

func calendarOccurrenceWithReader(ctx context.Context, r rowReader, scope SourceScope, ref string, budget *int) (CalendarOccurrence, error) {
	kind, parsed, _, err := ParseMeetingRef(ref)
	if err != nil || kind != calendarEventRefKind {
		return CalendarOccurrence{}, fmt.Errorf("%w: invalid occurrence ref", ErrInvalidArgument)
	}
	if err := spendLinkBudget(ctx, budget, 1); err != nil {
		return CalendarOccurrence{}, err
	}
	rows, err := calendarEventRowsByMessageIDs(ctx, r, scope, []int64{parsed.MessageID})
	if err != nil {
		return CalendarOccurrence{}, err
	}
	if len(rows) == 0 {
		return CalendarOccurrence{}, ErrNotFound
	}
	row := rows[0]
	if row.Event.Kind == calendarExceptionKind && parsed.Key == "" {
		parsed, err = resolveOccurrenceRefAlias(ctx, r, SourceScope{IDs: []int64{row.SourceID}}, parsed)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return CalendarOccurrence{}, ErrNotFound
			}
			return CalendarOccurrence{}, err
		}
		if parsed.MessageID != row.MessageID {
			rows, err = calendarEventRowsByMessageIDs(ctx, r, scope, []int64{parsed.MessageID})
			if err != nil {
				return CalendarOccurrence{}, err
			}
			if len(rows) == 0 {
				return CalendarOccurrence{}, ErrNotFound
			}
			row = rows[0]
		}
	}
	if parsed.Key == "" {
		if row.Event.Kind == "series" {
			return CalendarOccurrence{}, fmt.Errorf("%w: series ref requires an original occurrence key", ErrInvalidArgument)
		}
		return calendarRowOccurrence(ctx, r, row)
	}
	if row.Event.Kind != "series" {
		return CalendarOccurrence{}, ErrNotFound
	}
	original, err := calrecur.ParseKey(parsed.Key)
	if err != nil {
		return CalendarOccurrence{}, fmt.Errorf("%w: invalid occurrence key", ErrInvalidArgument)
	}
	set, duration, days, err := calendarRowSet(ctx, r, row)
	if err != nil {
		return CalendarOccurrence{}, err
	}
	if original.AllDay != set.AllDay {
		return CalendarOccurrence{}, ErrNotFound
	}
	// This anchor is already authorized. Read only its source's exceptions,
	// with a sentinel row to detect overflow before considering any result.
	related, err := queryCalendarEventRows(ctx, r, SourceScope{IDs: []int64{row.SourceID}},
		"e.kind = 'exception' AND e.provider = ? AND e.namespace = ? AND e.series_provider_id = ?",
		[]any{row.Event.Provider, row.Event.Namespace, row.Event.ProviderEventID}, maxCalendarExceptions+1)
	if err != nil {
		return CalendarOccurrence{}, err
	}
	if len(related) > maxCalendarExceptions {
		return CalendarOccurrence{}, ErrQueryTooBroad
	}
	exceptions, byOriginal, err := calendarExceptions(ctx, r, related, row)
	if err != nil {
		return CalendarOccurrence{}, err
	}
	originalAt := original.Instant
	if original.AllDay {
		originalAt, err = time.ParseInLocation(time.DateOnly, original.Date, set.Zone)
		if err != nil {
			return CalendarOccurrence{}, fmt.Errorf("resolve calendar identity: %w", err)
		}
	}
	allowance, err := reserveLinkRecurrence(ctx, set, originalAt, budget)
	if err != nil {
		return CalendarOccurrence{}, err
	}
	exists, err := calrecur.Contains(ctx, set, exceptions, original, allowance)
	if err != nil {
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			return CalendarOccurrence{}, ErrQueryTooBroad
		}
		return CalendarOccurrence{}, err
	}
	if !exists {
		return CalendarOccurrence{}, ErrNotFound
	}
	start := original.Instant
	allDayDate := ""
	if original.AllDay {
		allDayDate = original.Date
	}
	for _, e := range exceptions {
		if e.Original.Key() == original.Key() {
			if e.Start != nil {
				start = *e.Start
			}
			if e.StartDate != "" || e.EndDate != "" {
				zone := e.Zone
				if zone == nil {
					zone = set.Zone
				}
				switch {
				case e.StartDate != "":
					allDayDate = e.StartDate
				case e.Start != nil:
					allDayDate = e.Start.In(zone).Format(time.DateOnly)
				case original.AllDay:
					allDayDate = original.Date
				default:
					allDayDate = e.Original.Instant.In(zone).Format(time.DateOnly)
				}
			} else if e.Start != nil {
				allDayDate = ""
			}
		}
	}
	from := start.Add(-time.Nanosecond)
	to := start.Truncate(time.Second).Add(time.Second)
	workUntil := to
	if allDayDate != "" {
		var err error
		from, to, err = calendarAllDaySearchWindow(allDayDate)
		if err != nil {
			return CalendarOccurrence{}, fmt.Errorf("resolve calendar occurrence date: %w", err)
		}
		workUntil = to
	}
	if err := recurrenceWork(ctx, set, workUntil, budget); err != nil {
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			return CalendarOccurrence{}, ErrQueryTooBroad
		}
		return CalendarOccurrence{}, err
	}
	expanded, err := calrecur.Expand(ctx, set, duration, days, exceptions, from, to, calrecur.SeriesReadBudget)
	if err != nil {
		if errors.Is(err, calrecur.ErrBudgetExceeded) {
			return CalendarOccurrence{}, ErrQueryTooBroad
		}
		return CalendarOccurrence{}, err
	}
	for _, e := range expanded {
		if e.Original.Key() == parsed.Key {
			return expandedCalendarOccurrence(row, byOriginal, e), nil
		}
	}
	return CalendarOccurrence{}, ErrNotFound
}
