package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/vcard"
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
	Formatted         *string        `json:"formatted,omitzero" nullable:"false"`
	FamilyName        *string        `json:"family_name,omitzero" nullable:"false"`
	GivenName         *string        `json:"given_name,omitzero" nullable:"false"`
	AdditionalNames   *string        `json:"additional_names,omitzero" nullable:"false"`
	HonorificPrefixes *string        `json:"honorific_prefixes,omitzero" nullable:"false"`
	HonorificSuffixes *string        `json:"honorific_suffixes,omitzero" nullable:"false"`
	SecondarySurname  *string        `json:"secondary_surname,omitzero" nullable:"false"`
	Generation        *string        `json:"generation,omitzero" nullable:"false"`
	Language          *string        `json:"language,omitzero" nullable:"false"`
	Script            *string        `json:"script,omitzero" nullable:"false"`
	PhoneticSystem    *string        `json:"phonetic_system,omitzero" nullable:"false"`
	PhoneticScript    *string        `json:"phonetic_script,omitzero" nullable:"false"`
	SortAs            *string        `json:"sort_as,omitzero" nullable:"false"`
	IsDerived         bool           `json:"is_derived"`
	OriginalValue     string         `json:"original_value"`
}

// PersonNameVCardProjection contains the name values and parameters rendered
// from a stored name. CardDAV inference review uses the same projection.
type PersonNameVCardProjection struct {
	Property       string   `json:"property"`
	Value          string   `json:"value"`
	Language       *string  `json:"language"`
	Script         *string  `json:"script"`
	PhoneticSystem *string  `json:"phonetic_system"`
	SortAs         []string `json:"sort_as"`
}

// VCardValue returns the property name and escaped value, or empty strings when
// the name cannot render. Projection and inference review use the same rules.
func (n PersonName) VCardValue() (propertyName, rawValue string) {
	switch n.NameKind {
	case PersonNameFormatted:
		if n.Formatted != nil && strings.TrimSpace(*n.Formatted) != "" {
			return "FN", vcard.EscapeText(*n.Formatted)
		}
	case PersonNameStructured, PersonNamePhonetic:
		components := []string{
			derefString(n.FamilyName), derefString(n.GivenName), derefString(n.AdditionalNames),
			derefString(n.HonorificPrefixes), derefString(n.HonorificSuffixes),
			derefString(n.SecondarySurname), derefString(n.Generation),
		}
		for _, component := range components {
			if strings.TrimSpace(component) != "" {
				return "N", vcard.JoinStructuredText(components)
			}
		}
	case PersonNameNickname:
		value := n.OriginalValue
		if n.Formatted != nil && strings.TrimSpace(*n.Formatted) != "" {
			value = *n.Formatted
		}
		if strings.TrimSpace(value) != "" {
			// Each row is one nickname, including any literal commas.
			return "NICKNAME", vcard.EscapeText(value)
		}
	case PersonNameSort:
		return "", ""
	}
	return "", ""
}

func (n PersonName) VCardProjection() (PersonNameVCardProjection, bool) {
	property, value := n.VCardValue()
	if property == "" {
		return PersonNameVCardProjection{}, false
	}
	script := n.Script
	if n.NameKind == PersonNamePhonetic && vcardProjectionParameterValue(n.PhoneticScript) != nil {
		script = n.PhoneticScript
	}
	var sortAs []string
	if value := vcardProjectionParameterValue(n.SortAs); value != nil {
		sortAs = strings.Split(*value, ",")
		for index := range sortAs {
			sortAs[index] = strings.TrimSpace(sortAs[index])
		}
	}
	return PersonNameVCardProjection{
		Property: property, Value: value,
		Language:       vcardProjectionParameterValue(n.Language),
		Script:         vcardProjectionParameterValue(script),
		PhoneticSystem: vcardProjectionParameterValue(n.PhoneticSystem),
		SortAs:         sortAs,
	}, true
}

