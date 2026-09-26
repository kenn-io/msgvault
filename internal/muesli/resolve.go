package muesli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/meetingarchive"
)

const (
	resolutionResolved = "resolved"
	resolutionCarried  = "carried_forward"
)

// participantRef hashes Muesli's participant identifier into a stable,
// non-reversible key for carrying identities forward between syncs.
func participantRef(identifier string) string {
	if identifier == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("muesli-participant:" + identifier))
	return hex.EncodeToString(sum[:])[:16]
}

// archivePerson turns a participant and its Contacts identities into an
// archive identity set. Muesli's own email stays primary.
func (p Participant) archivePerson() meetingarchive.Person {
	var emails []string
	for _, email := range append([]string{p.Email}, p.ContactEmails...) {
		if email != "" && !slices.Contains(emails, email) {
			emails = append(emails, email)
		}
	}
	person := meetingarchive.Person{Name: p.Name, Anchor: p.Anchor}
	phones := slices.Clone(p.ContactPhones)
	if len(emails) > 0 {
		person.Email = emails[0]
		person.OtherEmails = emails[1:]
	} else if len(phones) > 0 {
		person.Phone = phones[0]
		phones = phones[1:]
	}
	person.OtherPhones = phones
	return person.Normalized()
}

func (p Participant) raw(person meetingarchive.Person) rawParticipant {
	raw := rawParticipant{
		Ref: participantRef(p.Identifier), Name: p.Name, Email: p.Email, Source: p.Source,
	}
	if p.Resolution == "" {
		return raw
	}
	if person.Email == "" {
		raw.Phone = person.Phone
	}
	raw.Emails = slices.Clone(p.ContactEmails)
	raw.Phones = slices.Clone(p.ContactPhones)
	return raw
}

// resolveParticipants fills each participant's Contacts identities. When the
// Contacts read is not complete, a participant that cannot be resolved keeps
// the identities archived for it earlier, so a temporary outage never drops
// attendees or their person activity.
func (imp *Importer) resolveParticipants(
	sourceID int64, meeting *Meeting, contacts *Contacts, countryCode string,
) error {
	state := contacts.State()
	meeting.ContactsState = state
	if state == ContactsOff {
		return nil
	}
	var previous map[string]rawParticipant
	loaded := false
	for i := range meeting.Participants {
		participant := &meeting.Participants[i]
		// Only contact: identifiers are Contacts IDs; email: and calendar:
		// participants resolve by email alone.
		contactID, isContact := strings.CutPrefix(participant.Identifier, "contact:")
		if !isContact {
			contactID = ""
		}
		if card, ok := contacts.Resolve(contactID, participant.Email); ok {
			participant.ContactEmails = card.Emails
			participant.ContactPhones, participant.SkippedPhones = normalizedPhones(card.Phones, countryCode)
			participant.Anchor = meetingarchive.Anchor("apple-contact", card.GroupKey)
			participant.Resolution = resolutionResolved
			continue
		}
		if state == ContactsComplete {
			continue
		}
		if !loaded {
			loaded = true
			// An unkeyable meeting is reported when its snapshot is built.
			if key, keyErr := meeting.SourceMessageID(); keyErr == nil {
				var err error
				previous, err = imp.previousParticipants(sourceID, key)
				if err != nil {
					return err
				}
			}
		}
		earlier, ok := previous[participantRef(participant.Identifier)]
		if !ok || (len(earlier.Emails) == 0 && len(earlier.Phones) == 0) {
			continue
		}
		participant.ContactEmails = earlier.Emails
		participant.ContactPhones = earlier.Phones
		participant.Resolution = resolutionCarried
	}
	return nil
}

// normalizedPhones converts Contacts phones to E.164 and reports how many
// could not be converted, such as national numbers without a configured
// phone_country_code.
func normalizedPhones(raw []string, countryCode string) ([]string, int) {
	var phones []string
	skipped := 0
	for _, value := range raw {
		phone, ok := NormalizeContactPhone(value, countryCode)
		if !ok {
			skipped++
			continue
		}
		if !slices.Contains(phones, phone) {
			phones = append(phones, phone)
		}
	}
	slices.Sort(phones)
	return phones, skipped
}

// previousParticipants reads the meeting's archived participants by ref. A
// meeting that is not archived yet, or whose evidence cannot be read, has none.
func (imp *Importer) previousParticipants(sourceID int64, key string) (map[string]rawParticipant, error) {
	out := map[string]rawParticipant{}
	existing, err := imp.store.MessageExistsBatch(sourceID, []string{key})
	if err != nil {
		return nil, fmt.Errorf("look up archived Muesli meeting: %w", err)
	}
	messageID, ok := existing[key]
	if !ok {
		return out, nil
	}
	raw, err := imp.store.GetMessageRaw(messageID)
	if err != nil {
		return nil, fmt.Errorf("read archived Muesli meeting: %w", err)
	}
	// Evidence this version cannot read has nothing to carry forward.
	var evidence rawEvidence
	if decodeErr := json.Unmarshal(raw, &evidence); decodeErr == nil {
		for _, participant := range evidence.Participants {
			if participant.Ref != "" {
				out[participant.Ref] = participant
			}
		}
	}
	return out, nil
}
