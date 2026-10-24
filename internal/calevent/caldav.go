package calevent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-ical"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

const CalDAVRawFormat = "caldav_ics"
const CalDAVSourceType = "caldav"

// CalDAVMetadata selects one component of the preserved calendar resource.
type CalDAVMetadata struct {
	EventID      string `json:"event_id"`
	Href         string `json:"href,omitempty"`
	ETag         string `json:"etag,omitempty"`
	AccountEmail string `json:"account_email,omitempty"`
	Status       string `json:"status,omitempty"`
	AllDay       bool   `json:"all_day,omitempty"`
	// OverrideRemoved survives resource deletion to distinguish stale overrides
	// from exceptions that belonged to the last archived snapshot.
	OverrideRemoved bool `json:"override_removed,omitempty"`
}

type CalDAVEvent struct {
	Event                                 Event
	ExplicitAttendees                     []gcalmodel.Attendee
	Summary, OrganizerName                string
	Description, Location, OrganizerEmail string
}

// Keep one decoded resource while its components pass through the projection writer.
var calDAVCache struct {
	sync.Mutex

	hash   [32]byte
	zone   string
	events map[string]Event
}

func decodeCalDAV(raw, metadata []byte, calendarZone string) (Event, bool, error) {
	var meta CalDAVMetadata
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &meta); err != nil {
			return Event{}, false, fmt.Errorf("CalDAV metadata: %w", err)
		}
	}
	calDAVCache.Lock()
	defer calDAVCache.Unlock()
	hash := sha256.Sum256(raw)
	if calDAVCache.events == nil || calDAVCache.hash != hash || calDAVCache.zone != calendarZone {
		events, err := ParseCalDAVResource(context.Background(), raw, calendarZone)
		if err != nil {
			return Event{}, false, err
		}
		calDAVCache.events = make(map[string]Event, len(events))
		for _, item := range events {
			calDAVCache.events[item.Event.ProviderEventID] = item.Event
		}
		calDAVCache.hash, calDAVCache.zone = hash, calendarZone
	}
	e, ok := calDAVCache.events[meta.EventID]
	if ok {
		e.Attendees = slices.Clone(e.Attendees)
		e.Recurrence = slices.Clone(e.Recurrence)
		if e.Start != nil {
			e.Start = new(*e.Start)
		}
		if e.End != nil {
			e.End = new(*e.End)
		}
		if e.Original != nil {
			e.Original = new(*e.Original)
		}
		for i := range e.Attendees {
			a := &e.Attendees[i]
			a.IsCopySelf = a.Email != "" && a.Email == gcalmodel.NormalizeParticipantEmail(meta.AccountEmail)
		}
		return e, true, nil
	}
	return Event{}, false, errors.New("CalDAV component selector missing or ambiguous")
}

// ParseCalDAVResource decodes VEVENTs without expanding recurrence. The caller
// retains raw itself: parsing never reserializes the archived evidence.
func ParseCalDAVResource(ctx context.Context, raw []byte, calendarZone string) (events []CalDAVEvent, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > 2<<20 {
		return nil, errors.New("CalDAV resource exceeds 2 MiB")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("CalDAV resource is not valid UTF-8")
	}
	// go-ical's content-line parser can panic on malformed parameters. Convert
	// that to an import error at this bounded untrusted-input boundary.
	defer func() {
		if recover() != nil {
			events = nil
			err = errors.New("malformed CalDAV iCalendar content")
		}
	}()
	dec := ical.NewDecoder(bytes.NewReader(raw))
	calendar, err := dec.Decode()
	if err != nil {
		return nil, fmt.Errorf("decode CalDAV calendar: %w", err)
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		return nil, errors.New("CalDAV resource has trailing content")
	}
	if version := calendar.Props.Get(ical.PropVersion); version == nil || strings.TrimSpace(version.Value) != "2.0" {
		return nil, errors.New("CalDAV resource must declare iCalendar VERSION 2.0")
	}
	zone := time.UTC
	if calendarZone != "" {
		zone, err = time.LoadLocation(calendarZone)
		if err != nil {
			return nil, fmt.Errorf("CalDAV calendar zone: %w", err)
		}
	}
	if len(calendar.Children) > 1000 {
		return nil, errors.New("CalDAV resource exceeds 1000 components")
	}
	seen := map[string]bool{}
	for _, component := range calendar.Children {
		if component.Name != ical.CompEvent {
			continue
		}
		for _, child := range component.Children {
			if child.Name != ical.CompAlarm {
				return nil, errors.New("unsupported nested CalDAV event component")
			}
		}
		item, err := decodeCalDAVComponent(component, calendarZone, zone)
		if err != nil {
			return nil, err
		}
		if seen[item.Event.ProviderEventID] {
			return nil, errors.New("duplicate CalDAV event identity")
		}
		seen[item.Event.ProviderEventID] = true
		events = append(events, item)
	}
	return events, nil
}

