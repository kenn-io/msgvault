package calsync

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

// sourceConfig is the JSON persisted in sources.sync_config for a calendar.
type sourceConfig struct {
	AccountEmail    string `json:"account_email"`
	CalendarID      string `json:"calendar_id"`
	CalendarSummary string `json:"calendar_summary,omitempty"`
	AccessRole      string `json:"access_role,omitempty"`
	Primary         bool   `json:"primary,omitzero"`
	TimeZone        string `json:"time_zone,omitempty"`
}

func buildSourceConfigJSON(c sourceConfig) string {
	b, err := json.Marshal(c, json.Deterministic(true))
	if err != nil {
		return "{}"
	}
	return string(b)
}

// eventMetadata is the structured JSON stored in messages.metadata. It carries
// the event facts that don't fit the messages columns (the interval end, all-day
// flag, status, recurrence rules, series linkage, and source links).
type eventMetadata struct {
	Status            string   `json:"status,omitempty"`
	AllDay            bool     `json:"all_day"`
	Start             string   `json:"start,omitempty"`
	End               string   `json:"end,omitempty"`
	TimeZone          string   `json:"time_zone,omitempty"`
	Recurrence        []string `json:"recurrence,omitempty"`
	RecurringEventID  string   `json:"recurring_event_id,omitempty"`
	OriginalStartTime string   `json:"original_start_time,omitempty"`
	ICalUID           string   `json:"ical_uid,omitempty"`
	Sequence          *int     `json:"sequence,omitempty"`
	HTMLLink          string   `json:"html_link,omitempty"`
	HangoutLink       string   `json:"hangout_link,omitempty"`
	Transparency      string   `json:"transparency,omitempty"`
	Visibility        string   `json:"visibility,omitempty"`
	EventType         string   `json:"event_type,omitempty"`
	OrganizerEmail    string   `json:"organizer_email,omitempty"`
	CalendarID        string   `json:"calendar_id,omitempty"`
	AccountEmail      string   `json:"account_email,omitempty"`
}

