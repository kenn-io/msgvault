package calevent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	ical "github.com/emersion/go-ical"
	"go.kenn.io/msgvault/internal/calrecur"
)

const MaxICSBytes = 4 << 20
const MaxICSRetainedBytes = 64 << 20
const maxICSEvents = 1000

// ICSMetadata retains interpretation inputs alongside the verbatim document.
// Index is explicit so a missing selector cannot pick an arbitrary VEVENT.
type ICSMetadata struct {
	Version               int    `json:"ics_version"`
	Index                 *int   `json:"ics_event_index"`
	Namespace             string `json:"ics_namespace"`
	Method                string `json:"ics_method,omitempty"`
	TimeZone              string `json:"ics_time_zone"`
	Status                string `json:"status,omitempty"`
	AllDay                bool   `json:"all_day"`
	Inherited             *Event `json:"ics_inherited,omitempty"`
	FamilyCancelled       bool   `json:"ics_family_cancelled,omitempty"`
	ParentMessageID       *int64 `json:"invite_parent_message_id,omitempty"`
	ParentSourceMessageID string `json:"invite_source_message_id,omitempty"`
	PartKey               string `json:"invite_part_key,omitempty"`
}

type ICSComponent struct {
	Event    Event
	Summary  string
	Stamp    time.Time
	Index    int
	Method   string
	HasStart bool
	HasEnd   bool
}

// UnsupportedMethodError reports a calendar method this event importer cannot
// interpret while preserving the method value for callers with narrower scope.
type UnsupportedMethodError struct{ Method string }

func (e *UnsupportedMethodError) Error() string {
	return fmt.Sprintf("unsupported calendar method %q", e.Method)
}

// IsNonActionableMethod reports standard iTIP methods this email event
// projection does not apply as event revisions.
func IsNonActionableMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "ADD", "REFRESH", "REPLY", "COUNTER", "DECLINECOUNTER":
		return true
	default:
		return false
	}
}

// ICSMessageID is source-local and stable across input ordering and revisions.
func ICSMessageID(e Event) string {
	key := ""
	if e.Original != nil {
		key = e.Original.Key()
	}
	b, _ := json.Marshal([]string{e.ICalUID, key})
	sum := sha256.Sum256(b)
	return "calendar-invite:" + hex.EncodeToString(sum[:])
}

// ICSMessageIDForImport keeps each unverified email claim under a distinct
// archive identity that survives changes to the provider's message key. File
// imports have no parent message and retain the provider identity.
func ICSMessageIDForImport(e Event, meta ICSMetadata) string {
	return ICSMessageID(e) + icsClaimSuffix(meta)
}

// ScopeICSClaim separates provider event and series identities by archive
// message and MIME part when an authenticated organizer identity is unavailable.
func ScopeICSClaim(e Event, meta ICSMetadata) Event {
	suffix := icsClaimSuffix(meta)
	if suffix == "" {
		return e
	}
	e.ProviderEventID += suffix
	if e.SeriesProviderID != "" {
		e.SeriesProviderID += suffix
	}
	return e
}

func icsClaimSuffix(meta ICSMetadata) string {
	if meta.ParentMessageID == nil && meta.ParentSourceMessageID == "" {
		return ""
	}
	var key []byte
	if meta.ParentMessageID != nil {
		key, _ = json.Marshal([]any{*meta.ParentMessageID, meta.PartKey})
	} else {
		key, _ = json.Marshal([]string{meta.ParentSourceMessageID, meta.PartKey})
	}
	sum := sha256.Sum256(key)
	return ":email:" + hex.EncodeToString(sum[:])
}

func init() { Register("ics", decodeICS) }

