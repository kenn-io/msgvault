package calevent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/teambition/rrule-go"
	"go.kenn.io/msgvault/internal/calrecur"
)

// MSGraphRawFormat is the raw_format of a Microsoft Graph event snapshot.
const MSGraphRawFormat = "msgraph_event_json"

type graphDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphEmail struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// GraphEvent is the subset of a Graph event resource that the archive reads.
type GraphEvent struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	SeriesMasterID string `json:"seriesMasterId"`
	Subject        string `json:"subject"`
	Body           struct {
		Content string `json:"content"`
	} `json:"body"`
	Start                 graphDateTime `json:"start"`
	End                   graphDateTime `json:"end"`
	IsAllDay              bool          `json:"isAllDay"`
	IsCancelled           bool          `json:"isCancelled"`
	OriginalStart         string        `json:"originalStart"`
	OriginalStartTimeZone string        `json:"originalStartTimeZone"`
	ICalUID               string        `json:"iCalUId"`
	ShowAs                string        `json:"showAs"`
	Sensitivity           string        `json:"sensitivity"`
	WebLink               string        `json:"webLink"`
	Recurrence            *struct {
		Pattern struct {
			Type           string   `json:"type"`
			Interval       int      `json:"interval"`
			Month          int      `json:"month"`
			DayOfMonth     int      `json:"dayOfMonth"`
			DaysOfWeek     []string `json:"daysOfWeek"`
			FirstDayOfWeek string   `json:"firstDayOfWeek"`
			Index          string   `json:"index"`
		} `json:"pattern"`
		Range struct {
			Type                string `json:"type"`
			EndDate             string `json:"endDate"`
			RecurrenceTimeZone  string `json:"recurrenceTimeZone"`
			NumberOfOccurrences int    `json:"numberOfOccurrences"`
		} `json:"range"`
	} `json:"recurrence"`
	CancelledOccurrences []string     `json:"cancelledOccurrences"`
	ExceptionOccurrences []GraphEvent `json:"exceptionOccurrences"`
	Organizer            struct {
		EmailAddress graphEmail `json:"emailAddress"`
	} `json:"organizer"`
	Attendees []struct {
		Type   string `json:"type"`
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
		EmailAddress graphEmail `json:"emailAddress"`
	} `json:"attendees"`
	ResponseStatus struct {
		Response string `json:"response"`
	} `json:"responseStatus"`
	Location struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	OnlineMeeting *struct {
		JoinURL string `json:"joinUrl"`
	} `json:"onlineMeeting"`
}

// DecodeGraphEvent parses one Graph event snapshot.
func DecodeGraphEvent(raw []byte) (GraphEvent, error) {
	var g GraphEvent
	if err := json.Unmarshal(raw, &g, json.RejectUnknownMembers(false)); err != nil {
		return GraphEvent{}, fmt.Errorf("decode graph event: %w", err)
	}
	return g, nil
}

// GraphZone returns the IANA name for a Graph time zone. Graph reports both
// Windows and IANA names. Unknown names come back unchanged so that loading
// them fails, rather than silently becoming UTC.
func GraphZone(name string) string {
	if name == "tzone://Microsoft/Utc" {
		return "UTC"
	}
	if iana, ok := windowsZones[name]; ok {
		return iana
	}
	return name
}

// GraphDateTime reads a Graph dateTimeTimeZone value as an instant.
func GraphDateTime(v graphDateTime) (time.Time, error) {
	loc, err := time.LoadLocation(GraphZone(v.TimeZone))
	if err != nil {
		return time.Time{}, fmt.Errorf("graph time zone: %w", err)
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05.9999999", v.DateTime, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("graph date/time: %w", err)
	}
	return t.UTC(), nil
}

