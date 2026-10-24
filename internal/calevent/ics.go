package calevent

import (
	"bytes"
	"context"
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
	"unicode/utf8"

	ical "github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
)

const MaxICSBytes = 32 << 20
const maxICSEvents = 10000

// ICSMetadata records the accepted revision independently of the retained raw.
type ICSMetadata struct {
	Version         int       `json:"ics_version"`
	Superseded      bool      `json:"ics_superseded,omitempty"`
	Status          string    `json:"status,omitempty"`
	AllDay          bool      `json:"all_day"`
	Sequence        int64     `json:"ics_sequence"`
	Stamp           time.Time `json:"ics_stamp"`
	InviteOrganizer string    `json:"invite_organizer,omitempty"`
}

// ParseICSMetadata rejects formats whose revision semantics are unknown.
func ParseICSMetadata(raw []byte) (ICSMetadata, error) {
	var meta ICSMetadata
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, err
	}
	if meta.Version != 1 {
		return meta, fmt.Errorf("unsupported ICS metadata version %d", meta.Version)
	}
	return meta, nil
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

// DecodeICS reads a retained event within the caller's parsing budget.
func DecodeICS(ctx context.Context, raw, metadata []byte, calendarZone string, budget *ICSParseBudget) (Event, bool, error) {
	meta, err := ParseICSMetadata(metadata)
	if err != nil {
		return Event{}, false, err
	}
	if meta.Superseded {
		return Event{}, false, nil
	}
	components, _, err := ParseICSWithDiagnostics(ctx, raw, "", calendarZone, budget)
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

func unfoldICS(raw []byte) string {
	return strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "").Replace(string(raw))
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

func (doc icsDocument) retain(cal *ical.Calendar, raw []byte, index, remaining int) ([]byte, error) {
	if index < 0 || index >= len(doc.events) {
		return nil, errors.New("selected ICS event is unavailable")
	}
	size := len(doc.common) + len(doc.events[index]) + len(doc.closing)
	if len(cal.Children) == 1 {
		size = len(raw)
	}
	if size > remaining {
		return nil, errors.New("calendar retained event data exceeds 32 MiB limit")
	}
	if len(cal.Children) == 1 {
		return bytes.Clone(raw), nil
	}
	return []byte(doc.common + doc.events[index] + doc.closing), nil
}

// ParseICS accepts one complete VCALENDAR.
func ParseICS(raw []byte, method, calendarZone string) ([]ICSComponent, error) {
	components, _, err := ParseICSWithDiagnostics(context.Background(), raw, method, calendarZone, NewICSParseBudget())
	return components, err
}

var errICSGuestRemoval = errors.New("guest removal ignored")

func parseICSComponent(ctx context.Context, cal *ical.Calendar, comp *ical.Component, method string, loc *time.Location, budget *ICSParseBudget) (ICSComponent, error) {
	if err := validateICSTimeZones(ctx, cal, comp, budget); err != nil {
		return ICSComponent{}, err
	}
	normalizeICSParams(comp.Props)
	return parseICSEvent(comp, method, loc)
}

// ParseICSWithDiagnostics reports skipped components alongside accepted events.
// The lexical pass bounds work before the dependency parses the document.
func ParseICSWithDiagnostics(ctx context.Context, raw []byte, method, calendarZone string, budget *ICSParseBudget) (out []ICSComponent, diagnostics []string, err error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	unfolded, err := budget.preflight(raw)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("malformed iCalendar parameters")
		}
	}()
	decoder := ical.NewDecoder(strings.NewReader(unfolded))
	cal, err := decoder.Decode()
	if err != nil {
		return nil, nil, fmt.Errorf("decode iCalendar: %w", err)
	}
	if _, err := decoder.Decode(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("calendar has trailing content")
	}
	normalizeICSParams(cal.Props)
	method, err = icsMethod(cal, method)
	if err != nil {
		return nil, nil, err
	}
	switch method {
	case "REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER":
		return nil, nil, nil
	}
	if method != "" && cal.Props.Get("METHOD") == nil {
		// MIME supplies context that must survive retained-component decoding.
		first, rest, _ := strings.Cut(unfolded, "\n")
		unfolded = first + "\nMETHOD:" + method + "\r\n" + rest
		raw = []byte(unfolded)
	}
	if calendarZone == "" {
		calendarZone = "UTC"
	}
	if calendarZone == "Local" {
		return nil, nil, fmt.Errorf("unsupported time zone %q", calendarZone)
	}
	loc, err := time.LoadLocation(calendarZone)
	if err != nil {
		return nil, nil, fmt.Errorf("calendar time zone: %w", err)
	}
	doc := splitICSDocument(unfolded)
	return parseICSComponents(ctx, cal, doc, raw, method, loc, budget)
}

