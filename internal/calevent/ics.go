package calevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	ical "github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
)

// ICSMetadata records the accepted revision independently of the retained raw.
type ICSMetadata struct {
	Status          string    `json:"status,omitempty"`
	AllDay          bool      `json:"all_day"`
	Sequence        int64     `json:"sequence"`
	Start           string    `json:"start,omitempty"`
	End             string    `json:"end,omitempty"`
	TimeZone        string    `json:"time_zone,omitempty"`
	ICalUID         string    `json:"ical_uid,omitempty"`
	Stamp           time.Time `json:"ics_stamp"`
	InviteOrganizer string    `json:"invite_organizer,omitempty"`
}

type ICSComponent struct {
	Event                                          Event
	UID                                            string
	Sequence                                       int64
	Stamp                                          time.Time
	Summary, Description, Location, OrganizerEmail string
	Raw                                            []byte
}

// ICSMessageID is source-local and stable across input ordering and revisions.
func ICSMessageID(uid, recurrenceID string) string {
	b, _ := json.Marshal([]string{uid, recurrenceID})
	sum := sha256.Sum256(b)
	return "calendar-invite:" + hex.EncodeToString(sum[:])
}

// DecodeICS reads a retained event; metadata carries the accepted status.
func DecodeICS(raw, metadata []byte, calendarZone string) (Event, bool, error) {
	var meta ICSMetadata
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return Event{}, false, fmt.Errorf("decode ICS metadata: %w", err)
	}
	components, err := ParseICS(raw, "", calendarZone)
	if err != nil {
		return Event{}, false, err
	}
	if len(components) != 1 {
		return Event{}, false, errors.New("stored ICS requires one VEVENT")
	}
	e := components[0].Event
	if meta.Status != "" {
		e.Status = meta.Status
	}
	return e, true, nil
}

type icsDocument struct {
	common  string
	events  []string
	closing string
}

// Split once so retaining an event copies only its own lines and shared data.
// Keep raw content lines because the encoder requires optional input fields
// such as DTSTAMP, and retained evidence must preserve omitted properties.
func splitICSDocument(unfolded string) icsDocument {
	var doc icsDocument
	var common strings.Builder
	depth, offset, start := 0, 0, 0
	var component string
	for line := range strings.SplitAfterSeq(unfolded, "\n") {
		name, value, _ := strings.Cut(strings.TrimRight(line, "\r\n"), ":")
		if strings.EqualFold(name, "BEGIN") {
			if depth == 1 {
				start, component = offset, strings.ToUpper(value)
			}
			depth++
		} else if strings.EqualFold(name, "END") {
			depth--
			if depth == 0 {
				doc.closing = line
			}
			if depth == 1 {
				block := unfolded[start : offset+len(line)]
				switch component {
				case "VEVENT":
					doc.events = append(doc.events, block)
				case "VTIMEZONE":
					common.WriteString(block)
				}
				component = ""
			}
		}
		if depth == 1 && component == "" && !strings.EqualFold(name, "END") {
			common.WriteString(line)
		}
		offset += len(line)
	}
	doc.common = common.String()
	return doc
}

// ParseICS accepts one complete VCALENDAR. A single-event file is retained
// byte for byte; each event of a larger file keeps its own VEVENT and the
// shared calendar lines.
func ParseICS(raw []byte, method, calendarZone string) (out []ICSComponent, err error) {
	unfolded := strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "").Replace(string(raw))
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("malformed iCalendar parameters")
		}
	}()
	decoder := ical.NewDecoder(strings.NewReader(unfolded))
	cal, err := decoder.Decode()
	if err != nil {
		return nil, fmt.Errorf("decode iCalendar: %w", err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
		return nil, errors.New("calendar has trailing content")
	}
	method, err = icsMethod(cal, method)
	if err != nil {
		return nil, err
	}
	switch method {
	case "REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER":
		return nil, nil
	}
	if calendarZone == "" {
		calendarZone = "UTC"
	}
	loc, err := time.LoadLocation(calendarZone)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	doc := splitICSDocument(unfolded)
	index := 0
	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent {
			continue
		}
		normalizeICSParams(comp.Props)
		result, err := parseICSEvent(comp, method, loc)
		if err != nil {
			return nil, fmt.Errorf("VEVENT %d: %w", index+1, err)
		}
		if len(doc.events) == 1 {
			result.Raw = bytes.Clone(raw)
		} else if index < len(doc.events) {
			result.Raw = []byte(doc.common + doc.events[index] + doc.closing)
		}
		out = append(out, result)
		index++
	}
	if index == 0 {
		return nil, errors.New("calendar has no VEVENT")
	}
	return out, nil
}

