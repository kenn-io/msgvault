package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	sentLabel := `EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
	   WHERE ml.message_id=m.id AND l.source_id=m.source_id AND (` + sentFolderLabelSQL("l") + ` OR ` + sentGmailLabelSQL("l", "src") + `))`
	providerSent := `((src.source_type IN ('gmail','imap','msmail') AND ` + sentLabel + `) OR
	  (src.source_type='imap' AND COALESCE(` + origin + `,'')='sent'))`
	email := `COALESCE(NULLIF(m.message_type,''),'email')='email'`
	// Microsoft Graph's legacy source_is_from_me value was address-derived.
	// Only its provider Sent Items role is native evidence for email ownership.
	nativeFlag := `(COALESCE(m.source_is_from_me,FALSE) AND NOT (` + email + ` AND src.source_type='msmail'))`
	return `(` + nativeFlag + ` OR ` + providerSent + `) AND NOT (` + email + ` AND ` + s.senderRawImportedIDSQL(original) + `)`
}

// senderRawImportedIDSQL recognizes complete importer-generated email IDs.
// Prefix-only checks misclassify native IMAP mailbox IDs such as eml-sent|1.
func (s *Store) senderRawImportedIDSQL(sourceMessageID string) string {
	if s.IsPostgreSQL() {
		return `(` + sourceMessageID + ` ~ '^(mbox-[0-9a-f]{64}-[0-9]+|eml-[0-9a-f]{64}|maildir-[0-9a-f]{64}|emlx-[0-9a-f]{64}|pst-[0-9a-f]{12}-[0-9]+|pst-[0-9]+)$')`
	}

	hex64 := strings.Repeat("[0-9a-f]", 64)
	hex12 := strings.Repeat("[0-9a-f]", 12)
	digitsFrom := func(start int) string {
		suffix := `SUBSTR(` + sourceMessageID + `,` + strconv.Itoa(start) + `)`
		return `(` + suffix + `<>'' AND ` + suffix + ` NOT GLOB '*[^0-9]*')`
	}
	return `((` + sourceMessageID + ` GLOB 'eml-` + hex64 + `') OR
		(` + sourceMessageID + ` GLOB 'maildir-` + hex64 + `') OR
		(` + sourceMessageID + ` GLOB 'emlx-` + hex64 + `') OR
		(` + sourceMessageID + ` GLOB 'mbox-` + hex64 + `-*' AND ` + digitsFrom(71) + `) OR
		(` + sourceMessageID + ` GLOB 'pst-` + hex12 + `-*' AND ` + digitsFrom(18) + `) OR
		(` + sourceMessageID + ` GLOB 'pst-*' AND ` + digitsFrom(5) + `))`
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
	query, args := s.senderContactRelationshipsQuery(keys, sources, scoped, excluded)
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

func (s *Store) senderContactRelationshipsQuery(keys []string, sources []int64, scoped bool, excluded int64) (string, []any) {
	values := strings.TrimSuffix(strings.Repeat("(?),", len(keys)), ",")
	args := make([]any, 0, len(keys)+(len(sources)+1)*4)
	for _, key := range keys {
		args = append(args, key)
	}
	scope, sourceArgs := senderSourceScope(sources, excluded)
	// Resolve every requested address in set-based joins. Correlated EXISTS
	// subqueries here made the database walk message history again for each
	// address in a batch.
	archiveUseCTE := ""
	archiveUseJoin := ""
	contactHistory := "real_history.address IS NOT NULL"
	if !scoped {
		// Mutable participant pointers are negative evidence only: they can
		// disprove empty history when a legacy draft lacks snapshots, but can
		// never establish a real observation or transfer trust to an address.
		archiveUseCTE = `,
		requested_participants(address, participant_id) AS (
			SELECT requested.address, p.id FROM requested JOIN participants p
			ON LOWER(TRIM(p.email_address))=requested.address
		),
		pointer_history(address) AS (
			SELECT rp.address FROM requested_participants rp
			JOIN messages m ON m.sender_id=rp.participant_id WHERE 1=1` + scope + `
			UNION
			SELECT rp.address FROM requested_participants rp
			JOIN message_recipients mr ON mr.participant_id=rp.participant_id
			JOIN messages m ON m.id=mr.message_id WHERE 1=1` + scope + `
			UNION
			SELECT rp.address FROM requested_participants rp
			JOIN conversation_participants cp ON cp.participant_id=rp.participant_id
			JOIN messages m ON m.conversation_id=cp.conversation_id WHERE 1=1` + scope + `
		),
		archive_use(address) AS (
			SELECT address FROM recipient_history
			UNION
			SELECT address FROM pointer_history
		)`
		archiveUseJoin = `LEFT JOIN archive_use ON archive_use.address=requested.address`
		contactHistory = `(real_history.address IS NOT NULL OR archive_use.address IS NULL)`
	}
	query := `WITH requested(address) AS (VALUES ` + values + `),
		recipient_history(address, message_id) AS (
			SELECT requested.address, m.id FROM requested
			JOIN message_recipients mr ON ` + senderRecipientAddress + `=requested.address
			JOIN messages m ON m.id=mr.message_id WHERE 1=1` + scope + `
		),
		real_history(address) AS (
			SELECT history.address FROM recipient_history history
			JOIN messages m ON m.id=history.message_id
			JOIN sources src ON src.id=m.source_id
			WHERE m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
			AND NOT ` + s.activityUnsentSQL() + `
			GROUP BY history.address
		)` + archiveUseCTE + `
		SELECT requested.address,
		CASE WHEN EXISTS (SELECT 1 FROM participants p JOIN person_participants pp ON pp.participant_id=p.id
		WHERE LOWER(p.email_address)=requested.address) AND ` + contactHistory + ` THEN 1 ELSE 0 END,
		CASE WHEN real_history.address IS NOT NULL THEN 1 ELSE 0 END
		FROM requested
		LEFT JOIN real_history ON real_history.address=requested.address ` + archiveUseJoin
	args = append(args, sourceArgs...)
	if !scoped {
		for range 3 {
			args = append(args, sourceArgs...)
		}
	}
	return query, args
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