func decodeICS(raw, metadata []byte, sc SourceContext) (Event, bool, error) {
	var meta ICSMetadata
	if err := json.Unmarshal(metadata, &meta); err != nil || meta.Version != 1 || meta.Index == nil {
		return Event{}, false, errors.New("ICS metadata requires version and event index")
	}
	sc.CalendarTimeZone = meta.TimeZone
	component, err := ParseICSAtIndex(raw, meta.Method, *meta.Index, sc)
	if err != nil {
		return Event{}, false, err
	}
	e := component.Event
	e.Namespace = meta.Namespace
	if e.Status == "cancelled" && meta.Inherited != nil {
		old := meta.Inherited
		if e.Description == "" {
			e.Description, e.DescriptionTruncated = old.Description, old.DescriptionTruncated
		}
		if e.Location == "" {
			e.Location = old.Location
		}
		if !component.HasStart {
			e.HasBounds, e.AllDay, e.Start, e.End, e.StartDate, e.EndDate, e.EventTimeZone = old.HasBounds, old.AllDay, old.Start, old.End, old.StartDate, old.EndDate, old.EventTimeZone
		} else if !component.HasEnd && e.AllDay == old.AllDay && old.HasBounds {
			if err := inheritICSBounds(&e, *old); err != nil {
				return Event{}, false, err
			}
		}
		if e.Kind == "single" && old.Kind == "series" {
			e.Kind, e.SeriesProviderID = "series", e.ProviderEventID
		}
		if len(e.Recurrence) == 0 {
			e.Recurrence, e.RecurrenceError = old.Recurrence, old.RecurrenceError
		}
		if len(e.Attendees) == 0 {
			e.Attendees = old.Attendees
		}
		if e.OrganizerEmail == "" {
			e.OrganizerEmail = old.OrganizerEmail
		}
		if e.OrganizerDisplayName == "" {
			e.OrganizerDisplayName = old.OrganizerDisplayName
		}
	}
	if err := validateICSBounds(e); err != nil {
		return Event{}, false, err
	}
	if meta.FamilyCancelled {
		e.Status = "cancelled"
	}
	return ScopeICSClaim(e, meta), true, nil
}

// ParseICS accepts one complete VCALENDAR. The lexical pass bounds work before
// the dependency's recursive parser and its parameter concatenation run.
func ParseICS(raw []byte, method string, sc SourceContext) (out []ICSComponent, err error) {
	return ParseICSWithContextResolver(raw, method, func(string) (SourceContext, error) {
		return sc, nil
	})
}

// ParseICSWithContextResolver resolves source interpretation from each
// VEVENT's UID before parsing its dates and bounds.
func ParseICSWithContextResolver(raw []byte, method string, resolve func(string) (SourceContext, error)) (out []ICSComponent, err error) {
	return parseICSWithContextResolver(raw, method, resolve, nil)
}

// ParseICSAtIndex parses only the selected VEVENT. Stored raw documents retain
// the interpretation context for one event, which may differ from sibling
// events if the source time zone later changes.
func ParseICSAtIndex(raw []byte, method string, index int, sc SourceContext) (ICSComponent, error) {
	components, err := parseICSWithContextResolver(raw, method, func(string) (SourceContext, error) {
		return sc, nil
	}, &index)
	if err != nil {
		return ICSComponent{}, err
	}
	return components[0], nil
}

