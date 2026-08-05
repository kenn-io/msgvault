package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// IdentityObservation is one address occurrence in message envelope metadata.
// Sent evidence is repeated on every participant row so callers can classify
// From observations without loading message content or labels separately.
type IdentityObservation struct {
	MessageID     int64
	Identifier    string
	RecipientType string
	// IsFromMe carries provider-native attribution only. Identity-derived
	// attribution is excluded: it is computed from confirmed identities, so
	// feeding it back would let a confirmation act as evidence for itself.
	IsFromMe      bool
	HasSentFolder bool
	HasSentLabel  bool
	ObservedAt    time.Time
}

// IdentityDiscoveryPage is one keyset-bounded page of source messages and
// their envelope observations.
type IdentityDiscoveryPage struct {
	Observations []IdentityObservation
	NextAfterID  int64
	Scanned      int64
}

// IdentityConfirmation is one source-scoped identifier and the evidence
// signals to merge into account_identities.
type IdentityConfirmation struct {
	Identifier string
	Signals    []string
}

// IdentityConfirmationOutcome reports the deterministic input spelling and
// signals for one normalized confirmation, plus whether this call created a
// new source-to-identifier ownership row.
type IdentityConfirmationOutcome struct {
	Identifier string   `json:"identifier"`
	Added      bool     `json:"added"`
	Signals    []string `json:"signals"`
}

type identityDiscoveryMessage struct {
	id         int64
	isFromMe   bool
	observedAt time.Time
}

// CountIdentityDiscoveryMessagesContext counts source messages rather than
// participant or label join rows. It is used only as a progress denominator;
// pages remain the authoritative scan unit.
func (s *Store) CountIdentityDiscoveryMessagesContext(ctx context.Context, sourceID int64) (int64, error) {
	var count int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT m.id) FROM messages m WHERE m.source_id = ?`, sourceID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count identity discovery messages: %w", err)
	}
	return count, nil
}

// ScanIdentityDiscoveryPageContext reads one messages.id keyset page and then
// loads participant/label metadata for only those message IDs.
func (s *Store) ScanIdentityDiscoveryPageContext(
	ctx context.Context,
	sourceID, afterID int64,
	limit int,
) (IdentityDiscoveryPage, error) {
	if limit <= 0 {
		return IdentityDiscoveryPage{}, fmt.Errorf("identity discovery page limit must be positive: %d", limit)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id,
		       COALESCE(m.source_is_from_me, FALSE),
		       COALESCE(m.sent_at, m.received_at, m.internal_date, m.archived_at)
		FROM messages m
		WHERE m.source_id = ? AND m.id > ?
		ORDER BY m.id
		LIMIT ?
	`, sourceID, afterID, limit)
	if err != nil {
		return IdentityDiscoveryPage{}, fmt.Errorf("query identity discovery page: %w", err)
	}

	messages, err := scanIdentityDiscoveryMessages(rows)
	if err != nil {
		return IdentityDiscoveryPage{}, err
	}
	page := IdentityDiscoveryPage{
		NextAfterID: afterID,
		Scanned:     int64(len(messages)),
	}
	if len(messages) == 0 {
		return page, nil
	}
	page.NextAfterID = messages[len(messages)-1].id
	page.Observations, err = s.scanIdentityObservationsContext(ctx, sourceID, messages)
	if err != nil {
		return IdentityDiscoveryPage{}, err
	}
	return page, nil
}