// persistCalendarSnapshot prepares provider data for one atomic Store write.
// The Store owns the prior-state comparison and sparse cancellation merge.
func (s *Syncer) persistCalendarSnapshot(ctx context.Context, sourceID int64, cal gcal.Calendar, ev gcal.Event) (int64, bool, error) {
	smid := deriveSourceMessageID(ev)
	ev.Organizer.Email = normalizeParticipantEmail(ev.Organizer.Email)
	for i := range ev.Attendees {
		ev.Attendees[i].Email = normalizeParticipantEmail(ev.Attendees[i].Email)
	}

	// Organizer → sender, resolved through the email-keyed participant path so
	// calendar people dedupe with email contacts.
	var senderID int64
	if ev.Organizer.Email != "" {
		id, err := s.store.EnsureParticipant(ev.Organizer.Email, ev.Organizer.DisplayName, emailDomain(ev.Organizer.Email))
		if err != nil {
			return 0, false, fmt.Errorf("organizer participant: %w", err)
		}
		senderID = id
	}

	// Attendees → 'to' recipients + FTS toAddrs.
	var attendeeIDs []int64
	var attendeeNames []string
	var attendeeEmails []string
	for _, a := range ev.Attendees {
		if a.Email == "" {
			continue
		}
		pid, err := s.store.EnsureParticipant(a.Email, a.DisplayName, emailDomain(a.Email))
		if err != nil {
			return 0, false, fmt.Errorf("attendee participant: %w", err)
		}
		attendeeIDs = append(attendeeIDs, pid)
		attendeeNames = append(attendeeNames, a.DisplayName)
		attendeeEmails = append(attendeeEmails, a.Email)
	}

	// Only the series master (or a standalone event) sets the conversation
	// title. A per-instance exception keeps its edited summary on its own message
	// row, but must not overwrite the shared series title — otherwise the
	// conversation label flaps as the master and edited instances re-deliver
	// across syncs. Passing "" preserves the existing title (EnsureConversation
	// only overwrites with a non-empty title).
	convTitle := ev.Summary
	if ev.RecurringEventID != "" {
		convTitle = ""
	}

	body := serializeBody(ev)
	subject := ev.Summary
	identityFromMe := !ev.Organizer.Self &&
		ev.Organizer.Email != "" &&
		strings.EqualFold(ev.Organizer.Email, s.opts.AccountEmail)
	fromMe := ev.Organizer.Self || identityFromMe

	message := &store.Message{
		SourceID:                sourceID,
		SourceMessageID:         smid,
		MessageType:             gcal.MessageTypeCalendarEvent,
		SentAt:                  eventSentAt(ev),
		SenderID:                sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:                fromMe,
		IdentityDerivedIsFromMe: identityFromMe,
		Subject:                 sql.NullString{String: subject, Valid: subject != ""},
		Snippet:                 sql.NullString{String: Snippet(body), Valid: body != ""},
		SizeEstimate:            int64(len(body)),
	}
	metaJSON, err := json.Marshal(buildMetadata(ev, cal, s.opts.AccountEmail), json.Deterministic(true))
	if err != nil {
		return 0, false, fmt.Errorf("marshal metadata: %w", err)
	}
	raw := []byte(ev.Raw)
	if len(raw) == 0 {
		if raw, err = json.Marshal(ev, json.Deterministic(true)); err != nil {
			return 0, false, fmt.Errorf("marshal raw event: %w", err)
		}
	}
	// Replace recipients UNCONDITIONALLY (even with empty sets) so re-syncing an
	// event that lost its organizer or all attendees clears the stale rows.
	// ReplaceMessageRecipients DELETEs the existing rows of that type first, then
	// no-ops the insert on an empty slice — a guarded call would skip the DELETE
	// and leave stale 'from'/'to' rows that desync from the (always-rewritten)
	// FTS to_addr column.
	var fromIDs []int64
	var fromNames []string
	if senderID != 0 {
		fromIDs = []int64{senderID}
		fromNames = []string{ev.Organizer.DisplayName}
	}
	metadata := sql.NullString{String: string(metaJSON), Valid: true}
	data := &store.MessagePersistData{
		Message: message,
		Conversation: &store.ConversationPersistData{
			SourceConversationID: conversationKey(ev),
			ConversationType:     gcal.ConversationType,
			Title:                convTitle,
		},
		Metadata:  &metadata,
		BodyText:  sql.NullString{String: body, Valid: body != ""},
		RawMIME:   raw,
		RawFormat: gcal.RawFormat,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames},
			{Type: "to", ParticipantIDs: attendeeIDs, DisplayNames: attendeeNames},
		},
		// Keep raw attendee emails in the FTS address column only.
		FTS: &store.FTSDoc{Subject: subject, Body: body, FromAddr: ev.Organizer.Email, ToAddrs: strings.Join(attendeeEmails, " ")},
	}
	return s.store.PersistCalendarEventContext(ctx, data, ev.Updated, ev.IsCancelled())
}

func normalizeParticipantEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// deriveSourceMessageID is the idempotency key: a standalone event or series
// master uses event.id; a recurring instance/exception/cancellation uses
// recurringEventId|originalStartTime so each occurrence upserts independently
// and a single cancelled occurrence flags only its own row.
func deriveSourceMessageID(ev gcal.Event) string {
	if ev.RecurringEventID != "" {
		if key := originalStartKey(ev.OriginalStartTime); key != "" {
			return ev.RecurringEventID + "|" + key
		}
	}
	return ev.ID
}

func originalStartKey(dt gcal.EventDateTime) string {
	if dt.Date != "" {
		return dt.Date
	}
	if !dt.DateTime.IsZero() {
		return dt.DateTime.UTC().Format(time.RFC3339)
	}
	return ""
}

// conversationKey groups a recurring series under one conversation; standalone
// events each get their own.
func conversationKey(ev gcal.Event) string {
	if ev.RecurringEventID != "" {
		return "event:" + ev.RecurringEventID
	}
	return "event:" + ev.ID
}