func parseICSComponents(ctx context.Context, cal *ical.Calendar, doc icsDocument, raw []byte, method string, loc *time.Location, budget *ICSParseBudget) (out []ICSComponent, diagnostics []string, err error) {
	eventIndex := 0
	for _, comp := range cal.Children {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if comp.Name != ical.CompEvent {
			continue
		}
		result, err := parseICSComponent(ctx, cal, comp, method, loc, budget)
		if errors.Is(err, errICSGuestRemoval) {
			diagnostics = append(diagnostics, fmt.Sprintf("VEVENT %d: %s", eventIndex, err))
			eventIndex++
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("VEVENT %d: %w", eventIndex, err)
		}
		if err := validateICSTimeZoneUseRange(ctx, cal, comp, result.Event, budget); err != nil {
			if !errors.Is(err, calrecur.ErrUnsupportedRecurrence) {
				return nil, nil, fmt.Errorf("VEVENT %d: %w", eventIndex, err)
			}
			// The iterator guard also rejects recurrences we cannot expand.
			// Retain their evidence and readable siblings without iterating them.
			result.Event.RecurrenceError = err.Error()
			diagnostics = append(diagnostics, fmt.Sprintf("VEVENT %d: %s", eventIndex, err))
		}
		result.Raw, err = budget.retain(doc, cal, raw, eventIndex)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, result)
		eventIndex++
	}
	if eventIndex == 0 {
		return nil, nil, errors.New("calendar has no VEVENT")
	}
	return out, diagnostics, nil
}

func icsMethod(cal *ical.Calendar, method string) (string, error) {
	if len(cal.Props.Values("METHOD")) > 1 {
		return "", errors.New("duplicate calendar METHOD")
	}
	declared, err := propText(cal.Props, "METHOD")
	if err != nil {
		return "", err
	}
	declared = strings.ToUpper(strings.TrimSpace(declared))
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != "" && declared != "" && method != declared {
		return "", errors.New("calendar METHOD conflicts with MIME method")
	}
	if declared != "" {
		method = declared
	}
	switch method {
	case "", "REQUEST", "PUBLISH", "CANCEL":
	case "REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER":
		return method, nil
	default:
		return "", fmt.Errorf("unsupported calendar method %q", method)
	}
	return method, nil
}

func preflightICS(raw []byte) (string, error) {
	for _, b := range raw {
		if (b < 0x20 && b != '\r' && b != '\n' && b != '\t') || b == 0x7f {
			return "", errors.New("calendar contains forbidden control bytes")
		}
	}
	if len(raw) > MaxICSBytes {
		return "", errors.New("calendar exceeds 32 MiB limit")
	}

	depth, events := 0, 0
	logicalRaw := unfoldICS(raw)
	if !utf8.ValidString(logicalRaw) {
		return "", errors.New("calendar is not valid UTF-8")
	}
	for line := range strings.SplitSeq(logicalRaw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > 8192 {
			return "", errors.New("calendar unfolded line exceeds 8 KiB limit")
		}
		name, value, _ := strings.Cut(line, ":")
		name, _, _ = strings.Cut(name, ";")
		switch strings.ToUpper(name) {
		case "BEGIN":
			depth++
			if depth > 32 {
				return "", errors.New("calendar exceeds nesting limit")
			}
			if strings.EqualFold(value, "VEVENT") {
				events++
				if events > maxICSEvents {
					return "", errors.New("calendar exceeds 10000 VEVENT limit")
				}
			}
		case "END":
			depth--
		}
	}
	return logicalRaw, nil
}