func calDAVID(uid, key string) (string, error) {
	raw, err := json.Marshal([2]string{uid, key})
	return string(raw), err
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

func decodeCalDAVComponent(c *ical.Component, calendarZone string, zone *time.Location) (CalDAVEvent, error) {
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
	item := CalDAVEvent{Event: Event{CalendarTimeZone: calendarZone, Kind: "single", Status: "confirmed"}}
	e := &item.Event
	var uid string
	for _, field := range []struct {
		name string
		out  *string
	}{{"UID", &uid}, {"SUMMARY", &item.Summary}, {"DESCRIPTION", &item.Description}, {"LOCATION", &item.Location}} {
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
	if uid == "" {
		return item, errors.New("CalDAV event has no UID")
	}
	var err error
	e.ProviderEventID, err = calDAVID(uid, "")
	if err != nil {
		return item, fmt.Errorf("CalDAV identity: %w", err)
	}
	if v := p.Get("STATUS"); v != nil {
		e.Status = strings.ToLower(v.Value)
	}
	if v := p.Get("SEQUENCE"); v != nil {
		n, err := v.Int()
		if err != nil || n < 0 {
			return item, errors.New("invalid CalDAV sequence")
		}
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
			e.EventTimeZone = zone.String()
			startDate, err = calDAVCivilDate(start)
			if err == nil {
				s, _, err = calrecur.CivilDateBoundary(startDate, zone)
			}
		} else {
			s, err = calDAVDateTime(start, zone)
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
				if err == nil && endDate <= startDate {
					err = errors.New("all-day DTEND must follow DTSTART")
				}
				if err == nil {
					finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
				}
			} else {
				finish, err = calDAVDateTime(end, zone)
			}
		case duration != nil:
			if e.AllDay {
				var days int
				_, days, err = calDAVDurationEnd(s, duration.Value, true)
				if err == nil {
					endDate, err = calrecur.AddCivilDateDays(startDate, days)
				}
				if err == nil {
					finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
				}
			} else {
				finish, e.RecurrenceDurationDays, err = calDAVDurationEnd(s, duration.Value, false)
			}
		case e.AllDay:
			endDate, err = calrecur.AddCivilDateDays(startDate, 1)
			if err == nil {
				finish, _, err = calrecur.CivilDateBoundary(endDate, zone)
			}
		}
		if err != nil {
			return item, fmt.Errorf("CalDAV end: %w", err)
		}
		if e.AllDay && endDate <= startDate {
			return item, errors.New("CalDAV all-day event end must follow DTSTART")
		}
		if finish.Before(s) {
			return item, errors.New("CalDAV event ends before start")
		}
		if e.AllDay {
			e.StartDate, e.EndDate = startDate, endDate
		}
		s, finish = s.UTC(), finish.UTC()
		e.Start, e.End = &s, &finish
	} else if e.Status != "cancelled" {
		return item, errors.New("CalDAV live event has no DTSTART")
	} else if p.Get("RECURRENCE-ID") == nil {
		return item, errors.New("CalDAV cancelled event has no RECURRENCE-ID")
	}
	for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
		for _, v := range p[name] {
			v.Value = strings.ToUpper(v.Value)
			if (name == "RDATE" || name == "EXDATE") && v.Params.Get("TZID") != "" {
				if _, err := time.LoadLocation(v.Params.Get("TZID")); err != nil {
					return item, fmt.Errorf("CalDAV time zone: %w", err)
				}
				if !strings.EqualFold(v.Params.Get("VALUE"), "PERIOD") {
					for value := range strings.SplitSeq(v.Value, ",") {
						date := v
						date.Value = value
						if _, err := calDAVDateTime(&date, zone); err != nil {
							return item, err
						}
					}
				}
			}
			var line strings.Builder
			line.WriteString(name)
			for _, param := range []string{"TZID", "VALUE"} {
				if value := v.Params.Get(param); value != "" {
					if param == "VALUE" {
						value = strings.ToUpper(value)
					}
					line.WriteString(";" + param + "=" + value)
				}
			}
			e.Recurrence = append(e.Recurrence, line.String()+":"+v.Value)
		}
	}
	if len(e.Recurrence) > 0 {
		e.Kind = "series"
		e.SeriesProviderID = e.ProviderEventID
		if start != nil && !e.AllDay && !strings.HasSuffix(start.Value, "Z") {
			local, err := calDAVDateTime(start, zone)
			if err != nil {
				return item, err
			}
			if local.Format("20060102T150405") != start.Value {
				e.Recurrence = append(e.Recurrence, "DTSTART:"+start.Value)
			}
		}
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
			original, err := calDAVDateTime(rid, zone)
			if err != nil {
				return item, fmt.Errorf("CalDAV RECURRENCE-ID: %w", err)
			}
			id = calrecur.Identity{Instant: original.UTC()}
		}
		if start != nil && id.AllDay != e.AllDay {
			return item, errors.New("CalDAV recurrence value type differs from DTSTART")
		}
		e.Kind, e.SeriesProviderID, e.Original = "exception", e.ProviderEventID, &id
		key := id.Key()
		if !id.AllDay && !strings.HasSuffix(rid.Value, "Z") {
			name := rid.Params.Get("TZID")
			// Persist the producer's local time and zone. Only occurrence matching
			// uses the UTC instant, which follows the reader's current tzdata.
			key = "TZID=" + name + ":" + rid.Value
			if name == "" {
				key = "FLOATING:" + rid.Value
			}
		}
		e.ProviderEventID, err = calDAVID(uid, key)
		if err != nil {
			return item, fmt.Errorf("CalDAV exception identity: %w", err)
		}
	}
	if organizer := p.Get("ORGANIZER"); organizer != nil {
		item.OrganizerEmail = calDAVEmail(organizer.Value)
		item.OrganizerName = organizer.Params.Get("CN")
	}
	var attendees []gcalmodel.Attendee
	for _, v := range p["ATTENDEE"] {
		email := calDAVEmail(v.Value)
		if email == "" {
			continue
		}
		rsvp := strings.ToLower(v.Params.Get("PARTSTAT"))
		if rsvp == "" || rsvp == "needs-action" {
			rsvp = "needsAction"
		}
		attendees = append(attendees, gcalmodel.Attendee{
			Email: email, DisplayName: v.Params.Get("CN"), ResponseStatus: rsvp,
			Optional: strings.EqualFold(v.Params.Get("ROLE"), "OPT-PARTICIPANT") || strings.EqualFold(v.Params.Get("ROLE"), "NON-PARTICIPANT"),
			Resource: strings.EqualFold(v.Params.Get("CUTYPE"), "ROOM") || strings.EqualFold(v.Params.Get("CUTYPE"), "RESOURCE"),
		})
	}
	item.ExplicitAttendees = attendees
	e.Attendees = buildAttendees(gcalmodel.Person{Email: item.OrganizerEmail, DisplayName: item.OrganizerName}, attendees)
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
	return gcalmodel.NormalizeParticipantEmail(mailbox)
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
	end, err := calrecur.AddCivilDays(start, int(days))
	if err != nil {
		return time.Time{}, 0, err
	}
	return end.Add(time.Duration(elapsed) * time.Second), int(days), nil
}

func calDAVDateTime(p *ical.Prop, zone *time.Location) (time.Time, error) {
	if strings.HasSuffix(p.Value, "Z") {
		instant, err := time.Parse("20060102T150405Z", p.Value)
		if err != nil {
			return time.Time{}, fmt.Errorf("CalDAV UTC time: %w", err)
		}
		return instant, nil
	}
	if name := p.Params.Get("TZID"); name != "" {
		var err error
		zone, err = time.LoadLocation(name)
		if err != nil {
			return time.Time{}, fmt.Errorf("CalDAV time zone: %w", err)
		}
	}
	local, err := calrecur.ParseLocalDateTime("20060102T150405", p.Value, zone)
	if err != nil {
		return time.Time{}, err
	}
	return local, nil
}
