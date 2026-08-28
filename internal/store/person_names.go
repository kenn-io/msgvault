package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonNameKind string

const (
	PersonNameFormatted  PersonNameKind = "formatted"
	PersonNameStructured PersonNameKind = "structured"
	PersonNameNickname   PersonNameKind = "nickname"
	PersonNamePhonetic   PersonNameKind = "phonetic"
	PersonNameSort       PersonNameKind = "sort"
)

func (k PersonNameKind) Valid() bool {
	switch k {
	case PersonNameFormatted, PersonNameStructured, PersonNameNickname,
		PersonNamePhonetic, PersonNameSort:
		return true
	default:
		return false
	}
}

type PersonName struct {
	Envelope          ValueEnvelope  `json:"envelope"`
	PersonID          int64          `json:"person_id"`
	NameKind          PersonNameKind `json:"name_kind"`
	Formatted         *string        `json:"formatted,omitempty"`
	FamilyName        *string        `json:"family_name,omitempty"`
	GivenName         *string        `json:"given_name,omitempty"`
	AdditionalNames   *string        `json:"additional_names,omitempty"`
	HonorificPrefixes *string        `json:"honorific_prefixes,omitempty"`
	HonorificSuffixes *string        `json:"honorific_suffixes,omitempty"`
	SecondarySurname  *string        `json:"secondary_surname,omitempty"`
	Generation        *string        `json:"generation,omitempty"`
	Language          *string        `json:"language,omitempty"`
	Script            *string        `json:"script,omitempty"`
	PhoneticSystem    *string        `json:"phonetic_system,omitempty"`
	PhoneticScript    *string        `json:"phonetic_script,omitempty"`
	SortAs            *string        `json:"sort_as,omitempty"`
	IsDerived         bool           `json:"is_derived"`
	OriginalValue     string         `json:"original_value"`
}

type PersonNameInput struct {
	NameKind          PersonNameKind     `json:"name_kind"`
	Formatted         *string            `json:"formatted,omitempty"`
	FamilyName        *string            `json:"family_name,omitempty"`
	GivenName         *string            `json:"given_name,omitempty"`
	AdditionalNames   *string            `json:"additional_names,omitempty"`
	HonorificPrefixes *string            `json:"honorific_prefixes,omitempty"`
	HonorificSuffixes *string            `json:"honorific_suffixes,omitempty"`
	SecondarySurname  *string            `json:"secondary_surname,omitempty"`
	Generation        *string            `json:"generation,omitempty"`
	Language          *string            `json:"language,omitempty"`
	Script            *string            `json:"script,omitempty"`
	PhoneticSystem    *string            `json:"phonetic_system,omitempty"`
	PhoneticScript    *string            `json:"phonetic_script,omitempty"`
	SortAs            *string            `json:"sort_as,omitempty"`
	IsDerived         bool               `json:"is_derived,omitempty"`
	OriginalValue     string             `json:"original_value"`
	Envelope          ValueEnvelopeInput `json:"envelope"`
}

var (
	ErrInvalidPersonNameKind  = errors.New("invalid person name kind")
	ErrPersonNameValueMissing = errors.New("person name requires at least one non-empty component")
)

func (s *Store) AddPersonNameContext(ctx context.Context, personID int64, input PersonNameInput) (*PersonName, error) {
	var result *PersonName
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonNameTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, personID)
	})
	return result, err
}

func (s *Store) ListPersonNamesContext(ctx context.Context, personID int64, currentOnly bool) ([]PersonName, error) {
	var names []PersonName
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		names, err = s.listPersonNamesTx(ctx, tx, personID, currentOnly)
		return err
	})
	return names, err
}

