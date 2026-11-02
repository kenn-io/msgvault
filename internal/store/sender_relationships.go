package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Raw email imports can carry historical address-derived ownership. Source
// merges rewrite provider IDs, so inspect their preserved original ID too.
func (s *Store) senderNativeOwnershipSQL() string {
	original := `COALESCE(NULLIF((SELECT p.original_source_message_id
  FROM source_merge_archive_only_messages p WHERE p.message_id=m.id), ''), m.source_message_id)`
	origin := `m.metadata ->> 'imap_content_origin'`
	if s.dialect.DriverName() != postgresDriverName {
		origin = `CASE WHEN json_valid(m.metadata) THEN m.metadata ->> 'imap_content_origin' END`
	}
	providerSent := `(src.source_type IN ('gmail','imap') AND (
  (src.source_type='imap' AND COALESCE(` + origin + `,'')='sent') OR
  EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
   WHERE ml.message_id=m.id AND l.source_id=m.source_id AND (` + sentFolderLabelSQL("l") + ` OR ` + sentGmailLabelSQL("l", "src") + `))))`
	return `(COALESCE(m.source_is_from_me,FALSE) OR ` + providerSent + `) AND NOT (
  COALESCE(NULLIF(m.message_type,''),'email')='email' AND (
  ` + original + ` LIKE 'mbox-%' OR ` + original + ` LIKE 'eml-%' OR
  ` + original + ` LIKE 'maildir-%' OR ` + original + ` LIKE 'emlx-%' OR
  ` + original + ` LIKE 'pst-%'))`
}

const (
	MaxSenderRelationshipAddresses = 100
	maxSenderRelationshipSources   = 512
	maxSenderRelationshipMessages  = 2048
	maxSenderRelationshipAudience  = 10000
)

// ErrSenderRelationshipLimit means the lookup could not establish a complete
// answer within its metadata budget. Callers must report uncertainty rather
// than treating the missing evidence as an unrelated address.
var ErrSenderRelationshipLimit = errors.New("sender relationship metadata budget exceeded")

type SenderRelationshipEvidence struct {
	// Relationship is one of contact, known, or none. It is independent of
	// message authentication and never inferred from message content.
	Relationship  string `json:"relationship"`
	SeenInArchive bool   `json:"seen_in_archive"`
}

type SenderRelationshipOptions struct {
	AllowedSourceIDs []int64
	// ExcludeMessageID prevents a draft receipt from using the draft itself
	// as prior history, even if dedup retained source-native sent evidence.
	ExcludeMessageID int64
}

// SenderRelationships derives address-level relationships without reading
// message bodies. Keys are lowercase, trimmed addresses. Participant aliases
// never transfer a curated binding or a send to another envelope address.
// nil allowedSourceIDs means the whole archive; a nonnil empty slice means no
// evidence. Every history lookup, including scoped contact reachability, uses
// that source scope. The result is not an authentication verdict.
func (s *Store) SenderRelationships(ctx context.Context, addresses []string, allowedSourceIDs []int64) (map[string]SenderRelationshipEvidence, error) {
	return s.SenderRelationshipsWithOptions(ctx, addresses, SenderRelationshipOptions{AllowedSourceIDs: allowedSourceIDs})
}

func (s *Store) SenderRelationshipsWithOptions(ctx context.Context, addresses []string, options SenderRelationshipOptions) (map[string]SenderRelationshipEvidence, error) {
	allowedSourceIDs := options.AllowedSourceIDs
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.ExcludeMessageID < 0 {
		return nil, errors.New("sender relationship excluded message id must not be negative")
	}
	if len(addresses) > MaxSenderRelationshipAddresses || len(allowedSourceIDs) > maxSenderRelationshipSources {
		return nil, ErrSenderRelationshipLimit
	}
	result := make(map[string]SenderRelationshipEvidence, len(addresses))
	for _, raw := range addresses {
		address := strings.ToLower(strings.TrimSpace(raw))
		if address == "" || len(address) > 512 || strings.ContainsFunc(address, unicode.IsControl) {
			return nil, errors.New("invalid sender relationship address")
		}
		result[address] = SenderRelationshipEvidence{Relationship: "none"}
	}
	sources := slices.Clone(allowedSourceIDs)
	for _, id := range sources {
		if id <= 0 {
			return nil, errors.New("sender relationship source id must be positive")
		}
	}
	slices.Sort(sources)
	sources = slices.Compact(sources)
	if len(result) == 0 || (allowedSourceIDs != nil && len(sources) == 0) {
		return result, nil
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if err := s.senderContactRelationships(ctx, tx, keys, sources, allowedSourceIDs != nil, options.ExcludeMessageID, result); err != nil {
			return err
		}
		return s.senderKnownRelationships(ctx, tx, keys, sources, options.ExcludeMessageID, result)
	})
	if err != nil {
		return nil, fmt.Errorf("resolve sender relationships: %w", err)
	}
	return result, nil
}

