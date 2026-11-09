package store

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

// Later emails must match the stored organizer once an accepted revision names
// one. Addresses remain unauthenticated: a stranger who
// copies the ORGANIZER can update or cancel the derived event, and every
// email stays archived as evidence.
func acceptedICSSnapshot(q querier, sourceID int64, item preparedICS, summary *ICalendarImportSummary) (preparedICS, bool, error) {
	c := item.component
	key := calevent.ICSMessageID(c.UID, "")
	var organizer string
	var metadata sql.NullString
	// Start from the family IDs; COALESCE keeps SQLite off the message_type index.
	err := q.QueryRow(`SELECT m.metadata FROM (SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=? UNION ALL SELECT conversation_id FROM source_merge_conversations WHERE original_conversation_id=?) family JOIN conversations c ON c.id=family.id JOIN messages m ON m.conversation_id=c.id WHERE c.source_id=? AND COALESCE(m.message_type, '')='calendar_event' AND COALESCE(m.metadata ->> 'invite_organizer', '') <> '' ORDER BY m.id LIMIT 1`, sourceID, key, key, sourceID).Scan(&metadata)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		organizer = c.OrganizerEmail
	case err != nil:
		return preparedICS{}, false, fmt.Errorf("read ICS family organizer: %w", err)
	default:
		var family calevent.ICSMetadata
		if err := json.Unmarshal([]byte(metadata.String), &family); err != nil {
			return preparedICS{}, false, fmt.Errorf("decode ICS family metadata: %w", err)
		}
		organizer = family.InviteOrganizer
	}
	if item.fromEmail && organizer != "" && !strings.EqualFold(c.OrganizerEmail, organizer) {
		summary.Diagnostics = append(summary.Diagnostics, fmt.Sprintf("calendar UID %q update ignored: ORGANIZER does not match invite_organizer", c.UID))
		return preparedICS{}, false, nil
	}
	if organizer == "" {
		organizer = c.OrganizerEmail
	}
	item.meta.InviteOrganizer = organizer
	return item, true, nil
}