func (s *Store) SupersedePersonNameContext(ctx context.Context, personID, nameID int64, activeUntil *time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonNameTx(ctx, tx, personID, nameID, activeUntil); err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return s.invalidatePersonEnrichmentIdentitiesAfterRevisionTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonNameTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonNameInput,
) (*PersonName, error) {
	if !input.NameKind.Valid() {
		return nil, ErrInvalidPersonNameKind
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		original = firstNonBlankNameComponent(input)
	}
	if original == "" {
		return nil, ErrPersonNameValueMissing
	}
	env, err := resolveProfileEnvelopeTx(
		ctx, tx, "person_names", "name_kind", personID, input.NameKind, input.Envelope,
	)
	if err != nil {
		return nil, err
	}
	args := []any{
		personID, input.NameKind, stringValue(input.Formatted),
		stringValue(input.FamilyName), stringValue(input.GivenName),
		stringValue(input.AdditionalNames), stringValue(input.HonorificPrefixes),
		stringValue(input.HonorificSuffixes), stringValue(input.SecondarySurname),
		stringValue(input.Generation), stringValue(input.Language),
		stringValue(input.Script), stringValue(input.PhoneticSystem),
		stringValue(input.PhoneticScript), stringValue(input.SortAs),
		input.IsDerived, original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_names (
		person_id, name_kind, formatted, family_name, given_name,
		additional_names, honorific_prefixes, honorific_suffixes,
		secondary_surname, generation, language, script, phonetic_system,
		phonetic_script, sort_as, is_derived, original_value, `+
		profileEnvelopeWriteColumns+`, created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person name: %w", err)
	}
	return getPersonNameTx(ctx, tx, personID, id)
}

func (s *Store) listPersonNamesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonName, error) {
	query := personNameSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY name_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonName, personID)
}

func (s *Store) supersedePersonNameTx(
	ctx context.Context, tx *loggedTx, personID, nameID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_names", personID, nameID, activeUntil,
	)
}

func firstNonBlankNameComponent(input PersonNameInput) string {
	for _, value := range []*string{
		input.Formatted, input.FamilyName, input.GivenName, input.AdditionalNames,
		input.HonorificPrefixes, input.HonorificSuffixes, input.SecondarySurname,
		input.Generation, input.SortAs,
	} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

const personNameSelect = `SELECT
	id, person_id, name_kind, formatted, family_name, given_name,
	additional_names, honorific_prefixes, honorific_suffixes,
	secondary_surname, generation, language, script, phonetic_system,
	phonetic_script, sort_as, is_derived, original_value,
	pref, ordinal, type_label, type_tokens, vcard_property, vcard_group,
	vcard_prop_id, vcard_pid, vcard_altid, source, source_ref, source_resource_uid, confidence,
	active_from, active_until, created_at, updated_at, superseded_at
	FROM person_names`

func getPersonNameTx(ctx context.Context, tx *loggedTx, personID, id int64) (*PersonName, error) {
	name, err := scanPersonName(tx.QueryRowContext(ctx,
		personNameSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person name: %w", err)
	}
	return name, nil
}

func scanPersonName(row scanner) (*PersonName, error) {
	var name PersonName
	var formatted, family, given, additional, prefixes, suffixes sql.NullString
	var secondary, generation, language, script, phoneticSystem sql.NullString
	var phoneticScript, sortAs sql.NullString
	var env profileEnvelopeScanValues
	dest := []any{
		&name.Envelope.ID, &name.PersonID, &name.NameKind,
		&formatted, &family, &given, &additional, &prefixes, &suffixes,
		&secondary, &generation, &language, &script, &phoneticSystem,
		&phoneticScript, &sortAs, &name.IsDerived, &name.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	name.Formatted = nullStringPtr(formatted)
	name.FamilyName = nullStringPtr(family)
	name.GivenName = nullStringPtr(given)
	name.AdditionalNames = nullStringPtr(additional)
	name.HonorificPrefixes = nullStringPtr(prefixes)
	name.HonorificSuffixes = nullStringPtr(suffixes)
	name.SecondarySurname = nullStringPtr(secondary)
	name.Generation = nullStringPtr(generation)
	name.Language = nullStringPtr(language)
	name.Script = nullStringPtr(script)
	name.PhoneticSystem = nullStringPtr(phoneticSystem)
	name.PhoneticScript = nullStringPtr(phoneticScript)
	name.SortAs = nullStringPtr(sortAs)
	if err := env.apply(&name.Envelope); err != nil {
		return nil, err
	}
	return &name, nil
}
