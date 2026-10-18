package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/calrecur"
)

const meetingLinkKeyVersion = 2

type meetingLinkKey struct {
	Kind, Value, Base, InstanceKey string
	ScheduledAt                    *time.Time
}

// Metadata is the importer-normalized evidence; retained raw fills older imports.
// Recording timestamps and provider-local opaque IDs are never match evidence.
func deriveMeetingLinkKeys(format string, raw, metadata []byte) []meetingLinkKey {
	var fields, meta map[string]any
	_ = json.Unmarshal(raw, &fields)
	_ = json.Unmarshal(metadata, &meta)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return strings.TrimSpace(v) }
	nested := func(k string) map[string]any { v, _ := fields[k].(map[string]any); return v }
	first := func(values ...string) string {
		for _, v := range values {
			if v != "" {
				return v
			}
		}
		return ""
	}
	platform := str(meta, "platform")
	var kind, value, scheduled string
	var scheduleCandidates []string
	switch {
	case format == "granola_json" || platform == "granola":
		kind = "google_event_id"
		calendar := nested("calendar_event")
		value = first(str(meta, "calendar_event_id"), str(calendar, "calendar_event_id"), str(fields, "calendar_event_id"))
		scheduled = first(str(meta, "scheduled_start"), str(calendar, "scheduled_start_time"), str(fields, "scheduled_start"))
	case format == "twenty_json" || platform == "twenty" || str(meta, "provider") == "twenty":
		kind = "ical_uid"
		calendar := nested("calendar_event")
		value = first(str(meta, "calendar_ical_uid"), str(calendar, "iCalUid"))
		scheduled = first(str(meta, "scheduled_start"), str(calendar, "startsAt"))
	case format == "circleback_json" || platform == "circleback":
		kind = "join_key"
		meeting := nested("meeting")
		value = calevent.JoinKey(first(str(meta, "meeting_url"), str(meta, "join_url"), str(meeting, "url"), str(meeting, "meetingUrl"), str(meeting, "joinUrl"), str(meeting, "meeting_url"), str(fields, "meeting_url"), str(fields, "join_url")))
		scheduleCandidates = []string{
			str(meta, "scheduled_start"), str(meeting, "scheduled_start"), str(meta, "start"),
			str(meeting, "startTime"), str(meeting, "date"), str(fields, "startTime"), str(fields, "date"), str(fields, "scheduled_start"),
		}
	}
	if value == "" {
		return nil
	}
	key := meetingLinkKey{Kind: kind, Value: value}
	if kind == "google_event_id" {
		if base, id, ok := calrecur.ParseInstanceID(value); ok {
			key.Base, key.InstanceKey = base, id.Key()
		}
	}
	layouts := []string{time.RFC3339Nano}
	// Circleback's importer and content decoder also accept these legacy UTC forms.
	if kind == "join_key" {
		layouts = append(layouts, "2006-01-02T15:04:05", "2006-01-02 15:04:05", time.DateOnly)
	}
	if len(scheduleCandidates) == 0 {
		scheduleCandidates = []string{scheduled}
	}
	for _, candidate := range scheduleCandidates {
		for _, layout := range layouts {
			if at, err := time.Parse(layout, candidate); err == nil {
				at = at.UTC()
				key.ScheduledAt = &at
				break
			}
		}
		if key.ScheduledAt != nil {
			break
		}
	}

	return []meetingLinkKey{key}
}

func (s *Store) refreshMeetingLinkKeysWith(ctx context.Context, tx *loggedTx, messageID int64, messageType, format string, raw, metadata []byte) error {
	q := boundQuerier{ctx: ctx, q: tx}
	if messageType != meetingTranscriptMessageType {
		if _, err := q.Exec(`DELETE FROM meeting_link_keys WHERE message_id = ?`, messageID); err != nil {
			return fmt.Errorf("clear meeting keys: %w", err)
		}
		_, err := q.Exec(`DELETE FROM meeting_link_key_state WHERE message_id = ?`, messageID)
		return err
	}
	keys := deriveMeetingLinkKeys(format, raw, metadata)
	encoded, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	var version int
	var oldHash string
	err = q.QueryRow(`SELECT key_version, key_hash FROM meeting_link_key_state WHERE message_id = ?`, messageID).Scan(&version, &oldHash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read meeting key state: %w", err)
	}
	if err == nil && version == meetingLinkKeyVersion && oldHash == hash {
		return nil
	}
	if _, err := q.Exec(`DELETE FROM meeting_link_keys WHERE message_id = ?`, messageID); err != nil {
		return err
	}
	for ordinal, key := range keys {
		if _, err := q.Exec(`INSERT INTO meeting_link_keys(message_id, ordinal, kind, value, base, instance_key, scheduled_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, messageID, ordinal, key.Kind, key.Value, key.Base, key.InstanceKey, key.ScheduledAt); err != nil {
			return fmt.Errorf("write meeting key: %w", err)
		}
	}
	_, err = q.Exec(`INSERT INTO meeting_link_key_state(message_id, key_version, key_hash) VALUES (?, ?, ?) ON CONFLICT(message_id) DO UPDATE SET key_version = excluded.key_version, key_hash = excluded.key_hash`, messageID, meetingLinkKeyVersion, hash)
	return err
}
