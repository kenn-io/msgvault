package callhistory

import (
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

type calendarTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

func (v calendarTime) parse() (time.Time, string, error) {
	if v.DateTime != "" && v.Date == "" {
		at, err := time.Parse(time.RFC3339Nano, v.DateTime)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("parse calendar date-time: %w", err)
		}
		return at, "instant", nil
	}
	if v.Date != "" && v.DateTime == "" {
		at, err := time.Parse(time.DateOnly, v.Date)
		if err != nil {
			return time.Time{}, "", fmt.Errorf("parse calendar date: %w", err)
		}
		return at, "day", nil
	}
	return time.Time{}, "", invalid("calendar start/end")
}

var calendarLink = regexp.MustCompile(`https?://[^\s<>"']+|tel:\+?[0-9][0-9().;-]*`)

func (r ImportRequest) calendarObservation() (Observation, error) {
	var event struct {
		ID, Status            string
		Description, Location string
		HangoutLink           string
		Start, End            calendarTime
		Recurrence            []string
		Attendees             []struct{ Email string }
		ConferenceData        struct {
			EntryPoints []struct{ EntryPointType, URI string }
		}
	}
	if err := json.Unmarshal(r.Record, &event, json.MatchCaseInsensitiveNames(true)); err != nil || len(event.Recurrence) != 0 {
		return Observation{}, invalid("calendar event; recurring masters require expanded instances")
	}
	start, precision, err := event.Start.parse()
	if err != nil {
		return Observation{}, invalid("calendar start requires a date or RFC3339 offset")
	}
	o := Observation{
		RecordID: event.ID,

		Version: 1, Provider: "google_calendar", Account: r.Account, Kind: "scheduled_meeting",
		Direction: unknown, Outcome: unknown, OwnerParticipation: unknown,
		OccurredAt: start, Timezone: event.Start.TimeZone, Precision: precision,

		Raw: r.Record,
	}
	if event.End.Date != "" || event.End.DateTime != "" {
		end, _, err := event.End.parse()
		if err != nil {
			return Observation{}, invalid("calendar end")
		}
		o.EndedAt = &end
	}
	if event.Status == "cancelled" {
		o.Outcome = "cancelled"
	}
	used := make(map[string]bool)
	for _, attendee := range event.Attendees {
		if id, ok := r.Bindings[attendee.Email]; ok {
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
	for _, entry := range event.ConferenceData.EntryPoints {
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
