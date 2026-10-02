package notionmeetings

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

// CalendarEvent identifies a synced event and retains its source evidence.
type CalendarEvent struct {
	MessageID       int64
	SourceID        int64
	CalendarID      string
	Event           gcal.Event
	notionPages     []string
	hasNotionLink   bool
	signalsPrepared bool
}

// CalendarSource reads only already-synced events, without new OAuth access.
type CalendarSource interface {
	CalendarEvents(ctx context.Context, account string) ([]CalendarEvent, error)
}

type archiveCalendarSource struct{ store *store.Store }

func (s archiveCalendarSource) CalendarEvents(ctx context.Context, account string) ([]CalendarEvent, error) {
	archived, err := s.store.CalendarJoinEventsContext(ctx, account)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	var skipped int
	events := make([]CalendarEvent, 0, len(archived))
	for _, a := range archived {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var event gcal.Event
		if json.Unmarshal(a.Raw, &event) != nil || event.ID == "" {
			skipped++
			continue
		}
		event.Raw = a.Raw
		events = append(events, CalendarEvent{MessageID: a.MessageID, SourceID: a.SourceID, CalendarID: a.CalendarID, Event: event})
	}
	if skipped > 0 {
		err = errors.Join(err, fmt.Errorf("skipped %d invalid archived Google Calendar events", skipped))
	}
	return events, err
}

// Prepare once per import, since link evidence does not depend on the meeting.
func prepareCalendarEvents(events []CalendarEvent) {
	for i := range events {
		events[i].notionPages, events[i].hasNotionLink = eventNotionPages(events[i].Event)
		events[i].signalsPrepared = true
	}
}

type calendarInvitee struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

type calendarMatch struct {
	MessageID          int64             `json:"message_id"`
	SourceID           int64             `json:"source_id"`
	CalendarID         string            `json:"calendar_id"`
	EventID            string            `json:"event_id"`
	Basis              string            `json:"basis"`
	Confidence         float64           `json:"confidence"`
	NotionPageID       string            `json:"notion_page_id,omitempty"`
	StartTime          string            `json:"start_time,omitempty"`
	EndTime            string            `json:"end_time,omitempty"`
	TitleMatched       bool              `json:"title_matched,omitzero"`
	ParticipantOverlap int               `json:"participant_overlap,omitzero"`
	Attendees          []calendarInvitee `json:"attendees,omitempty"`
}

type calendarCandidate struct {
	event CalendarEvent
	match calendarMatch
}

func joinCalendar(h *HydratedMeeting, events []CalendarEvent) {
	joinCalendarWithHeuristics(h, events, true)
}