func propText(props ical.Props, name string) (string, error) {
	text, err := props.Text(name)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

func normalizeICSParams(props ical.Props) {
	for _, values := range props {
		for _, prop := range values {
			if value := prop.Params.Get("VALUE"); value != "" {
				prop.Params.Set("VALUE", strings.ToUpper(value))
			}
			zone := prop.Params.Get("TZID")
			if mapped := windowsZones[zone]; mapped != "" {
				if _, err := time.LoadLocation(zone); err != nil {
					prop.Params.Set("TZID", mapped)
				}
			}
		}
	}
}

func icsDateTime(p *ical.Prop, loc *time.Location) (time.Time, error) {
	if strings.HasSuffix(p.Value, "Z") {
		v, err := time.Parse("20060102T150405Z", p.Value)
		if err != nil {
			return time.Time{}, fmt.Errorf("parse UTC calendar time: %w", err)
		}
		return v, nil
	}
	if zone := p.Params.Get("TZID"); zone != "" {
		if zone == "Local" {
			return time.Time{}, fmt.Errorf("unsupported time zone %q", zone)
		}
		var err error
		loc, err = time.LoadLocation(zone)
		if err != nil {
			return time.Time{}, fmt.Errorf("load calendar time zone: %w", err)
		}
	}
	return calrecur.ParseLocalDateTime("20060102T150405", p.Value, loc)
}

func icsTime(p *ical.Prop, loc *time.Location) (time.Time, bool, error) {
	if p == nil {
		return time.Time{}, false, nil
	}
	date := p.ValueType() == ical.ValueDate
	if (!date && p.ValueType() != ical.ValueDateTime) || (date && len(p.Value) != 8) || (!date && len(p.Value) != 15 && len(p.Value) != 16) {
		return time.Time{}, false, errors.New("invalid calendar date type")
	}
	if p.Params.Get("TZID") != "" && (date || strings.HasSuffix(p.Value, "Z")) {
		return time.Time{}, false, errors.New("TZID on DATE or UTC value")
	}
	if date {
		v, err := time.Parse("20060102", p.Value)
		if err == nil && (v.Year() < 0 || v.Year() > 9999) {
			err = errors.New("calendar date is outside the supported date range")
		}
		return v, true, err
	}
	v, err := icsDateTime(p, loc)
	if err == nil && (v.UTC().Year() < 0 || v.UTC().Year() > 9999) {
		err = errors.New("calendar instant is outside the supported date range")
	}
	return v, date, err
}

var icsDurationPattern = regexp.MustCompile(`^\+?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

func icsCivilDateTime(p *ical.Prop) (time.Time, error) {
	if p == nil || p.ValueType() != ical.ValueDateTime {
		return time.Time{}, errors.New("missing DATE-TIME value")
	}
	value, err := time.ParseInLocation("20060102T150405", p.Value, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse civil DATE-TIME: %w", err)
	}
	return value, nil
}

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
	days := n[0]*7 + n[1]
	seconds := n[2]*3600 + n[3]*60 + n[4]
	if days > 36600 || seconds > 36600*86400 || days+seconds == 0 {
		return 0, 0, errors.New("calendar DURATION must be positive and bounded")
	}
	return int(days), time.Duration(seconds) * time.Second, nil
}

func parseICSEvent(comp *ical.Component, method string, loc *time.Location) (ICSComponent, error) {
	p := comp.Props
	for _, name := range []string{"UID", "SEQUENCE", "DTSTAMP", "DTSTART", "DTEND", "DURATION", "RECURRENCE-ID", "STATUS", "ORGANIZER", "SUMMARY", "DESCRIPTION", "LOCATION", "URL", "TRANSP", "CLASS"} {
		if len(p.Values(name)) > 1 {
			return ICSComponent{}, fmt.Errorf("duplicate %s", name)
		}
	}
	c := ICSComponent{OrganizerEmail: icsEmail(p.Get("ORGANIZER"))}
	var status string
	textFields := map[string]*string{"UID": &c.UID, "SUMMARY": &c.Summary, "DESCRIPTION": &c.Description, "LOCATION": &c.Location, "STATUS": &status}
	// Validate every TEXT property, including extension properties.
	for name, props := range p {
		for _, prop := range props {
			target := textFields[name]
			if prop.ValueType() == ical.ValueText || target != nil {
				text, err := prop.Text()
				if err != nil {
					return ICSComponent{}, fmt.Errorf("%s: %w", name, err)
				}
				if target != nil {
					*target = text
				}
			}
		}
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
	status = strings.ToUpper(status)
	switch status {
	case "", "CONFIRMED":
	case "TENTATIVE":
		e.Status = "tentative"
	case "CANCELLED":
		e.Status = "cancelled"
	default:
		return ICSComponent{}, errors.New("invalid event STATUS")
	}
	if method == "CANCEL" && e.Status != "cancelled" {
		if len(p.Values("ATTENDEE")) > 0 {
			return ICSComponent{}, errICSGuestRemoval
		}
		e.Status = "cancelled"
	}
	start, allDay, err := icsTime(p.Get("DTSTART"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
	}
	startDate := ""
	if allDay {
		startDate = start.Format(time.DateOnly)
		start, _, err = calrecur.CivilDateBoundary(startDate, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
		}
	}
	if start.IsZero() && e.Status != "cancelled" {
		return ICSComponent{}, errors.New("active event requires DTSTART")
	}
	if startProp := p.Get("DTSTART"); startProp != nil && !allDay && !strings.HasSuffix(strings.ToUpper(startProp.Value), "Z") {
		wall, err := icsCivilDateTime(startProp)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("DTSTART wall time: %w", err)
		}
		e.StartWall = &wall
	}
	end, endDate, err := icsTime(p.Get("DTEND"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
	}
	endDateValue := ""
	if endDate {
		endDateValue = end.Format(time.DateOnly)
		end, _, err = calrecur.CivilDateBoundary(endDateValue, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
		}
	}
	nominalDuration := false
	dur := p.Get("DURATION")
	if dur != nil && p.Get("DTEND") != nil {
		return ICSComponent{}, errors.New("DTEND and DURATION are mutually exclusive")
	}
	if start.IsZero() && (!end.IsZero() || dur != nil) {
		return ICSComponent{}, errors.New("event end requires DTSTART")
	}
	if !start.IsZero() {
		if !end.IsZero() && endDate != allDay {
			return ICSComponent{}, errors.New("DTEND type differs from DTSTART")
		}
		if dur != nil {
			days, elapsed, err := icsDuration(dur.Value)
			if err != nil {
				return ICSComponent{}, err
			}
			if allDay && elapsed != 0 {
				return ICSComponent{}, errors.New("DATE duration requires whole days")
			}
			if allDay {
				endDateValue, err = addICSDateDays(startDate, days)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DURATION: %w", err)
				}
				end, _, err = calrecur.CivilDateBoundary(endDateValue, loc)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DURATION: %w", err)
				}
			} else {
				_, zoneEnd := start.ZoneBounds()
				nominalDuration = days > 0 && !zoneEnd.IsZero()
				end = start.AddDate(0, 0, days).Add(elapsed)
			}
		} else if end.IsZero() {
			if allDay {
				endDateValue, err = addICSDateDays(startDate, 1)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
				}
				end, _, err = calrecur.CivilDateBoundary(endDateValue, loc)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
				}
			} else {
				end = start
			}
		}
		if end.Year() < 0 || end.Year() > 9999 || end.UTC().Year() < 0 || end.UTC().Year() > 9999 {
			return ICSComponent{}, errors.New("calendar end is outside the supported date range")
		}
		// A skipped civil day can resolve both boundaries to the same instant.
		if (allDay && endDateValue <= startDate) || (!allDay && end.Before(start)) {
			return ICSComponent{}, errors.New("event ends before its start")
		}
		e.AllDay = allDay
		if allDay {
			e.StartDate, e.EndDate = startDate, endDateValue
		}
		e.EventTimeZone = start.Location().String()
		first, last := start.UTC(), end.UTC()
		e.Start, e.End = &first, &last
	}
	if rid := p.Get("RECURRENCE-ID"); rid != nil {
		if rid.Params.Get("RANGE") != "" {
			return ICSComponent{}, errors.New("RECURRENCE-ID RANGE is unsupported")
		}
		original, date, err := icsTime(rid, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("RECURRENCE-ID: %w", err)
		}
		if !start.IsZero() && date != allDay {
			return ICSComponent{}, errors.New("RECURRENCE-ID type differs from DTSTART")
		}
		e.Kind = "exception"
		e.Original = &calrecur.Identity{Instant: original.UTC(), AllDay: date}
		if date {
			e.Original.Date = original.Format(time.DateOnly)
		}
		if start.IsZero() {
			e.AllDay = date
		}
	}
	recurrenceID := ""
	if e.Original != nil {
		recurrenceID = e.Original.Key()
		rid := p.Get("RECURRENCE-ID")
		if !e.Original.AllDay && rid.Params.Get("TZID") == "" && !strings.HasSuffix(rid.Value, "Z") {
			recurrenceID = "floating:" + rid.Value
		}
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
					line.WriteByte(';')
					line.WriteString(param)
					line.WriteByte('=')
					line.WriteString(v)
				}
			}
			line.WriteByte(':')
			line.WriteString(prop.Value)
			e.Recurrence = append(e.Recurrence, line.String())
		}
	}
	if len(e.Recurrence) > 0 && e.Kind != "exception" {
		e.Kind = "series"
		e.SeriesProviderID = e.ProviderEventID
	}
	if e.Kind == "series" && nominalDuration {
		e.RecurrenceError = "nominal DURATION with a timed start in a changing time zone is unsupported"
	}
	organizerName := ""
	if organizer := p.Get("ORGANIZER"); organizer != nil {
		organizerName = organizer.Params.Get("CN")
	}
	organizerSeen := false
	for _, a := range p.Values("ATTENDEE") {
		role := "required"
		switch {
		case IsRoomAttendee(icsEmail(&a), strings.EqualFold(a.Params.Get("CUTYPE"), "RESOURCE") || strings.EqualFold(a.Params.Get("CUTYPE"), "ROOM")):
			role = "room"
		case strings.EqualFold(a.Params.Get("ROLE"), "OPT-PARTICIPANT"):
			role = "optional"
		}
		if c.OrganizerEmail != "" && icsEmail(&a) == c.OrganizerEmail {
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
		e.Attendees = append(e.Attendees, Attendee{Email: icsEmail(&a), DisplayName: a.Params.Get("CN"), Role: role, RSVP: rsvp})
	}
	if c.OrganizerEmail != "" && !organizerSeen {
		e.Attendees = append(e.Attendees, Attendee{Email: c.OrganizerEmail, DisplayName: organizerName, Role: "organizer", RSVP: "unknown"})
	}
	stamp, _, err := icsTime(p.Get("DTSTAMP"), time.UTC)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTAMP: %w", err)
	}
	c.Event, c.Stamp = e, stamp
	return c, nil
}

func addICSDateDays(date string, days int) (string, error) {
	civil, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return "", fmt.Errorf("parse civil date: %w", err)
	}
	return civil.AddDate(0, 0, days).Format(time.DateOnly), nil
}

func icsEmail(p *ical.Prop) string {
	if p == nil || !strings.HasPrefix(strings.ToLower(p.Value), "mailto:") {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(p.Value[len("mailto:"):]))
}