// ScanIdentityObservationsForSourceMessageIDsContext loads the same metadata
// observations for a bounded ingestion batch. Requested IDs are deduplicated
// and chunked before querying so retries and oversized provider pages do not
// duplicate observations or exceed SQLite's bind limit.
func (s *Store) ScanIdentityObservationsForSourceMessageIDsContext(
	ctx context.Context,
	sourceID int64,
	sourceMessageIDs []string,
) ([]IdentityObservation, error) {
	if len(sourceMessageIDs) == 0 {
		return []IdentityObservation{}, nil
	}

	unique := make(map[string]struct{}, len(sourceMessageIDs))
	for _, sourceMessageID := range sourceMessageIDs {
		unique[sourceMessageID] = struct{}{}
	}
	deduped := make([]string, 0, len(unique))
	for sourceMessageID := range unique {
		deduped = append(deduped, sourceMessageID)
	}
	sort.Strings(deduped)

	messageByID := make(map[int64]identityDiscoveryMessage)
	err := queryInChunksContext(ctx, s.db, deduped, []any{sourceID}, `
		SELECT m.id,
		       COALESCE(m.source_is_from_me, FALSE),
		       COALESCE(m.sent_at, m.received_at, m.internal_date, m.archived_at)
		FROM messages m
		WHERE m.source_id = ? AND m.source_message_id IN (%s)
		ORDER BY m.id
	`, func(rows *loggedRows) error {
		message, scanErr := scanIdentityDiscoveryMessage(rows)
		if scanErr != nil {
			return scanErr
		}
		messageByID[message.id] = message
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query identity discovery source message IDs: %w", err)
	}

	messages := make([]identityDiscoveryMessage, 0, len(messageByID))
	for _, message := range messageByID {
		messages = append(messages, message)
	}
	sort.Slice(messages, func(i, j int) bool { return messages[i].id < messages[j].id })
	return s.scanIdentityObservationsContext(ctx, sourceID, messages)
}

func scanIdentityDiscoveryMessages(rows *loggedRows) ([]identityDiscoveryMessage, error) {
	defer func() { _ = rows.Close() }()
	var messages []identityDiscoveryMessage
	for rows.Next() {
		message, err := scanIdentityDiscoveryMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate identity discovery messages: %w", err)
	}
	return messages, nil
}

func scanIdentityDiscoveryMessage(row scanner) (identityDiscoveryMessage, error) {
	var message identityDiscoveryMessage
	var observedAt nullableTimestamp
	if err := row.Scan(&message.id, &message.isFromMe, &observedAt); err != nil {
		return identityDiscoveryMessage{}, fmt.Errorf("scan identity discovery message: %w", err)
	}
	if observedAt.Valid {
		message.observedAt = observedAt.Time.UTC()
	}
	return message, nil
}

func (s *Store) scanIdentityObservationsContext(
	ctx context.Context,
	sourceID int64,
	messages []identityDiscoveryMessage,
) ([]IdentityObservation, error) {
	if len(messages) == 0 {
		return []IdentityObservation{}, nil
	}

	messageByID := make(map[int64]identityDiscoveryMessage, len(messages))
	messageIDs := make([]int64, len(messages))
	for i, message := range messages {
		messageByID[message.id] = message
		messageIDs[i] = message.id
	}

	// The identifier is the recipient row's envelope address: it is written
	// at ingest and never changes, so a later participant merge cannot turn
	// one address's provider-native evidence into evidence for the merge
	// survivor's primary email. Legacy rows ingested before the column
	// existed fall back to the participant's current email, which remains
	// mutable under merges; routine re-sync skips already-archived messages,
	// so those rows keep the fallback until the message is re-ingested (a
	// future repair command could backfill from the stored raw MIME).
	observations := make([]IdentityObservation, 0)
	err := queryInChunksContext(ctx, s.db, messageIDs, []any{sourceID}, `
		SELECT m.id,
		       COALESCE(NULLIF(mr.email_address, ''), p.email_address),
		       mr.recipient_type,
		       EXISTS (
		           SELECT 1
		           FROM message_labels ml
		           JOIN labels l ON l.id = ml.label_id
		           WHERE ml.message_id = m.id
		             AND l.source_id = m.source_id
		             AND l.system_role = 'sent'
		       ) AS has_sent_folder,
		       EXISTS (
		           SELECT 1
		           FROM message_labels ml
		           JOIN labels l ON l.id = ml.label_id
		           WHERE ml.message_id = m.id
		             AND l.source_id = m.source_id
		             AND src.source_type = 'gmail'
		             AND l.source_label_id = 'SENT'
		       ) AS has_sent_label
		FROM messages m
		JOIN sources src ON src.id = m.source_id
		JOIN message_recipients mr ON mr.message_id = m.id
		JOIN participants p ON p.id = mr.participant_id
		WHERE m.source_id = ?
		  AND m.id IN (%s)
		  AND mr.recipient_type IN ('from', 'to', 'cc', 'bcc')
		  AND COALESCE(NULLIF(mr.email_address, ''), p.email_address) IS NOT NULL
		ORDER BY m.id, mr.id
	`, func(rows *loggedRows) error {
		var observation IdentityObservation
		if err := rows.Scan(
			&observation.MessageID,
			&observation.Identifier,
			&observation.RecipientType,
			&observation.HasSentFolder,
			&observation.HasSentLabel,
		); err != nil {
			return fmt.Errorf("scan identity observation: %w", err)
		}
		message := messageByID[observation.MessageID]
		observation.IsFromMe = message.isFromMe
		observation.ObservedAt = message.observedAt
		observations = append(observations, observation)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query identity observations: %w", err)
	}
	return observations, nil
}

type normalizedIdentityConfirmation struct {
	identifier string
	normalized string
	signals    []string
}

const identityConfirmationChunkSize = 500

// AddAccountIdentitiesBatchContext deterministically deduplicates candidates,
// then confirms them in short writer-locked transactions. Each chunk bumps
// identity revisions once when it creates at least one new ownership row;
// signal-only merges preserve both revisions and confirmed_at.
func (s *Store) AddAccountIdentitiesBatchContext(
	ctx context.Context,
	sourceID int64,
	candidates []IdentityConfirmation,
) ([]IdentityConfirmationOutcome, error) {
	normalized, err := normalizeIdentityConfirmations(candidates)
	if err != nil {
		return nil, err
	}
	outcomes := make([]IdentityConfirmationOutcome, 0, len(normalized))
	for start := 0; start < len(normalized); start += identityConfirmationChunkSize {
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}
		end := min(start+identityConfirmationChunkSize, len(normalized))
		chunkOutcomes, err := s.addAccountIdentityConfirmationChunk(ctx, sourceID, normalized[start:end])
		if err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, chunkOutcomes...)
	}
	return outcomes, nil
}

