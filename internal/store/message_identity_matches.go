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
	// email column compares case-insensitively and only when non-blank,
	// participant_identifiers rows compare per their own identifier_type
	// (case-insensitive only when type = 'email', and email identifiers are
	// consulted only when the participant carries no primary email — the
	// attribution fallback's primary-email guard), and
	// participants.phone_number is never consulted —
	// EnsureParticipantByPhone always backs a phone identity with a
	// participant_identifiers row, so that row is the parity-correct match
	// surface. Do not reintroduce identifierMatch/EqualIdentifier here: their
	// global, shape-based case rule is what this query replaces.
	const query = `
		SELECT p.id FROM participants p
		WHERE p.email_address IS NOT NULL
		  AND TRIM(p.email_address) <> ''
		  AND LOWER(p.email_address) = LOWER(?)
		UNION
		SELECT pi.participant_id FROM participant_identifiers pi
		WHERE (pi.identifier_type = 'email'
		       AND NOT EXISTS (
		         SELECT 1 FROM participants pp
		         WHERE pp.id = pi.participant_id
		           AND pp.email_address IS NOT NULL
		           AND TRIM(pp.email_address) <> ''
		       )
		       AND LOWER(pi.identifier_value) = LOWER(?))
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
	messageID int64
	sourceID  int64
	// sender holds envelope-derived candidates; senderFallback holds
	// participant-derived candidates from From rows without an envelope
	// snapshot. Attribution's fallback is message-scoped — ANY non-empty
	// From envelope suppresses participant matching for the sender — so
	// senderFallback only applies when senderHasEnvelope is false.
	sender            *identityCandidateSet
	senderFallback    *identityCandidateSet
	senderHasEnvelope bool
	recipients        *identityCandidateSet
}

// identityCandidateSet keeps one key set per comparison rule so the
// intersection can mirror messageIdentityAttributionMatch: envelope and
// participant-email candidates compare by identifier shape (case-insensitive
// only when email-shaped, preserving synthetic-address exactness),
// email-typed identifier rows compare case-insensitively, and every other
// identifier type compares verbatim.
type identityCandidateSet struct {
	shape map[string]struct{}
	lower map[string]struct{}
	raw   map[string]struct{}
}

func newIdentityCandidateSet() *identityCandidateSet {
	return &identityCandidateSet{
		shape: make(map[string]struct{}),
		lower: make(map[string]struct{}),
		raw:   make(map[string]struct{}),
	}
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
		       pi.identifier_type,
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
			identifierType  sql.NullString
			identifierValue sql.NullString
		)
		if err := rows.Scan(
			&messageID,
			&sourceID,
			&recipientType,
			&envelopeAddress,
			&emailAddress,
			&identifierType,
			&identifierValue,
		); err != nil {
			return fmt.Errorf("scan message identity candidate: %w", err)
		}

		candidates := candidatesByMessage[messageID]
		if candidates == nil {
			candidates = &messageIdentityCandidates{
				messageID:      messageID,
				sourceID:       sourceID,
				sender:         newIdentityCandidateSet(),
				senderFallback: newIdentityCandidateSet(),
				recipients:     newIdentityCandidateSet(),
			}
			candidatesByMessage[messageID] = candidates
			sourceSet[sourceID] = struct{}{}
		}

		var target *identityCandidateSet
		switch recipientType.String {
		case "from":
			if envelopeAddress.Valid && strings.TrimSpace(envelopeAddress.String) != "" {
				candidates.senderHasEnvelope = true
				target = candidates.sender
			} else {
				target = candidates.senderFallback
			}
		case "to", "cc", "bcc":
			target = candidates.recipients
		default:
			return nil
		}
		target.addRow(envelopeAddress, emailAddress, identifierType, identifierValue)
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

	identitiesBySource := make(map[int64]*storedIdentityIndex, len(sourceIDs))
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
		index := identitiesBySource[sourceID]
		if index == nil {
			index = newStoredIdentityIndex()
			identitiesBySource[sourceID] = index
		}
		index.add(address)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query message account identities: %w", err)
	}

	matches := make(map[int64]MessageIdentityMatch, len(candidatesByMessage))
	for messageID, candidates := range candidatesByMessage {
		identities := identitiesBySource[candidates.sourceID]
		sender := candidates.sender
		if !candidates.senderHasEnvelope {
			sender = candidates.senderFallback
		}
		matches[messageID] = MessageIdentityMatch{
			MessageID:  candidates.messageID,
			SourceID:   candidates.sourceID,
			Sender:     intersectStoredIdentities(sender, identities),
			Recipients: intersectStoredIdentities(candidates.recipients, identities),
		}
	}
	return matches, nil
}

// addRow records one candidate-link row's identifier under the comparison
// rule messageIdentityAttributionMatch applies to that column.
//
// The envelope snapshot (message_recipients.email_address) is the
// message's own record of the address on this recipient row. It is
// immutable under participant merges, so when present it is the only
// candidate: falling through to the participant's current fields would
// badge one alias's mail with every alias the merge survivor carries.
// Rows without a snapshot (legacy ingests, non-email writers) keep the
// participant-derived candidates: the participant's email plus its non-email
// identifier rows compared per their own identifier_type. Email-typed
// identifiers are considered case-insensitively only when no primary email is
// present, so alternate email aliases remain suppressed while preserved phone
// and chat identifiers survive participant merges, exactly as attribution
// does. participants.phone_number is deliberately NOT a candidate:
// attribution never consults it (EnsureParticipantByPhone always backs a
// phone identity with a participant_identifiers row), so badging through
// it would keep matching a phone identity after its identifier mapping
// was removed.
func (set *identityCandidateSet) addRow(
	envelopeAddress sql.NullString,
	emailAddress sql.NullString,
	identifierType sql.NullString,
	identifierValue sql.NullString,
) {
	if addCandidateKey(set.shape, envelopeAddress, NormalizeIdentifierForCompare) {
		return
	}
	hasPrimaryEmail := addCandidateKey(set.shape, emailAddress, NormalizeIdentifierForCompare)
	if identifierType.Valid && identifierType.String == string(AttributeFieldEmail) {
		if !hasPrimaryEmail {
			addCandidateKey(set.lower, identifierValue, strings.ToLower)
		}
		return
	}
	addCandidateKey(set.raw, identifierValue, func(value string) string { return value })
}

func addCandidateKey(
	target map[string]struct{},
	value sql.NullString,
	normalize func(string) string,
) bool {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return false
	}
	target[normalize(strings.TrimSpace(value.String))] = struct{}{}
	return true
}

// storedIdentityIndex holds one source's confirmed identities keyed per
// comparison rule, so each candidate key set intersects under the semantics
// its column carries in messageIdentityAttributionMatch.
type storedIdentityIndex struct {
	byShape map[string][]string // NormalizeIdentifierForCompare(address)
	byLower map[string][]string // LOWER(address), for email-typed identifier rows
	byRaw   map[string][]string // address verbatim, for non-email identifier rows
}

func newStoredIdentityIndex() *storedIdentityIndex {
	return &storedIdentityIndex{
		byShape: make(map[string][]string),
		byLower: make(map[string][]string),
		byRaw:   make(map[string][]string),
	}
}

func (index *storedIdentityIndex) add(address string) {
	shapeKey := NormalizeIdentifierForCompare(address)
	index.byShape[shapeKey] = append(index.byShape[shapeKey], address)
	lowerKey := strings.ToLower(address)
	index.byLower[lowerKey] = append(index.byLower[lowerKey], address)
	index.byRaw[address] = append(index.byRaw[address], address)
}

func intersectStoredIdentities(
	candidates *identityCandidateSet,
	identities *storedIdentityIndex,
) []string {
	matchedSet := make(map[string]struct{})
	if identities != nil {
		collect := func(keys map[string]struct{}, byKey map[string][]string) {
			for candidate := range keys {
				for _, stored := range byKey[candidate] {
					matchedSet[stored] = struct{}{}
				}
			}
		}
		collect(candidates.shape, identities.byShape)
		collect(candidates.lower, identities.byLower)
		collect(candidates.raw, identities.byRaw)
	}
	matched := make([]string, 0, len(matchedSet))
	for stored := range matchedSet {
		matched = append(matched, stored)
	}
	sort.Strings(matched)
	return matched
}
