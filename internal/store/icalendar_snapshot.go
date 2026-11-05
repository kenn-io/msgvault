package store

import (
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

// An invite's later emails apply only when their ORGANIZER matches the one
// that created the event. Addresses remain unauthenticated: a stranger who
// copies the ORGANIZER can update or cancel the derived event, and every
// email stays archived as evidence.
func acceptedICSSnapshot(q querier, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) ([]preparedICS, error) {
	organizers := make(map[string]string)
	accepted := make([]preparedICS, 0, len(items))
	for _, item := range items {
		c := item.component
		key := calevent.ICSMessageID(c.UID, "")
		organizer, seen := organizers[key]
		existed := false
		if !seen {
			var metadata sql.NullString
			err := q.QueryRow(`SELECT m.metadata FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE c.source_id=? AND c.source_conversation_id=? AND m.message_type='calendar_event' ORDER BY m.id LIMIT 1`, sourceID, key).Scan(&metadata)
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
				organizer, existed = family.InviteOrganizer, true
			}
			organizers[key] = organizer
		}
		// Within the creating calendar, siblings may share an omitted organizer.
		matches := strings.EqualFold(c.OrganizerEmail, organizer)
		if existed || c.Event.Status == SyncStatusCancelled {
			matches = matches && c.OrganizerEmail != ""
		}
		if item.fromEmail && !matches {
			summary.Diagnostics = append(summary.Diagnostics, fmt.Sprintf("calendar UID %q update ignored: ORGANIZER does not match invite_organizer", c.UID))
			continue
		}
		item.meta.InviteOrganizer = organizer
		accepted = append(accepted, item)
	}
	return accepted, nil
}
