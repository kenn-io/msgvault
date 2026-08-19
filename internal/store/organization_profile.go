package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	OrganizationNameKindAlias        = "alias"
	OrganizationNameKindLegal        = "legal"
	OrganizationNameKindFormer       = "former"
	OrganizationNameKindAbbreviation = "abbreviation"
	OrganizationNameKindSort         = "sort"

	OrganizationIdentifierKindDomain   = "domain"
	OrganizationIdentifierKindLinkedIn = "linkedin"
	OrganizationIdentifierKindDUNS     = "duns"
	OrganizationIdentifierKindTaxID    = "tax_id"
	OrganizationIdentifierKindRegistry = "registry"
	OrganizationIdentifierKindOther    = "other"
)

type OrganizationName struct {
	Envelope       ValueEnvelope `json:"envelope"`
	OrganizationID int64         `json:"organization_id"`
	Name           string        `json:"name"`
	NameNormalized string        `json:"name_normalized"`
	NameKind       string        `json:"name_kind"`
}

type OrganizationNameInput struct {
	Name     string             `json:"name"`
	NameKind string             `json:"name_kind"`
	Envelope ValueEnvelopeInput `json:"envelope"`
}

type OrganizationIdentifier struct {
	Envelope        ValueEnvelope `json:"envelope"`
	OrganizationID  int64         `json:"organization_id"`
	IdentifierKind  string        `json:"identifier_kind"`
	Value           string        `json:"identifier_value"`
	NormalizedValue string        `json:"normalized_value"`
}

type OrganizationIdentifierInput struct {
	IdentifierKind string             `json:"identifier_kind"`
	Value          string             `json:"identifier_value"`
	Envelope       ValueEnvelopeInput `json:"envelope"`
}

type OrganizationAddress struct {
	Envelope           ValueEnvelope     `json:"envelope"`
	OrganizationID     int64             `json:"organization_id"`
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

type OrganizationAddressInput = PersonAddressInput

type OrganizationContactPoint struct {
	Envelope             ValueEnvelope      `json:"envelope"`
	OrganizationID       int64              `json:"organization_id"`
	AddressKind          ContactAddressKind `json:"address_kind"`
	ServiceSlug          *string            `json:"service_slug,omitempty"`
	ServiceID            *int64             `json:"-"`
	ScopeKind            *string            `json:"scope_kind,omitempty"`
	ScopeValue           *string            `json:"scope_value,omitempty"`
	OriginalValue        string             `json:"original_value"`
	NormalizedValue      string             `json:"normalized_value"`
	Normalization        string             `json:"normalization"`
	NormalizationVersion int                `json:"normalization_version"`
	URI                  *string            `json:"uri,omitempty"`
}

type OrganizationContactPointInput = PersonContactPointInput

type OrganizationMedia struct {
	Envelope       ValueEnvelope   `json:"envelope"`
	OrganizationID int64           `json:"organization_id"`
	MediaKind      PersonMediaKind `json:"media_kind"`
	MediaType      *string         `json:"media_type,omitempty"`
	URI            *string         `json:"uri,omitempty"`
	ByteSize       *int64          `json:"byte_size,omitempty"`
	ContentHash    *string         `json:"content_hash,omitempty"`
	HasData        bool            `json:"has_data"`
	OriginalValue  string          `json:"original_value"`
}

// OrganizationMediaInput mirrors PersonMediaInput plus ContentHash, a
// retention identifier for full-replace profile writes: reads expose media
// metadata without the inline bytes, so a GET-derived PUT re-sends the row's
// content_hash instead of data and the stored bytes are kept. Omitting both
// data and content_hash makes the row URI-only.
type OrganizationMediaInput struct {
	MediaKind     PersonMediaKind    `json:"media_kind"`
	MediaType     *string            `json:"media_type,omitempty"`
	URI           *string            `json:"uri,omitempty"`
	Data          []byte             `json:"data,omitempty"`
	ContentHash   *string            `json:"content_hash,omitempty"`
	OriginalValue string             `json:"original_value"`
	Envelope      ValueEnvelopeInput `json:"envelope"`
}

type OrganizationCategory struct {
	Envelope           ValueEnvelope `json:"envelope"`
	OrganizationID     int64         `json:"organization_id"`
	Category           string        `json:"category"`
	CategoryNormalized string        `json:"category_normalized"`
}

type OrganizationCategoryInput struct {
	Category string             `json:"category"`
	Envelope ValueEnvelopeInput `json:"envelope"`
}

type OrganizationProfile struct {
	Organization  Organization               `json:"organization"`
	Names         []OrganizationName         `json:"names"`
	Identifiers   []OrganizationIdentifier   `json:"identifiers"`
	Addresses     []OrganizationAddress      `json:"addresses"`
	ContactPoints []OrganizationContactPoint `json:"contact_points"`
	Media         []OrganizationMedia        `json:"media"`
	Categories    []OrganizationCategory     `json:"categories"`
}

type OrganizationProfileInput struct {
	Names         []OrganizationNameInput
	Identifiers   []OrganizationIdentifierInput
	Addresses     []OrganizationAddressInput
	ContactPoints []OrganizationContactPointInput
	Media         []OrganizationMediaInput
	Categories    []OrganizationCategoryInput
}

type preparedOrganizationContact struct {
	input                OrganizationContactPointInput
	serviceID            any
	serviceSlug          *string
	normalized           string
	normalization        string
	normalizationVersion int
}

type preparedOrganizationProfile struct {
	input          OrganizationProfileInput
	nameKeys       []string
	identifierKeys []string
	addressKeys    []string
	contacts       []preparedOrganizationContact
	contactKeys    []string
	mediaKeys      []string
	categoryKeys   []string
}

func (s *Store) GetOrganizationProfileContext(
	ctx context.Context, id int64, includeSuperseded bool,
) (*OrganizationProfile, error) {
	var profile *OrganizationProfile
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		organization, err := getOrganizationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		profile, err = s.loadOrganizationProfileContext(
			ctx, tx, organization, includeSuperseded)
		return err
	})
	if err != nil {
		return nil, err
	}
	return profile, nil
}

func (s *Store) ReplaceOrganizationProfileContext(
	ctx context.Context, id, expectedRevision int64, input OrganizationProfileInput,
) (*OrganizationProfile, error) {
	prepared, err := s.prepareOrganizationProfileContext(ctx, input)
	if err != nil {
		return nil, err
	}
	// Same lock order as ReplaceOrganizationContext (organization, then its
	// employed persons), so the same employment-write deadlock applies and the
	// same retry resolves it.
	return retryContendedWrite(ctx, s, "replace organization profile",
		func() (*OrganizationProfile, error) {
			return s.replaceOrganizationProfileOnce(ctx, id, expectedRevision, prepared)
		})
}