type PersonNameInput struct {
	NameKind          PersonNameKind     `json:"name_kind"`
	Formatted         *string            `json:"formatted,omitzero" nullable:"false"`
	FamilyName        *string            `json:"family_name,omitzero" nullable:"false"`
	GivenName         *string            `json:"given_name,omitzero" nullable:"false"`
	AdditionalNames   *string            `json:"additional_names,omitzero" nullable:"false"`
	HonorificPrefixes *string            `json:"honorific_prefixes,omitzero" nullable:"false"`
	HonorificSuffixes *string            `json:"honorific_suffixes,omitzero" nullable:"false"`
	SecondarySurname  *string            `json:"secondary_surname,omitzero" nullable:"false"`
	Generation        *string            `json:"generation,omitzero" nullable:"false"`
	Language          *string            `json:"language,omitzero" nullable:"false"`
	Script            *string            `json:"script,omitzero" nullable:"false"`
	PhoneticSystem    *string            `json:"phonetic_system,omitzero" nullable:"false"`
	PhoneticScript    *string            `json:"phonetic_script,omitzero" nullable:"false"`
	SortAs            *string            `json:"sort_as,omitzero" nullable:"false"`
	IsDerived         bool               `json:"is_derived,omitzero"`
	OriginalValue     string             `json:"original_value"`
	Envelope          ValueEnvelopeInput `json:"envelope"`
}

var (
	ErrInvalidPersonNameKind  = errors.New("invalid person name kind")
	ErrInvalidPersonNameText  = errors.New("person name contains invalid vCard text")
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
		if err := s.reviewInferenceExportChangesTx(ctx, tx, personID, func() error {
			var err error
			result, err = s.addPersonNameTx(ctx, tx, personID, input)
			return err
		}); err != nil {
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
	return retryContendedWriteErr(ctx, s, "supersede person name", func() error {
		return s.supersedePersonNameOnce(ctx, personID, nameID, activeUntil)
	})
}

func (s *Store) supersedePersonNameOnce(
	ctx context.Context, personID, nameID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.reviewInferenceExportChangesTx(ctx, tx, personID, func() error {
			return s.supersedePersonNameTx(ctx, tx, personID, nameID, activeUntil)
		}); err != nil {
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
	if err := validatePersonNameInput(input); err != nil {
		return nil, err
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

func validatePersonNameText(field, value string) error {
	if strings.ContainsAny(value, "\r\x00") {
		return fmt.Errorf("%w: %s contains CR or NUL", ErrInvalidPersonNameText, field)
	}
	return nil
}

func validatePersonNameInput(input PersonNameInput) error {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{name: "formatted", value: input.Formatted},
		{name: "family_name", value: input.FamilyName},
		{name: "given_name", value: input.GivenName},
		{name: "additional_names", value: input.AdditionalNames},
		{name: "honorific_prefixes", value: input.HonorificPrefixes},
		{name: "honorific_suffixes", value: input.HonorificSuffixes},
		{name: "secondary_surname", value: input.SecondarySurname},
		{name: "generation", value: input.Generation},
	} {
		if field.value != nil {
			if err := validatePersonNameText(field.name, *field.value); err != nil {
				return err
			}
		}
	}
	if err := validatePersonNameText("original_value", input.OriginalValue); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{name: "language", value: input.Language},
		{name: "script", value: input.Script},
		{name: "phonetic_system", value: input.PhoneticSystem},
		{name: "phonetic_script", value: input.PhoneticScript},
		{name: "sort_as", value: input.SortAs},
	} {
		if field.value != nil && strings.ContainsAny(*field.value, "\r\n\x00") {
			return fmt.Errorf("%w: %s contains CR, LF, or NUL", ErrInvalidPersonNameText, field.name)
		}
	}
	return nil
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
	name, err := getPersonNameTx(ctx, tx, personID, nameID)
	if err != nil {
		return err
	}
	if err := s.supersedeProfileValueTx(
		ctx, tx, "person_names", personID, nameID, activeUntil,
	); err != nil {
		return err
	}
	if name.NameKind != PersonNameFormatted || !provenanceIsInferred(name.Envelope.Source) || name.Formatted == nil {
		return nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE persons SET display_name = NULL, display_name_source_name_id = NULL
		WHERE id = ? AND display_name = ? AND display_name_source_name_id = ?`,
		personID, strings.TrimSpace(*name.Formatted), name.Envelope.ID,
	)
	if err != nil {
		return fmt.Errorf("clear retired inferred display name for person %d: %w", personID, err)
	}
	cleared, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count cleared inferred display name for person %d: %w", personID, err)
	}
	if cleared == 0 {
		return nil
	}
	if err := s.bumpPersonDisplayNameRevisionContext(ctx, tx); err != nil {
		return err
	}
	return s.bumpDisplayNameCounterpartVCardProjectionsTx(ctx, tx, personID)
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
