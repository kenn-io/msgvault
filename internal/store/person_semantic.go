package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/vector"
	"golang.org/x/text/unicode/norm"
)

const (
	// PersonSemanticRendererPolicy changes whenever the selected fields,
	// labels, ordering, normalization, or cap changes.
	PersonSemanticRendererPolicy = vector.SemanticPersonRendererPolicy

	// MaxPersonSemanticDocumentBytes bounds the exact UTF-8 provider input.
	// The cap is applied before Revision is calculated.
	MaxPersonSemanticDocumentBytes = 32 * 1024
)

// PersonSemanticDocument is the canonical, privacy-bounded embedding source
// for one durable person. Revision is the lowercase SHA-256 digest of the
// renderer policy, a NUL separator, and Text's exact capped bytes.
type PersonSemanticDocument struct {
	PersonID       int64  `json:"person_id"`
	RendererPolicy string `json:"renderer_policy"`
	Revision       string `json:"revision"`
	Text           string `json:"text"`
}

// PersonSemanticCandidate identifies one person ANN hit by the exact semantic
// revision published with its vector.
type PersonSemanticCandidate struct {
	PersonID int64
	Revision string
}

type personSemanticSnapshot struct {
	person        Person
	names         []PersonName
	categories    []PersonCategory
	addresses     []PersonAddress
	attributes    []personSemanticAttribute
	employments   []personSemanticEmployment
	relationships []PersonRelationshipView
}

type personSemanticAttribute struct {
	definition AttributeDefinition
	value      PersonAttributeValue
}

type personSemanticEmployment struct {
	employment             Employment
	organization           Organization
	organizationNames      []OrganizationName
	organizationCategories []OrganizationCategory
	organizationAddresses  []OrganizationAddress
}

// LoadPersonSemanticDocumentContext returns one extant person's canonical
// semantic document from a single repeatable-read snapshot.
func (s *Store) LoadPersonSemanticDocumentContext(
	ctx context.Context, personID int64,
) (*PersonSemanticDocument, error) {
	var document *PersonSemanticDocument
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		allowed, err := s.loadPersonSemanticAttributeDefinitionsTx(ctx, tx)
		if err != nil {
			return err
		}
		snapshot, err := s.loadPersonSemanticSnapshotTx(ctx, tx, personID, allowed)
		if err != nil {
			return err
		}
		document, err = renderPersonSemanticDocument(snapshot)
		return err
	})
	return document, err
}

