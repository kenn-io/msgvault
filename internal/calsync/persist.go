package calsync

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
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
	Sequence          int      `json:"sequence,omitzero"`
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
	ev.Organizer, ev.Attendees = MapParticipants(ev.Organizer, ev.Attendees, false)
	// Organizer → sender, resolved through the email-keyed participant path so
	// calendar people dedupe with email contacts.
	var senderID int64
	if ev.Organizer.Email != "" {
		id, err := s.store.EnsureParticipant(ev.Organizer.Email, ev.Organizer.DisplayName, EmailDomain(ev.Organizer.Email))
		if err != nil {
			return 0, fmt.Errorf("organizer participant: %w", err)
		}
		senderID = id
	}

	// Attendees → 'to' recipients + FTS toAddrs.
	var attendeeIDs []int64
	for _, a := range ev.Attendees {
		if a.Email == "" {
			continue
		}
		pid, err := s.store.EnsureParticipant(a.Email, a.DisplayName, EmailDomain(a.Email))
		if err != nil {
			return 0, fmt.Errorf("attendee participant: %w", err)
		}
		attendeeIDs = append(attendeeIDs, pid)
	}

	body := calevent.SerializeBody(ev)
	data := MapEventMessage(ev, s.opts.AccountEmail, &store.MessagePersistData{
		Message:      &store.Message{SourceID: sourceID, SourceMessageID: smid, SizeEstimate: int64(len(body))},
		Conversation: &store.ConversationPersistData{SourceConversationID: conversationKey(ev)},
		BodyText:     store.NullIfEmpty(body),
	}, senderID, attendeeIDs)
	convID, err := s.store.EnsureConversationWithType(sourceID, data.Conversation.SourceConversationID, data.Conversation.ConversationType, data.Conversation.Title)
	if err != nil {
		return 0, fmt.Errorf("ensure conversation: %w", err)
	}
	data.Message.ConversationID = convID
	msgID, err := s.store.UpsertMessage(data.Message)
	if err != nil {
		return 0, fmt.Errorf("upsert message: %w", err)
	}

	metaJSON, err := json.Marshal(buildMetadata(ev, cal, s.opts.AccountEmail), json.Deterministic(true))
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
		if raw, err = json.Marshal(ev, json.Deterministic(true)); err != nil {
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
	for _, recipients := range data.Recipients {
		if err := s.store.ReplaceMessageRecipients(msgID, recipients.Type, recipients.ParticipantIDs, recipients.DisplayNames); err != nil {
			return 0, fmt.Errorf("replace %s recipients: %w", recipients.Type, err)
		}
	}

	// FTS: raw attendee emails go ONLY through the toAddrs column, never the
	// body, so BM25/ts_rank doesn't double-count them and embeddings see only
	// semantic prose.
	if err := s.store.UpsertFTS(msgID, data.FTS.Subject, data.FTS.Body, data.FTS.FromAddr, data.FTS.ToAddrs, data.FTS.CcAddrs); err != nil {
		s.logger.Warn("upsert calendar event fts failed", "message_id", msgID, "event_id", smid, "error", err)
	}

	return msgID, nil
}

// MapEventMessage maps shared event fields onto a provider's message snapshot.
func MapEventMessage(ev gcal.Event, account string, data *store.MessagePersistData, senderID int64, attendeeIDs []int64) *store.MessagePersistData {
	var toNames, toEmails []string
	for _, a := range ev.Attendees {
		if a.Email != "" {
			toNames, toEmails = append(toNames, a.DisplayName), append(toEmails, a.Email)
		}
	}
	identityFromMe := !ev.Organizer.Self && ev.Organizer.Email != "" && strings.EqualFold(ev.Organizer.Email, account)
	data.Message.MessageType = gcal.MessageTypeCalendarEvent
	data.Message.SentAt = eventSentAt(ev)
	data.Message.IsFromMe = ev.Organizer.Self || identityFromMe
	data.Message.IdentityDerivedIsFromMe = identityFromMe
	data.Message.Subject = store.NullIfEmpty(ev.Summary)
	data.Message.Snippet = store.NullIfEmpty(calevent.Snippet(data.BodyText.String))
	data.Conversation.ConversationType = gcal.ConversationType
	data.Conversation.Title = ev.Summary
	if ev.RecurringEventID != "" {
		data.Conversation.Title = ""
	}
	data.FTS = &store.FTSDoc{Subject: ev.Summary, Body: data.BodyText.String, FromAddr: ev.Organizer.Email, ToAddrs: strings.Join(toEmails, " ")}
	data.Message.SenderID = sql.NullInt64{Int64: senderID, Valid: senderID != 0}
	var fromIDs []int64
	var fromNames, fromEmails []string
	if senderID != 0 {
		fromIDs, fromNames, fromEmails = []int64{senderID}, []string{ev.Organizer.DisplayName}, []string{ev.Organizer.Email}
	}
	data.Recipients = []store.RecipientSet{
		{Type: "from", ParticipantIDs: fromIDs, DisplayNames: fromNames, EmailAddresses: fromEmails},
		{Type: "to", ParticipantIDs: attendeeIDs, DisplayNames: toNames, EmailAddresses: toEmails},
	}
	return data
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
	b, err := json.Marshal(m, json.Deterministic(true))
	if err != nil {
		return sql.NullString{}, fmt.Errorf("marshal merged metadata: %w", err)
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

// deriveSourceMessageID is the idempotency key: a standalone event or series
// master uses event.id; a recurring instance/exception/cancellation uses
// recurringEventId|originalStartTime so each occurrence upserts independently
// and a single cancelled occurrence flags only its own row. An original start
// that was only resolved from an offset-free dateTime plus timeZone keeps
// event.id, the key archives held before such values decoded to an instant.
// Google keeps one original-start representation per occurrence, so this key
// stays stable across syncs.
func deriveSourceMessageID(ev gcal.Event) string {
	if ev.RecurringEventID != "" {
		if orig := ev.OriginalStartTime; orig.Date == "" && !orig.DateTime.IsZero() && orig.ResolvedFromTimeZone {
			return ev.ID
		}
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

// MapParticipants normalizes provider people. humanOnly excludes rooms and
// resources and deduplicates explicit attendees for CalDAV recipients.
func MapParticipants(organizer gcalmodel.Person, attendees []gcalmodel.Attendee, humanOnly bool) (gcalmodel.Person, []gcalmodel.Attendee) {
	organizer.Email = gcalmodel.NormalizeParticipantEmail(organizer.Email)
	if humanOnly && calevent.IsRoomAttendee(organizer.Email, false) {
		organizer.Email = ""
	}
	var people []gcalmodel.Attendee
	seen := map[string]bool{}
	nonhuman := map[string]bool{}
	if humanOnly {
		for _, a := range attendees {
			email := gcalmodel.NormalizeParticipantEmail(a.Email)
			if email != "" && calevent.IsRoomAttendee(email, a.Resource) {
				nonhuman[email] = true
			}
		}
	}
	for _, a := range attendees {
		a.Email = gcalmodel.NormalizeParticipantEmail(a.Email)
		if humanOnly {
			if a.Email == "" || nonhuman[a.Email] || seen[a.Email] {
				continue
			}
		}
		seen[a.Email] = true
		people = append(people, a)
	}
	if nonhuman[organizer.Email] {
		organizer.Email = ""
	}
	return organizer, people
}
