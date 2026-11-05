package callhistory

import (
	"encoding/json/v2"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
	"go.kenn.io/msgvault/internal/meetingurl"
)

var calendarLink = regexp.MustCompile(`https?://[^\s<>"']+|tel:\+?[0-9][^\s<>"']*`)

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
		RecordID: event.ID, Sparse: event.Status == "cancelled" && event.Start.IsZero(),

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
	bindings := make(map[string]int64, len(r.Bindings))
	for email, id := range r.Bindings {
		bindings[gcalmodel.NormalizeParticipantEmail(email)] = id
	}
	for _, attendee := range event.Attendees {
		email := gcalmodel.NormalizeParticipantEmail(attendee.Email)
		if id, ok := bindings[email]; ok && !start.IsZero() {
			o.Participants = append(o.Participants, Participant{ID: id, Participation: "invited"})
			used[email] = true
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
		} else if join := meetingurl.KnownJoinURL(entry.URI); join != "" {
			add("url", join)
		}
	}
	if join := meetingurl.KnownJoinURL(event.HangoutLink); join != "" {
		add("url", join)
	}
	for _, link := range calendarLink.FindAllString(event.Description+" "+event.Location, -1) {
		if strings.HasPrefix(link, "tel:") {
			add("phone", link)
		} else if join := meetingurl.KnownJoinURL(strings.TrimRight(link, ".,);]")); join != "" {
			add("url", join)
		}
	}
	return o, nil
}
