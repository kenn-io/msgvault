package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/personfacts"
)

var (
	ErrPersonCreateInvalid = errors.New("invalid person creation request")
	ErrPersonContactExists = errors.New("contact already belongs to an existing person or participant")
)

// MaxPersonCreateContacts bounds each of the email and phone lists.
const MaxPersonCreateContacts = 200

// Character limits for the free-text creation fields.
const (
	MaxPersonCreateNameLength    = 256
	MaxPersonCreateOrgLength     = 256
	MaxPersonCreateTitleLength   = 280
	MaxPersonCreateAddressLength = 1000
	MaxPersonCreateNoteLength    = 10000
)

// personCreateContactTypePattern admits the vCard parameter token characters
// the CardDAV renderer can publish as a TYPE value.
var personCreateContactTypePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// PersonCreateContact is a curated email or phone with an optional vCard type.
type PersonCreateContact struct {
	Value string `json:"value"`
	Type  string `json:"type,omitzero" pattern:"^[A-Za-z0-9-]{1,64}$"`
}

// PersonCreateInput creates a profile without inventing message participants.
type PersonCreateInput struct {
	Name    string                `json:"name" minLength:"1" maxLength:"256"`
	Emails  []PersonCreateContact `json:"emails,omitempty" maxItems:"200"`
	Phones  []PersonCreateContact `json:"phones,omitempty" maxItems:"200"`
	Org     string                `json:"org,omitzero" maxLength:"256"`
	Title   string                `json:"title,omitzero" maxLength:"280" doc:"Job title at org; requires org"`
	Address string                `json:"address,omitzero" maxLength:"1000"`
	Note    string                `json:"note,omitzero" maxLength:"10000"`
	Source  Provenance            `json:"source,omitzero" enum:"user,enrichment" doc:"Provenance of every created value; defaults to user"`
}

// PersonContactExistsError identifies a match and the explicit existing-person
// or promotion path. Creation never silently links identities.
type PersonContactExistsError struct {
	Kind          ContactAddressKind
	Value         string
	PersonID      int64
	ParticipantID int64
	Name          string
}

func (e *PersonContactExistsError) Error() string {
	name := ""
	if e.Name != "" {
		name = " (" + e.Name + ")"
	}
	if e.PersonID > 0 {
		return fmt.Sprintf("%s: %s %q matches person %d%s; use msgvault person get %d; "+
			"link observed participants explicitly through POST /api/v1/identity/links",
			ErrPersonContactExists, e.Kind, e.Value, e.PersonID, name, e.PersonID)
	}
	return fmt.Sprintf("%s: %s %q matches participant %d%s; use msgvault person promote %d",
		ErrPersonContactExists, e.Kind, e.Value, e.ParticipantID, name, e.ParticipantID)
}

func (e *PersonContactExistsError) Unwrap() error { return ErrPersonContactExists }

type personCreateContactPoint struct {
	kind       ContactAddressKind
	value      string
	normalized string
	typeToken  string
}

// ValidatePersonCreateInput checks creation fields before any archive mutation.
func ValidatePersonCreateInput(input PersonCreateInput) error {
	_, err := validatePersonCreateInput(input)
	return err
}

func validatePersonCreateInput(input PersonCreateInput) ([]personCreateContactPoint, error) {
	if strings.TrimSpace(input.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrPersonCreateInvalid)
	}
	for _, field := range []struct {
		name, value string
		limit       int
	}{
		{"name", input.Name, MaxPersonCreateNameLength},
		{"org", input.Org, MaxPersonCreateOrgLength},
		{"title", input.Title, MaxPersonCreateTitleLength},
		{"address", input.Address, MaxPersonCreateAddressLength},
		{"note", input.Note, MaxPersonCreateNoteLength},
	} {
		if utf8.RuneCountInString(strings.TrimSpace(field.value)) > field.limit {
			return nil, fmt.Errorf("%w: %s exceeds %d characters",
				ErrPersonCreateInvalid, field.name, field.limit)
		}
	}
	if strings.TrimSpace(input.Title) != "" && strings.TrimSpace(input.Org) == "" {
		return nil, fmt.Errorf("%w: title requires org", ErrPersonCreateInvalid)
	}
	switch input.Source {
	case "", ProvenanceUser, ProvenanceEnrichment:
	default:
		return nil, fmt.Errorf("%w: source must be user or enrichment", ErrPersonCreateInvalid)
	}
	points := make([]personCreateContactPoint, 0, len(input.Emails)+len(input.Phones))
	for _, group := range []struct {
		kind     ContactAddressKind
		contacts []PersonCreateContact
	}{{ContactAddressEmail, input.Emails}, {ContactAddressPhone, input.Phones}} {
		if len(group.contacts) > MaxPersonCreateContacts {
			return nil, fmt.Errorf("%w: at most %d %s contacts are allowed",
				ErrPersonCreateInvalid, MaxPersonCreateContacts, group.kind)
		}
		seen := make(map[string]bool, len(group.contacts))
		for _, contact := range group.contacts {
			point, err := newPersonCreateContactPoint(group.kind, contact)
			if err != nil {
				return nil, err
			}
			if seen[point.normalized] {
				return nil, fmt.Errorf("%w: repeated %s %q",
					ErrPersonCreateInvalid, group.kind, contact.Value)
			}
			seen[point.normalized] = true
			points = append(points, point)
		}
	}
	return points, nil
}

