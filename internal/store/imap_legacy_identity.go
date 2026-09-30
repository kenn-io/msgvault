package store

import (
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
)

// resolveLegacyMessageID is used only after indexed exact candidates miss.
// Historical values are compared in memory without rewriting archive metadata.
func (r *imapMembershipResolver) resolveLegacyMessageID(value string) (int64, error) {
	key := mime.LegacyMessageIDMatchKey(value)
	if key == "" {
		return 0, nil
	}
	if r.legacyMessages == nil {
		rows, err := r.tx.QueryContext(r.ctx, `
			SELECT id, rfc822_message_id FROM messages
			WHERE source_id = ? AND rfc822_message_id IS NOT NULL
		`, r.sourceID)
		if err != nil {
			return 0, fmt.Errorf("load legacy IMAP identities: %w", err)
		}
		defer func() { _ = rows.Close() }()
		messages := make(map[string]int64)
		for rows.Next() {
			var id int64
			var stored string
			if err := rows.Scan(&id, &stored); err != nil {
				return 0, fmt.Errorf("scan legacy IMAP identity: %w", err)
			}
			storedKey := mime.LegacyMessageIDMatchKey(stored)
			if storedKey == "" {
				continue
			}
			if prior, exists := messages[storedKey]; exists && prior != id {
				// Zero marks ambiguity, including live/deleted row collisions.
				messages[storedKey] = 0
			} else {
				messages[storedKey] = id
			}
		}
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("iterate legacy IMAP identities: %w", err)
		}
		r.legacyMessages = messages
	}
	return r.legacyMessages[key], nil
}
