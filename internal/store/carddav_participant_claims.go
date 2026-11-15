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
		for i, r := range raw {
			if r >= '0' && r <= '9' || unicode.IsSpace(r) || strings.ContainsRune("()-./", r) || r == '+' && i == 0 {
				continue
			}
			return cardDAVParticipantAddress{}, false
		}
	}
	value, err := NormalizeServiceValue(nil, kind, raw)
	return cardDAVParticipantAddress{kind, value}, err == nil
}

func (s *Store) cardDAVParticipantClaimsTx(ctx context.Context, tx *loggedTx) (map[cardDAVParticipantAddress]cardDAVPersonSet, int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT resource.person_id, resource.remote_body
 FROM carddav_resources resource
 WHERE resource.mapping_status = ? AND resource.person_id IS NOT NULL
 AND EXISTS (SELECT 1 FROM carddav_address_books book
   WHERE book.id = resource.address_book_id AND book.is_subscribed = TRUE)
 AND NOT EXISTS (SELECT 1 FROM carddav_conflicts conflict
   WHERE conflict.address_book_id = resource.address_book_id AND conflict.href = resource.href
   AND conflict.status = 'unresolved')
 AND NOT EXISTS (SELECT 1 FROM carddav_publications publication
   WHERE publication.person_id = resource.person_id
   AND publication.address_book_id = resource.address_book_id AND publication.href = resource.href
   AND publication.pending_operation = 'create') ORDER BY resource.id`, CardDAVMappingMapped)
	if err != nil {
		return nil, 0, fmt.Errorf("read CardDAV participant claims: %w", err)
	}
	defer func() { _ = rows.Close() }()
	claims := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	scanned := 0
	for rows.Next() {
		var personID int64
		var body []byte
		if err := rows.Scan(&personID, &body); err != nil {
			return nil, 0, err
		}
		envelope, err := vcard.ParseResourceEnvelope(body)
		if err != nil {
			continue
		}
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
				continue
			}
			value, uri, err := vcard.ContactPointValue(envelope.RenderMetadata.StoredVersion, property)
			if err != nil {
				continue
			}
			if kind == ContactAddressPhone && uri != "" {
				value = uri
			}
			key, ok := cardDAVParticipantAddressValue(kind, value)
			if !ok {
				continue
			}
			if claims[key] == nil {
				claims[key] = cardDAVPersonSet{}
			}
			claims[key][personID] = struct{}{}
		}
		scanned++
	}
	return claims, scanned, rows.Err()
}

func cardDAVParticipantMatchesTx(ctx context.Context, tx *loggedTx, claims map[cardDAVParticipantAddress]cardDAVPersonSet) (map[int64]cardDAVPersonSet, map[int64][]int64, error) {
	if len(claims) == 0 {
		return map[int64]cardDAVPersonSet{}, nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, 'email', email_address, TRUE FROM participants WHERE email_address IS NOT NULL
 UNION ALL SELECT id, 'phone', phone_number, FALSE FROM participants WHERE phone_number IS NOT NULL
 UNION ALL SELECT participant_id, identifier_type, identifier_value, FALSE FROM participant_identifiers
 WHERE scope_kind IS NULL AND scope_value IS NULL
 AND identifier_type IN ('email','phone','apple_id','whatsapp','imessage','synctech_sms','google_voice')`)
	if err != nil {
		return nil, nil, fmt.Errorf("read participant addresses for CardDAV binding: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []cardDAVAddressMatch
	primaryOwners := map[cardDAVParticipantAddress]cardDAVPersonSet{}
	for rows.Next() {
		var id int64
		var kind, value string
		var primary bool
		if err := rows.Scan(&id, &kind, &value, &primary); err != nil {
			return nil, nil, err
		}
		addressKind := ContactAddressPhone
		if kind == "email" || (kind == "apple_id" || kind == "imessage" || kind == "synctech_sms") && strings.Contains(value, "@") {
			addressKind = ContactAddressEmail
		}
		key, ok := cardDAVParticipantAddressValue(addressKind, value)
		if !ok || len(claims[key]) == 0 {
			continue
		}
		if primary {
			if primaryOwners[key] == nil {
				primaryOwners[key] = cardDAVPersonSet{}
			}
			primaryOwners[key][id] = struct{}{}
		}
		candidates = append(candidates, cardDAVAddressMatch{id, key, primary})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	matched, shadowed := cardDAVMatchCandidates(claims, candidates, primaryOwners)
	return matched, shadowed, nil
}

type cardDAVAddressMatch struct {
	id      int64
	key     cardDAVParticipantAddress
	primary bool
}

// A primary email outranks another participant's alias for the same address:
// the alias holder is not matched, but is returned in shadowed under each
// primary owner so its manual person binding still counts as a competing owner.
func cardDAVMatchCandidates(claims map[cardDAVParticipantAddress]cardDAVPersonSet, candidates []cardDAVAddressMatch, primaryOwners map[cardDAVParticipantAddress]cardDAVPersonSet) (map[int64]cardDAVPersonSet, map[int64][]int64) {
	matched, shadowed := map[int64]cardDAVPersonSet{}, map[int64][]int64{}
	for _, candidate := range candidates {
		if !candidate.primary && candidate.key.kind == ContactAddressEmail {
			owners := primaryOwners[candidate.key]
			if _, ownsPrimary := owners[candidate.id]; len(owners) > 0 && !ownsPrimary {
				for owner := range owners {
					shadowed[owner] = append(shadowed[owner], candidate.id)
				}
				continue
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
