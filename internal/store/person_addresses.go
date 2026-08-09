package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type PersonAddressKind string

const (
	PersonAddressPostal     PersonAddressKind = "postal"
	PersonAddressBirthPlace PersonAddressKind = "birth_place"
	PersonAddressDeathPlace PersonAddressKind = "death_place"
)

func (k PersonAddressKind) Valid() bool {
	switch k {
	case PersonAddressPostal, PersonAddressBirthPlace, PersonAddressDeathPlace:
		return true
	default:
		return false
	}
}

type PersonAddress struct {
	Envelope           ValueEnvelope     `json:"envelope"`
	PersonID           int64             `json:"person_id"`
	AddressKind        PersonAddressKind `json:"address_kind"`
	PostOfficeBox      *string           `json:"post_office_box,omitempty"`
	ExtendedAddress    *string           `json:"extended_address,omitempty"`
	StreetAddress      *string           `json:"street_address,omitempty"`
	Locality           *string           `json:"locality,omitempty"`
	Region             *string           `json:"region,omitempty"`
	PostalCode         *string           `json:"postal_code,omitempty"`
	CountryName        *string           `json:"country_name,omitempty"`
	ExtendedComponents *string           `json:"extended_components,omitempty"`
	FreeText           *string           `json:"free_text,omitempty"`
	Label              *string           `json:"label,omitempty"`
	GeoURI             *string           `json:"geo_uri,omitempty"`
	Timezone           *string           `json:"timezone,omitempty"`
	CountryCode        *string           `json:"country_code,omitempty"`
	PlaceURI           *string           `json:"place_uri,omitempty"`
	OriginalValue      string            `json:"original_value"`
}

type PersonAddressInput struct {
	AddressKind        PersonAddressKind  `json:"address_kind"`
	PostOfficeBox      *string            `json:"post_office_box,omitempty"`
	ExtendedAddress    *string            `json:"extended_address,omitempty"`
	StreetAddress      *string            `json:"street_address,omitempty"`
	Locality           *string            `json:"locality,omitempty"`
	Region             *string            `json:"region,omitempty"`
	PostalCode         *string            `json:"postal_code,omitempty"`
	CountryName        *string            `json:"country_name,omitempty"`
	ExtendedComponents *string            `json:"extended_components,omitempty"`
	FreeText           *string            `json:"free_text,omitempty"`
	Label              *string            `json:"label,omitempty"`
	GeoURI             *string            `json:"geo_uri,omitempty"`
	Timezone           *string            `json:"timezone,omitempty"`
	CountryCode        *string            `json:"country_code,omitempty"`
	PlaceURI           *string            `json:"place_uri,omitempty"`
	OriginalValue      string             `json:"original_value"`
	Envelope           ValueEnvelopeInput `json:"envelope"`
}

var (
	ErrInvalidPersonAddressKind  = errors.New("invalid person address kind")
	ErrPersonAddressValueMissing = errors.New("person address requires at least one component")
)

func (s *Store) AddPersonAddressContext(
	ctx context.Context, personID int64, input PersonAddressInput,
) (*PersonAddress, error) {
	var result *PersonAddress
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := ensureProfilePersonTx(ctx, tx, personID); err != nil {
			return err
		}
		var err error
		result, err = s.addPersonAddressTx(ctx, tx, personID, input)
		if err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, personID); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (s *Store) ListPersonAddressesContext(
	ctx context.Context, personID int64, currentOnly bool,
) ([]PersonAddress, error) {
	var addresses []PersonAddress
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var err error
		addresses, err = s.listPersonAddressesTx(ctx, tx, personID, currentOnly)
		return err
	})
	return addresses, err
}

func (s *Store) SupersedePersonAddressContext(
	ctx context.Context, personID, addressID int64, activeUntil *time.Time,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		if err := s.supersedePersonAddressTx(
			ctx, tx, personID, addressID, activeUntil,
		); err != nil {
			return err
		}
		return s.bumpPersonRevisionsTx(ctx, tx, personID)
	})
}