func icsMethod(cal *ical.Calendar, method string) (string, error) {
	declared, err := propText(cal.Props, "METHOD")
	if err != nil {
		return "", err
	}
	declared = strings.ToUpper(strings.TrimSpace(declared))
	method = strings.ToUpper(strings.TrimSpace(method))
	if declared != "" {
		method = declared
	}
	switch method {
	case "", "REQUEST", "PUBLISH", "CANCEL", "REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER":
		return method, nil
	default:
		return "", fmt.Errorf("unsupported calendar method %q", method)
	}
}

func propText(props ical.Props, name string) (string, error) {
	text, err := props.Text(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

// Outlook names zones by their Windows IDs.
func normalizeICSParams(props ical.Props) {
	for _, values := range props {
		for _, prop := range values {
			zone := prop.Params.Get("TZID")
			if mapped := windowsZones[zone]; mapped != "" {
				if _, err := time.LoadLocation(zone); err != nil {
					prop.Params.Set("TZID", mapped)
				}
			}
		}
	}
}

func icsTime(p *ical.Prop, loc *time.Location) (time.Time, bool, error) {
	if p == nil {
		return time.Time{}, false, nil
	}
	if date := p.ValueType() == ical.ValueDate; date || strings.HasSuffix(p.Value, "Z") {
		layout := "20060102T150405Z"
		if date {
			layout = "20060102"
		}
		v, err := time.Parse(layout, p.Value)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse calendar time: %w", err)
		}
		return v, date, nil
	}
	if zone := p.Params.Get("TZID"); zone != "" {
		var err error
		if loc, err = time.LoadLocation(zone); err != nil {
			return time.Time{}, false, fmt.Errorf("load calendar time zone: %w", err)
		}
	}
	v, err := calrecur.ParseLocalDateTime("20060102T150405", p.Value, loc)
	return v, false, err
}

var icsDurationPattern = regexp.MustCompile(`^\+?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

// icsDuration splits a DURATION into nominal days and exact elapsed time.
func icsDuration(value string) (int, time.Duration, error) {
	m := icsDurationPattern.FindStringSubmatch(value)
	if m == nil {
		return 0, 0, errors.New("invalid calendar DURATION")
	}
	var n [5]int64
	for i, s := range m[1:] {
		if s == "" {
			continue
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v > 36600*86400 {
			return 0, 0, errors.New("calendar DURATION exceeds limit")
		}
		n[i] = v
	}
	return int(n[0]*7 + n[1]), time.Duration(n[2]*3600+n[3]*60+n[4]) * time.Second, nil
}

func parseICSEvent(comp *ical.Component, method string, loc *time.Location) (ICSComponent, error) {
	p := comp.Props
	c := ICSComponent{OrganizerEmail: icsEmail(p.Get("ORGANIZER"))}
	var status string
	for name, target := range map[string]*string{"UID": &c.UID, "SUMMARY": &c.Summary, "DESCRIPTION": &c.Description, "LOCATION": &c.Location, "STATUS": &status} {
		text, err := propText(p, name)
		if err != nil {
			return ICSComponent{}, err
		}
		*target = text
	}

	e := Event{Kind: "single", Status: "confirmed", CalendarTimeZone: loc.String()}
	if strings.TrimSpace(c.UID) == "" {
		return ICSComponent{}, errors.New("event has no UID")
	}
	if seq := p.Get("SEQUENCE"); seq != nil {
		n, err := strconv.ParseInt(seq.Value, 10, 64)
		if err != nil || n < 0 {
			return ICSComponent{}, errors.New("invalid SEQUENCE")
		}
		c.Sequence = n
	}
	switch strings.ToUpper(status) {
	case "TENTATIVE":
		e.Status = "tentative"
	case "CANCELLED":
		e.Status = "cancelled"
	}
	if method == "CANCEL" {
		e.Status = "cancelled"
	}
	start, allDay, err := icsTime(p.Get("DTSTART"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
	}
	if start.IsZero() && e.Status != "cancelled" {
		return ICSComponent{}, errors.New("active event requires DTSTART")
	}
	end, _, err := icsTime(p.Get("DTEND"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
	}
	if !start.IsZero() {
		days, elapsed := 0, time.Duration(0)
		if dur := p.Get("DURATION"); dur != nil && end.IsZero() {
			if days, elapsed, err = icsDuration(dur.Value); err != nil {
				return ICSComponent{}, err
			}
		} else if end.IsZero() && allDay {
			days = 1
		}
		if allDay {
			e.AllDay = true
			e.StartDate = start.Format(time.DateOnly)
			if end.IsZero() {
				end = start.AddDate(0, 0, days)
			}
			e.EndDate = end.Format(time.DateOnly)
			if e.EndDate <= e.StartDate {
				return ICSComponent{}, errors.New("event ends before its start")
			}
			if start, _, err = calrecur.CivilDateBoundary(e.StartDate, loc); err != nil {
				return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
			}
			if end, _, err = calrecur.CivilDateBoundary(e.EndDate, loc); err != nil {
				return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
			}
		} else if end.IsZero() {
			// Nominal days keep the wall time across offset changes;
			// hours, minutes and seconds are exact elapsed time.
			end = start
			if days > 0 {
				wall := time.Date(start.Year(), start.Month(), start.Day()+days, start.Hour(), start.Minute(), start.Second(), 0, time.UTC)
				if end, err = calrecur.ParseLocalDateTime("20060102T150405", wall.Format("20060102T150405"), start.Location()); err != nil {
					return ICSComponent{}, fmt.Errorf("DURATION: %w", err)
				}
			}
			end = end.Add(elapsed)
		}
		if end.Before(start) {
			return ICSComponent{}, errors.New("event ends before its start")
		}
		e.EventTimeZone = start.Location().String()
		first, last := start.UTC(), end.UTC()
		e.Start, e.End = &first, &last
	}
	recurrenceID := ""
	if rid := p.Get("RECURRENCE-ID"); rid != nil {
		original, date, err := icsTime(rid, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("RECURRENCE-ID: %w", err)
		}
		e.Kind = "exception"
		e.Original = &calrecur.Identity{Instant: original.UTC(), AllDay: date}
		if date {
			e.Original.Date = original.Format(time.DateOnly)
		}
		if start.IsZero() {
			e.AllDay = date
		}
		recurrenceID = e.Original.Key()
	}
	e.ProviderEventID = ICSMessageID(c.UID, recurrenceID)
	if e.Kind == "exception" {
		e.SeriesProviderID = ICSMessageID(c.UID, "")
	}
	for _, name := range []string{"RRULE", "RDATE", "EXDATE", "EXRULE"} {
		for _, prop := range p.Values(name) {
			var line strings.Builder
			line.WriteString(name)
			for _, param := range []string{"VALUE", "TZID"} {
				if v := prop.Params.Get(param); v != "" {
					line.WriteString(";" + param + "=" + v)
				}
			}
			line.WriteString(":" + prop.Value)
			e.Recurrence = append(e.Recurrence, line.String())
		}
	}
	if len(e.Recurrence) > 0 && e.Kind != "exception" {
		e.Kind = "series"
		e.SeriesProviderID = e.ProviderEventID
	}
	organizerSeen := false
	for _, a := range p.Values("ATTENDEE") {
		email := icsEmail(&a)
		role := "required"
		switch {
		case IsRoomAttendee(email, strings.EqualFold(a.Params.Get("CUTYPE"), "RESOURCE") || strings.EqualFold(a.Params.Get("CUTYPE"), "ROOM")):
			role = "room"
		case strings.EqualFold(a.Params.Get("ROLE"), "OPT-PARTICIPANT"):
			role = "optional"
		}
		if c.OrganizerEmail != "" && email == c.OrganizerEmail {
			role = "organizer"
			organizerSeen = true
		}
		rsvp := "unknown"
		switch strings.ToUpper(a.Params.Get("PARTSTAT")) {
		case "ACCEPTED":
			rsvp = "accepted"
		case "DECLINED":
			rsvp = "declined"
		case "TENTATIVE":
			rsvp = "tentative"
		case "NEEDS-ACTION":
			rsvp = "needs_action"
		}
		e.Attendees = append(e.Attendees, Attendee{Email: email, DisplayName: a.Params.Get("CN"), Role: role, RSVP: rsvp})
	}
	if organizer := p.Get("ORGANIZER"); c.OrganizerEmail != "" && !organizerSeen {
		e.Attendees = append(e.Attendees, Attendee{Email: c.OrganizerEmail, DisplayName: organizer.Params.Get("CN"), Role: "organizer", RSVP: "unknown"})
	}
	stamp, _, err := icsTime(p.Get("DTSTAMP"), time.UTC)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTAMP: %w", err)
	}
	c.Event, c.Stamp = e, stamp
	return c, nil
}

func icsEmail(p *ical.Prop) string {
	if p == nil || !strings.HasPrefix(strings.ToLower(p.Value), "mailto:") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(p.Value[len("mailto:"):]))
}