// eventSentAt is the universal time axis: the event start, falling back to the
// occurrence's original start (for cancellation tombstones that omit start).
func eventSentAt(ev gcal.Event) sql.NullTime {
	if t, ok := ev.Start.Instant(); ok {
		return sql.NullTime{Time: t, Valid: true}
	}
	if t, ok := ev.OriginalStartTime.Instant(); ok {
		return sql.NullTime{Time: t, Valid: true}
	}
	return sql.NullTime{}
}

// buildMetadata projects an event into the metadata payload.
func buildMetadata(ev gcal.Event, cal gcal.Calendar, accountEmail string) eventMetadata {
	// A complete event uses iCalendar's default sequence 0. A sparse
	// cancellation can omit the value, so preserve it as unknown there.
	var sequence *int
	if !ev.IsCancelled() || ev.Sequence != 0 {
		value := ev.Sequence
		sequence = &value
	}
	if len(ev.Raw) > 0 {
		var provider struct {
			Sequence *int `json:"sequence"`
		}
		if json.Unmarshal([]byte(ev.Raw), &provider) == nil {
			if provider.Sequence != nil {
				sequence = provider.Sequence
			} else if ev.IsCancelled() {
				sequence = nil
			}
		}
	}
	return eventMetadata{
		Status:            ev.Status,
		AllDay:            ev.Start.IsAllDay(),
		Start:             dateTimeString(ev.Start),
		End:               dateTimeString(ev.End),
		TimeZone:          ev.Start.TimeZone,
		Recurrence:        ev.Recurrence,
		RecurringEventID:  ev.RecurringEventID,
		OriginalStartTime: originalStartKey(ev.OriginalStartTime),
		ICalUID:           ev.ICalUID,
		Sequence:          sequence,
		HTMLLink:          ev.HTMLLink,
		HangoutLink:       ev.HangoutLink,
		Transparency:      ev.Transparency,
		Visibility:        ev.Visibility,
		EventType:         ev.EventType,
		OrganizerEmail:    ev.Organizer.Email,
		CalendarID:        cal.ID,
		AccountEmail:      accountEmail,
	}
}

func dateTimeString(dt gcal.EventDateTime) string {
	if dt.Date != "" {
		return dt.Date
	}
	if !dt.DateTime.IsZero() {
		return dt.DateTime.Format(time.RFC3339)
	}
	return ""
}

// serializeBody is the single body_text shared by FTS body and embeddings:
// title, time range, location, description, and attendee DISPLAY NAMES. Raw
// attendee email addresses are deliberately excluded (they reach FTS via the
// toAddrs column only).
func serializeBody(ev gcal.Event) string {
	var b strings.Builder
	writeLine := func(s string) {
		if s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	writeLine(ev.Summary)
	writeLine(whenLine(ev))
	if ev.Location != "" {
		writeLine("Location: " + ev.Location)
	}
	writeLine(ev.Description)

	var names []string
	for _, a := range ev.Attendees {
		if a.DisplayName != "" {
			names = append(names, a.DisplayName)
		}
	}
	if len(names) > 0 {
		writeLine("Attendees: " + strings.Join(names, ", "))
	}
	return strings.TrimSpace(b.String())
}

// whenLine renders a human/searchable time range.
func whenLine(ev gcal.Event) string {
	start, ok := ev.Start.Instant()
	if !ok {
		return ""
	}
	if ev.Start.IsAllDay() {
		return "When: " + start.Format("2006-01-02") + " (all day)"
	}
	if end, ok := ev.End.Instant(); ok {
		return "When: " + start.Format("2006-01-02 15:04") + " - " + end.Format("2006-01-02 15:04")
	}
	return "When: " + start.Format("2006-01-02 15:04")
}

// Snippet returns a trimmed calendar-event preview of at most 200 bytes without splitting valid UTF-8.
func Snippet(body string) string {
	const maxSnippetBytes = 200
	body = strings.TrimSpace(body)
	if len(body) <= maxSnippetBytes {
		return body
	}

	end := maxSnippetBytes
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	return body[:end]
}
