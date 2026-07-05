package store

import (
	"fmt"
)

// IMAPFolderState records the UIDVALIDITY and UIDNEXT of one IMAP
// mailbox as observed at the start of the last fully completed sync.
// A mailbox whose current values match the saved ones has received no
// new messages since that sync and can be skipped entirely.
type IMAPFolderState struct {
	Mailbox     string
	UIDValidity uint32
	UIDNext     uint32
}

// GetIMAPFolderStates returns the saved per-mailbox sync states for a source.
func (s *Store) GetIMAPFolderStates(sourceID int64) ([]IMAPFolderState, error) {
	rows, err := s.db.Query(`
		SELECT mailbox, uidvalidity, uidnext
		FROM imap_folder_state
		WHERE source_id = ?
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query imap folder states for source %d: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()

	var states []IMAPFolderState
	for rows.Next() {
		var st IMAPFolderState
		if err := rows.Scan(&st.Mailbox, &st.UIDValidity, &st.UIDNext); err != nil {
			return nil, fmt.Errorf("scan imap folder state: %w", err)
		}
		states = append(states, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate imap folder states: %w", err)
	}
	return states, nil
}

// UpsertIMAPFolderStates saves per-mailbox sync states for a source,
// overwriting any existing state for the same mailbox. States for
// mailboxes not in the given slice are left untouched.
func (s *Store) UpsertIMAPFolderStates(sourceID int64, states []IMAPFolderState) error {
	for _, st := range states {
		_, err := s.db.Exec(fmt.Sprintf(`
			INSERT INTO imap_folder_state (source_id, mailbox, uidvalidity, uidnext, updated_at)
			VALUES (?, ?, ?, ?, %s)
			ON CONFLICT(source_id, mailbox) DO UPDATE SET
				uidvalidity = excluded.uidvalidity,
				uidnext = excluded.uidnext,
				updated_at = %s
		`, s.dialect.Now(), s.dialect.Now()),
			sourceID, st.Mailbox, st.UIDValidity, st.UIDNext)
		if err != nil {
			return fmt.Errorf("upsert imap folder state for %q: %w", st.Mailbox, err)
		}
	}
	return nil
}
