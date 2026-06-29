package calsync

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

// sourceConfig is the JSON persisted in sources.sync_config for a calendar.
type sourceConfig struct {
	AccountEmail    string `json:"account_email"`
	CalendarID      string `json:"calendar_id"`
	CalendarSummary string `json:"calendar_summary,omitempty"`
	AccessRole      string `json:"access_role,omitempty"`
	Primary         bool   `json:"primary,omitempty"`
	TimeZone        string `json:"time_zone,omitempty"`
}

func buildSourceConfigJSON(c sourceConfig) string {
	b, err := json.Marshal(c)
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
	Sequence          int      `json:"sequence,omitempty"`
	HTMLLink          string   `json:"html_link,omitempty"`
	HangoutLink       string   `json:"hangout_link,omitempty"`
	Transparency      string   `json:"transparency,omitempty"`
	Visibility        string   `json:"visibility,omitempty"`
	EventType         string   `json:"event_type,omitempty"`
	OrganizerEmail    string   `json:"organizer_email,omitempty"`
	CalendarID        string   `json:"calendar_id,omitempty"`
	AccountEmail      string   `json:"account_email,omitempty"`
}

// ingestEvent persists a non-cancelled event through the canonical write path
// plus the metadata helper, and indexes it for FTS/embeddings. It is idempotent
// via UpsertMessage's ON CONFLICT(source_id, source_message_id).
func (s *Syncer) ingestEvent(sourceID int64, cal gcal.Calendar, ev gcal.Event) (int64, error) {
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
			return 0, fmt.Errorf("organizer participant: %w", err)
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
			return 0, fmt.Errorf("attendee participant: %w", err)
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
	convID, err := s.store.EnsureConversationWithType(sourceID, conversationKey(ev), gcal.ConversationType, convTitle)
	if err != nil {
		return 0, fmt.Errorf("ensure conversation: %w", err)
	}

	body := serializeBody(ev)
	subject := ev.Summary
	fromMe := ev.Organizer.Self || (ev.Organizer.Email != "" && strings.EqualFold(ev.Organizer.Email, s.opts.AccountEmail))

	msgID, err := s.store.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: smid,
		MessageType:     gcal.MessageTypeCalendarEvent,
		SentAt:          eventSentAt(ev),
		SenderID:        sql.NullInt64{Int64: senderID, Valid: senderID != 0},
		IsFromMe:        fromMe,
		Subject:         sql.NullString{String: subject, Valid: subject != ""},
		Snippet:         sql.NullString{String: snippet(body), Valid: body != ""},
		SizeEstimate:    int64(len(body)),
	})
	if err != nil {
		return 0, fmt.Errorf("upsert message: %w", err)
	}

	metaJSON, err := json.Marshal(buildMetadata(ev, cal, s.opts.AccountEmail))
	if err != nil {
		return 0, fmt.Errorf("marshal metadata: %w", err)
	}
	if err := s.store.SetMessageMetadata(msgID, sql.NullString{String: string(metaJSON), Valid: true}); err != nil {
		return 0, fmt.Errorf("set metadata: %w", err)
	}

	if err := s.store.UpsertMessageBody(msgID, sql.NullString{String: body, Valid: body != ""}, sql.NullString{}); err != nil {
		return 0, fmt.Errorf("upsert body: %w", err)
	}

	raw := []byte(ev.Raw)
	if len(raw) == 0 {
		if raw, err = json.Marshal(ev); err != nil {
			return 0, fmt.Errorf("marshal raw event: %w", err)
		}
	}
	if err := s.store.UpsertMessageRawWithFormat(msgID, raw, gcal.RawFormat); err != nil {
		return 0, fmt.Errorf("upsert raw: %w", err)
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
	if err := s.store.ReplaceMessageRecipients(msgID, "from", fromIDs, fromNames); err != nil {
		return 0, fmt.Errorf("replace from recipient: %w", err)
	}
	if err := s.store.ReplaceMessageRecipients(msgID, "to", attendeeIDs, attendeeNames); err != nil {
		return 0, fmt.Errorf("replace to recipients: %w", err)
	}

	// FTS: raw attendee emails go ONLY through the toAddrs column, never the
	// body, so BM25/ts_rank doesn't double-count them and embeddings see only
	// semantic prose.
	if err := s.store.UpsertFTS(msgID, subject, body, ev.Organizer.Email, strings.Join(attendeeEmails, " "), ""); err != nil {
		s.logger.Warn("upsert calendar event fts failed", "message_id", msgID, "event_id", smid, "error", err)
	}

	return msgID, nil
}

// flagCancelled retains a cancelled event rather than soft-deleting it. If the
// row already exists, it flips metadata.status to "cancelled" while preserving
// every other stored field (a cancellation delta usually arrives with empty
// summary/start, so re-upserting would wipe the archived event). If the row was
// never seen, it inserts a minimal tombstone whose metadata records the
// cancellation. Returns (messageID, insertedNew).
func (s *Syncer) flagCancelled(sourceID int64, cal gcal.Calendar, ev gcal.Event) (int64, bool, error) {
	smid := deriveSourceMessageID(ev)
	existing, err := s.store.MessageExistsBatch(sourceID, []string{smid})
	if err != nil {
		return 0, false, fmt.Errorf("lookup existing event: %w", err)
	}
	if id, ok := existing[smid]; ok {
		merged, err := mergeStatusCancelled(s.store, id)
		if err != nil {
			return 0, false, err
		}
		if err := s.store.SetMessageMetadata(id, merged); err != nil {
			return 0, false, fmt.Errorf("flag cancelled metadata: %w", err)
		}
		return id, false, nil
	}
	// Never-seen cancellation: record it as a tombstone via the normal path.
	// ev.Status == "cancelled" flows into metadata.status.
	id, err := s.ingestEvent(sourceID, cal, ev)
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// mergeStatusCancelled reads a message's existing metadata, sets status to
// "cancelled", and returns the merged JSON, preserving all other keys.
func mergeStatusCancelled(st *store.Store, messageID int64) (sql.NullString, error) {
	existing, err := st.GetMessageMetadata(messageID)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("read metadata: %w", err)
	}
	m := map[string]any{}
	if existing.Valid && existing.String != "" {
		if err := json.Unmarshal([]byte(existing.String), &m); err != nil {
			// Corrupt/absent metadata shouldn't block the cancellation flag.
			m = map[string]any{}
		}
	}
	m["status"] = gcal.StatusCancelled
	b, err := json.Marshal(m)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal merged metadata: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
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
		Sequence:          ev.Sequence,
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

// snippet is a short preview derived from the body.
func snippet(body string) string {
	const maxSnippetLength = 200
	body = strings.TrimSpace(body)
	if len(body) <= maxSnippetLength {
		return body
	}
	return body[:maxSnippetLength]
}
