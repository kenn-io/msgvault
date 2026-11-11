package calevent

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ical "github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/textutil"
)

// MaxRawBytes bounds the uncompressed raw event used by calendar readers.
const MaxRawBytes = 64 << 20

const ICSRangeRecurrence = "RECURRENCE-ID;RANGE=THISANDFUTURE"

// EmailInviteSQL identifies email-derived events; source merges preserve source types.
func EmailInviteSQL(messageType, sourceType, archiveOnly string) string {
	return fmt.Sprintf("(COALESCE(%s, '') = 'calendar_event' AND COALESCE(%s, '') NOT IN ('gcal', 'ics', 'message_import') AND %s)", messageType, sourceType, archiveOnly)
}

// ICSMetadata records the accepted revision independently of the retained raw.
type ICSMetadata struct {
	Status    string       `json:"status,omitempty"`
	AllDay    bool         `json:"all_day"`
	Sequence  int64        `json:"sequence"`
	Start     string       `json:"start,omitempty"`
	End       string       `json:"end,omitempty"`
	TimeZone  string       `json:"time_zone,omitempty"`
	ICalUID   string       `json:"ical_uid,omitempty"`
	Stamp     time.Time    `json:"ics_stamp"`
	Organizer string       `json:"organizer,omitempty"`
	Details   *ICSRevision `json:"ics_details,omitempty"`
	Range     bool         `json:"ics_range,omitempty"`
}

type ICSRevision struct {
	Sequence int64     `json:"sequence"`
	Stamp    time.Time `json:"stamp"`
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
	method := ""
	if meta.Status == "cancelled" {
		method = "CANCEL"
	}
	components, err := ParseICS(raw, method, calendarZone)
	if err != nil {
		return Event{}, false, err
	}
	if len(components) != 1 {
		return Event{}, false, errors.New("stored ICS requires one VEVENT")
	}
	e := components[0].Event
	e.Recurrence = slices.DeleteFunc(e.Recurrence, func(line string) bool { return line == ICSRangeRecurrence })
	if meta.Range {
		e.Recurrence = append(e.Recurrence, ICSRangeRecurrence)
	}
	if meta.Status != "" {
		e.Status = meta.Status
	}
	return e, true, nil
}

// icsEventZoneComponents keeps only the VTIMEZONE definitions an event references.
func icsEventZoneComponents(cal *ical.Calendar, event *ical.Component) []*ical.Component {
	used := icsEventZones(event)
	return slices.DeleteFunc(slices.Clone(cal.Children), func(comp *ical.Component) bool {
		return comp.Name != ical.CompTimezone || !used[normalizeICSZone(comp.Props.Get("TZID").Value)]
	})
}

func encodeICSEvent(cal *ical.Calendar, zones []*ical.Component, event *ical.Component) ([]byte, error) {
	retained := ical.NewCalendar()
	retained.Props = cal.Props
	retained.Children = append(slices.Clone(zones), event)
	var buf bytes.Buffer
	err := ical.NewEncoder(&buf).Encode(retained)
	return buf.Bytes(), err
}

// ParseICS accepts one complete VCALENDAR. A single-event file is retained
// byte for byte; each event of a larger file keeps its own VEVENT and the
// shared calendar lines. An unsupported zone rejects the calendar.
func ParseICS(raw []byte, method, calendarZone string) ([]ICSComponent, error) {
	return parseICS(raw, method, calendarZone, false)
}

// ParseEmailICS requires the MIME method to agree with the calendar's METHOD.
func ParseEmailICS(raw []byte, mimeMethod, calendarZone string) ([]ICSComponent, error) {
	return parseICS(raw, mimeMethod, calendarZone, true)
}

func parseICS(raw []byte, method, calendarZone string, strict bool) (out []ICSComponent, err error) {
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("malformed iCalendar parameters")
		}
	}()
	// RFC 5545 requires UTF-8, but some producers write another charset
	// without declaring it. Convert it as email text is; the caller still
	// retains the original bytes.
	text := raw
	if !utf8.Valid(raw) {
		text = []byte(textutil.EnsureUTF8(string(raw)))
	}
	decoder := ical.NewDecoder(bytes.NewReader(text))
	cal, err := decoder.Decode()
	if err != nil {
		return nil, fmt.Errorf("decode iCalendar: %w", err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
		return nil, errors.New("calendar has trailing content")
	}
	method, err = icsMethod(cal, method, strict)
	if err != nil {
		return nil, err
	}
	switch method {
	case "REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER":
		if !strict {
			return nil, fmt.Errorf("unsupported calendar method %q", method)
		}
		return nil, nil
	}
	calendarZone = cmp.Or(calendarZone, "UTC")
	loc, err := time.LoadLocation(calendarZone)
	if err != nil {
		return nil, fmt.Errorf("calendar time zone: %w", err)
	}
	events := cal.Events()
	if len(events) == 0 {
		return nil, errors.New("calendar has no VEVENT")
	}
	for index, event := range events {
		comp := event.Component
		zones, err := eventICSZones(cal, comp)
		if err != nil {
			return nil, fmt.Errorf("VEVENT %d: %w", index+1, err)
		}
		retained := raw
		if len(events) > 1 {
			retained, err = encodeICSEvent(cal, icsEventZoneComponents(cal, comp), comp)
			if err != nil {
				return nil, fmt.Errorf("retain VEVENT %d: %w", index+1, err)
			}
		}
		if len(retained) > MaxRawBytes {
			return nil, fmt.Errorf("VEVENT %d: retained calendar exceeds %d bytes", index+1, MaxRawBytes)
		}
		if err := normalizeICSParams(comp.Props, zones); err != nil {
			return nil, fmt.Errorf("VEVENT %d: %w", index+1, err)
		}
		result, err := parseICSEvent(comp, method, loc)
		if err != nil {
			return nil, fmt.Errorf("VEVENT %d: %w", index+1, err)
		}
		result.Raw = bytes.Clone(retained)
		out = append(out, result)
	}
	return out, nil
}