func normalizeIdentityConfirmations(candidates []IdentityConfirmation) ([]normalizedIdentityConfirmation, error) {
	type accumulator struct {
		identifier string
		signals    map[string]struct{}
	}
	byNormalized := make(map[string]*accumulator, len(candidates))
	for _, candidate := range candidates {
		identifier := strings.TrimSpace(candidate.Identifier)
		if identifier == "" {
			continue
		}
		normalized := NormalizeIdentifierForCompare(identifier)
		entry, ok := byNormalized[normalized]
		if !ok {
			entry = &accumulator{identifier: identifier, signals: make(map[string]struct{})}
			byNormalized[normalized] = entry
		}
		for _, signal := range candidate.Signals {
			if strings.Contains(signal, ",") {
				return nil, fmt.Errorf("signal names cannot contain commas: %q", signal)
			}
			if signal != "" {
				entry.signals[signal] = struct{}{}
			}
		}
	}

	normalized := make([]normalizedIdentityConfirmation, 0, len(byNormalized))
	for key, entry := range byNormalized {
		signals := make([]string, 0, len(entry.signals))
		for signal := range entry.signals {
			signals = append(signals, signal)
		}
		sort.Strings(signals)
		normalized = append(normalized, normalizedIdentityConfirmation{
			identifier: entry.identifier,
			normalized: key,
			signals:    signals,
		})
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].normalized == normalized[j].normalized {
			return normalized[i].identifier < normalized[j].identifier
		}
		return normalized[i].normalized < normalized[j].normalized
	})
	return normalized, nil
}

func (s *Store) addAccountIdentityConfirmationChunk(
	ctx context.Context,
	sourceID int64,
	confirmations []normalizedIdentityConfirmation,
) ([]IdentityConfirmationOutcome, error) {
	const maxAttempts = 5
	for range maxAttempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		outcomes, err := s.addAccountIdentityConfirmationChunkOnce(ctx, sourceID, confirmations)
		if err == nil {
			return outcomes, nil
		}
		if !s.dialect.IsConflictError(err) && !s.dialect.IsBusyError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("add account identities batch: gave up after %d retries", maxAttempts)
}