// RetainICSComponentAtIndex returns a calendar document containing only the
// selected VEVENT and its nested alarms. Subset archives use it to keep
// unselected event content out of the copied raw payload while preserving the
// selected event's properties.
func RetainICSComponentAtIndex(raw []byte, method string, index int, sc SourceContext) ([]byte, error) {
	if _, err := ParseICSAtIndex(raw, method, index, sc); err != nil {
		return nil, fmt.Errorf("select ICS event: %w", err)
	}
	cal, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil {
		return nil, fmt.Errorf("decode ICS calendar for retention: %w", err)
	}
	eventIndex := 0
	var selected *ical.Component
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		if eventIndex == index {
			selected = child
		}
		eventIndex++
	}
	if selected == nil {
		return nil, errors.New("selected ICS event is unavailable")
	}
	if eventIndex == 1 {
		return bytes.Clone(raw), nil
	}

	retained := ical.NewCalendar()
	for _, name := range []string{
		ical.PropProductID,
		ical.PropVersion,
		ical.PropCalendarScale,
		ical.PropMethod,
		"X-WR-TIMEZONE",
	} {
		if values := cal.Props.Values(name); len(values) != 0 {
			prop := values[0]
			retained.Props.Add(&prop)
		}
	}
	if retained.Props.Get(ical.PropProductID) == nil {
		retained.Props.SetText(ical.PropProductID, "-//msgvault//ICS subset//EN")
	}
	if retained.Props.Get(ical.PropVersion) == nil {
		retained.Props.SetText(ical.PropVersion, "2.0")
	}
	if retained.Props.Get(ical.PropMethod) == nil && strings.TrimSpace(method) != "" {
		retained.Props.SetText(ical.PropMethod, strings.ToUpper(strings.TrimSpace(method)))
	}
	retained.Children = []*ical.Component{selected}

	var filtered bytes.Buffer
	if err := ical.NewEncoder(&filtered).Encode(retained); err != nil {
		return nil, fmt.Errorf("encode retained ICS event: %w", err)
	}
	if _, err := ParseICSAtIndex(filtered.Bytes(), method, 0, sc); err != nil {
		return nil, fmt.Errorf("validate retained ICS event: %w", err)
	}
	return filtered.Bytes(), nil
}

func parseICSWithContextResolver(raw []byte, method string, resolve func(string) (SourceContext, error), selectedIndex *int) (out []ICSComponent, err error) {
	if err = preflightICS(raw); err != nil {
		return nil, err
	}
	defer func() {
		if recover() != nil {
			out = nil
			err = errors.New("malformed iCalendar parameters")
		}
	}()
	cal, err := ical.NewDecoder(bytes.NewReader(raw)).Decode()
	if err != nil {
		return nil, fmt.Errorf("decode iCalendar: %w", err)
	}
	if len(cal.Props.Values("METHOD")) > 1 {
		return nil, errors.New("duplicate calendar METHOD")
	}
	declared := strings.ToUpper(strings.TrimSpace(propText(cal.Props, "METHOD")))
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != "" && declared != "" && method != declared {
		return nil, errors.New("calendar METHOD conflicts with MIME method")
	}
	if declared != "" {
		method = declared
	}
	switch method {
	case "", "REQUEST", "PUBLISH", "CANCEL":
	default:
		return nil, &UnsupportedMethodError{Method: method}
	}
	if resolve == nil {
		return nil, errors.New("calendar source context resolver is required")
	}
	if selectedIndex != nil && *selectedIndex < 0 {
		return nil, errors.New("ICS event index out of range")
	}
	eventIndex := 0
	for _, comp := range cal.Children {
		if comp.Name != ical.CompEvent {
			continue
		}
		if selectedIndex != nil && eventIndex != *selectedIndex {
			eventIndex++
			continue
		}
		sc, err := resolve(propText(comp.Props, "UID"))
		if err != nil {
			return nil, fmt.Errorf("resolve VEVENT %d source context: %w", eventIndex, err)
		}
		zone := sc.CalendarTimeZone
		if zone == "" {
			zone = "UTC"
		}
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("VEVENT %d calendar time zone: %w", eventIndex, err)
		}
		result, err := parseICSEvent(comp, method, loc)
		if err != nil {
			return nil, fmt.Errorf("VEVENT %d: %w", eventIndex, err)
		}
		result.Index = eventIndex
		result.Method = method
		out = append(out, result)
		eventIndex++
	}
	if eventIndex == 0 {
		return nil, errors.New("calendar has no VEVENT")
	}
	if selectedIndex != nil && *selectedIndex >= eventIndex {
		return nil, errors.New("ICS event index out of range")
	}
	return out, nil
}