func icsMethod(cal *ical.Calendar, method string, strict bool) (string, error) {
	declared, err := propText(cal.Props, "METHOD")
	if err != nil {
		return "", err
	}
	declared = strings.ToUpper(strings.TrimSpace(declared))
	method = strings.ToUpper(strings.TrimSpace(method))
	if strict && declared != "" && method != "" && declared != method {
		return "", fmt.Errorf("calendar METHOD %q conflicts with MIME method %q", declared, method)
	}
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
	return textutil.ReplaceNUL(text), nil
}

// Resolve Windows IDs and libical's prefixed IANA names.
func normalizeICSParams(props ical.Props, zones map[string]string) error {
	for _, values := range props {
		for _, prop := range values {
			zone := normalizeICSZone(prop.Params.Get("TZID"))
			if custom := zones[zone]; custom != "" {
				zone = custom
			}
			if zone != "" {
				if _, err := LoadZone(zone); err != nil {
					return err
				}
			}
			prop.Params.Set("TZID", zone)
		}
	}
	return nil
}

func normalizeICSZone(zone string) string {
	if iana, ok := strings.CutPrefix(zone, "/freeassociation.sourceforge.net/"); ok {
		zone = strings.TrimPrefix(iana, "Tzfile/")
	}
	if mapped := windowsZones[zone]; mapped != "" {
		if _, err := time.LoadLocation(zone); err != nil {
			return mapped
		}
	}
	return zone
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
		if loc, err = LoadZone(zone); err != nil {
			return time.Time{}, false, fmt.Errorf("load calendar time zone: %w", err)
		}
	}
	v, err := calrecur.ParseLocalDateTime("20060102T150405", p.Value, loc)
	return v, false, err
}

