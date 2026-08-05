package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ErrAccountIdentityNotFound is returned when an identifier is not confirmed
// for the requested source.
var ErrAccountIdentityNotFound = errors.New("account identity not found")

// MessageIdentityMatch is the source-scoped intersection between one
// message's envelope participants and its source's confirmed identities.
type MessageIdentityMatch struct {
	MessageID  int64
	SourceID   int64
	Sender     []string
	Recipients []string
}

// ResolvedAccountIdentity is one confirmed source identity and every
// participant row that carries the same identifier.
type ResolvedAccountIdentity struct {
	SourceID   int64
	Identifier string
	// IdentifierIsEmail reports whether Identifier is email-shaped
	// (looksLikeEmail). Email identities can be compared against the
	// immutable envelope snapshot message_recipients.email_address; other
	// identifier types (phone, matrix, handles) have no envelope column and
	// match through participants only.
	IdentifierIsEmail bool
	ParticipantIDs    []int64
}

// ResolveAccountIdentityContext verifies that identifier is confirmed for
// sourceID, then resolves every participant row carrying that identifier.
func (s *Store) ResolveAccountIdentityContext(
	ctx context.Context,
	sourceID int64,
	identifier string,
) (ResolvedAccountIdentity, error) {
	identities, err := s.ListAccountIdentitiesContext(ctx, sourceID)
	if err != nil {
		return ResolvedAccountIdentity{}, err
	}

	var storedIdentifier string
	for _, identity := range identities {
		if EqualIdentifier(identity.Address, identifier) {
			storedIdentifier = identity.Address
			break
		}
	}
	if storedIdentifier == "" {
		return ResolvedAccountIdentity{}, fmt.Errorf(
			"source %d identity %q: %w", sourceID, identifier, ErrAccountIdentityNotFound)
	}

	// Mirrors messageIdentityAttributionMatch (messages.go) exactly: the
	// email column compares case-insensitively, participant_identifiers rows
	// compare per their own identifier_type (case-insensitive only when
	// type = 'email'), and participants.phone_number is never consulted —
	// EnsureParticipantByPhone always backs a phone identity with a
	// participant_identifiers row, so that row is the parity-correct match
	// surface. Do not reintroduce identifierMatch/EqualIdentifier here: their
	// global, shape-based case rule is what this query replaces.
	const query = `
		SELECT p.id FROM participants p
		WHERE p.email_address IS NOT NULL AND LOWER(p.email_address) = LOWER(?)
		UNION
		SELECT pi.participant_id FROM participant_identifiers pi
		WHERE (pi.identifier_type = 'email' AND LOWER(pi.identifier_value) = LOWER(?))
		   OR (pi.identifier_type <> 'email' AND pi.identifier_value = ?)
		ORDER BY 1
	`
	rows, err := s.db.QueryContext(
		ctx,
		query,
		storedIdentifier,
		storedIdentifier,
		storedIdentifier,
	)
	if err != nil {
		return ResolvedAccountIdentity{}, fmt.Errorf("resolve account identity participants: %w", err)
	}
	defer func() { _ = rows.Close() }()

	participantIDs := make([]int64, 0)
	for rows.Next() {
		var participantID int64
		if err := rows.Scan(&participantID); err != nil {
			return ResolvedAccountIdentity{}, fmt.Errorf("scan account identity participant: %w", err)
		}
		participantIDs = append(participantIDs, participantID)
	}
	if err := rows.Err(); err != nil {
		return ResolvedAccountIdentity{}, fmt.Errorf("iterate account identity participants: %w", err)
	}

	return ResolvedAccountIdentity{
		SourceID:          sourceID,
		Identifier:        storedIdentifier,
		IdentifierIsEmail: looksLikeEmail(storedIdentifier),
		ParticipantIDs:    participantIDs,
	}, nil
}

type messageIdentityCandidates struct {
	messageID  int64
	sourceID   int64
	sender     map[string]struct{}
	recipients map[string]struct{}
}