// senderSourceScope is only used with queries whose message alias is m.
func senderSourceScope(sources []int64, excluded int64) (string, []any) {
	var predicate string
	var args []any
	if excluded > 0 {
		predicate = " AND m.id<>?"
		args = append(args, excluded)
	}
	if len(sources) > 0 {
		predicate += " AND m.source_id IN (" + int64Placeholders(len(sources)) + ")"
		args = append(args, personMergeSnapshotIDArgs(sources)...)
	}
	return predicate, args
}

// Recipient evidence requires the historical envelope. Missing snapshots must
// not inherit the current address of a renamed or merged participant.
const senderRecipientAddress = `LOWER(COALESCE(TRIM(mr.email_address), ''))`

func (s *Store) senderContactRelationships(ctx context.Context, tx *loggedTx, keys []string, sources []int64, scoped bool, excluded int64, result map[string]SenderRelationshipEvidence) error {
	values := strings.TrimSuffix(strings.Repeat("(?),", len(keys)), ",")
	args := make([]any, 0, len(keys)+len(sources)*3)
	for _, key := range keys {
		args = append(args, key)
	}
	scope, sourceArgs := senderSourceScope(sources, excluded)
	// A mutable sender or membership pointer cannot establish which address
	// actually occurred in a historical message after participant merges.
	use := `EXISTS (SELECT 1 FROM message_recipients mr
		WHERE mr.message_id=m.id AND ` + senderRecipientAddress + `=requested.address)`
	real := `m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL AND NOT ` + s.activityUnsentSQL() + scope + ` AND ` + use
	// Unscoped, a saved contact with no archive history still counts. A
	// participant occurring exclusively in unsent drafts does not. Scoped
	// callers require a real observation inside their allowed sources.
	contactReach := `EXISTS (SELECT 1 FROM messages m JOIN sources src ON src.id=m.source_id WHERE ` + real + `)`
	if !scoped {
		// Mutable participant pointers are negative evidence only: they can
		// disprove empty history when a legacy draft lacks snapshots, but can
		// never establish a real observation or transfer trust to an address.
		anyUse := `(` + use + ` OR EXISTS (SELECT 1 FROM participants p
			WHERE LOWER(TRIM(p.email_address))=requested.address AND (
			m.sender_id=p.id OR EXISTS (SELECT 1 FROM message_recipients mr WHERE mr.message_id=m.id AND mr.participant_id=p.id)
			OR EXISTS (SELECT 1 FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id AND cp.participant_id=p.id))))`
		contactReach = `(` + contactReach + ` OR NOT EXISTS (SELECT 1 FROM messages m WHERE ` + anyUse + scope + `))`
	}
	query := `WITH requested(address) AS (VALUES ` + values + `)
		SELECT requested.address,
		CASE WHEN EXISTS (SELECT 1 FROM participants p JOIN person_participants pp ON pp.participant_id=p.id
		WHERE LOWER(p.email_address)=requested.address) AND ` + contactReach + ` THEN 1 ELSE 0 END,
		CASE WHEN EXISTS (SELECT 1 FROM messages m JOIN sources src ON src.id=m.source_id WHERE ` + real + `) THEN 1 ELSE 0 END
		FROM requested`
	args = append(args, sourceArgs...)
	if !scoped {
		args = append(args, sourceArgs...)
	}
	args = append(args, sourceArgs...)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read sender contacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var address string
		var contact, seen int
		if err := rows.Scan(&address, &contact, &seen); err != nil {
			return fmt.Errorf("scan sender contacts: %w", err)
		}
		e := result[address]
		e.SeenInArchive = seen != 0
		if contact != 0 {
			e.Relationship = "contact"
		}
		result[address] = e
	}
	return rows.Err()
}

// Chat evidence must be 1:1 and carry an exact historical recipient address.
const senderOneToOneChat = `c.conversation_type IN ('chat','direct_chat') AND (SELECT COUNT(*) FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id)=2`