func decodeGraphEvent(raw, metadata []byte, calendarZone string) (Event, bool, error) {
	g, err := DecodeGraphEvent(raw)
	if err != nil {
		return Event{}, false, err
	}
	if g.ID == "" {
		return Event{}, false, errors.New("calendar event has no id")
	}
	var meta struct {
		AccountEmail string `json:"account_email"`
		// MailboxEmail is the primary address when the account was added
		// through an alias. Graph reports attendees by primary address.
		MailboxEmail string `json:"mailbox_email"`
		// An exception's original identity has the series' value type and
		// zone, which its own snapshot does not carry.
		SeriesAllDay   *bool  `json:"series_all_day"`
		SeriesTimeZone string `json:"series_time_zone"`
		// Superseded marks an edit that its series no longer lists: it was
		// undone in Outlook. Its snapshot stays archived but overrides nothing.
		Superseded bool `json:"superseded"`
	}
	// Corrupt legacy metadata does not prevent decoding preserved raw evidence.
	_ = json.Unmarshal(metadata, &meta)
	if meta.Superseded {
		return Event{}, false, nil
	}
	selves := map[string]bool{}
	for _, address := range []string{meta.AccountEmail, meta.MailboxEmail} {
		if address = NormalizeEmail(address); address != "" {
			selves[address] = true
		}
	}

	e := Event{ProviderEventID: g.ID, Status: "confirmed", Kind: "single", AllDay: g.IsAllDay, CalendarTimeZone: calendarZone}
	if g.IsCancelled {
		e.Status = "cancelled"
	}
	switch g.Type {
	case "singleInstance", "":
	case "seriesMaster":
		e.Kind, e.SeriesProviderID = "series", g.ID
	case "exception":
		e.Kind, e.SeriesProviderID = "exception", g.SeriesMasterID
	default:
		// Plain occurrences are expanded from their series, never stored.
		return Event{}, false, nil
	}

	zone := ""
	if g.Recurrence != nil {
		zone = GraphZone(g.Recurrence.Range.RecurrenceTimeZone)
	}
	if zone == "" {
		zone = GraphZone(g.OriginalStartTimeZone)
	}
	if zone == "" {
		zone = calendarZone
	}
	if g.IsAllDay {
		// All-day bounds are floating midnights; their date part is the civil date.
		e.StartDate = GraphAllDayDate(g.Start, g.OriginalStartTimeZone, calendarZone)
		e.EndDate = GraphAllDayDate(g.End, g.OriginalStartTimeZone, calendarZone)
		loc, err := loadZone(calendarZone)
		if err != nil {
			return Event{}, false, err
		}
		if e.Start, err = civilStart(e.StartDate, loc); err != nil {
			return Event{}, false, err
		}
		if e.End, err = civilStart(e.EndDate, loc); err != nil {
			return Event{}, false, err
		}
	} else {
		e.EventTimeZone = zone
		if _, err := time.LoadLocation(zone); err != nil && e.Kind == "exception" {
			// A timed edit is placed by its instants and needs no zone of its
			// own. An unknown one would make the whole series unexpandable.
			e.EventTimeZone = ""
		}
		if g.Start.DateTime != "" {
			start, err := GraphDateTime(g.Start)
			if err != nil {
				return Event{}, false, err
			}
			e.Start = &start
		}
		if g.End.DateTime != "" {
			end, err := GraphDateTime(g.End)
			if err != nil {
				return Event{}, false, err
			}
			e.End = &end
		}
	}

	if e.Kind == "exception" {
		original, err := time.Parse(time.RFC3339, g.OriginalStart)
		if err != nil {
			return Event{}, false, fmt.Errorf("calendar original start: %w", err)
		}
		seriesAllDay := g.IsAllDay
		if meta.SeriesAllDay != nil {
			seriesAllDay = *meta.SeriesAllDay
		}
		if seriesAllDay {
			zone := meta.SeriesTimeZone
			if zone == "" {
				zone = g.OriginalStartTimeZone
			}
			date, _ := GraphOriginalDate(original, true, zone, calendarZone)
			e.Original = &calrecur.Identity{Date: date, AllDay: true}
		} else {
			e.Original = &calrecur.Identity{Instant: original.UTC()}
		}
	}
	if e.Kind == "series" && g.Recurrence != nil {
		e.Recurrence = graphRecurrence(g, e)
		if err := alignGraphSeriesStart(&e, calendarZone); err != nil {
			return Event{}, false, err
		}
	}

	organizer := NormalizeEmail(g.Organizer.EmailAddress.Address)
	organizerSeen := false
	for _, a := range g.Attendees {
		email := NormalizeEmail(a.EmailAddress.Address)
		role := "required"
		switch {
		case IsRoomAttendee(email, a.Type == "resource"):
			role = "room"
		case organizer != "" && email == organizer:
			role, organizerSeen = "organizer", true
		case a.Type == "optional":
			role = "optional"
		}
		e.Attendees = append(e.Attendees, Attendee{Email: email, DisplayName: a.EmailAddress.Name, Role: role,
			RSVP: graphRSVP(a.Status.Response), IsCopySelf: selves[email]})
	}
	if organizer != "" && !organizerSeen {
		e.Attendees = append(e.Attendees, Attendee{Email: organizer, DisplayName: g.Organizer.EmailAddress.Name,
			Role: "organizer", RSVP: "unknown", IsCopySelf: g.ResponseStatus.Response == "organizer" || selves[organizer]})
	}
	return e, true, nil
}