func joinCalendarWithHeuristics(h *HydratedMeeting, events []CalendarEvent, allowHeuristics bool) {
	pages := map[string]bool{}
	for _, id := range []string{h.Discovery.Parent.PageID, parentPageID(h.MeetingBlock)} {
		if id = normalizedPageID(id); id != "" {
			pages[id] = true
		}
	}
	var exact, heuristic []calendarCandidate
	for _, e := range events {
		if e.Event.ID == "" || e.Event.IsCancelled() {
			continue
		}
		linked, hasLink := e.notionPages, e.hasNotionLink
		if !e.signalsPrepared {
			linked, hasLink = eventNotionPages(e.Event)
		}
		match := calendarMatch{MessageID: e.MessageID, SourceID: e.SourceID, CalendarID: e.CalendarID, EventID: e.Event.ID,
			StartTime: formatOptionalTime(e.Event.Start.DateTime), EndTime: formatOptionalTime(e.Event.End.DateTime)}
		found := ""
		for _, id := range linked {
			if pages[id] {
				found = id
				break
			}
		}
		if found != "" {
			match.Basis, match.Confidence, match.NotionPageID = "notion_link", 1, found
			exact = append(exact, calendarCandidate{e, match})
			continue
		}
		if hasLink {
			continue
		} // A link to another page rules out a title/time guess.
		start := parseNotionTime(h.Discovery.MeetingNotes.CalendarEvent.StartTime)
		if start.IsZero() {
			start = parseNotionTime(h.Discovery.MeetingNotes.Recording.StartTime)
		}
		if start.IsZero() || e.Event.Start.DateTime.IsZero() || math.Abs(e.Event.Start.DateTime.Sub(start).Seconds()) > 300 {
			continue
		}
		title := normalizedTitle(h.Discovery.Title())
		match.TitleMatched = title != "" && title == normalizedTitle(e.Event.Summary)
		for _, person := range h.Attendees {
			for _, a := range e.Event.Attendees {
				if !a.Resource && strings.EqualFold(strings.TrimSpace(a.Email), person.Email) {
					match.ParticipantOverlap++
					break
				}
			}
		}
		// A title match is enough only with matching interval ends. Participant
		// overlap permits a renamed event; it never binds names or IDs to emails.
		end := parseNotionTime(h.Discovery.MeetingNotes.CalendarEvent.EndTime)
		if end.IsZero() {
			end = parseNotionTime(h.Discovery.MeetingNotes.Recording.EndTime)
		}
		endMatches := !end.IsZero() && !e.Event.End.DateTime.IsZero() && math.Abs(e.Event.End.DateTime.Sub(end).Seconds()) <= 300
		matchesIntervalTitle := match.TitleMatched && endMatches
		matchesParticipantsTitle := match.ParticipantOverlap > 0 && similarTitles(title, normalizedTitle(e.Event.Summary))
		if !matchesIntervalTitle && !matchesParticipantsTitle {
			continue
		}
		match.Basis = "heuristic"
		match.Confidence = 0.8
		if match.TitleMatched && match.ParticipantOverlap > 0 {
			match.Confidence = 0.9
		}
		heuristic = append(heuristic, calendarCandidate{e, match})
	}
	candidates := exact
	if len(candidates) == 0 && allowHeuristics {
		candidates = heuristic
	}
	if len(candidates) == 0 {
		return
	}
	// Identical copies in multiple synced calendars represent one event. Copies
	// with conflicting times, titles, or invitees still require review.
	for _, candidate := range candidates[1:] {
		if !sameCalendarCandidate(candidates[0], candidate) {
			h.Warnings = append(h.Warnings, "Google Calendar join was ambiguous; invitees remain unresolved")
			return
		}
	}
	chosen := candidates[0]
	match := chosen.match
	seen := map[string]bool{}
	for _, p := range h.Attendees {
		seen[strings.ToLower(strings.TrimSpace(p.Email))] = true
	}
	for _, a := range chosen.event.Event.Attendees {
		if a.Resource {
			continue
		}
		person := (meetingarchive.Person{Name: a.DisplayName, Email: a.Email}).Normalized()
		if person.Email == "" {
			continue
		}
		match.Attendees = append(match.Attendees, calendarInvitee{Name: person.Name, Email: person.Email})
		if !seen[person.Email] {
			h.Attendees = append(h.Attendees, person)
			seen[person.Email] = true
		}
	}
	slices.SortFunc(match.Attendees, func(a, b calendarInvitee) int { return strings.Compare(a.Email, b.Email) })
	h.CalendarMatch = &match
}

func preserveArchivedCalendarJoin(h *HydratedMeeting, archived *calendarMatch) {
	if h == nil || archived == nil || h.CalendarMatch != nil {
		return
	}

	match := *archived
	match.Attendees = slices.Clone(archived.Attendees)
	h.CalendarMatch = &match

	seen := make(map[string]struct{}, len(h.Attendees)+len(match.Attendees))
	for _, attendee := range h.Attendees {
		if email := strings.ToLower(strings.TrimSpace(attendee.Email)); email != "" {
			seen[email] = struct{}{}
		}
	}
	for _, invitee := range match.Attendees {
		person := (meetingarchive.Person{Name: invitee.Name, Email: invitee.Email}).Normalized()
		if person.Email == "" {
			continue
		}
		if _, exists := seen[person.Email]; exists {
			continue
		}
		h.Attendees = append(h.Attendees, person)
		seen[person.Email] = struct{}{}
	}
}

