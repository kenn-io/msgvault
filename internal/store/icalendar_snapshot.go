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
func acceptedICSSnapshot(q querier, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) ([]preparedICS, error) {
	organizers := make(map[string]string)
	accepted := make([]preparedICS, 0, len(items))
	for _, item := range items {
		c := item.component
		key := calevent.ICSMessageID(c.UID, "")
		organizer, seen := organizers[key]
		if !seen {
			var metadata sql.NullString
			err := q.QueryRow(`SELECT m.metadata FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE c.source_id=? AND c.id IN (SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=? UNION ALL SELECT conversation_id FROM source_merge_conversations WHERE original_conversation_id=?) AND m.message_type='calendar_event' AND COALESCE(m.metadata ->> 'invite_organizer', '') <> '' ORDER BY m.id LIMIT 1`, sourceID, sourceID, key, key).Scan(&metadata)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				organizer = c.OrganizerEmail
			case err != nil:
				return nil, fmt.Errorf("read ICS family organizer: %w", err)
			default:
				var family calevent.ICSMetadata
				if err := json.Unmarshal([]byte(metadata.String), &family); err != nil {
					return nil, fmt.Errorf("decode ICS family metadata: %w", err)
				}
				organizer = family.InviteOrganizer
			}
			organizers[key] = organizer
		}
		if item.fromEmail && organizer != "" && !strings.EqualFold(c.OrganizerEmail, organizer) {
			summary.Diagnostics = append(summary.Diagnostics, fmt.Sprintf("calendar UID %q update ignored: ORGANIZER does not match invite_organizer", c.UID))
			continue
		}
		if organizer == "" {
			organizer = c.OrganizerEmail
			organizers[key] = organizer
		}
		item.meta.InviteOrganizer = organizer
		accepted = append(accepted, item)
	}
	return accepted, nil
}
