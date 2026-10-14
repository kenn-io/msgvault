package calevent

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
)

const CalDAVRawFormat = "caldav_ics"

// CalDAVMetadata selects one component of the preserved calendar resource.
type CalDAVMetadata struct {
	EventID      string `json:"event_id"`
	Href         string `json:"href,omitempty"`
	ETag         string `json:"etag,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	Status       string `json:"status,omitempty"`
	AllDay       bool   `json:"all_day,omitempty"`
}

type CalDAVEvent struct {
	Event                  Event
	Summary, OrganizerName string
	Description            string
}

func init() { Register(CalDAVRawFormat, decodeCalDAV) }

func decodeCalDAV(raw, metadata []byte, sc SourceContext) (Event, bool, error) {
	var meta CalDAVMetadata
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &meta); err != nil {
			return Event{}, false, fmt.Errorf("CalDAV metadata: %w", err)
		}
	}
	events, err := ParseCalDAVResource(raw, sc)
	if err != nil {
		return Event{}, false, err
	}
	for _, item := range events {
		if item.Event.ProviderEventID != meta.EventID && (meta.EventID != "" || len(events) != 1) {
			continue
		}
		e := item.Event
		for i := range e.Attendees {
			a := &e.Attendees[i]
			a.IsCopySelf = a.Email != "" && a.Email == normalizeEmail(meta.AccountEmail)
			if a.IsCopySelf {
				e.CopySelfRSVP = a.RSVP
			}
		}
		return e, true, nil
	}
	return Event{}, false, errors.New("CalDAV component selector missing or ambiguous")
}

// ParseCalDAVResource decodes VEVENTs without expanding recurrence. The caller
// retains raw itself: parsing never reserializes the archived evidence.
func ParseCalDAVResource(raw []byte, sc SourceContext) (events []CalDAVEvent, err error) {
	_, events, err = parseCalDAVResource(raw, sc)
	return events, err
}

// CalDAVResourceScoper can serialize one VEVENT from a preserved VCALENDAR.
// The original bytes remain untouched in the source archive; callers use the
// scoped form when copying only one event across an archive boundary.
type CalDAVResourceScoper struct {
	calendar *ical.Calendar
	events   map[string]*ical.Component
}

// NewCalDAVResourceScoper parses a resource once so callers copying multiple
// selected events from the same resource can scope each payload without
// repeatedly decoding the full resource.
func NewCalDAVResourceScoper(raw []byte, sc SourceContext) (*CalDAVResourceScoper, error) {
	calendar, events, err := parseCalDAVResource(raw, sc)
	if err != nil {
		return nil, err
	}
	components := make(map[string]*ical.Component, len(events))
	index := 0
	for _, component := range calendar.Children {
		if component.Name != ical.CompEvent {
			continue
		}
		if index >= len(events) {
			return nil, errors.New("CalDAV event component index mismatch")
		}
		components[events[index].Event.ProviderEventID] = component
		index++
	}
	if index != len(events) {
		return nil, errors.New("CalDAV event component index mismatch")
	}
	return &CalDAVResourceScoper{calendar: calendar, events: components}, nil
}

// ForEvent returns a VCALENDAR containing only the selected VEVENT and its
// timezone definitions. Other event, task, journal, and free/busy components
// are omitted. Only structural calendar properties are retained so calendar
// names, URLs, and custom metadata do not cross the subset boundary.
func (resource *CalDAVResourceScoper) ForEvent(eventID string) ([]byte, error) {
	selected := resource.events[eventID]
	if selected == nil && eventID == "" && len(resource.events) == 1 {
		for _, component := range resource.events {
			selected = component
		}
	}
	if selected == nil {
		return nil, errors.New("CalDAV component selector missing or ambiguous")
	}

	scoped := ical.NewCalendar()
	for _, name := range []string{ical.PropProductID, ical.PropVersion, ical.PropCalendarScale, ical.PropMethod} {
		if properties := resource.calendar.Props[name]; len(properties) > 0 {
			scoped.Props[name] = append([]ical.Prop(nil), properties...)
		}
	}
	for _, component := range resource.calendar.Children {
		if component.Name == ical.CompTimezone || component == selected {
			scoped.Children = append(scoped.Children, component)
		}
	}

	var output bytes.Buffer
	if err := ical.NewEncoder(&output).Encode(scoped); err != nil {
		return nil, fmt.Errorf("encode scoped CalDAV resource: %w", err)
	}
	return output.Bytes(), nil
}

func parseCalDAVResource(raw []byte, sc SourceContext) (calendar *ical.Calendar, events []CalDAVEvent, err error) {
	if len(raw) > 2<<20 {
		return nil, nil, errors.New("CalDAV resource exceeds 2 MiB")
	}
	// go-ical's content-line parser can panic on malformed parameters. Convert
	// that to an import error at this bounded untrusted-input boundary.
	defer func() {
		if recover() != nil {
			calendar = nil
			events = nil
			err = errors.New("malformed CalDAV iCalendar content")
		}
	}()
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	unfolded := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			if len(unfolded) == 0 {
				return nil, nil, errors.New("invalid iCalendar folding")
			}
			unfolded[len(unfolded)-1] += line[1:]
		} else if line != "" {
			unfolded = append(unfolded, line)
		}
		if len(unfolded) > 20000 || (len(unfolded) > 0 && len(unfolded[len(unfolded)-1]) > 16384) {
			return nil, nil, errors.New("CalDAV content-line limit exceeded")
		}
	}
	depth := 0
	for _, line := range unfolded {
		upper := strings.ToUpper(line)
		if strings.HasPrefix(upper, "BEGIN:") {
			depth++
			if depth > 8 {
				return nil, nil, errors.New("CalDAV component nesting limit exceeded")
			}
		}
		if strings.HasPrefix(upper, "END:") {
			depth--
		}
		if depth < 0 {
			return nil, nil, errors.New("invalid CalDAV component nesting")
		}
	}
	if depth != 0 {
		return nil, nil, errors.New("unclosed CalDAV component")
	}
	dec := ical.NewDecoder(bytes.NewReader(raw))
	calendar, err = dec.Decode()
	if err != nil {
		return nil, nil, fmt.Errorf("decode CalDAV calendar: %w", err)
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("CalDAV resource has trailing content")
	}
	zone := time.UTC
	if sc.CalendarTimeZone != "" {
		zone, err = time.LoadLocation(sc.CalendarTimeZone)
		if err != nil {
			return nil, nil, fmt.Errorf("CalDAV calendar zone: %w", err)
		}
	}
	seen := map[string]bool{}
	for _, component := range calendar.Children {
		if component.Name != ical.CompEvent {
			continue
		}
		if len(events) >= 1000 {
			return nil, nil, errors.New("CalDAV resource exceeds 1000 events")
		}
		for _, child := range component.Children {
			if child.Name != ical.CompAlarm {
				return nil, nil, errors.New("unsupported nested CalDAV event component")
			}
		}
		item, err := decodeCalDAVComponent(component, sc, zone)
		if err != nil {
			return nil, nil, err
		}
		if seen[item.Event.ProviderEventID] {
			return nil, nil, errors.New("duplicate CalDAV event identity")
		}
		seen[item.Event.ProviderEventID] = true
		events = append(events, item)
	}
	return calendar, events, nil
}

func calDAVID(uid, key string) string {
	raw, _ := json.Marshal([2]string{uid, key})
	return string(raw)
}
func dateProp(p *ical.Prop) bool {
	return p != nil && (p.ValueType() == ical.ValueDate || (p.ValueType() == ical.ValueDefault && len(p.Value) == 8))
}

func calDAVCivilDate(p *ical.Prop) (string, error) {
	date, err := time.Parse("20060102", p.Value)
	if err != nil {
		return "", fmt.Errorf("invalid DATE value: %w", err)
	}
	return date.Format(time.DateOnly), nil
}

func decodeCalDAVComponent(c *ical.Component, sc SourceContext, zone *time.Location) (CalDAVEvent, error) {
	p := c.Props
	// RFC 5545 enumerated parameter values are case-insensitive.
	for _, values := range p {
		for i := range values {
			if value := values[i].Params.Get("VALUE"); value != "" {
				values[i].Params.Set("VALUE", strings.ToUpper(value))
			}
		}
	}
	for _, name := range []string{"DTSTART", "DTEND", "RECURRENCE-ID", "RDATE", "EXDATE"} {
		for _, prop := range p[name] {
			if strings.EqualFold(prop.Params.Get("TZID"), "Local") {
				return CalDAVEvent{}, fmt.Errorf("CalDAV %s TZID cannot be Local", name)
			}
		}
	}
	for _, name := range []string{"UID", "RECURRENCE-ID", "DTSTART", "DTEND", "DURATION", "SEQUENCE", "STATUS"} {
		if len(p[name]) > 1 {
			return CalDAVEvent{}, fmt.Errorf("duplicate CalDAV %s", name)
		}
	}
	item := CalDAVEvent{Event: Event{Provider: "caldav", Namespace: sc.CalendarID, CalendarID: sc.CalendarID, CalendarTimeZone: sc.CalendarTimeZone, Kind: "single", Status: "confirmed"}}
	e := &item.Event
	for _, field := range []struct {
		name string
		out  *string
	}{{"UID", &e.ICalUID}, {"SUMMARY", &item.Summary}, {"DESCRIPTION", &e.Description}, {"LOCATION", &e.Location}, {"CLASS", &e.Visibility}, {"TRANSP", &e.Transparency}} {
		prop := p.Get(field.name)
		if prop == nil {
			continue
		}
		values, err := prop.TextList()
		if err != nil {
			return item, fmt.Errorf("CalDAV %s: %w", field.name, err)
		}
		*field.out = strings.Join(values, ",")
	}
	item.Description = e.Description
	if e.ICalUID == "" {
		return item, errors.New("CalDAV event has no UID")
	}
	e.ProviderEventID = calDAVID(e.ICalUID, "")
	if v := p.Get("STATUS"); v != nil {
		e.Status = strings.ToLower(v.Value)
	}
	if v := p.Get("SEQUENCE"); v != nil {
		n, err := v.Int()
		if err != nil || n < 0 {
			return item, errors.New("invalid CalDAV sequence")
		}
		e.Sequence = int64(n)
	}
	start := p.Get("DTSTART")
	end := p.Get("DTEND")
	duration := p.Get("DURATION")
	if start != nil {
		e.AllDay = dateProp(start)
		e.EventTimeZone = start.Params.Get("TZID")
		if strings.HasSuffix(start.Value, "Z") {
			e.EventTimeZone = "UTC"
		}
		var startDate, endDate string
		var s time.Time
		var err error
		if e.AllDay {
			startDate, err = calDAVCivilDate(start)
			if err == nil {
				s, err = calrecur.CivilDateStart(startDate, zone)
			}
		} else {
			s, err = start.DateTime(zone)
		}
		if err != nil {
			return item, fmt.Errorf("CalDAV DTSTART: %w", err)
		}
		finish := s
		if end != nil && duration != nil {
			return item, errors.New("CalDAV event has both DTEND and DURATION")
		}
		switch {
		case end != nil:
			if dateProp(end) != e.AllDay {
				return item, errors.New("CalDAV bound value types differ")
			}
			if e.AllDay {
				endDate, err = calDAVCivilDate(end)
				if err == nil {
					finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
				}
			} else {
				finish, err = end.DateTime(zone)
			}
		case duration != nil:
			if e.AllDay {
				var days int
				_, days, err = calDAVDurationEnd(s, duration.Value, true)
				if err == nil {
					endDate, err = calrecur.AddCivilDays(startDate, days)
				}
				if err == nil {
					finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
				}
			} else {
				var days int
				finish, days, err = calDAVDurationEnd(s, duration.Value, false)
				e.RecurrenceDurationDays = days
			}
		case e.AllDay:
			endDate, err = calrecur.AddCivilDays(startDate, 1)
			if err == nil {
				finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
			}
		}
		if err != nil {
			return item, fmt.Errorf("CalDAV end: %w", err)
		}
		if e.AllDay && endDate < startDate || finish.Before(s) {
			return item, errors.New("CalDAV event ends before start")
		}
		if e.AllDay {
			e.StartDate, e.EndDate = startDate, endDate
		}
		s, finish = s.UTC(), finish.UTC()
		e.Start, e.End = &s, &finish
		e.HasBounds = true
	} else if e.Status != "cancelled" {
		return item, errors.New("CalDAV live event has no DTSTART")
	}
	for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
		for _, v := range p[name] {
			var line strings.Builder
			line.WriteString(name)
			for _, param := range []string{"TZID", "VALUE"} {
				if value := v.Params.Get(param); value != "" {
					line.WriteString(";" + param + "=" + value)
				}
			}
			e.Recurrence = append(e.Recurrence, line.String()+":"+v.Value)
		}
	}
	if len(e.Recurrence) > 0 {
		e.Kind = "series"
		e.SeriesProviderID = e.ProviderEventID
	}
	if rid := p.Get("RECURRENCE-ID"); rid != nil {
		if rid.Params.Get("RANGE") != "" {
			return item, errors.New("CalDAV recurrence RANGE is unsupported")
		}
		var id calrecur.Identity
		if dateProp(rid) {
			date, err := calDAVCivilDate(rid)
			if err != nil {
				return item, fmt.Errorf("CalDAV RECURRENCE-ID: %w", err)
			}
			id = calrecur.Identity{Date: date, AllDay: true}
			if start == nil {
				e.AllDay = true
			}
		} else {
			original, err := rid.DateTime(zone)
			if err != nil {
				return item, fmt.Errorf("CalDAV RECURRENCE-ID: %w", err)
			}
			id = calrecur.Identity{Instant: original.UTC()}
		}
		if start != nil && id.AllDay != e.AllDay {
			return item, errors.New("CalDAV recurrence value type differs from DTSTART")
		}
		e.Kind, e.SeriesProviderID, e.Original = "exception", calDAVID(e.ICalUID, ""), &id
		e.ProviderEventID = calDAVID(e.ICalUID, id.Key())
	}
	if organizer := p.Get("ORGANIZER"); organizer != nil {
		e.OrganizerEmail = calDAVEmail(organizer.Value)
		item.OrganizerName = organizer.Params.Get("CN")
	}
	for _, v := range p["ATTENDEE"] {
		email := calDAVEmail(v.Value)
		role := "required"
		switch {
		case IsRoomAttendee(email, strings.EqualFold(v.Params.Get("CUTYPE"), "ROOM") || strings.EqualFold(v.Params.Get("CUTYPE"), "RESOURCE")):
			role = "room"
		case strings.EqualFold(v.Params.Get("ROLE"), "CHAIR"):
			role = "organizer"
		case strings.EqualFold(v.Params.Get("ROLE"), "OPT-PARTICIPANT"):
			role = "optional"
		}
		rsvp := strings.ToLower(v.Params.Get("PARTSTAT"))
		if rsvp == "" || rsvp == "needs-action" {
			rsvp = "unknown"
		}
		e.Attendees = append(e.Attendees, Attendee{Email: email, DisplayName: v.Params.Get("CN"), Role: role, RSVP: rsvp})
	}
	for _, name := range []string{"CONFERENCE", "URL"} {
		v := p.Get(name)
		if v == nil {
			continue
		}
		if key := JoinKey(v.Value); key != "" {
			e.JoinURL, e.JoinKey = v.Value, key
			break
		}
		if name == "CONFERENCE" && validConferenceJoinURL(v.Value) {
			e.JoinURL = v.Value
			break
		}
	}
	if len(e.Description) > 4096 {
		n := 4096
		for !utf8.RuneStart(e.Description[n]) {
			n--
		}
		e.Description = e.Description[:n]
		e.DescriptionTruncated = true
	}
	return item, nil
}
func calDAVEmail(value string) string {
	uri, err := url.Parse(value)
	if err != nil || !strings.EqualFold(uri.Scheme, "mailto") {
		return ""
	}
	mailbox := uri.Opaque
	if mailbox == "" {
		mailbox = uri.Path
	}
	mailbox, err = url.PathUnescape(mailbox)
	if err != nil {
		return ""
	}
	return normalizeEmail(mailbox)
}

var calDAVDurationPattern = regexp.MustCompile(`^\+?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

// Days/weeks are nominal civil units; hours/minutes/seconds are elapsed units.
func calDAVDurationEnd(start time.Time, value string, allDay bool) (time.Time, int, error) {
	parts := calDAVDurationPattern.FindStringSubmatch(strings.ToUpper(value))
	if parts == nil {
		return time.Time{}, 0, errors.New("invalid CalDAV duration")
	}
	units := [5]int64{}
	present := false
	for i, part := range parts[1:] {
		if part == "" {
			continue
		}
		present = true
		n, err := strconv.ParseInt(part, 10, 32)
		if err != nil {
			return time.Time{}, 0, errors.New("CalDAV duration exceeds limit")
		}
		units[i] = n
	}
	if !present || (allDay && (units[2] != 0 || units[3] != 0 || units[4] != 0)) {
		return time.Time{}, 0, errors.New("invalid CalDAV duration")
	}
	days := units[0]*7 + units[1]
	if days > 3650000 {
		return time.Time{}, 0, errors.New("CalDAV duration exceeds limit")
	}
	elapsed := units[2]*3600 + units[3]*60 + units[4]
	// time.Duration is signed nanoseconds; avoid overflow before conversion.
	if elapsed > (1<<63-1)/int64(time.Second) {
		return time.Time{}, 0, errors.New("CalDAV duration exceeds limit")
	}
	return start.AddDate(0, 0, int(days)).Add(time.Duration(elapsed) * time.Second), int(days), nil
}