func sameCalendarCandidate(a, b calendarCandidate) bool {
	x, y := a.event.Event, b.event.Event
	if x.ID != y.ID || !x.Start.DateTime.Equal(y.Start.DateTime) || !x.End.DateTime.Equal(y.End.DateTime) ||
		x.Start.Date != y.Start.Date || x.End.Date != y.End.Date || normalizedTitle(x.Summary) != normalizedTitle(y.Summary) ||
		a.match.Basis != b.match.Basis || a.match.NotionPageID != b.match.NotionPageID {
		return false
	}
	invitees := func(e gcal.Event) []string {
		var emails []string
		for _, p := range e.Attendees {
			if !p.Resource {
				emails = append(emails, strings.ToLower(strings.TrimSpace(p.Email)))
			}
		}
		slices.Sort(emails)
		return slices.Compact(emails)
	}
	return slices.Equal(invitees(x), invitees(y))
}

func parentPageID(block *Block) string {
	if block == nil {
		return ""
	}
	return block.Parent.PageID
}
func normalizedTitle(value string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }), " ")
}

func similarTitles(a, b string) bool {
	aa, bb := strings.Fields(a), strings.Fields(b)
	if len(aa) == 0 || len(bb) == 0 {
		return false
	}
	uniqueA := make(map[string]struct{}, len(aa))
	uniqueB := make(map[string]struct{}, len(bb))
	for _, word := range aa {
		uniqueA[word] = struct{}{}
	}
	for _, word := range bb {
		uniqueB[word] = struct{}{}
	}
	overlap := 0
	for word := range uniqueA {
		if _, ok := uniqueB[word]; ok {
			overlap++
		}
	}
	return float64(overlap)/float64(max(len(uniqueA), len(uniqueB))) >= 0.5
}

var notionURLPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
var pageIDPattern = regexp.MustCompile(`(?i)([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{32})`)

func normalizedPageID(id string) string {
	id = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(id), "-", ""))
	if len(id) != 32 || pageIDPattern.FindString(id) != id {
		return ""
	}
	return id
}

// eventNotionPages scans the link-bearing fields, including unmodeled Calendar
// JSON. A recognized Notion URL, even without a parseable ID, blocks heuristics.
func eventNotionPages(event gcal.Event) ([]string, bool) {
	values := []string{event.Description, event.Location, event.HangoutLink}
	var fields map[string]any
	if json.Unmarshal(event.Raw, &fields) == nil {
		var walk func(any)
		walk = func(v any) {
			switch v := v.(type) {
			case string:
				values = append(values, v)
			case []any:
				for _, x := range v {
					walk(x)
				}
			case map[string]any:
				for _, x := range v {
					walk(x)
				}
			}
		}
		for _, key := range []string{"conferenceData", "attachments", "source"} {
			walk(fields[key])
		}
	}
	var ids []string
	hasLink := false
	for _, value := range values {
		for _, link := range notionURLPattern.FindAllString(html.UnescapeString(value), -1) {
			link = strings.TrimRight(link, ".,);]}")
			u, err := url.Parse(link)
			if err != nil {
				continue
			}
			host := strings.ToLower(u.Hostname())
			if host != "notion.so" && host != "notion.site" && !strings.HasSuffix(host, ".notion.so") && !strings.HasSuffix(host, ".notion.site") {
				continue
			}
			hasLink = true
			// The final UUID in a titled page path is the page ID.
			matches := pageIDPattern.FindAllString(u.Path, -1)
			if len(matches) > 0 {
				ids = append(ids, normalizedPageID(matches[len(matches)-1]))
			}
			for key, vs := range u.Query() {
				key = strings.ReplaceAll(strings.ToLower(key), "_", "")
				if key != "pageid" && key != "notionpageid" && key != "meetingnotespageid" {
					continue
				}
				for _, v := range vs {
					if id := normalizedPageID(v); id != "" {
						ids = append(ids, id)
					}
				}
			}
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), hasLink
}