// ListPersonSemanticDocumentsContext returns every extant person's canonical
// semantic document in durable-person-ID order from one repeatable-read
// snapshot.
func (s *Store) ListPersonSemanticDocumentsContext(
	ctx context.Context,
) ([]PersonSemanticDocument, error) {
	documents := make([]PersonSemanticDocument, 0)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		allowed, err := s.loadPersonSemanticAttributeDefinitionsTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM persons ORDER BY id`)
		if err != nil {
			return fmt.Errorf("list people for semantic projection: %w", err)
		}
		personIDs := make([]int64, 0)
		for rows.Next() {
			var personID int64
			if err := rows.Scan(&personID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan person for semantic projection: %w", err)
			}
			personIDs = append(personIDs, personID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate people for semantic projection: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close people semantic projection rows: %w", err)
		}

		for _, personID := range personIDs {
			snapshot, err := s.loadPersonSemanticSnapshotTx(ctx, tx, personID, allowed)
			if err != nil {
				return err
			}
			document, err := renderPersonSemanticDocument(snapshot)
			if err != nil {
				return err
			}
			documents = append(documents, *document)
		}
		return nil
	})
	return documents, err
}

// ResolvePersonSemanticCandidatesContext validates candidate revisions and
// returns their durable roots in candidate order from one repeatable-read
// snapshot. A stale or deleted candidate is omitted. Keeping render,
// comparison, and root hydration inside this single snapshot prevents an edit
// from pairing a score for an old semantic document with a newer person root.
// Revisions intentionally include rendered organization and relationship
// counterpart text. Editing either linked record therefore suppresses stale
// hits for affected people until the scheduled person worker republishes them.
func (s *Store) ResolvePersonSemanticCandidatesContext(
	ctx context.Context, candidates []PersonSemanticCandidate,
) ([]Person, error) {
	people := make([]Person, 0, len(candidates))
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		allowed, err := s.loadPersonSemanticAttributeDefinitionsTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, candidate := range candidates {
			snapshot, err := s.loadPersonSemanticSnapshotTx(ctx, tx, candidate.PersonID, allowed)
			if errors.Is(err, ErrPersonNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("load person %d semantic search candidate: %w", candidate.PersonID, err)
			}
			document, err := renderPersonSemanticDocument(snapshot)
			if err != nil {
				slog.Warn("skipping unrenderable person semantic search candidate",
					"person_id", candidate.PersonID, "error", err)
				continue
			}
			if document.Text == "" || document.Revision != candidate.Revision {
				continue
			}
			people = append(people, snapshot.person)
		}
		return nil
	})
	return people, err
}

func (s *Store) loadPersonSemanticSnapshotTx(
	ctx context.Context, tx *loggedTx, personID int64,
	allowedAttributes map[string]AttributeDefinition,
) (*personSemanticSnapshot, error) {
	person, err := s.getPersonTx(ctx, tx, personID)
	if err != nil {
		return nil, err
	}
	snapshot := &personSemanticSnapshot{person: *person}
	if snapshot.names, err = s.listPersonNamesTx(ctx, tx, personID, true); err != nil {
		return nil, fmt.Errorf("load current person names for semantic projection: %w", err)
	}
	if snapshot.categories, err = s.listPersonCategoriesTx(ctx, tx, personID, true); err != nil {
		return nil, fmt.Errorf("load current person categories for semantic projection: %w", err)
	}
	if snapshot.addresses, err = s.listPersonAddressesTx(ctx, tx, personID, true); err != nil {
		return nil, fmt.Errorf("load current person locations for semantic projection: %w", err)
	}
	if snapshot.attributes, err = s.loadPersonSemanticAttributesTx(
		ctx, tx, personID, allowedAttributes,
	); err != nil {
		return nil, err
	}
	if snapshot.employments, err = s.loadPersonSemanticEmploymentsTx(ctx, tx, personID); err != nil {
		return nil, err
	}
	if snapshot.relationships, err = s.listPersonRelationshipsContext(
		ctx, tx, personID, PersonRelationshipListOptions{},
	); err != nil {
		return nil, fmt.Errorf("load active person relationships for semantic projection: %w", err)
	}
	return snapshot, nil
}

func (s *Store) loadPersonSemanticAttributeDefinitionsTx(
	ctx context.Context, tx *loggedTx,
) (map[string]AttributeDefinition, error) {
	definitions, err := s.listAttributeDefinitionsContext(ctx, tx,
		AttributeDefinitionFilter{ObjectType: AttributeObjectPerson})
	if err != nil {
		return nil, fmt.Errorf("load active person attribute definitions for semantic projection: %w", err)
	}
	allowed := make(map[string]AttributeDefinition, len(definitions))
	for _, definition := range definitions {
		if !definition.IsSearchable || definition.IsSensitive ||
			definition.FieldType == AttributeFieldEmail ||
			definition.FieldType == AttributeFieldPhone ||
			definition.ValueType == AttributeValueDate ||
			definition.ValueType == AttributeValueTimestamp {
			continue
		}
		allowed[definition.Slug] = definition
	}
	return allowed, nil
}

func (s *Store) loadPersonSemanticAttributesTx(
	ctx context.Context, tx *loggedTx, personID int64,
	allowed map[string]AttributeDefinition,
) ([]personSemanticAttribute, error) {
	values, err := s.listPersonAttributeValuesContext(
		ctx, tx, personID, PersonAttributeQuery{},
	)
	if err != nil {
		return nil, fmt.Errorf("load current person attributes for semantic projection: %w", err)
	}
	attributes := make([]personSemanticAttribute, 0, len(values))
	for _, value := range values {
		definition, ok := allowed[value.DefinitionSlug]
		if !ok {
			continue
		}
		attributes = append(attributes, personSemanticAttribute{
			definition: definition,
			value:      value,
		})
	}
	return attributes, nil
}

func (s *Store) loadPersonSemanticEmploymentsTx(
	ctx context.Context, tx *loggedTx, personID int64,
) ([]personSemanticEmployment, error) {
	employments, err := s.listAllEmploymentsContext(ctx, tx, EmploymentFilter{
		PersonID: personID, CurrentOnly: true,
	})
	if err != nil {
		return nil, fmt.Errorf("load current employments for semantic projection: %w", err)
	}
	result := make([]personSemanticEmployment, 0, len(employments))
	for _, employment := range employments {
		organization, err := getOrganizationTx(ctx, tx, employment.OrganizationID)
		if err != nil {
			return nil, fmt.Errorf(
				"load employment organization %d for semantic projection: %w",
				employment.OrganizationID, err,
			)
		}
		item := personSemanticEmployment{
			employment: employment, organization: *organization,
		}
		if item.organizationNames, err = queryOrganizationRows(
			ctx, tx, organizationNameSelect, organization.ID, false, scanOrganizationName,
		); err != nil {
			return nil, fmt.Errorf("load current organization names for semantic projection: %w", err)
		}
		if item.organizationCategories, err = queryOrganizationRows(
			ctx, tx, organizationCategorySelect, organization.ID, false, scanOrganizationCategory,
		); err != nil {
			return nil, fmt.Errorf("load current organization categories for semantic projection: %w", err)
		}
		if item.organizationAddresses, err = queryOrganizationRows(
			ctx, tx, organizationAddressSelect, organization.ID, false, scanOrganizationAddress,
		); err != nil {
			return nil, fmt.Errorf("load current organization locations for semantic projection: %w", err)
		}
		result = append(result, item)
	}
	return result, nil
}

func renderPersonSemanticDocument(
	snapshot *personSemanticSnapshot,
) (*PersonSemanticDocument, error) {
	sections := make([][]string, 10)
	add := func(section int, label, value string) {
		value = normalizePersonSemanticText(value)
		if value != "" {
			sections[section] = append(sections[section], label+": "+value)
		}
	}

	if snapshot.person.DisplayName != nil {
		add(0, "Display name", *snapshot.person.DisplayName)
	}
	displayName := normalizePersonSemanticText(derefString(snapshot.person.DisplayName))
	for _, name := range snapshot.names {
		value := renderPersonSemanticName(name)
		if value != "" && value != displayName {
			add(1, "Alternate name", value)
		}
	}
	for _, category := range snapshot.categories {
		add(2, "Category", category.OriginalValue)
	}
	for _, address := range snapshot.addresses {
		add(3, "Location", renderPersonSemanticLocation(
			address.Locality, address.Region, address.CountryName, address.CountryCode,
		))
	}
	for _, attribute := range snapshot.attributes {
		value, err := renderPersonSemanticAttribute(attribute.value.Value)
		if err != nil {
			return nil, fmt.Errorf(
				"render person %d attribute %s: %w",
				snapshot.person.ID, attribute.definition.Slug, err,
			)
		}
		add(4, fmt.Sprintf("Attribute %s [%s]",
			attribute.definition.Label, attribute.definition.Slug), value)
	}
	for _, employment := range snapshot.employments {
		add(5, "Current employment", renderPersonSemanticEmployment(employment))
		add(6, "Organization", employment.organization.Name)
		for _, name := range employment.organizationNames {
			add(7, "Organization alternate name", name.Name)
		}
		for _, category := range employment.organizationCategories {
			add(7, "Organization category", category.Category)
		}
		if employment.organization.Description != nil {
			add(7, "Organization description", *employment.organization.Description)
		}
		if employment.organization.PrimaryDomain != nil {
			add(7, "Organization domain", *employment.organization.PrimaryDomain)
		}
		add(7, "Organization kind", string(employment.organization.Kind))
		for _, address := range employment.organizationAddresses {
			add(8, "Organization location", renderPersonSemanticLocation(
				address.Locality, address.Region, address.CountryName, address.CountryCode,
			))
		}
	}
	for _, relationship := range snapshot.relationships {
		value := relationship.CounterpartLabel
		if relationship.CounterpartDisplayName != nil {
			counterpart := normalizePersonSemanticText(*relationship.CounterpartDisplayName)
			if counterpart != "" {
				value += " — " + counterpart
			}
		}
		add(9, "Relationship", value)
	}

	lines := make([]string, 0)
	seen := make(map[string]struct{})
	for _, section := range sections {
		slices.Sort(section)
		for _, line := range section {
			if _, exists := seen[line]; exists {
				continue
			}
			seen[line] = struct{}{}
			lines = append(lines, line)
		}
	}
	text := capPersonSemanticUTF8(strings.Join(lines, "\n"))
	digest := sha256.Sum256([]byte(PersonSemanticRendererPolicy + "\x00" + text))
	return &PersonSemanticDocument{
		PersonID:       snapshot.person.ID,
		RendererPolicy: PersonSemanticRendererPolicy,
		Revision:       hex.EncodeToString(digest[:]),
		Text:           text,
	}, nil
}

func renderPersonSemanticName(name PersonName) string {
	if formatted := normalizePersonSemanticText(derefString(name.Formatted)); formatted != "" {
		return formatted
	}
	parts := make([]string, 0, 7)
	for _, value := range []*string{
		name.HonorificPrefixes, name.GivenName, name.AdditionalNames,
		name.FamilyName, name.SecondarySurname, name.HonorificSuffixes,
		name.Generation,
	} {
		if normalized := normalizePersonSemanticText(derefString(value)); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	if sortAs := normalizePersonSemanticText(derefString(name.SortAs)); sortAs != "" {
		return sortAs
	}
	return normalizePersonSemanticText(name.OriginalValue)
}

func renderPersonSemanticLocation(
	locality, region, countryName, countryCode *string,
) string {
	parts := make([]string, 0, 3)
	for _, value := range []*string{locality, region} {
		if normalized := normalizePersonSemanticText(derefString(value)); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	country := normalizePersonSemanticText(derefString(countryName))
	if country == "" {
		country = normalizePersonSemanticText(derefString(countryCode))
	}
	if country != "" {
		parts = append(parts, country)
	}
	return strings.Join(parts, ", ")
}

func renderPersonSemanticEmployment(item personSemanticEmployment) string {
	parts := make([]string, 0, 5)
	for _, value := range []*string{
		item.employment.Title, item.employment.Role, item.employment.Department,
		item.employment.Location, item.employment.Description,
	} {
		if normalized := normalizePersonSemanticText(derefString(value)); normalized != "" {
			parts = append(parts, normalized)
		}
	}
	organization := normalizePersonSemanticText(item.organization.Name)
	if len(parts) == 0 {
		return organization
	}
	if organization == "" {
		return strings.Join(parts, "; ")
	}
	return strings.Join(parts, "; ") + " at " + organization
}

func renderPersonSemanticAttribute(value AttributeValue) (string, error) {
	switch value.Type {
	case AttributeValueJSON:
		return canonicalPersonSemanticJSON(value.JSON)
	case AttributeValueRecordReference:
		if value.RecordType == nil || value.RecordID == nil {
			return "", errors.New("incomplete record reference")
		}
		return fmt.Sprintf("%s:%d", normalizePersonSemanticText(*value.RecordType), *value.RecordID), nil
	case AttributeValueDate, AttributeValueTimestamp:
		return "", nil
	default:
		canonical, err := value.CanonicalString()
		if err != nil {
			return "", err
		}
		return normalizePersonSemanticText(canonical), nil
	}
}

func canonicalPersonSemanticJSON(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		if err == nil {
			return "", errors.New("multiple JSON values")
		}
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func normalizePersonSemanticText(value string) string {
	return strings.Join(strings.Fields(norm.NFC.String(value)), " ")
}

func capPersonSemanticUTF8(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	if len(value) <= MaxPersonSemanticDocumentBytes {
		return value
	}
	end := MaxPersonSemanticDocumentBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}