// NormalizeEmail trims and lowercases an address.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func graphRSVP(response string) string {
	switch response {
	case "accepted", "declined":
		return response
	case "tentativelyAccepted":
		return "tentative"
	case "notResponded":
		return "needs_action"
	}
	return "unknown"
}

// GraphAllDayDate returns the civil date of an all-day bound. Graph returns
// all-day bounds as floating midnights, whose date part is the date. A bound
// at any other time was converted to the requested zone, so its date is read
// back in the event's zone, or the calendar's.
func GraphAllDayDate(v graphDateTime, eventZone, calendarZone string) string {
	date, clock, _ := strings.Cut(v.DateTime, "T")
	if strings.TrimRight(strings.ReplaceAll(clock, ":", ""), "0.") == "" {
		return date
	}
	instant, err := GraphDateTime(v)
	if err != nil {
		return date
	}
	loc, err := GraphLocation(eventZone, calendarZone)
	if err != nil {
		return date
	}
	return instant.In(loc).Format(time.DateOnly)
}

// GraphOriginalDate returns the date of an occurrence's original start in its
// series. A timed series reads the instant in the series zone. An all-day
// series reads it as GraphAllDayDate reads the series bounds, so that the date
// matches the occurrence it replaces.
func GraphOriginalDate(original time.Time, allDay bool, seriesZone, calendarZone string) (string, error) {
	if allDay {
		at := graphDateTime{DateTime: original.UTC().Format("2006-01-02T15:04:05"), TimeZone: "UTC"}
		return GraphAllDayDate(at, seriesZone, calendarZone), nil
	}
	loc, err := loadZone(GraphZone(seriesZone))
	if err != nil {
		return "", err
	}
	return original.In(loc).Format(time.DateOnly), nil
}

