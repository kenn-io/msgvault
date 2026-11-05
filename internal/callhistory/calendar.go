package callhistory

import (
	"encoding/json/v2"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
)

var calendarLink = regexp.MustCompile(`https?://[^\s<>"']+|tel:\+?[0-9][0-9().;-]*`)

func (r ImportRequest) calendarObservation() (Observation, error) {
	event, err := gcalmodel.DecodeEvent(r.Record)
	if err != nil || len(event.Recurrence) != 0 {
		return Observation{}, invalid("calendar event; recurring masters require expanded instances")
	}
	var conference struct {
		ConferenceData struct {
			EntryPoints []struct{ EntryPointType, URI string }
		}
	}
	if err := json.Unmarshal(r.Record, &conference, json.MatchCaseInsensitiveNames(true)); err != nil {
		return Observation{}, invalid("calendar conference data")
	}
	var start time.Time
	var precision string
	if event.Status != "cancelled" || !event.Start.IsZero() {
		var ok bool
		start, ok = event.Start.Instant()
		if !ok {
			return Observation{}, invalid("calendar start requires a date or RFC3339 offset")
		}
		precision = "instant"
		if event.Start.IsAllDay() {
			precision = "day"
		}
	}
	o := Observation{
		RecordID: event.ID,

		Version: 1, Provider: "google_calendar", Account: r.Account, Kind: "scheduled_meeting",
		Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: start, Timezone: event.Start.TimeZone, Precision: precision,

		Raw: r.Record,
	}
	if !event.End.IsZero() {
		end, ok := event.End.Instant()
		if !ok {
			return Observation{}, invalid("calendar end")
		}
		o.EndedAt = &end
	}
	if event.Status == "cancelled" {
		o.Outcome = "cancelled"
	}
	used := make(map[string]bool)
	for _, attendee := range event.Attendees {
		if id, ok := r.Bindings[attendee.Email]; ok && !start.IsZero() {
			o.Participants = append(o.Participants, Participant{ID: id, Participation: "invited"})
			used[attendee.Email] = true
		}
	}
	if len(used) != len(r.Bindings) {
		return Observation{}, invalid("calendar binding must name an attendee")
	}
	add := func(kind, value string) {
		point := AccessPoint{Kind: kind, Value: value}
		if !slices.Contains(o.AccessPoints, point) {
			o.AccessPoints = append(o.AccessPoints, point)
		}
	}
	for _, entry := range conference.ConferenceData.EntryPoints {
		if entry.EntryPointType == "phone" {
			add("phone", entry.URI)
		} else if isMeetingURL(entry.URI) {
			add("url", entry.URI)
		}
	}
	if isMeetingURL(event.HangoutLink) {
		add("url", event.HangoutLink)
	}
	for _, link := range calendarLink.FindAllString(event.Description+" "+event.Location, -1) {
		link = strings.TrimRight(link, ".,);]")
		if strings.HasPrefix(link, "tel:") {
			add("phone", link)
		} else if isMeetingURL(link) {
			add("url", link)
		}
	}
	return o, nil
}

func isMeetingURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if slices.Contains([]string{"meet.google.com", "teams.microsoft.com", "teams.live.com", "cal.com"}, host) {
		return true
	}
	return host == "zoom.us" || strings.HasSuffix(host, ".zoom.us")
}