func newPersonCreateContactPoint(
	kind ContactAddressKind, contact PersonCreateContact,
) (personCreateContactPoint, error) {
	value := strings.TrimSpace(contact.Value)
	normalized, err := NormalizeServiceValue(nil, kind, value)
	if err != nil {
		return personCreateContactPoint{}, fmt.Errorf("%w: %s: %w", ErrPersonCreateInvalid, kind, err)
	}
	if kind == ContactAddressEmail {
		parsed, err := mail.ParseAddress(value)
		if err != nil || parsed.Address != value {
			return personCreateContactPoint{}, fmt.Errorf(
				"%w: email must be a bare email address", ErrPersonCreateInvalid)
		}
	}
	typeToken := strings.TrimSpace(contact.Type)
	if typeToken != "" && !personCreateContactTypePattern.MatchString(typeToken) {
		return personCreateContactPoint{}, fmt.Errorf(
			"%w: %s type %q must be 1-64 letters, digits, or hyphens",
			ErrPersonCreateInvalid, kind, contact.Type)
	}
	return personCreateContactPoint{
		kind: kind, value: value, normalized: normalized, typeToken: typeToken,
	}, nil
}

// CreateStandalonePersonContext commits the person and its curated values in
// one identity-serialized transaction, including duplicate checks.
func (s *Store) CreateStandalonePersonContext(
	ctx context.Context, input PersonCreateInput,
) (*Person, error) {
	points, err := validatePersonCreateInput(input)
	if err != nil {
		return nil, err
	}
	var person *Person
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		for _, point := range points {
			if err := s.refusePersonContactDuplicateTx(ctx, tx, point.kind, point.normalized); err != nil {
				return err
			}
		}
		uid, err := newVCardUID()
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?) RETURNING id`,
			uid, strings.TrimSpace(input.Name),
		).Scan(&id); err != nil {
			return fmt.Errorf("create standalone person: %w", err)
		}
		if err := s.addStandalonePersonValuesTx(ctx, tx, id, input, points); err != nil {
			return err
		}
		if err := s.bumpPersonDisplayNameRevisionContext(ctx, tx); err != nil {
			return err
		}
		if _, err := s.bumpIdentityRevisionContext(ctx, tx); err != nil {
			return err
		}
		person, err = s.getPersonTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return person, nil
}

// addStandalonePersonValuesTx stores the curated values under input.Source.
// An inferred profile advances its export review revision whatever fields it
// has, so CardDAV publishes the card only after exact-card approval. Its name
// is also kept as an inferred formatted-name row: the display_name column
// records no provenance, and the row keeps the review requirement with the
// name when a merge moves it or a split restores it.
func (s *Store) addStandalonePersonValuesTx(
	ctx context.Context, tx *loggedTx, personID int64,
	input PersonCreateInput, points []personCreateContactPoint,
) error {
	source := input.Source
	if source == "" {
		source = ProvenanceUser
	}
	if err := s.addStandalonePersonDetailsTx(ctx, tx, personID, input, source, points); err != nil {
		return err
	}
	if !provenanceIsInferred(source) {
		return nil
	}
	name := strings.TrimSpace(input.Name)
	if _, err := s.addPersonNameTx(ctx, tx, personID, PersonNameInput{
		NameKind: PersonNameFormatted, Formatted: &name, OriginalValue: name,
		Envelope: ValueEnvelopeInput{Source: source},
	}); err != nil {
		return err
	}
	return s.advancePersonInferenceExportRevisionTx(ctx, tx, personID)
}

func (s *Store) addStandalonePersonDetailsTx(
	ctx context.Context, tx *loggedTx, personID int64,
	input PersonCreateInput, source Provenance, points []personCreateContactPoint,
) error {
	for _, point := range points {
		envelope := ValueEnvelopeInput{Source: source}
		if point.typeToken != "" {
			envelope.TypeTokens = []string{point.typeToken}
		}
		if _, err := s.addPersonContactPointTx(ctx, tx, personID, PersonContactPointInput{
			AddressKind: point.kind, OriginalValue: point.value, Envelope: envelope,
		}); err != nil {
			return err
		}
	}
	if address := strings.TrimSpace(input.Address); address != "" {
		if _, err := s.addPersonAddressTx(ctx, tx, personID, PersonAddressInput{
			AddressKind: PersonAddressPostal, StreetAddress: &address, OriginalValue: address,
			Envelope: ValueEnvelopeInput{Source: source},
		}); err != nil {
			return err
		}
	}
	if org := strings.TrimSpace(input.Org); org != "" {
		// Person-fact generation locks the attribute catalog, then the
		// organizations table, then definitions. Taking them in the same
		// order, before the Notes definition below, keeps PostgreSQL from
		// deadlocking the two transactions.
		if err := s.lockAttributeDefinitionCatalogTx(ctx, tx, false); err != nil {
			return err
		}
		if err := s.createPersonEmploymentTx(ctx, tx, personID, org, input.Title, source); err != nil {
			return err
		}
	}
	if note := strings.TrimSpace(input.Note); note != "" {
		definition, err := s.getAttributeDefinitionByUniversalIDTx(ctx, tx, AttributeUniversalIDNotes)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := s.setPersonAttributeValueTx(ctx, tx, *definition, PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: definition.Slug,
			Value:  AttributeValue{Type: AttributeValueText, Text: &note},
			Source: source,
		}, now, now); err != nil {
			return err
		}
	}
	return nil
}

// createPersonEmploymentTx resolves org with the person-fact organization
// resolver, so creation reuses the same company rows and aliases that sweeps
// and employment facts match.
func (s *Store) createPersonEmploymentTx(
	ctx context.Context, tx *loggedTx, personID int64, org, title string, source Provenance,
) error {
	ref := personfacts.OrganizationReference{Name: org}
	prepared, err := s.preparePersonFactOrganizationTx(ctx, tx, ref)
	if err != nil {
		return fmt.Errorf("resolve person organization: %w", err)
	}
	organization, status, err := s.materializePersonFactOrganizationTx(ctx, tx, ref, prepared)
	if err != nil {
		return fmt.Errorf("resolve person organization: %w", err)
	}
	if status == OrganizationAmbiguous {
		return fmt.Errorf("%w: org %q matches several organizations; create the person "+
			"without org, then run msgvault employment add --person <id> --organization <id>",
			ErrPersonCreateInvalid, org)
	}
	input := EmploymentInput{
		PersonID: personID, OrganizationID: organization.ID, Title: trimmedOrNil(&title),
		IsCurrent: new(true), IsPrimary: new(true), Source: source,
	}
	if _, err := s.addEmploymentTx(ctx, tx, input); err != nil {
		return err
	}
	if !source.IsDeclared() {
		return nil
	}
	return s.appendManualPersonFactEmploymentPinTx(ctx, tx, personID, string(source))
}

func (s *Store) refusePersonContactDuplicateTx(
	ctx context.Context, tx *loggedTx, kind ContactAddressKind, value string,
) error {
	match := &PersonContactExistsError{Kind: kind, Value: value}
	err := tx.QueryRowContext(ctx, `
		SELECT c.person_id, COALESCE(p.display_name, '')
		FROM person_contact_points c JOIN persons p ON p.id = c.person_id
		WHERE c.address_kind = ? AND c.normalized_value = ?
		  AND c.active_until IS NULL AND c.superseded_at IS NULL
		ORDER BY c.person_id LIMIT 1`, kind, value).Scan(&match.PersonID, &match.Name)
	if err == nil {
		return match
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check curated contact duplicates: %w", err)
	}
	found, err := s.findObservedContactParticipantTx(ctx, tx, kind, value, match)
	if err != nil || !found {
		return err
	}
	edges, err := s.loadLinkEdgesTx(tx)
	if err != nil {
		return err
	}
	ids, err := personIDsForParticipantsTx(ctx, tx, sortedComponentMembers(match.ParticipantID, edges))
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		match.PersonID = ids[0]
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(display_name, '') FROM persons WHERE id = ?`, match.PersonID,
		).Scan(&match.Name); err != nil {
			return fmt.Errorf("load matched person %d: %w", match.PersonID, err)
		}
	}
	return match
}