func (s *Store) replaceOrganizationProfileOnce(
	ctx context.Context, id, expectedRevision int64,
	prepared *preparedOrganizationProfile,
) (*OrganizationProfile, error) {
	now := time.Now().UTC()
	var profile *OrganizationProfile
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE organizations
			SET revision = revision + 1, updated_at = %s
			WHERE id = ? AND revision = ? AND merged_into_id IS NULL
		`, s.dialect.Now()), id, expectedRevision)
		if err != nil {
			return fmt.Errorf("bump organization profile revision: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check organization profile revision: %w", err)
		}
		if changed == 0 {
			return organizationMutableCASMissTx(ctx, tx, id, expectedRevision)
		}
		if err := s.bumpEmployedPersonVCardProjectionsTx(ctx, tx, id); err != nil {
			return err
		}
		organization, err := getOrganizationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		current, err := s.loadOrganizationProfileContext(ctx, tx, organization, false)
		if err != nil {
			return err
		}
		if err := s.reconcileOrganizationProfileTx(ctx, tx, id, current, prepared, now); err != nil {
			return err
		}
		profile, err = s.loadOrganizationProfileContext(ctx, tx, organization, false)
		return err
	})
	return profile, err
}

func (s *Store) prepareOrganizationProfileContext(
	ctx context.Context, input OrganizationProfileInput,
) (*preparedOrganizationProfile, error) {
	prepared := &preparedOrganizationProfile{input: input}
	nameSeen := map[string]int{}
	for i := range prepared.input.Names {
		row := &prepared.input.Names[i]
		row.Name = strings.TrimSpace(row.Name)
		if row.Name == "" {
			return nil, fmt.Errorf("%w: names[%d].name is required", ErrOrganizationInvalid, i)
		}
		if !organizationNameKindValid(row.NameKind) {
			return nil, fmt.Errorf("%w: names[%d].name_kind %q is unknown",
				ErrOrganizationInvalid, i, row.NameKind)
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		key := row.NameKind + "\x1f" + NormalizeOrganizationName(row.Name)
		if previous, exists := nameSeen[key]; exists {
			return nil, fmt.Errorf("%w: names[%d] duplicates names[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		nameSeen[key] = i
		prepared.nameKeys = append(prepared.nameKeys, key)
	}
	identifierSeen := map[string]int{}
	for i := range prepared.input.Identifiers {
		row := &prepared.input.Identifiers[i]
		row.IdentifierKind = strings.TrimSpace(row.IdentifierKind)
		row.Value = strings.TrimSpace(row.Value)
		if row.IdentifierKind == "" || row.Value == "" {
			return nil, fmt.Errorf("%w: identifiers[%d].identifier_value is required",
				ErrOrganizationInvalid, i)
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		normalized := normalizeOrganizationIdentifier(row.IdentifierKind, row.Value)
		if normalized == "" {
			return nil, fmt.Errorf("%w: identifiers[%d].identifier_value is invalid",
				ErrOrganizationInvalid, i)
		}
		key := row.IdentifierKind + "\x1f" + normalized
		if previous, exists := identifierSeen[key]; exists {
			return nil, fmt.Errorf("%w: identifiers[%d] duplicates identifiers[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		identifierSeen[key] = i
		prepared.identifierKeys = append(prepared.identifierKeys, key)
	}
	addressSeen := map[string]int{}
	for i := range prepared.input.Addresses {
		row := &prepared.input.Addresses[i]
		*row = canonicalOrganizationAddressInput(*row)
		if !row.AddressKind.Valid() || !personAddressHasValue(*row) {
			return nil, fmt.Errorf("%w: addresses[%d] is invalid", ErrOrganizationInvalid, i)
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		key := organizationAddressFingerprint(*row)
		seenKey := key + "\x1f" + organizationValueDiscriminator(row.Envelope)
		if previous, exists := addressSeen[seenKey]; exists {
			return nil, fmt.Errorf("%w: addresses[%d] duplicates addresses[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		addressSeen[seenKey] = i
		prepared.addressKeys = append(prepared.addressKeys, key)
	}
	contactSeen := map[string]int{}
	for i := range prepared.input.ContactPoints {
		row := &prepared.input.ContactPoints[i]
		row.ScopeKind = trimmedOrNil(row.ScopeKind)
		row.ScopeValue = trimmedOrNil(row.ScopeValue)
		if !row.AddressKind.Exportable() {
			return nil, fmt.Errorf("%w: contact_points[%d].address_kind %q is not exportable",
				ErrOrganizationInvalid, i, row.AddressKind)
		}
		if strings.TrimSpace(row.OriginalValue) == "" {
			return nil, fmt.Errorf("%w: contact_points[%d].value_original is required",
				ErrOrganizationInvalid, i)
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		service, _, err := s.resolveOptionalCommunicationServiceContext(ctx, row.ServiceSlug)
		if err != nil {
			return nil, err
		}
		if err := ValidateServiceScope(service, row.ScopeKind, row.ScopeValue); err != nil {
			return nil, err
		}
		normalized, err := NormalizeServiceValue(service, row.AddressKind, row.OriginalValue)
		if err != nil {
			return nil, err
		}
		contact := preparedOrganizationContact{
			input: *row, normalized: normalized,
			normalization:        fallbackContactNormalization(row.AddressKind),
			normalizationVersion: 1,
		}
		if service != nil {
			contact.serviceID = service.ID
			contact.serviceSlug = &service.Slug
			contact.normalization = service.Normalization
			contact.normalizationVersion = service.NormalizationVersion
		}
		key := organizationContactKey(
			row.AddressKind, contact.serviceID, row.ScopeKind, row.ScopeValue, normalized)
		seenKey := key + "\x1f" + organizationValueDiscriminator(row.Envelope)
		if previous, exists := contactSeen[seenKey]; exists {
			return nil, fmt.Errorf("%w: contact_points[%d] duplicates contact_points[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		contactSeen[seenKey] = i
		prepared.contacts = append(prepared.contacts, contact)
		prepared.contactKeys = append(prepared.contactKeys, key)
	}
	mediaSeen := map[string]int{}
	for i := range prepared.input.Media {
		row := &prepared.input.Media[i]
		*row = canonicalOrganizationMediaInput(*row)
		if !row.MediaKind.Valid() ||
			(len(row.Data) == 0 && row.ContentHash == nil &&
				(row.URI == nil || strings.TrimSpace(*row.URI) == "")) {
			return nil, fmt.Errorf("%w: media[%d] requires uri, data, or content_hash",
				ErrOrganizationInvalid, i)
		}
		if len(row.Data) > MaxPersonMediaBytes {
			return nil, ErrPersonMediaTooLarge
		}
		if len(row.Data) > 0 && row.ContentHash != nil {
			digest := sha256.Sum256(row.Data)
			if hex.EncodeToString(digest[:]) != *row.ContentHash {
				return nil, fmt.Errorf(
					"%w: media[%d].content_hash does not match the supplied data",
					ErrOrganizationInvalid, i)
			}
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		key := organizationMediaFingerprint(*row)
		seenKey := key + "\x1f" + organizationValueDiscriminator(row.Envelope)
		if previous, exists := mediaSeen[seenKey]; exists {
			return nil, fmt.Errorf("%w: media[%d] duplicates media[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		mediaSeen[seenKey] = i
		prepared.mediaKeys = append(prepared.mediaKeys, key)
	}
	categorySeen := map[string]int{}
	for i := range prepared.input.Categories {
		row := &prepared.input.Categories[i]
		row.Category = strings.TrimSpace(row.Category)
		if row.Category == "" {
			return nil, fmt.Errorf("%w: categories[%d].category is required",
				ErrOrganizationInvalid, i)
		}
		if err := row.Envelope.Validate(); err != nil {
			return nil, err
		}
		key := NormalizeOrganizationName(row.Category)
		if previous, exists := categorySeen[key]; exists {
			return nil, fmt.Errorf("%w: categories[%d] duplicates categories[%d]",
				ErrOrganizationInvalid, i, previous)
		}
		categorySeen[key] = i
		prepared.categoryKeys = append(prepared.categoryKeys, key)
	}
	return prepared, nil
}

func (s *Store) reconcileOrganizationProfileTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	current *OrganizationProfile, prepared *preparedOrganizationProfile, now time.Time,
) error {
	if err := reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_names", current.Names, prepared.input.Names,
		func(row OrganizationName) int64 { return row.Envelope.ID },
		func(row OrganizationName) string {
			return row.NameKind + "\x1f" + row.NameNormalized
		},
		func(_ OrganizationNameInput, i int) string { return prepared.nameKeys[i] },
		func(row OrganizationName) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row OrganizationNameInput, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.Envelope.VCard, Source: row.Envelope.Source,
				SourceRef:         row.Envelope.SourceRef,
				SourceResourceUID: row.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationName, desired OrganizationNameInput) bool {
			return current.NameKind == desired.NameKind && current.Name == desired.Name &&
				organizationEnvelopeMatches(current.Envelope, desired.Envelope)
		},
		func(row OrganizationNameInput) (int64, error) {
			return s.insertOrganizationNameTx(ctx, tx, organizationID, row, now)
		}, now); err != nil {
		return err
	}
	if err := reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_identifiers", current.Identifiers, prepared.input.Identifiers,
		func(row OrganizationIdentifier) int64 { return row.Envelope.ID },
		func(row OrganizationIdentifier) string {
			return row.IdentifierKind + "\x1f" + row.NormalizedValue
		},
		func(_ OrganizationIdentifierInput, i int) string { return prepared.identifierKeys[i] },
		func(row OrganizationIdentifier) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row OrganizationIdentifierInput, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.Envelope.VCard, Source: row.Envelope.Source,
				SourceRef:         row.Envelope.SourceRef,
				SourceResourceUID: row.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationIdentifier, desired OrganizationIdentifierInput) bool {
			return current.IdentifierKind == desired.IdentifierKind &&
				current.Value == desired.Value &&
				organizationEnvelopeMatches(current.Envelope, desired.Envelope)
		},
		func(row OrganizationIdentifierInput) (int64, error) {
			return s.insertOrganizationIdentifierTx(ctx, tx, organizationID, row, now)
		}, now); err != nil {
		return err
	}
	if err := reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_addresses", current.Addresses, prepared.input.Addresses,
		func(row OrganizationAddress) int64 { return row.Envelope.ID },
		organizationAddressRowFingerprint,
		func(_ OrganizationAddressInput, i int) string { return prepared.addressKeys[i] },
		func(row OrganizationAddress) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row OrganizationAddressInput, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.Envelope.VCard, Source: row.Envelope.Source,
				SourceRef:         row.Envelope.SourceRef,
				SourceResourceUID: row.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationAddress, desired OrganizationAddressInput) bool {
			desired = canonicalOrganizationAddressInput(desired)
			return current.AddressKind == desired.AddressKind &&
				equalOptionalString(current.PostOfficeBox, desired.PostOfficeBox) &&
				equalOptionalString(current.ExtendedAddress, desired.ExtendedAddress) &&
				equalOptionalString(current.StreetAddress, desired.StreetAddress) &&
				equalOptionalString(current.Locality, desired.Locality) &&
				equalOptionalString(current.Region, desired.Region) &&
				equalOptionalString(current.PostalCode, desired.PostalCode) &&
				equalOptionalString(current.CountryName, desired.CountryName) &&
				equalOptionalString(current.ExtendedComponents, desired.ExtendedComponents) &&
				equalOptionalString(current.FreeText, desired.FreeText) &&
				equalOptionalString(current.Label, desired.Label) &&
				equalOptionalString(current.GeoURI, desired.GeoURI) &&
				equalOptionalString(current.Timezone, desired.Timezone) &&
				equalOptionalString(current.CountryCode, desired.CountryCode) &&
				equalOptionalString(current.PlaceURI, desired.PlaceURI) &&
				current.OriginalValue == desired.OriginalValue &&
				organizationEnvelopeMatches(current.Envelope, desired.Envelope)
		},
		func(row OrganizationAddressInput) (int64, error) {
			return s.insertOrganizationAddressTx(ctx, tx, organizationID, row, now)
		}, now); err != nil {
		return err
	}
	if err := reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_contact_points", current.ContactPoints, prepared.contacts,
		func(row OrganizationContactPoint) int64 { return row.Envelope.ID },
		func(row OrganizationContactPoint) string {
			var serviceID any
			if row.ServiceID != nil {
				serviceID = *row.ServiceID
			}
			return organizationContactKey(
				row.AddressKind, serviceID, row.ScopeKind, row.ScopeValue, row.NormalizedValue)
		},
		func(_ preparedOrganizationContact, i int) string { return prepared.contactKeys[i] },
		func(row OrganizationContactPoint) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row preparedOrganizationContact, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.input.Envelope.VCard, Source: row.input.Envelope.Source,
				SourceRef:         row.input.Envelope.SourceRef,
				SourceResourceUID: row.input.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationContactPoint, desired preparedOrganizationContact) bool {
			var currentServiceID any
			if current.ServiceID != nil {
				currentServiceID = *current.ServiceID
			}
			return current.AddressKind == desired.input.AddressKind &&
				currentServiceID == desired.serviceID &&
				equalOptionalString(current.ScopeKind, desired.input.ScopeKind) &&
				equalOptionalString(current.ScopeValue, desired.input.ScopeValue) &&
				current.OriginalValue == desired.input.OriginalValue &&
				current.NormalizedValue == desired.normalized &&
				current.Normalization == desired.normalization &&
				current.NormalizationVersion == desired.normalizationVersion &&
				equalOptionalString(current.URI, desired.input.URI) &&
				organizationEnvelopeMatches(current.Envelope, desired.input.Envelope)
		},
		func(row preparedOrganizationContact) (int64, error) {
			return s.insertOrganizationContactTx(ctx, tx, organizationID, row, now)
		}, now); err != nil {
		return err
	}
	if err := s.resolveOrganizationMediaRetentionTx(
		ctx, tx, current.Media, prepared.input.Media); err != nil {
		return err
	}
	if err := reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_media", current.Media, prepared.input.Media,
		func(row OrganizationMedia) int64 { return row.Envelope.ID },
		organizationMediaRowFingerprint,
		func(_ OrganizationMediaInput, i int) string { return prepared.mediaKeys[i] },
		func(row OrganizationMedia) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row OrganizationMediaInput, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.Envelope.VCard, Source: row.Envelope.Source,
				SourceRef:         row.Envelope.SourceRef,
				SourceResourceUID: row.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationMedia, desired OrganizationMediaInput) bool {
			desired = canonicalOrganizationMediaInput(desired)
			desiredHash := organizationMediaInputHash(desired)
			return current.MediaKind == desired.MediaKind &&
				equalOptionalString(current.MediaType, desired.MediaType) &&
				equalOptionalString(current.URI, desired.URI) &&
				equalOptionalString(current.ContentHash, stringPtrOrNil(desiredHash)) &&
				current.OriginalValue == desired.OriginalValue &&
				organizationEnvelopeMatches(current.Envelope, desired.Envelope)
		},
		func(row OrganizationMediaInput) (int64, error) {
			return s.insertOrganizationMediaTx(ctx, tx, organizationID, row, now)
		}, now); err != nil {
		return err
	}
	return reconcileOrganizationCollection(ctx, tx, s, organizationID,
		"organization_categories", current.Categories, prepared.input.Categories,
		func(row OrganizationCategory) int64 { return row.Envelope.ID },
		func(row OrganizationCategory) string { return row.CategoryNormalized },
		func(_ OrganizationCategoryInput, i int) string { return prepared.categoryKeys[i] },
		func(row OrganizationCategory) string {
			return organizationVCardIdentityKey(row.Envelope)
		},
		func(row OrganizationCategoryInput, _ int) string {
			return organizationVCardIdentityKey(ValueEnvelope{
				VCard: row.Envelope.VCard, Source: row.Envelope.Source,
				SourceRef:         row.Envelope.SourceRef,
				SourceResourceUID: row.Envelope.SourceResourceUID,
			})
		},
		func(current OrganizationCategory, desired OrganizationCategoryInput) bool {
			desired.Category = strings.TrimSpace(desired.Category)
			return current.Category == desired.Category &&
				organizationEnvelopeMatches(current.Envelope, desired.Envelope)
		},
		func(row OrganizationCategoryInput) (int64, error) {
			return s.insertOrganizationCategoryTx(ctx, tx, organizationID, row, now)
		}, now)
}

func reconcileOrganizationCollection[C any, D any](
	ctx context.Context, tx *loggedTx, s *Store, organizationID int64, table string,
	current []C, desired []D, currentID func(C) int64, currentKey func(C) string,
	desiredKey func(D, int) string, currentIdentity func(C) string,
	desiredIdentity func(D, int) string, matches func(C, D) bool,
	insert func(D) (int64, error), now time.Time,
) error {
	existingByID := make(map[int64]C, len(current))
	existingByBusinessKey := make(map[string][]int64, len(current))
	existingByIdentity := make(map[string]int64, len(current))
	for _, row := range current {
		id := currentID(row)
		existingByID[id] = row
		key := currentKey(row)
		existingByBusinessKey[key] = append(existingByBusinessKey[key], id)
		if identity := currentIdentity(row); identity != "" {
			existingByIdentity[identity] = id
		}
	}
	matched := make(map[int64]bool, len(current))
	seenIdentities := make(map[string]int, len(desired))
	kept := make([]int64, 0, len(desired))
	toInsert := make([]D, 0, len(desired))
	for i, row := range desired {
		identity := desiredIdentity(row, i)
		if identity != "" {
			if previous, exists := seenIdentities[identity]; exists {
				return fmt.Errorf("%w: %s[%d] duplicates %s[%d] durable vCard identity",
					ErrOrganizationInvalid, table, i, table, previous)
			}
			seenIdentities[identity] = i
		}
		var existingRow C
		var id int64
		found := false
		if identity != "" {
			id, found = existingByIdentity[identity]
		}
		if found && matched[id] {
			found = false
		}
		if !found {
			// Several current rows can legitimately share a business value
			// (distinct PROP-IDs, TYPE labels, or ordinals). Prefer an
			// unclaimed row whose full envelope matches, so each desired row
			// keeps its own counterpart; otherwise claim any unclaimed row.
			candidates := existingByBusinessKey[desiredKey(row, i)]
			for _, candidate := range candidates {
				if !matched[candidate] && matches(existingByID[candidate], row) {
					id, found = candidate, true
					break
				}
			}
			if !found {
				for _, candidate := range candidates {
					if !matched[candidate] {
						id, found = candidate, true
						break
					}
				}
			}
		}
		if found {
			existingRow = existingByID[id]
			matched[id] = true
			if matches(existingRow, row) {
				kept = append(kept, id)
				continue
			}
		}
		toInsert = append(toInsert, row)
	}
	// Retire changed and removed rows before inserting replacements. In
	// addition to the business-key indexes, the active vCard property identity
	// indexes reject a replacement until the old row is superseded.
	if err := s.supersedeOrganizationRowsTx(ctx, tx, table, organizationID, kept, now); err != nil {
		return err
	}
	for _, row := range toInsert {
		id, err := insert(row)
		if err != nil {
			return err
		}
		kept = append(kept, id)
	}
	return nil
}

// organizationValueDiscriminator distinguishes input rows that share a
// business value: an imported profile can carry identical values under
// different PROP-IDs, TYPE labels, or ordinals, and each must survive a
// profile replacement instead of being rejected as a duplicate.
func organizationValueDiscriminator(env ValueEnvelopeInput) string {
	identity := organizationVCardIdentityKey(ValueEnvelope{
		VCard: env.VCard, Source: env.Source, SourceRef: env.SourceRef,
		SourceResourceUID: env.SourceResourceUID,
	})
	ordinal := ""
	if env.Ordinal != nil {
		ordinal = strconv.Itoa(*env.Ordinal)
	}
	return strings.Join([]string{
		identity, ordinal, derefString(env.TypeLabel),
		strings.Join(env.TypeTokens, ","),
	}, "\x1f")
}

// organizationVCardIdentityKey returns the identity that a source can keep
// stable while the value itself changes. The database uses the same five
// fields for its active property-identity uniqueness indexes.
func organizationVCardIdentityKey(env ValueEnvelope) string {
	if env.SourceRef == nil || *env.SourceRef == "" ||
		env.VCard.PropID == nil || *env.VCard.PropID == "" {
		return ""
	}
	return strings.Join([]string{
		string(env.Source), *env.SourceRef, derefString(env.SourceResourceUID),
		env.VCard.Property, *env.VCard.PropID,
	}, "\x1f")
}

// organizationEnvelopeMatches compares fields that the organization profile
// replacement can write. Omitted ordinal and active_from values request the
// normal store defaults, so they do not churn an existing row's resolved
// values when the durable value identity is unchanged.
func organizationEnvelopeMatches(current ValueEnvelope, desired ValueEnvelopeInput) bool {
	if !equalOptionalInt(current.Pref, desired.Pref) ||
		(desired.Ordinal != nil && current.Ordinal != *desired.Ordinal) ||
		!equalOptionalString(current.TypeLabel, desired.TypeLabel) ||
		!equalStringSlices(current.TypeTokens, desired.TypeTokens) ||
		!equalVCardIdentity(current.VCard, desired.VCard) ||
		current.Source != desired.Source ||
		!equalOptionalString(current.SourceRef, desired.SourceRef) ||
		!equalOptionalString(current.SourceResourceUID, desired.SourceResourceUID) ||
		!equalOptionalFloat(current.Confidence, desired.Confidence) {
		return false
	}
	if desired.ActiveFrom != nil {
		if current.ActiveFrom == nil || !current.ActiveFrom.Equal(*desired.ActiveFrom) {
			return false
		}
	}
	return true
}

func equalOptionalInt(current, desired *int) bool {
	if current == nil || desired == nil {
		return current == nil && desired == nil
	}
	return *current == *desired
}

func equalOptionalFloat(current, desired *float64) bool {
	if current == nil || desired == nil {
		return current == nil && desired == nil
	}
	return *current == *desired
}

func equalStringSlices(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	for i := range current {
		if current[i] != desired[i] {
			return false
		}
	}
	return true
}

func equalVCardIdentity(current, desired VCardIdentity) bool {
	return current.Property == desired.Property &&
		equalOptionalString(current.Group, desired.Group) &&
		equalOptionalString(current.PropID, desired.PropID) &&
		equalStringSlices(current.PID, desired.PID) &&
		equalOptionalString(current.AltID, desired.AltID)
}

func stringPtrOrNil(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (s *Store) supersedeOrganizationRowsTx(
	ctx context.Context, tx *loggedTx, table string, organizationID int64,
	kept []int64, now time.Time,
) error {
	where := ` WHERE organization_id = ?
		AND active_until IS NULL AND superseded_at IS NULL`
	args := []any{organizationID}
	if len(kept) > 0 {
		where += ` AND id NOT IN (` + placeholders(len(kept)) + `)`
		for _, id := range kept {
			args = append(args, id)
		}
	}
	futureArgs := append([]any{now}, args...)
	futureArgs = append(futureArgs, now)
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+`
		SET superseded_at = ?, updated_at = `+s.dialect.Now()+where+`
		  AND active_from IS NOT NULL AND active_from > ?`, futureArgs...); err != nil {
		return fmt.Errorf("retract future %s: %w", table, err)
	}
	effectiveArgs := append([]any{now, now}, args...)
	effectiveArgs = append(effectiveArgs, now)
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+`
		SET active_until = ?, superseded_at = ?, updated_at = `+s.dialect.Now()+where+`
		  AND (active_from IS NULL OR active_from <= ?)`, effectiveArgs...); err != nil {
		return fmt.Errorf("supersede %s: %w", table, err)
	}
	return nil
}

func (s *Store) insertOrganizationNameTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	input OrganizationNameInput, now time.Time,
) (int64, error) {
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_names", "name_kind",
		organizationID, input.NameKind, input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	args := []any{
		organizationID, input.NameKind, input.Name, input.Name,
		NormalizeOrganizationName(input.Name),
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_names",
		"organization_id, name_kind, formatted, original_value, name_normalized",
		args, env)
}

func (s *Store) insertOrganizationIdentifierTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	input OrganizationIdentifierInput, now time.Time,
) (int64, error) {
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_identifiers", "identifier_kind",
		organizationID, input.IdentifierKind, input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	args := []any{
		organizationID, input.IdentifierKind, input.Value,
		normalizeOrganizationIdentifier(input.IdentifierKind, input.Value),
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_identifiers",
		"organization_id, identifier_kind, identifier_value, normalized_value", args, env)
}

func (s *Store) insertOrganizationAddressTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	input OrganizationAddressInput, now time.Time,
) (int64, error) {
	input = canonicalOrganizationAddressInput(input)
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_addresses", "address_kind",
		organizationID, input.AddressKind, input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	args := []any{
		organizationID, input.AddressKind, stringValue(input.PostOfficeBox),
		stringValue(input.ExtendedAddress), stringValue(input.StreetAddress),
		stringValue(input.Locality), stringValue(input.Region),
		stringValue(input.PostalCode), stringValue(input.CountryName),
		stringValue(input.ExtendedComponents), stringValue(input.FreeText),
		stringValue(input.Label), stringValue(input.GeoURI), stringValue(input.Timezone),
		stringValue(input.CountryCode), stringValue(input.PlaceURI), input.OriginalValue,
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_addresses",
		`organization_id, address_kind, post_office_box, extended_address,
		street_address, locality, region, postal_code, country_name,
		extended_components, free_text, label, geo_uri, timezone, country_code,
		place_uri, original_value`, args, env)
}

func (s *Store) insertOrganizationContactTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	row preparedOrganizationContact, now time.Time,
) (int64, error) {
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_contact_points", "address_kind",
		organizationID, row.input.AddressKind, row.input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	args := []any{
		organizationID, row.input.AddressKind, row.serviceID,
		stringValue(row.input.ScopeKind), stringValue(row.input.ScopeValue),
		row.input.OriginalValue, row.normalized, row.normalization,
		row.normalizationVersion, stringValue(row.input.URI),
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_contact_points",
		`organization_id, address_kind, service_id, scope_kind, scope_value,
		original_value, normalized_value, normalization, normalization_version, uri`,
		args, env)
}

func (s *Store) insertOrganizationMediaTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	input OrganizationMediaInput, now time.Time,
) (int64, error) {
	input = canonicalOrganizationMediaInput(input)
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_media", "media_kind",
		organizationID, input.MediaKind, input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	if len(input.Data) == 0 && input.ContentHash != nil {
		// A retention hash only means "keep the stored bytes". Reaching insert
		// means no active row carries this content, so honouring the hash would
		// record content the database does not have.
		return 0, fmt.Errorf(
			"%w: media content_hash %q does not match an active media row; re-send data or drop content_hash",
			ErrOrganizationInvalid, *input.ContentHash)
	}
	var data, byteSize, contentHash any
	if len(input.Data) > 0 {
		digest := sha256.Sum256(input.Data)
		data, byteSize, contentHash = input.Data, int64(len(input.Data)), hex.EncodeToString(digest[:])
	}
	args := []any{
		organizationID, input.MediaKind, stringValue(input.MediaType),
		stringValue(input.URI), data, byteSize, contentHash, input.OriginalValue,
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_media",
		`organization_id, media_kind, media_type, uri, data, byte_size,
		content_hash, original_value`, args, env)
}

func (s *Store) insertOrganizationCategoryTx(
	ctx context.Context, tx *loggedTx, organizationID int64,
	input OrganizationCategoryInput, now time.Time,
) (int64, error) {
	env, err := resolveOrganizationEnvelopeTx(
		ctx, tx, "organization_categories", "",
		organizationID, nil, input.Envelope, now,
	)
	if err != nil {
		return 0, err
	}
	args := []any{
		organizationID, input.Category, NormalizeOrganizationName(input.Category),
	}
	return s.insertOrganizationProfileRowTx(ctx, tx, "organization_categories",
		"organization_id, original_value, normalized_value", args, env)
}

func (s *Store) insertOrganizationProfileRowTx(
	ctx context.Context, tx *loggedTx, table, columns string,
	args []any, env ValueEnvelope,
) (int64, error) {
	args = append(args, profileEnvelopeArgs(env)...)
	query := `INSERT INTO ` + table + ` (` + columns + `, ` +
		profileEnvelopeWriteColumns + `, created_at, updated_at) VALUES (` +
		placeholders(len(args)) + `, ` + s.dialect.Now() + `, ` + s.dialect.Now() +
		`) RETURNING id`
	var id int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return 0, fmt.Errorf("insert %s: %w", table, err)
	}
	return id, nil
}

func resolveOrganizationEnvelopeTx(
	ctx context.Context,
	tx *loggedTx,
	table, kindColumn string,
	organizationID int64,
	kind any,
	input ValueEnvelopeInput,
	now time.Time,
) (ValueEnvelope, error) {
	env, err := resolveProfileEnvelopeForOwnerTx(
		ctx, tx, table, "organization_id", kindColumn,
		organizationID, kind, input,
	)
	if err != nil {
		return ValueEnvelope{}, err
	}
	if env.ActiveFrom == nil {
		env.ActiveFrom = &now
	}
	env.ActiveUntil = nil
	env.SupersededAt = nil
	return env, nil
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func organizationNameKindValid(kind string) bool {
	switch kind {
	case OrganizationNameKindAlias, OrganizationNameKindLegal, OrganizationNameKindFormer,
		OrganizationNameKindAbbreviation, OrganizationNameKindSort:
		return true
	default:
		return false
	}
}

func normalizeOrganizationIdentifier(kind, value string) string {
	if kind == OrganizationIdentifierKindDomain {
		return NormalizeDomain(value)
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func canonicalOrganizationAddressInput(input OrganizationAddressInput) OrganizationAddressInput {
	input.PostOfficeBox = trimmedOrNil(input.PostOfficeBox)
	input.ExtendedAddress = trimmedOrNil(input.ExtendedAddress)
	input.StreetAddress = trimmedOrNil(input.StreetAddress)
	input.Locality = trimmedOrNil(input.Locality)
	input.Region = trimmedOrNil(input.Region)
	input.PostalCode = trimmedOrNil(input.PostalCode)
	input.CountryName = trimmedOrNil(input.CountryName)
	input.ExtendedComponents = trimmedOrNil(input.ExtendedComponents)
	input.FreeText = trimmedOrNil(input.FreeText)
	input.Label = trimmedOrNil(input.Label)
	input.GeoURI = trimmedOrNil(input.GeoURI)
	input.Timezone = trimmedOrNil(input.Timezone)
	input.CountryCode = trimmedOrNil(input.CountryCode)
	input.PlaceURI = trimmedOrNil(input.PlaceURI)
	input.OriginalValue = strings.TrimSpace(input.OriginalValue)
	if input.OriginalValue == "" {
		input.OriginalValue = personAddressOriginalValue(input)
	}
	return input
}

func canonicalOrganizationMediaInput(input OrganizationMediaInput) OrganizationMediaInput {
	input.MediaType = trimmedOrNil(input.MediaType)
	input.URI = trimmedOrNil(input.URI)
	input.ContentHash = trimmedOrNil(input.ContentHash)
	input.OriginalValue = strings.TrimSpace(input.OriginalValue)
	if input.OriginalValue == "" && input.URI != nil {
		input.OriginalValue = *input.URI
	}
	return input
}

// organizationMediaInputHash resolves the content identity a media input
// claims: the hash of its inline bytes, or the retention content_hash when
// the bytes are omitted, or "" for a row without inline content.
func organizationMediaInputHash(input OrganizationMediaInput) string {
	if len(input.Data) > 0 {
		digest := sha256.Sum256(input.Data)
		return hex.EncodeToString(digest[:])
	}
	return derefString(input.ContentHash)
}

// resolveOrganizationMediaRetentionTx loads the stored bytes behind every
// retention content_hash before reconciliation, so an edit to a media row's
// other fields — which supersedes and reinserts the row — carries the inline
// content into the replacement instead of failing for lack of data. A hash
// with no active match is a client error: honouring it would record content
// the database does not have.
func (s *Store) resolveOrganizationMediaRetentionTx(
	ctx context.Context, tx *loggedTx,
	currentMedia []OrganizationMedia, inputs []OrganizationMediaInput,
) error {
	for i := range inputs {
		input := &inputs[i]
		if len(input.Data) > 0 || input.ContentHash == nil {
			continue
		}
		var sourceID int64
		for _, row := range currentMedia {
			if row.HasData && row.ContentHash != nil &&
				*row.ContentHash == *input.ContentHash {
				sourceID = row.Envelope.ID
				break
			}
		}
		if sourceID == 0 {
			return fmt.Errorf(
				"%w: media[%d].content_hash %q does not match an active media row; re-send data or drop content_hash",
				ErrOrganizationInvalid, i, *input.ContentHash)
		}
		var data []byte
		if err := tx.QueryRowContext(ctx,
			`SELECT data FROM organization_media WHERE id = ?`, sourceID,
		).Scan(&data); err != nil {
			return fmt.Errorf("load retained media %d content: %w", sourceID, err)
		}
		input.Data = data
	}
	return nil
}

func organizationAddressFingerprint(input OrganizationAddressInput) string {
	input = canonicalOrganizationAddressInput(input)
	return strings.Join([]string{
		string(input.AddressKind), derefString(input.PostOfficeBox),
		derefString(input.ExtendedAddress), derefString(input.StreetAddress),
		derefString(input.Locality), derefString(input.Region),
		derefString(input.PostalCode), derefString(input.CountryName),
		derefString(input.ExtendedComponents), derefString(input.FreeText),
		derefString(input.Label), derefString(input.GeoURI),
		derefString(input.Timezone), derefString(input.CountryCode),
		derefString(input.PlaceURI), strings.TrimSpace(input.OriginalValue),
	}, "\x1f")
}

func organizationAddressRowFingerprint(row OrganizationAddress) string {
	return organizationAddressFingerprint(OrganizationAddressInput{
		AddressKind: row.AddressKind, PostOfficeBox: row.PostOfficeBox,
		ExtendedAddress: row.ExtendedAddress, StreetAddress: row.StreetAddress,
		Locality: row.Locality, Region: row.Region, PostalCode: row.PostalCode,
		CountryName: row.CountryName, ExtendedComponents: row.ExtendedComponents,
		FreeText: row.FreeText, Label: row.Label, GeoURI: row.GeoURI,
		Timezone: row.Timezone, CountryCode: row.CountryCode, PlaceURI: row.PlaceURI,
		OriginalValue: row.OriginalValue,
	})
}

func organizationContactKey(
	kind ContactAddressKind, serviceID any, scopeKind, scopeValue *string, normalized string,
) string {
	scopeKind = trimmedOrNil(scopeKind)
	scopeValue = trimmedOrNil(scopeValue)
	return strings.Join([]string{
		string(kind), fmt.Sprint(serviceID), derefString(scopeKind),
		derefString(scopeValue), normalized,
	}, "\x1f")
}

func organizationMediaFingerprint(input OrganizationMediaInput) string {
	input = canonicalOrganizationMediaInput(input)
	return strings.Join([]string{
		string(input.MediaKind), derefString(input.MediaType), derefString(input.URI),
		organizationMediaInputHash(input), strings.TrimSpace(input.OriginalValue),
	}, "\x1f")
}

func organizationMediaRowFingerprint(row OrganizationMedia) string {
	input := canonicalOrganizationMediaInput(OrganizationMediaInput{
		MediaKind: row.MediaKind, MediaType: row.MediaType, URI: row.URI,
		OriginalValue: row.OriginalValue,
	})
	return strings.Join([]string{
		string(input.MediaKind), derefString(input.MediaType), derefString(input.URI),
		derefString(row.ContentHash), input.OriginalValue,
	}, "\x1f")
}

// contextRowsQuerier is the multi-row read surface shared by *loggedTx and the
// store's own database handle, so a listing can run inside a caller's
// transaction or on its own.
type contextRowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*loggedRows, error)
}

func (s *Store) loadOrganizationProfileContext(
	ctx context.Context, queryer contextRowsQuerier,
	organization *Organization, includeSuperseded bool,
) (*OrganizationProfile, error) {
	profile := &OrganizationProfile{Organization: *organization}
	var err error
	if profile.Names, err = queryOrganizationRows(
		ctx, queryer, organizationNameSelect, organization.ID, includeSuperseded,
		scanOrganizationName); err != nil {
		return nil, err
	}
	if profile.Identifiers, err = queryOrganizationRows(
		ctx, queryer, organizationIdentifierSelect, organization.ID, includeSuperseded,
		scanOrganizationIdentifier); err != nil {
		return nil, err
	}
	if profile.Addresses, err = queryOrganizationRows(
		ctx, queryer, organizationAddressSelect, organization.ID, includeSuperseded,
		scanOrganizationAddress); err != nil {
		return nil, err
	}
	if profile.ContactPoints, err = queryOrganizationRows(
		ctx, queryer, organizationContactSelect, organization.ID, includeSuperseded,
		scanOrganizationContact); err != nil {
		return nil, err
	}
	if profile.Media, err = queryOrganizationRows(
		ctx, queryer, organizationMediaSelect, organization.ID, includeSuperseded,
		scanOrganizationMedia); err != nil {
		return nil, err
	}
	profile.Categories, err = queryOrganizationRows(
		ctx, queryer, organizationCategorySelect, organization.ID, includeSuperseded,
		scanOrganizationCategory)
	if err != nil {
		return nil, err
	}
	return profile, nil
}

func queryOrganizationRows[T any](
	ctx context.Context, queryer contextRowsQuerier, base string,
	organizationID int64, includeSuperseded bool,
	scan func(scanner) (*T, error),
) ([]T, error) {
	query := base + ` WHERE organization_id = ?`
	if !includeSuperseded {
		query += ` AND active_until IS NULL AND superseded_at IS NULL`
	}
	if includeSuperseded {
		query += ` ORDER BY CASE WHEN active_until IS NULL AND superseded_at IS NULL
			THEN 0 ELSE 1 END, ordinal, id`
	} else {
		query += ` ORDER BY ordinal, id`
	}
	rows, err := queryer.QueryContext(ctx, query, organizationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]T, 0)
	for rows.Next() {
		row, err := scan(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *row)
	}
	return result, rows.Err()
}

const organizationNameSelect = `SELECT id, organization_id, original_value,
	name_normalized, name_kind, ` + profileEnvelopeReadColumns + ` FROM organization_names`
const organizationIdentifierSelect = `SELECT id, organization_id, identifier_kind,
	identifier_value, normalized_value, ` + profileEnvelopeReadColumns + `
	FROM organization_identifiers`
const organizationAddressSelect = `SELECT id, organization_id, address_kind,
	post_office_box, extended_address, street_address, locality, region, postal_code,
	country_name, extended_components, free_text, label, geo_uri, timezone,
	country_code, place_uri, original_value, ` + profileEnvelopeReadColumns + `
	FROM organization_addresses`
const organizationContactSelect = `SELECT p.id, p.organization_id, p.address_kind,
	p.service_id, (SELECT slug FROM communication_services WHERE id = p.service_id),
	p.scope_kind, p.scope_value, p.original_value, p.normalized_value,
	p.normalization, p.normalization_version, p.uri, ` + profileEnvelopeReadColumns + `
	FROM organization_contact_points p`

// ReadOrganizationMediaDataContext returns the stored inline bytes for one
// organization media value. URI-only values have no local content.
func (s *Store) ReadOrganizationMediaDataContext(
	ctx context.Context, organizationID, mediaID int64,
) ([]byte, string, error) {
	var data []byte
	var mediaType sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT data, media_type FROM organization_media
		 WHERE organization_id = ? AND id = ?`,
		organizationID, mediaID,
	).Scan(&data, &mediaType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrProfileValueNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("read organization media data: %w", err)
	}
	if len(data) == 0 {
		return nil, "", ErrPersonMediaNoData
	}
	return data, mediaType.String, nil
}

