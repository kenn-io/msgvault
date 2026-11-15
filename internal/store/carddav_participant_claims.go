package store

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"go.kenn.io/msgvault/internal/vcard"
)

type cardDAVParticipantAddress struct {
	kind  ContactAddressKind
	value string
}
type cardDAVPersonSet map[int64]struct{}
type cardDAVUncertainKinds map[ContactAddressKind]bool

// Admission is deliberately stricter than phone normalization: the latter also
// accepts text containing digits, which is unsafe evidence for automatic ownership.
func cardDAVParticipantAddressValue(kind ContactAddressKind, raw string) (cardDAVParticipantAddress, bool) {
	if kind == ContactAddressPhone {
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(raw), "tel:") {
			if len(vcard.TelephoneParameters(raw)) != 0 {
				return cardDAVParticipantAddress{}, false
			}
			raw = vcard.TelephoneNumber(raw)
		}
		if !strings.HasPrefix(raw, "+") && !strings.HasPrefix(raw, "00") {
			return cardDAVParticipantAddress{}, false
		}
		for i, r := range raw {
			if r >= '0' && r <= '9' {
				continue
			}
			if unicode.IsSpace(r) || strings.ContainsRune("()-./", r) || r == '+' && i == 0 {
				continue
			}
			return cardDAVParticipantAddress{}, false
		}
	}
	value, err := NormalizeServiceValue(nil, kind, raw)
	return cardDAVParticipantAddress{kind, value}, err == nil
}

