package store

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/meetingcontent"
)

// meetingLinkKey is a tuple so provider identifiers cannot collide through separators.
type meetingLinkKey [4]string

func canonicalMeetingKey(kind, namespace, value string, at *time.Time) meetingLinkKey {
	key := meetingLinkKey{kind, strings.ToLower(strings.TrimSpace(namespace)), value, ""}
	if at != nil {
		key[3] = at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
	}
	return key
}

func (k meetingLinkKey) text() string {
	value, _ := json.Marshal(k)
	return string(value)
}

func deriveMeetingLinkKey(content meetingcontent.Content) meetingLinkKey {
	evidence := content.LinkEvidence
	if evidence.GoogleEventID != "" && strings.TrimSpace(evidence.CalendarOwner) != "" {
		return canonicalMeetingKey("google_event_id", evidence.CalendarOwner, evidence.GoogleEventID, nil)
	}
	if evidence.ICalUID != "" && evidence.ScheduledAt != nil {
		return canonicalMeetingKey("ical_uid", "", evidence.ICalUID, evidence.ScheduledAt)
	}
	return meetingLinkKey{}
}

func replaceMeetingLinkKey(ctx context.Context, tx *loggedTx, messageID int64, key meetingLinkKey) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM meeting_link_key WHERE message_id = ?`, messageID); err != nil {
		return fmt.Errorf("clear meeting key: %w", err)
	}
	if key[0] != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meeting_link_key(message_id,key) VALUES (?,?)`, messageID, key.text()); err != nil {
			return fmt.Errorf("write meeting key: %w", err)
		}
	}
	return nil
}