const organizationMediaSelect = `SELECT id, organization_id, media_kind, media_type,
	uri, byte_size, content_hash, (data IS NOT NULL) AS has_data, original_value,
	` + profileEnvelopeReadColumns + ` FROM organization_media`
const organizationCategorySelect = `SELECT id, organization_id, original_value,
	normalized_value, ` + profileEnvelopeReadColumns + ` FROM organization_categories`

func scanOrganizationName(row scanner) (*OrganizationName, error) {
	var value OrganizationName
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID, &value.Name,
		&value.NameNormalized, &value.NameKind,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &value, env.apply(&value.Envelope)
}

func scanOrganizationIdentifier(row scanner) (*OrganizationIdentifier, error) {
	var value OrganizationIdentifier
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID, &value.IdentifierKind,
		&value.Value, &value.NormalizedValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &value, env.apply(&value.Envelope)
}

func scanOrganizationAddress(row scanner) (*OrganizationAddress, error) {
	var value OrganizationAddress
	var postOfficeBox, extendedAddress, streetAddress, locality sql.NullString
	var region, postalCode, countryName, extendedComponents sql.NullString
	var freeText, label, geoURI, timezone, countryCode, placeURI sql.NullString
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID, &value.AddressKind,
		&postOfficeBox, &extendedAddress, &streetAddress, &locality, &region,
		&postalCode, &countryName, &extendedComponents, &freeText, &label,
		&geoURI, &timezone, &countryCode, &placeURI, &value.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	value.PostOfficeBox, value.ExtendedAddress = nullStringPtr(postOfficeBox), nullStringPtr(extendedAddress)
	value.StreetAddress, value.Locality = nullStringPtr(streetAddress), nullStringPtr(locality)
	value.Region, value.PostalCode = nullStringPtr(region), nullStringPtr(postalCode)
	value.CountryName, value.ExtendedComponents = nullStringPtr(countryName), nullStringPtr(extendedComponents)
	value.FreeText, value.Label = nullStringPtr(freeText), nullStringPtr(label)
	value.GeoURI, value.Timezone = nullStringPtr(geoURI), nullStringPtr(timezone)
	value.CountryCode, value.PlaceURI = nullStringPtr(countryCode), nullStringPtr(placeURI)
	return &value, env.apply(&value.Envelope)
}

