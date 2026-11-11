package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// RecipientProvenance describes archive evidence for an actual draft recipient.
// Address is untrusted display data. The other fields are derived labels, not
// authentication, permission to send, or proof of where a caller got an address.
type RecipientProvenance struct {
	Address       string `json:"address"`
	Relationship  string `json:"relationship"`
	SeenInArchive bool   `json:"seen_in_archive"`
	Flag          string `json:"flag"`
	Basis         string `json:"basis"`
}

// DraftRecipientProvenanceContext reads the persisted email envelope and its
// prior evidence in one snapshot. nil sourceIDs admits owner archive evidence;
// an empty non-nil slice admits none. The current draft is explicitly excluded
// from historical evidence. Message bodies are not read.
func (s *Store) DraftRecipientProvenanceContext(ctx context.Context, messageID int64, sourceIDs []int64) ([]RecipientProvenance, error) {
	if messageID <= 0 || (sourceIDs != nil && len(sourceIDs) == 0) {
		return nil, errors.New("recipient provenance requires a message and readable sources")
	}
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	var result []RecipientProvenance
	err = s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var sourceID int64
		var messageType string
		if err := tx.QueryRowContext(ctx, `SELECT source_id, message_type FROM messages WHERE id = ?`, messageID).Scan(&sourceID, &messageType); err != nil {
			return err
		}
		if messageType != MessageTypeEmail || (sourceIDs != nil && !slices.Contains(sourceIDs, sourceID)) {
			return errors.New("draft email envelope not readable")
		}
		addresses, err := provenanceRecipientsContext(ctx, tx, messageID)
		if err != nil {
			return err
		}
		relationships, err := s.SenderRelationshipsWithOptions(ctx, addresses, SenderRelationshipOptions{
			AllowedSourceIDs: sourceIDs, ExcludeMessageID: messageID,
		})
		if err != nil {
			return err
		}
		for _, address := range addresses {
			evidence, found := relationships[strings.ToLower(strings.TrimSpace(address))]
			if !found || (evidence.Relationship != "contact" && evidence.Relationship != "known" && evidence.Relationship != "none") {
				return errors.New("recipient relationship evidence unavailable")
			}
			recipient := RecipientProvenance{
				Address: address, Relationship: evidence.Relationship, SeenInArchive: evidence.SeenInArchive,
				Flag: "unseen", Basis: "no_prior_evidence",
			}
			switch {
			case evidence.Relationship == "contact":
				recipient.Flag, recipient.Basis = "ok", "curated_contact"
			case evidence.Relationship == "known":
				recipient.Flag, recipient.Basis = "ok", "owner_sent_to"
			case evidence.SeenInArchive:
				recipient.Flag, recipient.Basis = "archive_only", "no_relationship"
			}
			result = append(result, recipient)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read draft recipient provenance: %w", err)
	}
	return result, nil
}

func provenanceRecipientsContext(ctx context.Context, tx *loggedTx, messageID int64) ([]string, error) {
	// At most three roles for each unique address. Bound raw rows as well as
	// unique addresses before allocating or resolving historical evidence.
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(email_address, '') FROM message_recipients
        WHERE message_id = ? AND recipient_type IN ('to', 'cc', 'bcc')
        ORDER BY id LIMIT ?`, messageID, MaxSenderRelationshipAddresses*3+1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var addresses []string
	seen := make(map[string]bool)
	count := 0
	for rows.Next() {
		count++
		if count > MaxSenderRelationshipAddresses*3 {
			return nil, ErrSenderRelationshipLimit
		}
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		key := strings.ToLower(strings.TrimSpace(address))
		if key == "" {
			return nil, errors.New("draft recipient envelope unavailable")
		}
		if !seen[key] {
			seen[key] = true
			addresses = append(addresses, address)
			if len(addresses) > MaxSenderRelationshipAddresses {
				return nil, ErrSenderRelationshipLimit
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("draft recipient envelope unavailable")
	}
	return addresses, nil
}