func (s *Store) senderKnownRelationships(ctx context.Context, tx *loggedTx, keys []string, sources []int64, excluded int64, result map[string]SenderRelationshipEvidence) error {
	var before int64
	inspected := 0
	for {
		pending := make([]string, 0, len(keys))
		for _, key := range keys {
			if result[key].Relationship == "none" {
				pending = append(pending, key)
			}
		}
		if len(pending) == 0 {
			return nil
		}
		limit := min(256, maxSenderRelationshipMessages-inspected)
		ids, err := s.senderOutboundCandidateIDs(ctx, tx, pending, sources, excluded, before, limit+1)
		if err != nil {
			return err
		}
		more := len(ids) > limit
		if more {
			ids = ids[:limit]
		}
		if len(ids) == 0 {
			if more {
				return ErrSenderRelationshipLimit
			}
			return nil
		}
		addresses, err := s.senderOutboundAddresses(ctx, tx, ids)
		if err != nil {
			return err
		}
		candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, ids)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			// The candidate query already requires native ownership or trusted Sent
			// evidence; email sync does not set the native chat ownership flag.
			if !candidate.Eligible || candidate.Unsent {
				continue
			}
			classification := ClassifyActivityCandidate(candidate, DefaultMaxDirectActivityCounterparts)
			if classification.Broadcast || (classification.Channel != ChannelEmail && classification.Channel != ChannelChat) {
				continue
			}
			for _, address := range addresses[candidate.MessageID] {
				e, requested := result[address]
				if requested && e.Relationship == "none" {
					e.Relationship = "known"
					e.SeenInArchive = true
					result[address] = e
				}
			}
		}
		if !more {
			return nil
		}
		inspected += len(ids)
		before = ids[len(ids)-1]
	}
}

func (s *Store) senderOutboundCandidateIDs(ctx context.Context, tx *loggedTx, keys []string, sources []int64, excluded, before int64, limit int) ([]int64, error) {
	scope, sourceArgs := senderSourceScope(sources, excluded)
	args := append([]any{}, sourceArgs...)
	for _, key := range keys {
		args = append(args, key)
	}
	chat := senderOneToOneChat
	query := `SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id
		JOIN conversations c ON c.id=m.conversation_id
		WHERE ` + s.senderNativeOwnershipSQL() + ` AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND m.list_id IS NULL AND NOT ` + s.activityUnsentSQL() + scope + `
		AND (c.conversation_type='email_thread' OR (` + chat + `))
		AND EXISTS (SELECT 1 FROM message_recipients mr
		WHERE mr.message_id=m.id AND mr.recipient_type IN ('to','cc','bcc') AND ` + senderRecipientAddress + ` IN (` + int64Placeholders(len(keys)) + `))
		AND (?=0 OR m.id<?) ORDER BY m.id DESC LIMIT ?`
	args = append(args, before, before, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read sender outbound candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) senderOutboundAddresses(ctx context.Context, tx *loggedTx, ids []int64) (map[int64][]string, error) {
	marks := int64Placeholders(len(ids))
	args := personMergeSnapshotIDArgs(ids)
	args = append(args, personMergeSnapshotIDArgs(ids)...)
	// Count every envelope and membership before the projector loads the same
	// metadata, so a huge audience cannot cause an unbounded allocation.
	query := `SELECT m.id, ` + senderRecipientAddress + `,
		CASE WHEN mr.recipient_type IN ('to','cc','bcc') THEN 1 ELSE 0 END FROM messages m
		JOIN message_recipients mr ON mr.message_id=m.id
		WHERE m.id IN (` + marks + `)
		UNION ALL SELECT m.id, '', 0 FROM messages m
		JOIN conversation_participants cp ON cp.conversation_id=m.conversation_id
		WHERE m.id IN (` + marks + `)
		LIMIT ?`
	args = append(args, maxSenderRelationshipAudience+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read sender outbound envelopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := map[int64][]string{}
	count := 0
	for rows.Next() {
		count++
		if count > maxSenderRelationshipAudience {
			return nil, ErrSenderRelationshipLimit
		}
		var id int64
		var address string
		var chosen int
		if err := rows.Scan(&id, &address, &chosen); err != nil {
			return nil, err
		}
		if chosen != 0 {
			result[id] = append(result[id], address)
		}
	}
	return result, rows.Err()
}
