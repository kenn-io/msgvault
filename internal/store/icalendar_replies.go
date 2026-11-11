package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
)

type icsReply struct {
	Organizer string    `json:"organizer"`
	Attendee  string    `json:"attendee"`
	Original  string    `json:"original,omitempty"`
	RSVP      string    `json:"rsvp"`
	Sequence  int64     `json:"sequence"`
	Stamp     time.Time `json:"stamp"`
}

type icsReplies map[string]icsReply

func replyKey(original, attendee string) string {
	return original + "|" + attendee
}

func newerReply(a, b icsReply) bool {
	if a.Sequence != b.Sequence {
		return a.Sequence > b.Sequence
	}
	if !a.Stamp.Equal(b.Stamp) {
		return a.Stamp.After(b.Stamp)
	}
	return a.RSVP > b.RSVP
}

func readReplies(metadata []byte) (icsReplies, error) {
	var fields map[string]json.RawMessage
	if len(metadata) == 0 {
		return icsReplies{}, nil
	}
	if err := json.Unmarshal(metadata, &fields); err != nil {
		return nil, err
	}
	var replies icsReplies
	if raw := fields["ics_replies"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &replies); err != nil {
			return nil, err
		}
	}
	if replies == nil {
		replies = icsReplies{}
	}
	return replies, nil
}

func applyICSReplies(attendees, baseline []calevent.Attendee, replies icsReplies, meta calevent.ICSMetadata, original string) {
	revision := calevent.ICSRevision{Sequence: meta.Sequence, Stamp: meta.Stamp}
	if meta.Details != nil {
		revision = *meta.Details
	}
	for i := range attendees {
		a := &attendees[i]
		email := strings.ToLower(strings.TrimSpace(a.Email))
		global, hasGlobal := replies[replyKey("", email)]
		instance, hasInstance := replies[replyKey(original, email)]
		hasGlobal = hasGlobal && global.Organizer == meta.Organizer && global.Sequence == revision.Sequence
		hasInstance = hasInstance && instance.Organizer == meta.Organizer && instance.Sequence == revision.Sequence
		reply, ok := global, hasGlobal
		if original != "" && hasInstance && (!ok || newerReply(instance, reply)) {
			reply, ok = instance, true
		}
		if !ok {
			continue
		}
		status := a.RSVP
		for _, saved := range baseline {
			if strings.EqualFold(saved.Email, a.Email) {
				status = saved.RSVP
				break
			}
		}
		if (status == "accepted" || status == "declined" || status == "tentative") && !reply.Stamp.After(revision.Stamp) {
			continue
		}
		a.RSVP = reply.RSVP
	}
}

func calendarReplyBaseline(ctx context.Context, db *loggedDB, messageID int64) ([]calevent.Attendee, error) {
	var raw []byte
	var format, compression, metadata, config sql.NullString
	err := db.QueryRowContext(ctx, `SELECT r.raw_data,r.raw_format,r.compression,m.metadata,s.sync_config FROM messages m JOIN message_raw r ON r.message_id=m.id JOIN sources s ON s.id=m.source_id WHERE m.id=?`, messageID).Scan(&raw, &format, &compression, &metadata, &config)
	if err != nil {
		return nil, err
	}
	event, ok := decodeCalendarProjection(format.String, raw, compression, []byte(metadata.String), calendarConfigZone(config))
	if !ok {
		return nil, nil
	}
	return event.Attendees, nil
}

func (s *Store) persistICSReply(ctx context.Context, tx *loggedTx, sourceID int64, component calevent.ICSComponent) (bool, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	family, _, err := s.icsFamily(ctx, tx, sourceID, 0, calevent.ICSMessageID(component.UID, ""), false)
	if err != nil {
		return false, err
	}
	id, err := ensureConversationWithTypePolicy(q, s.dialect, sourceID, family, "calendar", "", false)
	if err != nil {
		return false, err
	}
	var metadata sql.NullString
	if err := q.QueryRow(`SELECT metadata FROM conversations WHERE id=?`, id).Scan(&metadata); err != nil {
		return false, err
	}
	replies, err := readReplies([]byte(metadata.String))
	if err != nil {
		return false, fmt.Errorf("decode reply ledger: %w", err)
	}
	original := ""
	if component.Event.Original != nil {
		original = component.Event.Original.Key()
	}
	a := component.Event.Attendees[0]
	reply := icsReply{Organizer: component.OrganizerEmail, Attendee: a.Email, Original: original, RSVP: a.RSVP, Sequence: component.Sequence, Stamp: component.Stamp}
	key := replyKey(original, a.Email)
	if prior, ok := replies[key]; ok && !newerReply(reply, prior) {
		return false, nil
	}
	replies[key] = reply
	fields := map[string]json.RawMessage{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return false, err
		}
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	encoded, err := json.Marshal(replies)
	if err != nil {
		return false, err
	}
	fields["ics_replies"] = encoded
	encoded, err = json.Marshal(fields)
	if err != nil {
		return false, err
	}
	if _, err := q.Exec(`UPDATE conversations SET metadata = `+s.dialect.JSONBindExpr()+` WHERE id=?`, string(encoded), id); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE conversation_id=? AND message_type='calendar_event'`, id)
	if err != nil {
		return false, err
	}
	var ids []int64
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return false, err
		}
		ids = append(ids, eventID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, eventID := range ids {
		if err := s.refreshCalendarEventProjectionWith(ctx, tx, eventID); err != nil {
			return false, err
		}
	}
	if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
		return false, err
	}
	return true, nil
}