var icsDurationPattern = regexp.MustCompile(`(?i)^\+?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

// icsDuration splits a DURATION into nominal days and exact elapsed time.
func icsDuration(value string) (int, time.Duration, error) {
	m := icsDurationPattern.FindStringSubmatch(value)
	if m == nil || strings.ContainsRune("PpTt", rune(value[len(value)-1])) {
		return 0, 0, errors.New("invalid calendar DURATION")
	}
	var n [5]int64
	for i, s := range m[1:] {
		if s == "" {
			continue
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, 0, errors.New("calendar DURATION exceeds limit")
		}
		n[i] = v
	}
	if n[1] > int64(math.MaxInt) || n[0] > (int64(math.MaxInt)-n[1])/7 {
		return 0, 0, errors.New("calendar DURATION exceeds limit")
	}
	days := n[0]*7 + n[1]
	var secs int64
	for i, multiplier := range [...]int64{3600, 60, 1} {
		if n[i+2] > (int64(math.MaxInt64/time.Second)-secs)/multiplier {
			return 0, 0, errors.New("calendar DURATION exceeds limit")
		}
		secs += n[i+2] * multiplier
	}
	return int(days), time.Duration(secs) * time.Second, nil
}

func parseICSEvent(comp *ical.Component, method string, loc *time.Location) (ICSComponent, error) {
	p := comp.Props
	c := ICSComponent{OrganizerEmail: icsEmail(p.Get("ORGANIZER"))}
	for name, target := range map[string]*string{"UID": &c.UID, "SUMMARY": &c.Summary, "DESCRIPTION": &c.Description, "LOCATION": &c.Location} {
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
	status, err := (&ical.Event{Component: comp}).Status()
	if err != nil {
		return ICSComponent{}, fmt.Errorf("STATUS: %w", err)
	}
	if status != "" {
		e.Status = strings.ToLower(string(status))
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
	end, endAllDay, err := icsTime(p.Get("DTEND"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
	}
	if p.Get("DTEND") != nil && p.Get("DURATION") != nil {
		return ICSComponent{}, errors.New("event has both DTEND and DURATION")
	}
	if !start.IsZero() && p.Get("DTEND") != nil && endAllDay != allDay {
		return ICSComponent{}, errors.New("DTEND value type differs from DTSTART")
	}
	days, elapsed := 0, time.Duration(0)
	if dur := p.Get("DURATION"); dur != nil && end.IsZero() {
		if days, elapsed, err = icsDuration(dur.Value); err != nil {
			return ICSComponent{}, err
		}
		if allDay && elapsed != 0 {
			return ICSComponent{}, errors.New("all-day DURATION must be whole days")
		}
	} else if allDay && e.Status != "cancelled" {
		days = 1
	}
	if !start.IsZero() {
		if prop := p.Get("DTSTART"); !allDay && prop != nil && !strings.HasSuffix(prop.Value, "Z") {
			e.StartLocal = prop.Value
		}
		if !allDay && days > 0 {
			e.DurationDays, e.DurationElapsedNS = days, int64(elapsed)
		}
		hasEnd := !end.IsZero() || p.Get("DURATION") != nil || e.Status != "cancelled"
		if allDay {
			e.AllDay = true
			e.StartDate = start.Format(time.DateOnly)
			if end.IsZero() && hasEnd {
				end = start.AddDate(0, 0, days)
			}
			if hasEnd {
				e.EndDate = end.Format(time.DateOnly)
			}
			if hasEnd && e.EndDate <= e.StartDate {
				return ICSComponent{}, errors.New("event ends before its start")
			}
			if start, _, err = calrecur.CivilDateBoundary(e.StartDate, loc); err != nil {
				return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
			}
			if hasEnd {
				if end, _, err = calrecur.CivilDateBoundary(e.EndDate, loc); err != nil {
					return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
				}
			}
		} else if end.IsZero() && hasEnd {
			// Nominal days keep the wall time across offset changes;
			// hours, minutes and seconds are exact elapsed time.
			end = start
			if days > 0 {
				if end, err = calrecur.AddNominalDuration(start, days, 0); err != nil {
					return ICSComponent{}, fmt.Errorf("DURATION: %w", err)
				}
			}
			end = end.Add(elapsed)
		}
		if hasEnd && end.Before(start) {
			return ICSComponent{}, errors.New("event ends before its start")
		}
		e.EventTimeZone = start.Location().String()
		first := start.UTC()
		e.Start = &first
		if hasEnd {
			last := end.UTC()
			e.End = &last
		}
	}
	recurrenceID := ""
	if rid := p.Get("RECURRENCE-ID"); rid != nil {
		if value := rid.Params.Get("RANGE"); value != "" {
			if !strings.EqualFold(value, "THISANDFUTURE") {
				return ICSComponent{}, errors.New("RECURRENCE-ID: invalid RANGE")
			}
			e.Recurrence = append(e.Recurrence, ICSRangeRecurrence)
		}
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
			custom := customZonePattern.MatchString(prop.Params.Get("TZID"))
			for _, param := range []string{"VALUE", "TZID"} {
				if v := prop.Params.Get(param); v != "" && (param != "TZID" || !custom) {
					line.WriteString(";" + param + "=" + v)
				}
			}
			value := prop.Value
			if (name == "RDATE" || name == "EXDATE") && (prop.Params.Get("TZID") == "" || custom) && prop.ValueType() == ical.ValueDateTime {
				values := strings.Split(value, ",")
				for i, v := range values {
					date := prop
					date.Value = v
					resolved, _, err := icsTime(&date, loc)
					if err != nil {
						return ICSComponent{}, fmt.Errorf("%s: %w", name, err)
					}
					values[i] = resolved.UTC().Format("20060102T150405Z")
				}
				value = strings.Join(values, ",")
			}
			line.WriteString(":" + value)
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
			organizerSeen = true
			role = "organizer"
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
		e.Attendees = append(e.Attendees, Attendee{Email: email, DisplayName: textutil.ReplaceNUL(a.Params.Get("CN")), Role: role, RSVP: rsvp})
	}
	if organizer := p.Get("ORGANIZER"); c.OrganizerEmail != "" && !organizerSeen {
		e.Attendees = append(e.Attendees, Attendee{Email: c.OrganizerEmail, DisplayName: textutil.ReplaceNUL(organizer.Params.Get("CN")), Role: "organizer", RSVP: "unknown"})
	}
	stamp, _, err := icsTime(p.Get("DTSTAMP"), time.UTC)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTAMP: %w", err)
	}
	// Stored projections encode instants as JSON, which allows years 0 to 9999.
	instants := []time.Time{stamp}
	if e.Start != nil {
		instants = append(instants, *e.Start)
	}
	if e.End != nil {
		instants = append(instants, *e.End)
	}
	if e.Original != nil {
		instants = append(instants, e.Original.Instant)
	}
	for _, instant := range instants {
		if year := instant.UTC().Year(); year < 0 || year > 9999 {
			return ICSComponent{}, errors.New("event time is outside years 0 to 9999")
		}
	}
	c.Event, c.Stamp = e, stamp
	return c, nil
}

func icsEmail(p *ical.Prop) string {
	if p == nil || !strings.HasPrefix(strings.ToLower(p.Value), "mailto:") {
		return ""
	}
	return textutil.ReplaceNUL(strings.ToLower(strings.TrimSpace(p.Value[len("mailto:"):])))
}