func inheritICSBounds(event *Event, old Event) error {
	if !old.HasBounds || old.Start == nil || old.End == nil || event.Start == nil {
		return errors.New("cannot inherit incomplete calendar bounds")
	}
	if event.AllDay {
		start, err := time.Parse(time.DateOnly, old.StartDate)
		if err != nil {
			return fmt.Errorf("read inherited all-day start: %w", err)
		}
		end, err := time.Parse(time.DateOnly, old.EndDate)
		if err != nil || !end.After(start) {
			return errors.New("inherited all-day bounds are invalid")
		}
		newStart, err := time.Parse(time.DateOnly, event.StartDate)
		if err != nil {
			return fmt.Errorf("read cancellation all-day start: %w", err)
		}
		days := int(end.Sub(start) / (24 * time.Hour))
		event.EndDate, err = addICSDateDays(newStart.Format(time.DateOnly), days)
		if err != nil {
			return fmt.Errorf("shift inherited all-day bounds: %w", err)
		}
		loc, err := time.LoadLocation(event.EventTimeZone)
		if err != nil {
			return fmt.Errorf("load cancellation time zone: %w", err)
		}
		newEnd, err := calrecur.CivilDateBoundary(event.EndDate, loc)
		if err != nil {
			return fmt.Errorf("resolve inherited all-day end: %w", err)
		}
		event.End = &newEnd
	} else {
		duration := old.End.Sub(*old.Start)
		if duration < 0 {
			return errors.New("inherited event ends before its start")
		}
		newEnd := event.Start.Add(duration)
		event.End = &newEnd
	}
	event.HasBounds = true
	return validateICSBounds(*event)
}

func validateICSBounds(event Event) error {
	if !event.HasBounds {
		return nil
	}
	if event.Start == nil || event.End == nil {
		return errors.New("calendar event has incomplete bounds")
	}
	if event.Start.UTC().Year() < 0 || event.Start.UTC().Year() > 9999 ||
		event.End.UTC().Year() < 0 || event.End.UTC().Year() > 9999 {
		return errors.New("calendar bounds are outside the supported date range")
	}
	if event.End.Before(*event.Start) || (event.AllDay && !event.End.After(*event.Start)) {
		return errors.New("event ends before its start")
	}
	if event.AllDay && (event.StartDate == "" || event.EndDate == "" || event.EndDate <= event.StartDate) {
		return errors.New("all-day calendar bounds are invalid")
	}
	return nil
}

func preflightICS(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("calendar is not valid UTF-8")
	}
	for _, b := range raw {
		if (b < 0x20 && b != '\r' && b != '\n' && b != '\t') || b == 0x7f {
			return errors.New("calendar contains forbidden control bytes")
		}
	}
	if len(raw) > MaxICSBytes {
		return errors.New("calendar exceeds 4 MiB limit")
	}
	var stack []string
	started, ended := false, false
	events, unfolded := 0, 0
	// Match the decoder's CRLF/LF folding before interpreting component names
	// and values, so the lexical bounds also cover folded framing lines.
	logicalRaw := strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "").Replace(string(raw))
	for line := range strings.SplitSeq(logicalRaw, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		continuation := line[0] == ' ' || line[0] == '\t'
		if continuation {
			unfolded += len(line) - 1
		} else {
			unfolded = len(line)
		}
		if unfolded > 8192 {
			return errors.New("calendar unfolded line exceeds 8 KiB limit")
		}
		if continuation {
			if ended || !started {
				return errors.New("calendar continuation outside component")
			}
			continue
		}
		if ended {
			return errors.New("calendar has trailing content")
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return errors.New("calendar content line has no value separator")
		}
		frameName, _, parameterized := strings.Cut(name, ";")
		if parameterized && (strings.EqualFold(frameName, "BEGIN") || strings.EqualFold(frameName, "END")) {
			return errors.New("calendar component framing cannot have parameters")
		}
		switch strings.ToUpper(frameName) {
		case "BEGIN":
			value = strings.ToUpper(value)
			if !started {
				if value != "VCALENDAR" {
					return errors.New("calendar requires VCALENDAR root")
				}
				started = true
			}
			if len(stack) == 1 && value == "VEVENT" {
				events++
				if events > maxICSEvents {
					return errors.New("calendar exceeds 1000 VEVENT limit")
				}
			}
			stack = append(stack, value)
			if len(stack) > 32 {
				return errors.New("calendar exceeds nesting limit")
			}
		case "END":
			if len(stack) == 0 || stack[len(stack)-1] != strings.ToUpper(value) {
				return errors.New("calendar component framing mismatch")
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				ended = true
			}
		default:
			if len(stack) == 0 {
				return errors.New("calendar property outside component")
			}
		}
	}
	if !ended {
		return errors.New("calendar component is incomplete")
	}
	return nil
}

