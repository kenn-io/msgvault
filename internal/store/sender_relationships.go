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
	microsoftSent := `m.metadata ->> 'msmail_sent_items_observed'='true'`
	if s.dialect.DriverName() != postgresDriverName {
		microsoftSent = `CASE WHEN json_valid(m.metadata) THEN json_extract(m.metadata, '$.msmail_sent_items_observed') END=1`
	}
	msmailSentEnvelope := `EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent
	   WHERE sent.message_id=m.id)`
	sentLabel := `EXISTS (SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
	   WHERE ml.message_id=m.id AND l.source_id=m.source_id AND (` + sentFolderLabelSQL("l") + ` OR ` + sentGmailLabelSQL("l", "src") + `))`
	providerSent := `((src.source_type IN ('gmail','imap','msmail') AND ` + sentLabel + `) OR
	  (src.source_type='imap' AND COALESCE(` + origin + `,'')='sent') OR
	  (src.source_type='msmail' AND (` + microsoftSent + ` OR ` + msmailSentEnvelope + `)))`
	email := `COALESCE(NULLIF(m.message_type,''),'email')='email'`
	// Microsoft Graph's legacy source_is_from_me value was address-derived.
	// Only provider-observed Sent envelope evidence can exempt a raw-import ID.
	nativeFlag := `(COALESCE(m.source_is_from_me,FALSE) AND NOT (` + email + ` AND src.source_type='msmail'))`
	rawImported := `(` + email + ` AND ` + s.senderRawImportedIDSQL(original) + `)`
	allowRawMicrosoftSent := `(src.source_type='msmail' AND ` + msmailSentEnvelope + `)`
	return `(` + nativeFlag + ` OR ` + providerSent + `) AND NOT (` + rawImported + ` AND NOT ` + allowRawMicrosoftSent + `)`
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
	MaxSenderRelationshipAddresses      = 100
	maxSenderRelationshipSources        = 512
	maxSenderRelationshipHistoryEntries = 2048
	maxSenderRelationshipAudience       = 10000
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
		history, err := s.senderContactRelationships(ctx, tx, keys, sources, allowedSourceIDs != nil, options.ExcludeMessageID, result)
		if err != nil {
			return err
		}
		return s.senderKnownRelationships(ctx, tx, keys, sources, options.ExcludeMessageID, history, result)
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