// findObservedContactParticipantTx looks up the lowest participant observed
// with value: as its primary address, as an identifier of any type (merge
// aliases and service identifiers, matched like EmailParticipantContext and
// PhoneParticipantContext), or as a current contact observation. Each arm
// reads through an index.
func (s *Store) findObservedContactParticipantTx(
	ctx context.Context, tx *loggedTx, kind ContactAddressKind, value string,
	match *PersonContactExistsError,
) (bool, error) {
	participantLookup := `SELECT id FROM participants WHERE LOWER(email_address) = ?`
	if kind == ContactAddressPhone {
		participantLookup = `SELECT id FROM participants WHERE phone_number = ?`
	}
	err := tx.QueryRowContext(ctx, `
		SELECT p.id, COALESCE(p.display_name, '') FROM participants p
		WHERE p.id IN (`+participantLookup+`
			UNION
			SELECT i.participant_id FROM participant_identifiers i
			WHERE LOWER(i.identifier_value) = ?
			UNION
			SELECT o.participant_id FROM participant_contact_observations o
			WHERE o.address_kind = ? AND o.normalized_value = ?
			  AND o.active_until IS NULL AND o.superseded_at IS NULL)
		ORDER BY p.id LIMIT 1`, value, value, kind, value).Scan(&match.ParticipantID, &match.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check observed contact duplicates: %w", err)
	}
	return true, nil
}