func (s *Store) addAccountIdentityConfirmationChunkOnce(
	ctx context.Context,
	sourceID int64,
	confirmations []normalizedIdentityConfirmation,
) ([]IdentityConfirmationOutcome, error) {
	outcomes := make([]IdentityConfirmationOutcome, 0, len(confirmations))
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}

		inserted := false
		for _, confirmation := range confirmations {
			added, err := s.mergeAccountIdentitySignalsTx(
				ctx,
				tx,
				sourceID,
				confirmation.identifier,
				confirmation.signals,
				newIdentifierMatch(confirmation.identifier),
			)
			if err != nil {
				return err
			}
			inserted = inserted || added
			outcomes = append(outcomes, IdentityConfirmationOutcome{
				Identifier: confirmation.identifier,
				Added:      added,
				Signals:    confirmation.signals,
			})
		}
		if !inserted {
			return nil
		}
		if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		if err := s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		participantIDs, err := participantIDsForConfirmationsContext(ctx, tx, confirmations)
		if err != nil {
			return err
		}
		if len(participantIDs) == 0 {
			return nil
		}
		return refreshParticipantMessageAttributionContext(ctx, tx, participantIDs...)
	})
	if err != nil {
		return nil, err
	}
	return outcomes, nil
}

// participantIDsForConfirmationsContext resolves the participants matched by
// a confirmation chunk's identifiers, using the same case-sensitivity rules
// as messageIdentityAttributionMatch: participants.email_address and
// email-typed participant_identifiers match case-insensitively, and every
// other identifier type matches the raw stored value. Scoping the
// attribution refresh that follows a chunk's inserts to just these
// participants replaces a full-source UPDATE with one bounded to the
// senders the chunk could actually affect.
func participantIDsForConfirmationsContext(
	ctx context.Context,
	tx *loggedTx,
	confirmations []normalizedIdentityConfirmation,
) ([]int64, error) {
	addresses := make([]string, 0, len(confirmations))
	loweredAddresses := make([]string, 0, len(confirmations))
	seenLowered := make(map[string]struct{}, len(confirmations))
	for _, confirmation := range confirmations {
		addresses = append(addresses, confirmation.identifier)
		lowered := strings.ToLower(confirmation.identifier)
		if _, ok := seenLowered[lowered]; !ok {
			seenLowered[lowered] = struct{}{}
			loweredAddresses = append(loweredAddresses, lowered)
		}
	}

	participantIDs := make(map[int64]struct{})
	scanParticipantID := func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan confirmation participant id: %w", err)
		}
		participantIDs[id] = struct{}{}
		return nil
	}

	if err := queryInChunksContext(ctx, tx, loweredAddresses, nil, `
		SELECT p.id FROM participants p
		WHERE p.email_address IS NOT NULL AND LOWER(p.email_address) IN (%s)
	`, scanParticipantID); err != nil {
		return nil, fmt.Errorf("resolve confirmation participants by email: %w", err)
	}
	if err := queryInChunksContext(ctx, tx, loweredAddresses, nil, `
		SELECT pi.participant_id FROM participant_identifiers pi
		WHERE pi.identifier_type = 'email' AND LOWER(pi.identifier_value) IN (%s)
	`, scanParticipantID); err != nil {
		return nil, fmt.Errorf("resolve confirmation participants by email identifier: %w", err)
	}
	if err := queryInChunksContext(ctx, tx, addresses, nil, `
		SELECT pi.participant_id FROM participant_identifiers pi
		WHERE pi.identifier_type <> 'email' AND pi.identifier_value IN (%s)
	`, scanParticipantID); err != nil {
		return nil, fmt.Errorf("resolve confirmation participants by non-email identifier: %w", err)
	}

	ids := make([]int64, 0, len(participantIDs))
	for id := range participantIDs {
		ids = append(ids, id)
	}
	return ids, nil
}