func propText(props ical.Props, name string) string { text, _ := props.Text(name); return text }

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
	v, err := p.DateTime(loc)
	if err == nil && (v.UTC().Year() < 0 || v.UTC().Year() > 9999) {
		err = errors.New("calendar instant is outside the supported date range")
	}
	return v, date, err
}

var icsDurationPattern = regexp.MustCompile(`^\+?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

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

var icsURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

func parseICSEvent(comp *ical.Component, method string, loc *time.Location) (ICSComponent, error) {
	p := comp.Props
	for _, name := range []string{"UID", "SEQUENCE", "DTSTAMP", "DTSTART", "DTEND", "DURATION", "RECURRENCE-ID", "STATUS", "ORGANIZER", "SUMMARY", "DESCRIPTION", "LOCATION", "URL", "TRANSP", "CLASS"} {
		if len(p.Values(name)) > 1 {
			return ICSComponent{}, fmt.Errorf("duplicate %s", name)
		}
	}
	e := Event{Provider: "ics", Kind: "single", ICalUID: propText(p, "UID"), Status: "confirmed", CalendarTimeZone: loc.String(), Location: propText(p, "LOCATION"), Transparency: strings.ToLower(propText(p, "TRANSP")), Visibility: strings.ToLower(propText(p, "CLASS"))}
	if strings.TrimSpace(e.ICalUID) == "" {
		return ICSComponent{}, errors.New("event has no UID")
	}
	if seq := p.Get("SEQUENCE"); seq != nil {
		n, err := strconv.ParseInt(seq.Value, 10, 64)
		if err != nil || n < 0 {
			return ICSComponent{}, errors.New("invalid SEQUENCE")
		}
		e.Sequence = n
	}
	status := strings.ToUpper(propText(p, "STATUS"))
	switch status {
	case "", "CONFIRMED":
	case "TENTATIVE":
		e.Status = "tentative"
	case "CANCELLED":
		e.Status = "cancelled"
	default:
		return ICSComponent{}, errors.New("invalid event STATUS")
	}
	if method == "CANCEL" {
		e.Status = "cancelled"
	}
	start, allDay, err := icsTime(p.Get("DTSTART"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
	}
	startDate := ""
	if allDay {
		startDate = start.Format(time.DateOnly)
		start, err = calrecur.CivilDateStart(startDate, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
		}
	}
	if start.IsZero() && e.Status != "cancelled" {
		return ICSComponent{}, errors.New("active event requires DTSTART")
	}
	end, endDate, err := icsTime(p.Get("DTEND"), loc)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
	}
	endDateValue := ""
	if endDate {
		endDateValue = end.Format(time.DateOnly)
		end, err = calrecur.CivilDateBoundary(endDateValue, loc)
		if err != nil {
			return ICSComponent{}, fmt.Errorf("DTEND: %w", err)
		}
	}
	dur := p.Get("DURATION")
	if dur != nil && p.Get("DTEND") != nil {
		return ICSComponent{}, errors.New("DTEND and DURATION are mutually exclusive")
	}
	if start.IsZero() && (!end.IsZero() || dur != nil) {
		return ICSComponent{}, errors.New("event end requires DTSTART")
	}
	days := 0
	if !start.IsZero() {
		if !end.IsZero() && endDate != allDay {
			return ICSComponent{}, errors.New("DTEND type differs from DTSTART")
		}
		if dur != nil {
			var elapsed time.Duration
			days, elapsed, err = icsDuration(dur.Value)
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
				end, err = calrecur.CivilDateBoundary(endDateValue, loc)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DURATION: %w", err)
				}
			} else {
				end = start.AddDate(0, 0, days).Add(elapsed)
			}
		} else if end.IsZero() {
			if allDay {
				endDateValue, err = addICSDateDays(startDate, 1)
				if err != nil {
					return ICSComponent{}, fmt.Errorf("DTSTART: %w", err)
				}
				end, err = calrecur.CivilDateBoundary(endDateValue, loc)
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
		if end.Before(start) || (allDay && !end.After(start)) {
			return ICSComponent{}, errors.New("event ends before its start")
		}
		e.AllDay = allDay
		e.HasBounds = true
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
	master := e
	master.Original = nil
	e.ProviderEventID = ICSMessageID(e)
	if e.Kind == "exception" {
		e.SeriesProviderID = ICSMessageID(master)
	}
	for _, name := range []string{"RRULE", "RDATE", "EXDATE"} {
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
	if len(p.Values("EXRULE")) > 0 {
		e.RecurrenceError = "EXRULE recurrence exclusions are unsupported"
	}
	if len(e.Recurrence) > 0 && e.Kind != "exception" {
		e.Kind = "series"
		e.SeriesProviderID = e.ProviderEventID
	}
	if e.Kind == "series" && !allDay && days > 0 {
		if e.RecurrenceError != "" {
			e.RecurrenceError += "; "
		}
		e.RecurrenceError += "timed recurring nominal-day DURATION is unsupported"
	}
	e.Description = propText(p, "DESCRIPTION")
	if len(e.Description) > 4096 {
		end := 4096
		for end > 0 && !utf8.RuneStart(e.Description[end]) {
			end--
		}
		e.Description, e.DescriptionTruncated = e.Description[:end], true
	}
	e.OrganizerEmail = icsEmail(p.Get("ORGANIZER"))
	if organizer := p.Get("ORGANIZER"); organizer != nil {
		e.OrganizerDisplayName = organizer.Params.Get("CN")
	}
	for _, a := range p.Values("ATTENDEE") {
		role := "required"
		switch {
		case IsRoomAttendee(icsEmail(&a), strings.EqualFold(a.Params.Get("CUTYPE"), "RESOURCE") || strings.EqualFold(a.Params.Get("CUTYPE"), "ROOM")):
			role = "room"
		case strings.EqualFold(a.Params.Get("ROLE"), "CHAIR"):
			role = "organizer"
		case strings.EqualFold(a.Params.Get("ROLE"), "OPT-PARTICIPANT"):
			role = "optional"
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
	if url := p.Get("URL"); url != nil {
		e.JoinURL = url.Value
	}
	if e.JoinURL == "" {
		for _, url := range icsURLPattern.FindAllString(e.Location+" "+e.Description, -1) {
			url = strings.TrimRight(url, ".,;)")
			if JoinKey(url) != "" {
				e.JoinURL = url
				break
			}
		}
	}
	e.JoinKey = JoinKey(e.JoinURL)
	stamp, _, err := icsTime(p.Get("DTSTAMP"), time.UTC)
	if err != nil {
		return ICSComponent{}, fmt.Errorf("DTSTAMP: %w", err)
	}
	return ICSComponent{
		Event: e, Summary: propText(p, "SUMMARY"), Stamp: stamp,
		HasStart: p.Get("DTSTART") != nil,
		HasEnd:   p.Get("DTEND") != nil || p.Get("DURATION") != nil,
	}, nil
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
	return normalizeEmail(p.Value[len("mailto:"):])
}