// alignGraphSeriesStart moves a series start that its pattern does not match
// to the first occurrence. Outlook starts the series there, but RFC 5545
// counts DTSTART as an occurrence. The duration and the interval alignment
// stay as they are.
func alignGraphSeriesStart(e *Event, calendarZone string) error {
	if len(e.Recurrence) == 0 || !strings.HasPrefix(e.Recurrence[0], "RRULE:") || e.Start == nil {
		return nil
	}
	loc, start := time.UTC, *e.Start
	if e.AllDay {
		day, err := time.Parse(time.DateOnly, e.StartDate)
		if err != nil {
			return nil //nolint:nilerr // A sparse snapshot has no date to align.
		}
		start = day
	} else {
		var err error
		if loc, err = time.LoadLocation(e.EventTimeZone); err != nil {
			return nil //nolint:nilerr // graphRecurrence already marked the rule unsupported.
		}
		start = start.In(loc)
	}
	opt, err := rrule.StrToROptionInLocation(strings.TrimPrefix(e.Recurrence[0], "RRULE:"), loc)
	if err != nil {
		return nil //nolint:nilerr // calrecur reports an invalid rule when it expands it.
	}
	opt.Dtstart = start
	r, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil //nolint:nilerr // As above.
	}
	first := r.After(start, true)
	if first.IsZero() || first.Equal(start) {
		return nil
	}
	if !e.AllDay {
		moved := first.UTC()
		if e.End != nil {
			end := moved.Add(e.End.Sub(*e.Start))
			e.End = &end
		}
		e.Start = &moved
		return nil
	}
	days := int(first.Sub(start).Hours() / 24)
	calLoc, err := loadZone(calendarZone)
	if err != nil {
		return err
	}
	for _, bound := range []struct {
		date *string
		at   **time.Time
	}{{&e.StartDate, &e.Start}, {&e.EndDate, &e.End}} {
		day, err := time.Parse(time.DateOnly, *bound.date)
		if err != nil {
			continue
		}
		*bound.date = day.AddDate(0, 0, days).Format(time.DateOnly)
		if *bound.at, err = civilStart(*bound.date, calLoc); err != nil {
			return err
		}
	}
	return nil
}

// GraphLocation returns the zone in which an event's clock and day are read:
// the event's own zone, or the calendar's. A UTC or unknown event zone says
// nothing about where the day falls, so it also falls back to the calendar's.
func GraphLocation(eventZone, calendarZone string) (*time.Location, error) {
	zone := GraphZone(eventZone)
	if zone != "" && zone != "UTC" && zone != "Etc/UTC" {
		if loc, err := time.LoadLocation(zone); err == nil {
			return loc, nil
		}
	}
	return loadZone(calendarZone)
}

func loadZone(name string) (*time.Location, error) {
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	return loc, nil
}

func civilStart(date string, loc *time.Location) (*time.Time, error) {
	if date == "" {
		return nil, nil //nolint:nilnil // A sparse snapshot has no bound.
	}
	instant, _, err := calrecur.CivilDateBoundary(date, loc)
	if err != nil {
		return nil, fmt.Errorf("calendar date: %w", err)
	}
	utc := instant.UTC()
	return &utc, nil
}

// monthDaySelector selects a day of the month. Outlook moves a day that a
// month lacks to its last day, which RFC 5545 expresses as the last of the
// candidate days 28 through the requested day.
func monthDaySelector(day int) string {
	if day <= 28 {
		return fmt.Sprintf("BYMONTHDAY=%d", day)
	}
	days := make([]string, 0, day-27)
	for d := 28; d <= day; d++ {
		days = append(days, strconv.Itoa(d))
	}
	return "BYMONTHDAY=" + strings.Join(days, ",") + ";BYSETPOS=-1"
}

var graphWeekdays = map[string]string{
	"sunday": "SU", "monday": "MO", "tuesday": "TU", "wednesday": "WE",
	"thursday": "TH", "friday": "FR", "saturday": "SA",
}

var graphIndexes = map[string]int{"first": 1, "second": 2, "third": 3, "fourth": 4, "last": -1}

// graphUnsupported is a property calrecur rejects, so the series keeps a
// recurrence error instead of a guessed rule.
const graphUnsupported = "X-MSGVAULT-UNSUPPORTED-GRAPH-RECURRENCE:"