func scanOrganizationContact(row scanner) (*OrganizationContactPoint, error) {
	var value OrganizationContactPoint
	var serviceSlug, scopeKind, scopeValue, uri sql.NullString
	var serviceID sql.NullInt64
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID, &value.AddressKind,
		&serviceID, &serviceSlug, &scopeKind, &scopeValue, &value.OriginalValue,
		&value.NormalizedValue, &value.Normalization,
		&value.NormalizationVersion, &uri,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	value.ServiceSlug, value.ScopeKind = nullStringPtr(serviceSlug), nullStringPtr(scopeKind)
	if serviceID.Valid {
		value.ServiceID = new(serviceID.Int64)
	}
	value.ScopeValue, value.URI = nullStringPtr(scopeValue), nullStringPtr(uri)
	return &value, env.apply(&value.Envelope)
}

func scanOrganizationMedia(row scanner) (*OrganizationMedia, error) {
	var value OrganizationMedia
	var mediaType, uri, contentHash sql.NullString
	var byteSize sql.NullInt64
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID, &value.MediaKind,
		&mediaType, &uri, &byteSize, &contentHash, &value.HasData,
		&value.OriginalValue,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	value.MediaType, value.URI = nullStringPtr(mediaType), nullStringPtr(uri)
	if byteSize.Valid {
		value.ByteSize = new(byteSize.Int64)
	}
	value.ContentHash = nullStringPtr(contentHash)
	return &value, env.apply(&value.Envelope)
}

func scanOrganizationCategory(row scanner) (*OrganizationCategory, error) {
	var value OrganizationCategory
	var env profileEnvelopeScanValues
	dest := []any{
		&value.Envelope.ID, &value.OrganizationID,
		&value.Category, &value.CategoryNormalized,
	}
	dest = append(dest, env.destinations()...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	return &value, env.apply(&value.Envelope)
}