func (s *Store) addPersonAddressTx(
	ctx context.Context, tx *loggedTx, personID int64, input PersonAddressInput,
) (*PersonAddress, error) {
	if !input.AddressKind.Valid() {
		return nil, ErrInvalidPersonAddressKind
	}
	if !personAddressHasValue(input) {
		return nil, ErrPersonAddressValueMissing
	}
	original := strings.TrimSpace(input.OriginalValue)
	if original == "" {
		original = personAddressOriginalValue(input)
	}
	env, err := resolveProfileEnvelopeTx(
		ctx, tx, "person_addresses", "address_kind",
		personID, input.AddressKind, input.Envelope,
	)
	if err != nil {
		return nil, err
	}
	args := []any{
		personID, input.AddressKind, stringValue(input.PostOfficeBox),
		stringValue(input.ExtendedAddress), stringValue(input.StreetAddress),
		stringValue(input.Locality), stringValue(input.Region),
		stringValue(input.PostalCode), stringValue(input.CountryName),
		stringValue(input.ExtendedComponents), stringValue(input.FreeText),
		stringValue(input.Label), stringValue(input.GeoURI),
		stringValue(input.Timezone), stringValue(input.CountryCode),
		stringValue(input.PlaceURI), original,
	}
	args = append(args, profileEnvelopeArgs(env)...)
	var id int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO person_addresses (
		person_id, address_kind, post_office_box, extended_address,
		street_address, locality, region, postal_code, country_name,
		extended_components, free_text, label, geo_uri, timezone,
		country_code, place_uri, original_value, `+profileEnvelopeWriteColumns+`,
		created_at, updated_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		`+s.dialect.Now()+`, `+s.dialect.Now()+`
	) RETURNING id`, args...).Scan(&id); err != nil {
		return nil, fmt.Errorf("add person address: %w", err)
	}
	return getPersonAddressTx(ctx, tx, personID, id)
}

func personAddressOriginalValue(input PersonAddressInput) string {
	postal := []*string{
		input.PostOfficeBox, input.ExtendedAddress, input.StreetAddress,
		input.Locality, input.Region, input.PostalCode, input.CountryName,
	}
	hasPostal := false
	components := make([]string, len(postal))
	for index, value := range postal {
		components[index] = derefString(value)
		hasPostal = hasPostal || strings.TrimSpace(components[index]) != ""
	}
	if hasPostal {
		return strings.Join(components, ";")
	}
	for _, value := range []*string{
		input.FreeText, input.GeoURI, input.PlaceURI, input.ExtendedComponents,
	} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return strings.TrimSpace(*value)
		}
	}
	return ""
}

func (s *Store) listPersonAddressesTx(
	ctx context.Context, tx *loggedTx, personID int64, currentOnly bool,
) ([]PersonAddress, error) {
	query := personAddressSelect + ` WHERE person_id = ?`
	if currentOnly {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	query += ` ORDER BY address_kind,
		CASE WHEN pref IS NULL THEN 1 ELSE 0 END, pref, ordinal, id`
	return queryProfileRowsTx(ctx, tx, query, scanPersonAddress, personID)
}

func (s *Store) supersedePersonAddressTx(
	ctx context.Context, tx *loggedTx, personID, addressID int64, activeUntil *time.Time,
) error {
	return s.supersedeProfileValueTx(
		ctx, tx, "person_addresses", personID, addressID, activeUntil,
	)
}

func personAddressHasValue(input PersonAddressInput) bool {
	if strings.TrimSpace(input.OriginalValue) != "" {
		return true
	}
	for _, value := range []*string{
		input.PostOfficeBox, input.ExtendedAddress, input.StreetAddress,
		input.Locality, input.Region, input.PostalCode, input.CountryName,
		input.ExtendedComponents, input.FreeText, input.PlaceURI, input.GeoURI,
	} {
		if value != nil && strings.TrimSpace(*value) != "" {
			return true
		}
	}
	return false
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

const personAddressSelect = `SELECT
	id, person_id, address_kind, post_office_box, extended_address,
	street_address, locality, region, postal_code, country_name,
	extended_components, free_text, label, geo_uri, timezone, country_code,
	place_uri, original_value, ` + profileEnvelopeReadColumns + `
	FROM person_addresses`

func getPersonAddressTx(
	ctx context.Context, tx *loggedTx, personID, id int64,
) (*PersonAddress, error) {
	address, err := scanPersonAddress(tx.QueryRowContext(ctx,
		personAddressSelect+` WHERE person_id = ? AND id = ?`, personID, id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProfileValueNotFound
	}
	return address, err
}

func scanPersonAddress(row scanner) (*PersonAddress, error) {
	var address PersonAddress
	var postOfficeBox, extendedAddress, streetAddress, locality sql.NullString
	var region, postalCode, countryName, extendedComponents sql.NullString
	var freeText, label, geoURI, timezone, countryCode, placeURI sql.NullString
	var env profileEnvelopeScanValues
	dest := []any{
		&address.Envelope.ID, &address.PersonID, &address.AddressKind,
		&postOfficeBox, &extendedAddress, &streetAddress, &locality, &region,
		&postalCode, &countryName, &extendedComponents, &freeText, &label,
		&geoURI, &timezone, &countryCode, &placeURI, &address.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	address.PostOfficeBox = nullStringPtr(postOfficeBox)
	address.ExtendedAddress = nullStringPtr(extendedAddress)
	address.StreetAddress = nullStringPtr(streetAddress)
	address.Locality = nullStringPtr(locality)
	address.Region = nullStringPtr(region)
	address.PostalCode = nullStringPtr(postalCode)
	address.CountryName = nullStringPtr(countryName)
	address.ExtendedComponents = nullStringPtr(extendedComponents)
	address.FreeText = nullStringPtr(freeText)
	address.Label = nullStringPtr(label)
	address.GeoURI = nullStringPtr(geoURI)
	address.Timezone = nullStringPtr(timezone)
	address.CountryCode = nullStringPtr(countryCode)
	address.PlaceURI = nullStringPtr(placeURI)
	if err := env.apply(&address.Envelope); err != nil {
		return nil, err
	}
	return &address, nil
}