// MatchMessageIdentitiesContext resolves requested messages in bounded batch
// queries. It reads envelope metadata only; bodies, raw messages, and
// attachments are never involved.
func (s *Store) MatchMessageIdentitiesContext(
	ctx context.Context,
	messageIDs []int64,
) (map[int64]MessageIdentityMatch, error) {
	candidatesByMessage := make(map[int64]*messageIdentityCandidates, len(messageIDs))
	sourceSet := make(map[int64]struct{})

	err := queryInChunksContext(ctx, s.db, messageIDs, nil, `
		WITH requested_messages AS (
			SELECT m.id, m.source_id, m.sender_id
			FROM messages m
			WHERE m.id IN (%s)
		), candidate_links AS (
			SELECT rm.id AS message_id,
			       rm.source_id,
			       mr.recipient_type,
			       mr.participant_id,
			       mr.email_address AS envelope_address
			FROM requested_messages rm
			LEFT JOIN message_recipients mr
			  ON mr.message_id = rm.id
			 AND mr.recipient_type IN ('from', 'to', 'cc', 'bcc')
			UNION ALL
			SELECT rm.id AS message_id,
			       rm.source_id,
			       'from' AS recipient_type,
			       rm.sender_id AS participant_id,
			       NULL AS envelope_address
			FROM requested_messages rm
			WHERE rm.sender_id IS NOT NULL
			  AND NOT EXISTS (
				SELECT 1
				FROM message_recipients sender_row
				WHERE sender_row.message_id = rm.id
				  AND sender_row.recipient_type = 'from'
			  )
		)
		SELECT cl.message_id,
		       cl.source_id,
		       cl.recipient_type,
		       cl.envelope_address,
		       p.email_address,
		       p.phone_number,
		       pi.identifier_value
		FROM candidate_links cl
		LEFT JOIN participants p ON p.id = cl.participant_id
		LEFT JOIN participant_identifiers pi ON pi.participant_id = p.id
		ORDER BY cl.message_id, cl.recipient_type, cl.participant_id, pi.id
	`, func(rows *loggedRows) error {
		var (
			messageID       int64
			sourceID        int64
			recipientType   sql.NullString
			envelopeAddress sql.NullString
			emailAddress    sql.NullString
			phoneNumber     sql.NullString
			identifierValue sql.NullString
		)
		if err := rows.Scan(
			&messageID,
			&sourceID,
			&recipientType,
			&envelopeAddress,
			&emailAddress,
			&phoneNumber,
			&identifierValue,
		); err != nil {
			return fmt.Errorf("scan message identity candidate: %w", err)
		}

		candidates := candidatesByMessage[messageID]
		if candidates == nil {
			candidates = &messageIdentityCandidates{
				messageID:  messageID,
				sourceID:   sourceID,
				sender:     make(map[string]struct{}),
				recipients: make(map[string]struct{}),
			}
			candidatesByMessage[messageID] = candidates
			sourceSet[sourceID] = struct{}{}
		}

		var target map[string]struct{}
		switch recipientType.String {
		case "from":
			target = candidates.sender
		case "to", "cc", "bcc":
			target = candidates.recipients
		default:
			return nil
		}
		addEnvelopeIdentifiers(target, envelopeAddress, emailAddress, phoneNumber, identifierValue)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query message identity candidates: %w", err)
	}

	sourceIDs := make([]int64, 0, len(sourceSet))
	for sourceID := range sourceSet {
		sourceIDs = append(sourceIDs, sourceID)
	}
	slices.Sort(sourceIDs)

	identitiesBySource := make(map[int64]map[string][]string, len(sourceIDs))
	err = queryInChunksContext(ctx, s.db, sourceIDs, nil, `
		SELECT source_id, address
		FROM account_identities
		WHERE source_id IN (%s)
		ORDER BY source_id, address
	`, func(rows *loggedRows) error {
		var sourceID int64
		var address string
		if err := rows.Scan(&sourceID, &address); err != nil {
			return fmt.Errorf("scan message account identity: %w", err)
		}
		byIdentifier := identitiesBySource[sourceID]
		if byIdentifier == nil {
			byIdentifier = make(map[string][]string)
			identitiesBySource[sourceID] = byIdentifier
		}
		normalized := NormalizeIdentifierForCompare(address)
		byIdentifier[normalized] = append(byIdentifier[normalized], address)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query message account identities: %w", err)
	}

	matches := make(map[int64]MessageIdentityMatch, len(candidatesByMessage))
	for messageID, candidates := range candidatesByMessage {
		identities := identitiesBySource[candidates.sourceID]
		matches[messageID] = MessageIdentityMatch{
			MessageID:  candidates.messageID,
			SourceID:   candidates.sourceID,
			Sender:     intersectStoredIdentities(candidates.sender, identities),
			Recipients: intersectStoredIdentities(candidates.recipients, identities),
		}
	}
	return matches, nil
}

func addEnvelopeIdentifiers(
	target map[string]struct{},
	envelopeAddress sql.NullString,
	emailAddress sql.NullString,
	phoneNumber sql.NullString,
	identifierValue sql.NullString,
) {
	// The envelope snapshot (message_recipients.email_address) is the
	// message's own record of the address on this recipient row. It is
	// immutable under participant merges, so when present it is the only
	// candidate: falling through to the participant's current fields would
	// badge one alias's mail with every alias the merge survivor carries.
	// Rows without a snapshot (legacy ingests, non-email writers) keep the
	// participant-derived candidates.
	if addNormalizedIdentifier(target, envelopeAddress) {
		return
	}
	if addNormalizedIdentifier(target, emailAddress) {
		return
	}
	if addNormalizedIdentifier(target, phoneNumber) {
		return
	}
	addNormalizedIdentifier(target, identifierValue)
}

func addNormalizedIdentifier(target map[string]struct{}, value sql.NullString) bool {
	if !value.Valid {
		return false
	}
	identifier := strings.TrimSpace(value.String)
	if identifier == "" {
		return false
	}
	target[NormalizeIdentifierForCompare(identifier)] = struct{}{}
	return true
}

func intersectStoredIdentities(
	candidates map[string]struct{},
	identities map[string][]string,
) []string {
	matchedSet := make(map[string]struct{})
	for candidate := range candidates {
		for _, stored := range identities[candidate] {
			matchedSet[stored] = struct{}{}
		}
	}
	matched := make([]string, 0, len(matchedSet))
	for stored := range matchedSet {
		matched = append(matched, stored)
	}
	sort.Strings(matched)
	return matched
}