func (s *Store) cardDAVParticipantClaimsTx(ctx context.Context, tx *loggedTx) (map[cardDAVParticipantAddress]cardDAVPersonSet, map[cardDAVParticipantAddress]cardDAVPersonSet, map[cardDAVParticipantAddress]cardDAVPersonSet, map[int64]cardDAVUncertainKinds, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT resource.person_id, resource.remote_body,
 conflict.id IS NOT NULL,
 CASE WHEN conflict.local_tombstone THEN NULL ELSE conflict.local_body END,
 CASE WHEN conflict.remote_tombstone THEN NULL ELSE conflict.remote_body END
 FROM carddav_resources resource
 LEFT JOIN carddav_conflicts conflict ON conflict.address_book_id = resource.address_book_id
   AND conflict.href = resource.href AND conflict.status = 'unresolved'
 WHERE resource.mapping_status = ? AND resource.person_id IS NOT NULL
 AND EXISTS (SELECT 1 FROM carddav_address_books book
   WHERE book.id = resource.address_book_id AND book.is_subscribed = TRUE)
 AND NOT EXISTS (SELECT 1 FROM carddav_publications publication
   WHERE publication.person_id = resource.person_id
   AND publication.address_book_id = resource.address_book_id AND publication.href = resource.href
   AND publication.pending_operation = 'create') ORDER BY resource.id`, CardDAVMappingMapped)
	if err != nil {
		return nil, nil, nil, nil, 0, fmt.Errorf("read CardDAV participant claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	claims := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	conflicted := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	common := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	uncertain := map[int64]cardDAVUncertainKinds{}
	markUncertain := func(personID int64, kinds cardDAVUncertainKinds) {
		if uncertain[personID] == nil {
			uncertain[personID] = cardDAVUncertainKinds{}
		}
		for kind := range kinds {
			uncertain[personID][kind] = true
		}
	}
	scanned := 0
	for rows.Next() {
		var personID int64
		var body, localBody, remoteBody []byte
		var hasConflict bool
		if err := rows.Scan(&personID, &body, &hasConflict, &localBody, &remoteBody); err != nil {
			return nil, nil, nil, nil, 0, err
		}
		if hasConflict {
			local, _, localUncertain := cardDAVParticipantAddresses(localBody)
			remote, _, remoteUncertain := cardDAVParticipantAddresses(remoteBody)
			if len(localBody) > 0 {
				markUncertain(personID, localUncertain)
			}
			if len(remoteBody) > 0 {
				markUncertain(personID, remoteUncertain)
			}
			for _, addresses := range []map[cardDAVParticipantAddress]struct{}{local, remote} {
				for key := range addresses {
					if conflicted[key] == nil {
						conflicted[key] = cardDAVPersonSet{}
					}
					conflicted[key][personID] = struct{}{}
				}
			}
			// An address both snapshots keep can still support an existing automatic member.
			for key := range local {
				if _, ok := remote[key]; ok {
					if common[key] == nil {
						common[key] = cardDAVPersonSet{}
					}
					common[key][personID] = struct{}{}
				}
			}
			continue
		}
		addresses, ok, incomplete := cardDAVParticipantAddresses(body)
		markUncertain(personID, incomplete)
		if !ok {
			continue
		}
		for key := range addresses {
			if claims[key] == nil {
				claims[key] = cardDAVPersonSet{}
			}
			claims[key][personID] = struct{}{}
		}
		scanned++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, nil, 0, err
	}
	return claims, conflicted, common, uncertain, scanned, nil
}

func cardDAVParticipantAddresses(body []byte) (map[cardDAVParticipantAddress]struct{}, bool, cardDAVUncertainKinds) {
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return nil, false, cardDAVUncertainKinds{"": true}
	}
	addresses := map[cardDAVParticipantAddress]struct{}{}
	incomplete := cardDAVUncertainKinds{}
	for _, occurrence := range envelope.PropertyTree {
		property := occurrence.Property
		var kind ContactAddressKind
		switch strings.ToUpper(property.Name) {
		case "EMAIL":
			kind = ContactAddressEmail
		case "TEL":
			kind = ContactAddressPhone
		default:
			continue
		}
		property.RawValue, err = vcard.DecodeLegacyRawValue(property)
		if err != nil {
			incomplete[kind] = true
			continue
		}
		value, uri, err := vcard.ContactPointValue(envelope.RenderMetadata.StoredVersion, property)
		if err != nil {
			incomplete[kind] = true
			continue
		}
		if kind == ContactAddressPhone && uri != "" {
			value = uri
		}
		key, ok := cardDAVParticipantAddressValue(kind, value)
		if !ok {
			continue
		}
		addresses[key] = struct{}{}
	}
	return addresses, true, incomplete
}

func cardDAVParticipantMatchesTx(ctx context.Context, tx *loggedTx, claims, conflicted, common map[cardDAVParticipantAddress]cardDAVPersonSet) (map[int64]cardDAVPersonSet, map[int64]cardDAVPersonSet, map[int64]cardDAVPersonSet, map[int64][]int64, map[int64]cardDAVUncertainKinds, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, 'email', email_address FROM participants WHERE email_address IS NOT NULL
 UNION ALL SELECT id, 'phone', phone_number FROM participants WHERE phone_number IS NOT NULL
 UNION ALL SELECT participant_id, identifier_type, identifier_value FROM participant_identifiers
 WHERE scope_kind IS NULL AND scope_value IS NULL
 AND identifier_type IN ('email','phone','apple_id','whatsapp','imessage','synctech_sms','google_voice','imazing_csv')`)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("read participant addresses for CardDAV binding: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates, disputedCandidates, commonCandidates []cardDAVAddressMatch
	participantKinds := map[int64]cardDAVUncertainKinds{}
	for rows.Next() {
		var id int64
		var kind, value string
		if err := rows.Scan(&id, &kind, &value); err != nil {
			return nil, nil, nil, nil, nil, err
		}
		addressKind := ContactAddressPhone
		if kind == "email" || (kind == "apple_id" || kind == "imessage" || kind == "synctech_sms") && strings.Contains(value, "@") {
			addressKind = ContactAddressEmail
		}
		key, ok := cardDAVParticipantAddressValue(addressKind, value)
		if !ok {
			continue
		}
		if participantKinds[id] == nil {
			participantKinds[id] = cardDAVUncertainKinds{}
		}
		participantKinds[id][addressKind] = true
		if len(claims[key]) > 0 {
			candidates = append(candidates, cardDAVAddressMatch{id, key})
		}
		if len(conflicted[key]) > 0 {
			disputedCandidates = append(disputedCandidates, cardDAVAddressMatch{id, key})
		}
		if len(common[key]) > 0 {
			commonCandidates = append(commonCandidates, cardDAVAddressMatch{id, key})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, nil, nil, err
	}
	matched, shadowed := cardDAVMatchCandidates(claims, candidates)
	disputed, _ := cardDAVMatchCandidates(conflicted, disputedCandidates)
	retained, _ := cardDAVMatchCandidates(common, commonCandidates)
	return matched, disputed, retained, shadowed, participantKinds, nil
}

type cardDAVAddressMatch struct {
	id  int64
	key cardDAVParticipantAddress
}

// All holders contribute manual owners when checking for conflicts.
func cardDAVMatchCandidates(claims map[cardDAVParticipantAddress]cardDAVPersonSet, candidates []cardDAVAddressMatch) (map[int64]cardDAVPersonSet, map[int64][]int64) {
	matched, shadowed := map[int64]cardDAVPersonSet{}, map[int64][]int64{}
	holders := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	for _, candidate := range candidates {
		if holders[candidate.key] == nil {
			holders[candidate.key] = cardDAVPersonSet{}
		}
		holders[candidate.key][candidate.id] = struct{}{}
	}
	for _, candidate := range candidates {
		for holder := range holders[candidate.key] {
			if holder != candidate.id {
				shadowed[candidate.id] = append(shadowed[candidate.id], holder)
			}
		}
		if matched[candidate.id] == nil {
			matched[candidate.id] = cardDAVPersonSet{}
		}
		for personID := range claims[candidate.key] {
			matched[candidate.id][personID] = struct{}{}
		}
	}
	return matched, shadowed
}