// graphRecurrence converts a Graph recurrence to RRULE and EXDATE lines.
func graphRecurrence(g GraphEvent, e Event) []string {
	p, r := g.Recurrence.Pattern, g.Recurrence.Range
	interval := p.Interval
	if interval <= 0 {
		interval = 1
	}
	days := make([]string, 0, len(p.DaysOfWeek))
	for _, d := range p.DaysOfWeek {
		code, ok := graphWeekdays[strings.ToLower(d)]
		if !ok {
			return []string{graphUnsupported + "weekday"}
		}
		days = append(days, code)
	}
	byDay := func() (string, bool) {
		n, ok := graphIndexes[p.Index]
		switch {
		case !ok || len(days) == 0:
			return "", false
		case len(days) == 1:
			return fmt.Sprintf("BYDAY=%d%s", n, days[0]), true
		default:
			return fmt.Sprintf("BYDAY=%s;BYSETPOS=%d", strings.Join(days, ","), n), true
		}
	}
	rule := fmt.Sprintf("RRULE:FREQ=%%s;INTERVAL=%d", interval)
	switch p.Type {
	case "daily":
		rule = fmt.Sprintf(rule, "DAILY")
	case "weekly":
		if len(days) == 0 {
			return []string{graphUnsupported + "weekly"}
		}
		rule = fmt.Sprintf(rule, "WEEKLY") + ";BYDAY=" + strings.Join(days, ",")
		if wkst, ok := graphWeekdays[strings.ToLower(p.FirstDayOfWeek)]; ok {
			rule += ";WKST=" + wkst
		}
	case "absoluteMonthly":
		rule = fmt.Sprintf(rule, "MONTHLY") + ";" + monthDaySelector(p.DayOfMonth)
	case "relativeMonthly":
		sel, ok := byDay()
		if !ok {
			return []string{graphUnsupported + "relativeMonthly"}
		}
		rule = fmt.Sprintf(rule, "MONTHLY") + ";" + sel
	case "absoluteYearly":
		day := p.DayOfMonth
		if p.Month == 2 && day == 29 {
			// Outlook moves February 29 to February 28 in other years. calrecur
			// has no yearly BYSETPOS, but the last day of February is the same.
			day = -1
		}
		rule = fmt.Sprintf(rule, "YEARLY") + fmt.Sprintf(";BYMONTH=%d;BYMONTHDAY=%d", p.Month, day)
	case "relativeYearly":
		sel, ok := byDay()
		if !ok || len(days) != 1 {
			// calrecur has no yearly BYSETPOS.
			return []string{graphUnsupported + "relativeYearly"}
		}
		rule = fmt.Sprintf(rule, "YEARLY") + fmt.Sprintf(";BYMONTH=%d;", p.Month) + sel
	default:
		return []string{graphUnsupported + p.Type}
	}

	var loc *time.Location
	if !e.AllDay {
		var err error
		if loc, err = time.LoadLocation(e.EventTimeZone); err != nil || e.Start == nil {
			return []string{graphUnsupported + "zone"}
		}
	}
	switch r.Type {
	case "numbered":
		if r.NumberOfOccurrences <= 0 {
			return []string{graphUnsupported + "count"}
		}
		rule += fmt.Sprintf(";COUNT=%d", r.NumberOfOccurrences)
	case "endDate":
		last, err := time.Parse(time.DateOnly, r.EndDate)
		if err != nil {
			return []string{graphUnsupported + "until"}
		}
		if e.AllDay {
			rule += ";UNTIL=" + last.Format("20060102")
		} else {
			// The end date is inclusive in the series zone.
			end := time.Date(last.Year(), last.Month(), last.Day(), 23, 59, 59, 0, loc)
			rule += ";UNTIL=" + end.UTC().Format("20060102T150405Z")
		}
	case "noEnd":
	default:
		return []string{graphUnsupported + "range"}
	}

	lines := []string{rule}
	for _, id := range g.CancelledOccurrences {
		// Graph names a cancelled occurrence OID.<item id>.<original local date>.
		date, err := time.Parse(time.DateOnly, id[strings.LastIndex(id, ".")+1:])
		if err != nil {
			return []string{graphUnsupported + "cancellation"}
		}
		if e.AllDay {
			lines = append(lines, "EXDATE;VALUE=DATE:"+date.Format("20060102"))
			continue
		}
		wall := e.Start.In(loc)
		lines = append(lines, "EXDATE:"+time.Date(date.Year(), date.Month(), date.Day(),
			wall.Hour(), wall.Minute(), wall.Second(), 0, time.UTC).Format("20060102T150405"))
	}
	return lines
}
