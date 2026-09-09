package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// IMAPRelocationCandidate binds a saved mailbox membership to the exact
// archived row whose canonical source location disappeared.
type IMAPRelocationCandidate struct {
	MessageIdentityGuard

	RFC822MessageID string
	Mailbox         string
	UIDValidity     uint32
	UID             uint32
}

// GetIMAPRelocationCandidatesContext reads only the requested canonical rows
// and their saved memberships. The caller validates membership epochs against
// the current authoritative mailbox topology before fetching a replacement.
func (s *Store) GetIMAPRelocationCandidatesContext(ctx context.Context, sourceID int64, lostSourceMessageIDs []string) ([]IMAPRelocationCandidate, error) {
	keys := slices.Clone(lostSourceMessageIDs)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	var candidates []IMAPRelocationCandidate
	for chunk := range slices.Chunk(keys, aliasUIDChunkSize) {
		found, err := s.readIMAPRelocationCandidates(ctx, sourceID, chunk)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, found...)
	}
	slices.SortFunc(candidates, func(a, b IMAPRelocationCandidate) int {
		if n := cmp.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Mailbox, b.Mailbox); n != 0 {
			return n
		}
		if n := cmp.Compare(a.UIDValidity, b.UIDValidity); n != 0 {
			return n
		}
		return cmp.Compare(a.UID, b.UID)
	})
	return candidates, nil
}

func (s *Store) readIMAPRelocationCandidates(ctx context.Context, sourceID int64, keys []string) ([]IMAPRelocationCandidate, error) {
	args := make([]any, 0, len(keys)+1)
	args = append(args, sourceID)
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := s.db.QueryContext(ctx, `
 SELECT m.id, m.source_id, m.source_message_id, m.rfc822_message_id,
        membership.mailbox, membership.uidvalidity, membership.uid
 FROM messages m
 JOIN imap_message_memberships membership
   ON membership.message_id = m.id AND membership.source_id = m.source_id
 WHERE m.source_id = ? AND m.source_message_id IN (?`+strings.Repeat(",?", len(keys)-1)+`)
   AND m.rfc822_message_id IS NOT NULL AND m.rfc822_message_id <> ''`, args...)
	if err != nil {
		return nil, fmt.Errorf("query IMAP relocation candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []IMAPRelocationCandidate
	for rows.Next() {
		var candidate IMAPRelocationCandidate
		if err := rows.Scan(&candidate.ID, &candidate.SourceID, &candidate.SourceMessageID, &candidate.RFC822MessageID, &candidate.Mailbox, &candidate.UIDValidity, &candidate.UID); err != nil {
			return nil, fmt.Errorf("scan IMAP relocation candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate IMAP relocation candidates: %w", err)
	}
	return candidates, nil
}