func (s *Store) senderContactRelationships(ctx context.Context, tx *loggedTx, keys []string, sources []int64, scoped bool, excluded int64, result map[string]SenderRelationshipEvidence) (map[string][]int64, error) {
	history, err := s.senderContactRelationshipHistory(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	query, args := s.senderContactRelationshipsQuery(keys, sources, scoped, excluded)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read sender contacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var address string
		var contact, seen int
		if err := rows.Scan(&address, &contact, &seen); err != nil {
			return nil, fmt.Errorf("scan sender contacts: %w", err)
		}
		e := result[address]
		e.SeenInArchive = seen != 0
		if contact != 0 {
			e.Relationship = "contact"
		}
		result[address] = e
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return history, nil
}

// senderContactRelationshipHistory snapshots every requested address's
// envelope history before source, deletion, and draft eligibility checks.
// Existence probes otherwise can scan unbounded deleted, draft-only, or
// out-of-scope rows while trying to establish that no real history exists.
func (s *Store) senderContactRelationshipHistory(ctx context.Context, tx *loggedTx, keys []string) (map[string][]int64, error) {
	history := make(map[string][]int64, len(keys))
	inspectedEntries := 0
	for _, key := range keys {
		remaining := maxSenderRelationshipHistoryEntries - inspectedEntries
		ids, err := s.senderHistoryMessageIDs(ctx, tx, key, nil, 0, 0, remaining+1)
		if err != nil {
			return nil, err
		}
		if len(ids) > remaining {
			return nil, ErrSenderRelationshipLimit
		}
		inspectedEntries += len(ids)
		history[key] = ids
	}
	return history, nil
}

func (s *Store) senderContactRelationshipsQuery(keys []string, sources []int64, scoped bool, excluded int64) (string, []any) {
	values := strings.TrimSuffix(strings.Repeat("(?),", len(keys)), ",")
	args := make([]any, 0, len(keys)+(len(sources)+1)*5)
	for _, key := range keys {
		args = append(args, key)
	}
	scope, sourceArgs := senderSourceScope(sources, excluded)
	// Use indexed probes for existence. Materializing every matching message
	// before the outbound history budget made high-volume addresses unbounded.
	recipientJoin := `JOIN messages m ON m.id=mr.message_id`
	recipientLink := ""
	if !s.IsPostgreSQL() {
		// Keep recipient envelopes first so SQLite probes the normalized-address
		// index for each requested address instead of scanning message history.
		recipientJoin = `CROSS JOIN messages m`
		recipientLink = `m.id=mr.message_id AND `
	}
	realHistory := `(EXISTS (SELECT 1 FROM message_recipients mr ` + recipientJoin + `
		JOIN sources src ON src.id=m.source_id
		WHERE ` + recipientLink + senderRecipientAddress + `=requested.address
		AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=m.id)
		AND NOT ` + s.activityUnsentSQL() + scope + `) OR EXISTS (
		SELECT 1 FROM msmail_sent_envelope_recipients sent
		JOIN messages m ON m.id=sent.message_id JOIN sources src ON src.id=m.source_id
		WHERE sent.email_address=requested.address
		AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND NOT ` + s.activityUnsentSQL() + scope + `))`
	archiveRecipientHistory := `(EXISTS (SELECT 1 FROM message_recipients mr ` + recipientJoin + `
		WHERE ` + recipientLink + senderRecipientAddress + `=requested.address
		AND NOT EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=m.id)` + scope + `) OR EXISTS (
		SELECT 1 FROM msmail_sent_envelope_recipients sent JOIN messages m ON m.id=sent.message_id
		WHERE sent.email_address=requested.address` + scope + `))`
	archiveUseCTE := ""
	archiveUseJoin := ""
	contactHistory := "real_history.address IS NOT NULL"
	if !scoped {
		// Mutable participant pointers are negative evidence only: they can
		// disprove empty history when a legacy draft lacks snapshots, but can
		// never establish a real observation or transfer trust to an address.
		pointerHistory := `(
			EXISTS (SELECT 1 FROM participants p JOIN messages m ON m.sender_id=p.id
				WHERE LOWER(TRIM(p.email_address))=requested.address` + scope + `) OR
			EXISTS (SELECT 1 FROM participants p JOIN message_recipients mr ON mr.participant_id=p.id
				JOIN messages m ON m.id=mr.message_id
				WHERE LOWER(TRIM(p.email_address))=requested.address` + scope + `) OR
			EXISTS (SELECT 1 FROM participants p JOIN conversation_participants cp ON cp.participant_id=p.id
				JOIN messages m ON m.conversation_id=cp.conversation_id
				WHERE LOWER(TRIM(p.email_address))=requested.address` + scope + `))`
		archiveUseCTE = `,
		archive_use(address) AS (
			SELECT requested.address FROM requested
			WHERE ` + archiveRecipientHistory + ` OR ` + pointerHistory + `
		)`
		archiveUseJoin = `LEFT JOIN archive_use ON archive_use.address=requested.address`
		contactHistory = `(real_history.address IS NOT NULL OR archive_use.address IS NULL)`
	}
	query := `WITH requested(address) AS (VALUES ` + values + `),
		real_history(address) AS (
			SELECT requested.address FROM requested WHERE ` + realHistory + `
		)` + archiveUseCTE + `
		SELECT requested.address,
		CASE WHEN EXISTS (SELECT 1 FROM participants p JOIN person_participants pp ON pp.participant_id=p.id
		WHERE LOWER(p.email_address)=requested.address) AND ` + contactHistory + ` THEN 1 ELSE 0 END,
		CASE WHEN real_history.address IS NOT NULL THEN 1 ELSE 0 END
		FROM requested
		LEFT JOIN real_history ON real_history.address=requested.address ` + archiveUseJoin
	args = append(args, sourceArgs...)
	scopeCopies := 1
	if !scoped {
		scopeCopies = 6
	}
	for range scopeCopies {
		args = append(args, sourceArgs...)
	}
	return query, args
}

// Chat evidence must be 1:1 and carry an exact historical recipient address.
const senderOneToOneChat = `c.conversation_type IN ('chat','direct_chat') AND (SELECT COUNT(*) FROM conversation_participants cp WHERE cp.conversation_id=m.conversation_id)=2`

func (s *Store) senderKnownRelationships(ctx context.Context, tx *loggedTx, keys []string, sources []int64, excluded int64, history map[string][]int64, result map[string]SenderRelationshipEvidence) error {
	for _, key := range senderKnownRelationshipKeys(keys, result) {
		if evidence := result[key]; evidence.Relationship != "none" || !evidence.SeenInArchive {
			continue
		}
		historyIDs := history[key]
		for start := 0; start < len(historyIDs); start += 256 {
			end := min(start+256, len(historyIDs))
			batch := historyIDs[start:end]

			// The shared history snapshot includes From rows and other
			// non-candidates. It was budgeted before source and eligibility
			// filtering; only the candidate query below applies those rules.
			messageIDs := make([]int64, 0, len(batch))
			for _, messageID := range batch {
				if len(messageIDs) == 0 || messageIDs[len(messageIDs)-1] != messageID {
					messageIDs = append(messageIDs, messageID)
				}
			}
			candidateIDs, err := s.senderOutboundEligibleIDs(ctx, tx, []string{key}, sources, excluded, messageIDs)
			if err != nil {
				return err
			}
			if len(candidateIDs) > 0 {
				addresses, err := s.senderOutboundAddresses(ctx, tx, candidateIDs)
				if err != nil {
					return err
				}
				candidates, err := s.loadActivityCandidatesByIDQueryerContext(ctx, tx, candidateIDs)
				if err != nil {
					return err
				}
				for _, candidate := range candidates {
					// The candidate query already requires native ownership or trusted
					// Sent evidence. Carry that proof into classification even when the
					// provider records sent-folder ownership separately from source_is_from_me.
					candidate.SourceIsFromMe = true
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
			}
			if result[key].Relationship != "none" {
				break
			}
		}
	}
	return nil
}

func senderKnownRelationshipKeys(keys []string, evidence map[string]SenderRelationshipEvidence) []string {
	pending := make([]string, 0, len(keys))
	for _, key := range keys {
		current := evidence[key]
		if current.Relationship == "none" && current.SeenInArchive {
			pending = append(pending, key)
		}
	}
	return pending
}

func (s *Store) senderHistoryMessageIDs(ctx context.Context, tx *loggedTx, key string, sources []int64, excluded, before int64, limit int) ([]int64, error) {
	if limit <= 0 {
		return nil, nil
	}
	read := func(query string, args []any) ([]int64, error) {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
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
	recipientQuery, recipientArgs := s.senderHistoryMessageIDsQuery(key, sources, excluded, before, limit)
	recipientIDs, err := read(recipientQuery, recipientArgs)
	if err != nil {
		return nil, fmt.Errorf("read sender recipient history entries: %w", err)
	}
	sentQuery, sentArgs := s.senderSentEnvelopeHistoryMessageIDsQuery(key, sources, excluded, before, limit)
	sentIDs, err := read(sentQuery, sentArgs)
	if err != nil {
		return nil, fmt.Errorf("read sender Sent envelope history entries: %w", err)
	}
	ids := make([]int64, 0, min(limit, len(recipientIDs)+len(sentIDs)))
	for i, j := 0, 0; len(ids) < limit; {
		switch {
		case i >= len(recipientIDs) && j >= len(sentIDs):
			return ids, nil
		case j >= len(sentIDs) || (i < len(recipientIDs) && recipientIDs[i] >= sentIDs[j]):
			ids = append(ids, recipientIDs[i])
			i++
		case i >= len(recipientIDs) || sentIDs[j] > recipientIDs[i]:
			ids = append(ids, sentIDs[j])
			j++
		}
	}
	return ids, nil
}

func (s *Store) senderHistoryMessageIDsQuery(key string, sources []int64, excluded, before int64, limit int) (string, []any) {
	scope, sourceArgs := senderSourceScope(sources, excluded)
	args := []any{key}
	beforeFilter := ""
	if before > 0 {
		beforeFilter = ` AND mr.message_id<?`
		args = append(args, before)
	}
	args = append(args, sourceArgs...)
	query := `SELECT mr.message_id FROM message_recipients mr
		JOIN messages m ON m.id=mr.message_id
		WHERE ` + senderRecipientAddress + `=?` + beforeFilter + `
		AND NOT EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=m.id)` + scope + `
		ORDER BY mr.message_id DESC LIMIT ?`
	args = append(args, limit)
	return query, args
}

func (s *Store) senderSentEnvelopeHistoryMessageIDsQuery(key string, sources []int64, excluded, before int64, limit int) (string, []any) {
	scope, sourceArgs := senderSourceScope(sources, excluded)
	args := []any{key}
	beforeFilter := ""
	if before > 0 {
		beforeFilter = ` AND sent.message_id<?`
		args = append(args, before)
	}
	args = append(args, sourceArgs...)
	query := `SELECT sent.message_id FROM msmail_sent_envelope_recipients sent
		JOIN messages m ON m.id=sent.message_id
		WHERE sent.email_address=?` + beforeFilter + scope + `
		ORDER BY sent.message_id DESC LIMIT ?`
	args = append(args, limit)
	return query, args
}

func (s *Store) senderOutboundEligibleIDs(ctx context.Context, tx *loggedTx, keys []string, sources []int64, excluded int64, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query, args := s.senderOutboundEligibleIDsQuery(keys, sources, excluded, ids)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("filter sender relationship history entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var eligible []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		eligible = append(eligible, id)
	}
	return eligible, rows.Err()
}

func (s *Store) senderOutboundEligibleIDsQuery(keys []string, sources []int64, excluded int64, ids []int64) (string, []any) {
	messageValues := strings.TrimSuffix(strings.Repeat("(?),", len(ids)), ",")
	addressValues := strings.TrimSuffix(strings.Repeat("(?),", len(keys)), ",")
	args := make([]any, 0, len(ids)+2*len(keys)+len(sources)+1)
	args = append(args, personMergeSnapshotIDArgs(ids)...)
	scope, sourceArgs := senderSourceScope(sources, excluded)
	args = append(args, sourceArgs...)
	for _, key := range keys {
		args = append(args, key)
	}
	for _, key := range keys {
		args = append(args, key)
	}
	query := `SELECT m.id FROM messages m
		JOIN sources src ON src.id=m.source_id
		JOIN conversations c ON c.id=m.conversation_id
		WHERE m.id IN (` + messageValues + `)
		AND ` + s.senderNativeOwnershipSQL() + `
		AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		AND m.list_id IS NULL AND NOT ` + s.activityUnsentSQL() + scope + `
		AND (
			EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent
				WHERE sent.message_id=m.id AND sent.email_address IN (` + addressValues + `))
			OR (NOT EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent
				WHERE sent.message_id=m.id)
			AND EXISTS (SELECT 1 FROM message_recipients mr
				WHERE mr.message_id=m.id AND mr.recipient_type IN ('to','cc','bcc')
				AND ` + senderRecipientAddress + ` IN (` + addressValues + `)))
		)
		AND (c.conversation_type='email_thread' OR (` + senderOneToOneChat + `))
		ORDER BY m.id DESC`
	return query, args
}

func (s *Store) senderOutboundAddresses(ctx context.Context, tx *loggedTx, ids []int64) (map[int64][]string, error) {
	marks := int64Placeholders(len(ids))
	args := personMergeSnapshotIDArgs(ids)
	args = append(args, personMergeSnapshotIDArgs(ids)...)
	args = append(args, personMergeSnapshotIDArgs(ids)...)
	// Count every envelope and membership before the projector loads the same
	// metadata, so a huge audience cannot cause an unbounded allocation.
	query := `SELECT m.id, ` + senderRecipientAddress + `,
		CASE WHEN mr.recipient_type IN ('to','cc','bcc') THEN 1 ELSE 0 END FROM messages m
		JOIN message_recipients mr ON mr.message_id=m.id
		WHERE m.id IN (` + marks + `)
		AND NOT EXISTS (SELECT 1 FROM msmail_sent_envelope_recipients sent WHERE sent.message_id=m.id)
		UNION ALL SELECT sent.message_id, sent.email_address, 1
		FROM msmail_sent_envelope_recipients sent
		WHERE sent.message_id IN (` + marks + `)
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
