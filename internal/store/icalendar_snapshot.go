package store

import (
	"fmt"

	"go.kenn.io/msgvault/internal/calevent"
)

// Only accepted organizers contribute to snapshot membership and revival.
func acceptedICSSnapshot(q querier, sourceID int64, items []preparedICS, summary *ICalendarImportSummary) ([]preparedICS, error) {
	families := make(map[string]icsFamilyContinuity)
	accepted := make([]preparedICS, 0, len(items))
	for _, item := range items {
		c := item.component
		key := calevent.ICSMessageID(c.UID, "")
		family, seen := families[key]
		if !seen {
			var err error
			family, err = readICSFamilyContinuity(q, sourceID, key, c.OrganizerEmail)
			if err != nil {
				if recordICSInterpretationDiagnostic(item, summary, fmt.Errorf("calendar UID %q update ignored: %w", c.UID, err)) {
					continue
				}
				return nil, err
			}
			families[key] = family
		}
		if item.fromEmail && !family.allows(c) {
			summary.Diagnostics = append(summary.Diagnostics, fmt.Sprintf("calendar UID %q update ignored: ORGANIZER does not match invite_organizer", c.UID))
			continue
		}
		item.meta.InviteOrganizer = family.organizer
		accepted = append(accepted, item)
	}
	return accepted, nil
}
