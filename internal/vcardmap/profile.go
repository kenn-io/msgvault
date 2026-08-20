package vcardmap

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

// projectedOwner names one typed row field that a vCard occurrence stands for.
type projectedOwner struct {
	Table string
	RowID int64
	Field string
}

// ownerField is one (table, field) pair the projection may own. Every field
// it manages is listed in managedOwnerFields, which is what decides whether a
// stale mapping's occurrence is retired from the card.
type ownerField struct {
	table string
	field string
}

func (f ownerField) row(id int64) projectedOwner {
	return projectedOwner{Table: f.table, RowID: id, Field: f.field}
}

// retainedOwner is an owner whose occurrence stays on the card as residue
// even though the row currently renders nothing. A row that records the
// resource it was imported from retains its occurrence only in that envelope:
// in every other envelope an occurrence it owns was projector-generated, and
// leaves the card when the row stops rendering. A row with no recorded
// resource is retained everywhere.
type retainedOwner struct {
	Owner             projectedOwner
	SourceRef         *string
	SourceResourceUID *string
}

func retainInResource(owner projectedOwner, envelope store.ValueEnvelope) retainedOwner {
	return retainedOwner{
		Owner:             owner,
		SourceRef:         envelope.SourceRef,
		SourceResourceUID: envelope.SourceResourceUID,
	}
}

func (r retainedOwner) appliesToResource(sourceRef, sourceResourceUID string) bool {
	if r.SourceRef != nil && *r.SourceRef != sourceRef {
		return false
	}
	if r.SourceResourceUID != nil && *r.SourceResourceUID != sourceResourceUID {
		return false
	}
	return true
}

const (
	// fieldValue is the owner field name of single-value projections.
	fieldValue = "value"

	// fieldRelated is the owner field name of relationship and review projections.
	fieldRelated = "related"

	tablePersonNames = "person_names"
	propertyEmail    = "EMAIL"
)

var (
	ownerDisplayName     = ownerField{"persons", "display_name"}
	ownerNameFormatted   = ownerField{tablePersonNames, "formatted"}
	ownerNameStructured  = ownerField{tablePersonNames, "structured"}
	ownerNamePhonetic    = ownerField{tablePersonNames, "phonetic"}
	ownerNameNickname    = ownerField{tablePersonNames, "nickname"}
	ownerNameDerivedFN   = ownerField{tablePersonNames, "derived_fn"}
	ownerContactPoint    = ownerField{"person_contact_points", "original_value"}
	ownerAddress         = ownerField{"person_addresses", fieldValue}
	ownerDate            = ownerField{"person_dates", "date"}
	ownerCategory        = ownerField{"person_categories", fieldValue}
	ownerMedia           = ownerField{"person_media", fieldValue}
	ownerAttributeValue  = ownerField{"person_attribute_values", fieldValue}
	ownerEmploymentOrg   = ownerField{"employments", "organization_id"}
	ownerEmploymentTitle = ownerField{"employments", "title"}
	ownerEmploymentRole  = ownerField{"employments", "role"}
	ownerRelationship    = ownerField{"person_relationships", fieldRelated}
	ownerReview          = ownerField{"person_relationship_reviews", fieldRelated}

	managedOwnerFields = map[ownerField]struct{}{
		ownerDisplayName: {}, ownerNameFormatted: {}, ownerNameStructured: {},
		ownerNamePhonetic: {}, ownerNameNickname: {}, ownerNameDerivedFN: {},
		ownerContactPoint: {}, ownerAddress: {}, ownerDate: {}, ownerCategory: {},
		ownerMedia: {}, ownerAttributeValue: {}, ownerEmploymentOrg: {},
		ownerEmploymentTitle: {}, ownerEmploymentRole: {}, ownerRelationship: {},
		ownerReview: {},
	}
)

func managedProjectionField(table, field string) bool {
	_, managed := managedOwnerFields[ownerField{table, field}]
	return managed
}

// Parameters each projection renders from typed fields. A parameter outside
// these sets belongs to the imported card and stays untouched as residue.
var (
	nameOwnedParameters    = []string{"LANGUAGE", "SCRIPT", "PHONETIC", "SORT-AS"}
	addressOwnedParameters = []string{"LABEL", "GEO", "TZ", "CC"}
	dateOwnedParameters    = []string{"VALUE", "CALSCALE"}
	mediaOwnedParameters   = []string{"MEDIATYPE"}
	valueOwnedParameters   = []string{"VALUE"}
)

// scopedIdentity is a vCard identity together with the complete identity of
// the resource the row was read from. A vCard identity names an occurrence on
// one card, so the fallback applies to that resource only (or, when the row
// records no resource, to any). Without both parts, a row imported from one
// card could claim an unrelated occurrence from the same address book that
// happens to share the property, group, and IDs.
type scopedIdentity struct {
	identity          store.VCardIdentity
	sourceRef         *string
	sourceResourceUID *string
}

func envelopeIdentity(envelope store.ValueEnvelope) scopedIdentity {
	return scopedIdentity{
		identity: envelope.VCard, sourceRef: envelope.SourceRef,
		sourceResourceUID: envelope.SourceResourceUID,
	}
}

// projectedProperty is one typed owner and the property it wants to render.
// Identity is a fallback only; a durable NativeMapping wins when present.
type projectedProperty struct {
	Owner    projectedOwner
	Identity scopedIdentity
	// ReviewIdentities are further scoped fallbacks a relationship inherits
	// from the accepted reviews it satisfied: each names the occurrence one
	// card's review stood for, in that card's resource.
	ReviewIdentities []scopedIdentity
	// OwnedParameters names the parameters this owner represents as typed
	// fields, so an imported parameter disappears from the envelope once the
	// field behind it is cleared.
	OwnedParameters []string
	// CarriedParameters names parameters the vCard layer regenerates for this
	// property on every edit but that this owner has no typed field for. They
	// are copied from the occurrence the owner binds to, so an imported value
	// survives a re-render instead of being dropped.
	CarriedParameters []string
	// FullNameFallback marks the derived FN. It is rendered only when the card
	// would otherwise carry no FN, which is decided where the tree is visible.
	FullNameFallback bool
	Property         vcard.Property
}

func ownedProjection(
	field ownerField, envelope store.ValueEnvelope, owned []string, property vcard.Property,
) projectedProperty {
	return projectedProperty{
		Owner: field.row(envelope.ID), Identity: envelopeIdentity(envelope),
		OwnedParameters: owned, Property: property,
	}
}

// projectionSection renders one kind of snapshot row: the properties it
// wants on the card and the owners whose rows have nothing to render but
// whose imported occurrences must stay as residue.
type projectionSection func(
	store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error)

var projectionSections = []projectionSection{
	projectNames, projectContactPoints, projectAddresses, projectDates,
	projectCategories, projectMediaItems, projectEmployments, projectRelationships,
	projectAttributes,
}

// projectPersonProperties converts the currently supported semantic snapshot
// rows into deterministic desired properties and retained owners. The complete
// mapping table is extended in the structured mapping layer; portable scalar
// attributes are handled here because their property name is data-driven.
func projectPersonProperties(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	properties := make([]projectedProperty, 0)
	retained := make([]retainedOwner, 0)
	for _, section := range projectionSections {
		sectionProperties, sectionRetained, err := section(snapshot)
		if err != nil {
			return nil, nil, err
		}
		properties = append(properties, sectionProperties...)
		retained = append(retained, sectionRetained...)
	}
	if !hasProjectedProperty(properties, "FN") {
		derived, ok, err := derivedFullName(snapshot.Profile)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, derived)
		}
	}
	for _, review := range snapshot.PendingRelationshipReviews {
		retained = append(retained, retainedOwner{
			Owner: ownerReview.row(review.ID), SourceRef: review.SourceRef,
			SourceResourceUID: review.SourceResourceUID,
		})
	}
	return properties, retained, nil
}

func projectNames(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	names := snapshot.Profile.Names
	properties := make([]projectedProperty, 0, len(names))
	retained := make([]retainedOwner, 0)
	for _, name := range names {
		projected, ok, err := projectPersonName(name)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, projected)
			continue
		}
		// A current row that renders nothing — a structured name carrying
		// only its wire-form original, a blank nickname — still owns the
		// occurrence it was read from; that stays as residue, as for the
		// other components.
		if owner, known := personNameOwner(name.NameKind); known {
			retained = append(retained, retainInResource(owner.row(name.Envelope.ID), name.Envelope))
		}
	}
	return properties, retained, nil
}

// personNameOwner is the owner field a name row of the given kind renders
// through.
func personNameOwner(kind store.PersonNameKind) (ownerField, bool) {
	switch kind {
	case store.PersonNameFormatted:
		return ownerNameFormatted, true
	case store.PersonNameStructured:
		return ownerNameStructured, true
	case store.PersonNamePhonetic:
		return ownerNamePhonetic, true
	case store.PersonNameNickname:
		return ownerNameNickname, true
	default:
		return ownerField{}, false
	}
}

func projectPersonName(name store.PersonName) (projectedProperty, bool, error) {
	owner, propertyName, rawValue, ok := personNameValue(name)
	if !ok {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty(propertyName, rawValue, name.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	if err := appendNameParameters(&property, name); err != nil {
		return projectedProperty{}, false, err
	}
	return ownedProjection(owner, name.Envelope, nameOwnedParameters, property), true, nil
}

// personNameValue picks the property, owner field, and raw value one name row
// renders.
func personNameValue(name store.PersonName) (ownerField, string, string, bool) {
	owner, known := personNameOwner(name.NameKind)
	if !known {
		return ownerField{}, "", "", false
	}
	switch name.NameKind {
	case store.PersonNameFormatted:
		if isBlank(name.Formatted) {
			return ownerField{}, "", "", false
		}
		return owner, "FN", vcard.EscapeText(*name.Formatted), true
	case store.PersonNameStructured, store.PersonNamePhonetic:
		components := structuredNameComponents(name)
		if allBlank(components) {
			return ownerField{}, "", "", false
		}
		return owner, "N", vcard.JoinStructuredText(components), true
	default:
		value := nicknameValue(name)
		if strings.TrimSpace(value) == "" {
			return ownerField{}, "", "", false
		}
		return owner, "NICKNAME", vcard.EscapeText(value), true
	}
}

// nicknameValue is the row's formatted nickname, else its original value. A
// NICKNAME row is one nickname: the text is escaped as a single list value,
// so a comma inside it is a literal comma. An import mapper that splits a
// comma-separated NICKNAME must store one row per value.
func nicknameValue(name store.PersonName) string {
	if !isBlank(name.Formatted) {
		return *name.Formatted
	}
	return name.OriginalValue
}

func structuredNameComponents(name store.PersonName) []string {
	return []string{
		deref(name.FamilyName), deref(name.GivenName), deref(name.AdditionalNames),
		deref(name.HonorificPrefixes), deref(name.HonorificSuffixes),
		deref(name.SecondarySurname), deref(name.Generation),
	}
}

// appendNameParameters renders the name's typed parameters. A phonetic N
// carries one SCRIPT: the phonetic script when recorded, else the name script.
func appendNameParameters(property *vcard.Property, name store.PersonName) error {
	script := name.Script
	if name.NameKind == store.PersonNamePhonetic && !isBlank(name.PhoneticScript) {
		script = name.PhoneticScript
	}
	for _, parameter := range []struct {
		name  string
		value *string
	}{
		{"LANGUAGE", name.Language}, {"SCRIPT", script}, {"PHONETIC", name.PhoneticSystem},
	} {
		if err := appendOptionalParameter(property, parameter.name, parameter.value); err != nil {
			return err
		}
	}
	if isBlank(name.SortAs) {
		return nil
	}
	values := strings.Split(*name.SortAs, ",")
	for index := range values {
		values[index] = strings.TrimSpace(values[index])
	}
	parameter, err := vcard.NewParameter("SORT-AS", values...)
	if err != nil {
		return err
	}
	property.Parameters = append(property.Parameters, parameter)
	return nil
}

// derivedFullName synthesizes FN when no name row projects one. It reports
// false when the profile offers nothing to derive from. The result is only a
// fallback: the merge drops it again when the card already carries an FN of
// its own, and a card that truly lacks one is rejected by rendering, where the
// mandatory-FN rule belongs. Components are ordered as the render layer
// orders the five vCard 3 components (prefix, given, additional, family,
// suffix), with the RFC 9554 secondary surname and generation after family.
func derivedFullName(profile store.PersonProfile) (projectedProperty, bool, error) {
	for _, name := range profile.Names {
		if name.NameKind != store.PersonNameStructured {
			continue
		}
		parts := []string{
			deref(name.HonorificPrefixes), deref(name.GivenName),
			deref(name.AdditionalNames), deref(name.FamilyName),
			deref(name.SecondarySurname), deref(name.Generation),
			deref(name.HonorificSuffixes),
		}
		value := strings.Join(nonBlank(parts), " ")
		if value == "" {
			continue
		}
		return derivedFullNameProperty(ownerNameDerivedFN.row(name.Envelope.ID), value)
	}
	if isBlank(profile.Person.DisplayName) {
		return projectedProperty{}, false, nil
	}
	return derivedFullNameProperty(
		ownerDisplayName.row(profile.Person.ID), *profile.Person.DisplayName,
	)
}

func derivedFullNameProperty(owner projectedOwner, value string) (projectedProperty, bool, error) {
	property, err := vcard.NewProperty("", "FN", vcard.EscapeText(value))
	if err != nil {
		return projectedProperty{}, false, err
	}
	derived, err := vcard.NewParameter("DERIVED", "true")
	if err != nil {
		return projectedProperty{}, false, err
	}
	property.Parameters = append(property.Parameters, derived)
	return projectedProperty{Owner: owner, FullNameFallback: true, Property: property}, true, nil
}

// contactForm says which vCard value forms a contact-point property accepts.
type contactForm int

const (
	// contactText is a text-only property (RFC 6350 EMAIL, LANG): the value
	// is always escaped text and never carries a VALUE parameter.
	contactText contactForm = iota
	// contactURI is a URI-only property: a value that is not an absolute URI
	// cannot be rendered and the existing occurrence is kept as residue.
	contactURI
	// contactURIOrText renders an absolute URI as such and anything else as
	// text labelled VALUE=text; the projection therefore owns VALUE.
	contactURIOrText
	// contactTelephone synthesizes a tel: URI from the normalized number when
	// the value is not already a URI.
	contactTelephone
)

type contactProjection struct {
	property    string
	form        contactForm
	serviceType bool
}

var contactProjections = map[store.ContactAddressKind]contactProjection{
	store.ContactAddressEmail:    {property: propertyEmail, form: contactText},
	store.ContactAddressPhone:    {property: "TEL", form: contactTelephone},
	store.ContactAddressUsername: {property: "IMPP", form: contactURI},
	store.ContactAddressIMPP:     {property: "IMPP", form: contactURI},
	store.ContactAddressURL:      {property: "URL", form: contactURI},
	store.ContactAddressSocial: {
		property: "SOCIALPROFILE", form: contactURIOrText, serviceType: true,
	},
	store.ContactAddressCalendar:     {property: "CALURI", form: contactURI},
	store.ContactAddressContactURI:   {property: "CONTACT-URI", form: contactURI},
	store.ContactAddressOrgDirectory: {property: "ORG-DIRECTORY", form: contactURI},
	store.ContactAddressLanguage:     {property: "LANG", form: contactText},
}

func projectContactPoints(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	points := snapshot.Profile.ContactPoints
	properties := make([]projectedProperty, 0, len(points))
	retained := make([]retainedOwner, 0)
	for _, point := range points {
		projected, ok, err := projectContactPoint(point)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, projected)
		} else {
			retained = append(retained,
				retainInResource(ownerContactPoint.row(point.Envelope.ID), point.Envelope))
		}
	}
	return properties, retained, nil
}

func projectContactPoint(point store.PersonContactPoint) (projectedProperty, bool, error) {
	spec, ok := contactProjections[point.AddressKind]
	if !ok {
		return projectedProperty{}, false, nil
	}
	value, textValue, ok := contactPointValue(point, spec.form)
	if !ok {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty(spec.property, value, point.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	if err := appendContactParameters(&property, spec, point, textValue); err != nil {
		return projectedProperty{}, false, err
	}
	projected := ownedProjection(ownerContactPoint, point.Envelope, valueOwnedParameters, property)
	return projected, true, nil
}

// contactPointValue returns the wire value of a contact point, whether it is
// rendered in the text form, and whether there is anything to render at all.
func contactPointValue(point store.PersonContactPoint, form contactForm) (string, bool, bool) {
	value := rawContactValue(point, form)
	if value == "" {
		return "", false, false
	}
	isURI := vcard.IsURIValue(value)
	if form == contactURI && !isURI {
		return "", false, false
	}
	if form == contactText || (form == contactURIOrText && !isURI) {
		return vcard.EscapeText(value), true, true
	}
	return value, false, true
}

// rawContactValue prefers the row's URI over its original text and, for a
// telephone that is not already a URI, synthesizes tel: from the normalized
// number.
func rawContactValue(point store.PersonContactPoint, form contactForm) string {
	value := strings.TrimSpace(point.OriginalValue)
	if !isBlank(point.URI) {
		value = strings.TrimSpace(*point.URI)
	}
	if form != contactTelephone || value == "" || vcard.IsURIValue(value) {
		return value
	}
	phone := strings.TrimSpace(point.NormalizedValue)
	if phone == "" {
		phone = strings.ReplaceAll(value, " ", "")
	}
	return "tel:" + phone
}

// appendContactParameters sets SERVICE-TYPE when the service is known (an
// unknown service leaves an imported SERVICE-TYPE alone so it survives) and
// labels the text form of a URI-or-text property with VALUE=text.
func appendContactParameters(
	property *vcard.Property, spec contactProjection, point store.PersonContactPoint,
	textValue bool,
) error {
	if spec.serviceType {
		if err := appendOptionalParameter(property, "SERVICE-TYPE", point.ServiceSlug); err != nil {
			return err
		}
	}
	if spec.form != contactURIOrText || !textValue {
		return nil
	}
	return appendOptionalParameter(property, "VALUE", new("text"))
}

func projectAddresses(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	addresses := snapshot.Profile.Addresses
	properties := make([]projectedProperty, 0, len(addresses))
	retained := make([]retainedOwner, 0)
	for _, address := range addresses {
		projected, ok, err := projectAddress(address)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, projected)
		} else {
			retained = append(retained,
				retainInResource(ownerAddress.row(address.Envelope.ID), address.Envelope))
		}
	}
	return properties, retained, nil
}

func projectAddress(address store.PersonAddress) (projectedProperty, bool, error) {
	if address.AddressKind == store.PersonAddressPostal {
		return projectPostalAddress(address)
	}
	propertyName := map[store.PersonAddressKind]string{
		store.PersonAddressBirthPlace: "BIRTHPLACE", store.PersonAddressDeathPlace: "DEATHPLACE",
	}[address.AddressKind]
	if propertyName == "" {
		return projectedProperty{}, false, nil
	}
	raw, placeURI := addressPlaceValue(address)
	if strings.TrimSpace(raw) == "" {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty(propertyName, raw, address.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	valueType := "text"
	if placeURI {
		valueType = "uri"
	}
	if err := appendOptionalParameter(&property, "VALUE", &valueType); err != nil {
		return projectedProperty{}, false, err
	}
	return ownedProjection(ownerAddress, address.Envelope, valueOwnedParameters, property), true, nil
}

func projectPostalAddress(address store.PersonAddress) (projectedProperty, bool, error) {
	raw, label, err := postalAddressValue(address)
	if err != nil {
		return projectedProperty{}, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty("ADR", raw, address.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	for _, parameter := range []struct {
		name  string
		value *string
	}{
		{"LABEL", label}, {"GEO", address.GeoURI}, {"TZ", address.Timezone},
		{"CC", address.CountryCode},
	} {
		if err := appendOptionalParameter(&property, parameter.name, parameter.value); err != nil {
			return projectedProperty{}, false, err
		}
	}
	return ownedProjection(ownerAddress, address.Envelope, addressOwnedParameters, property), true, nil
}

// postalAddressValue returns the ADR structured value and the LABEL text for
// a postal address. Structured components win: the seven typed fields render
// components 0-6, and ExtendedComponents contributes only the components past
// them (RFC 9554 adds room, apartment, floor, and so on from index 7). A row
// that has no components carries its address either as a structured original
// value (an imported ADR kept verbatim) or as free text, which vCard 4 puts in
// LABEL over empty components (RFC 6350 section 6.3.1). An empty raw value
// means the row has nothing to render and its existing occurrence, if any, is
// left as residue.
func postalAddressValue(address store.PersonAddress) (string, *string, error) {
	components, err := postalComponents(address)
	if err != nil {
		return "", nil, err
	}
	if !allBlank(components) {
		return vcard.JoinStructuredText(components), address.Label, nil
	}
	// SplitStructuredText decodes each component, so re-joining escapes
	// each exactly once and a wire-form original re-renders unchanged.
	original := strings.TrimSpace(address.OriginalValue)
	parsed, err := vcard.SplitStructuredText(original)
	if err == nil && len(parsed) > 1 && !allBlank(parsed) {
		return vcard.JoinStructuredText(parsed), address.Label, nil
	}
	for _, text := range []*string{address.Label, address.FreeText, &original} {
		if !isBlank(text) {
			return vcard.JoinStructuredText(components), text, nil
		}
	}
	return "", nil, nil
}

// postalComponents renders the seven typed ADR fields followed by any
// components past them that ExtendedComponents carries.
func postalComponents(address store.PersonAddress) ([]string, error) {
	components := []string{
		deref(address.PostOfficeBox), deref(address.ExtendedAddress),
		deref(address.StreetAddress), deref(address.Locality), deref(address.Region),
		deref(address.PostalCode), deref(address.CountryName),
	}
	if isBlank(address.ExtendedComponents) {
		return components, nil
	}
	parsed, err := vcard.SplitStructuredText(*address.ExtendedComponents)
	if err != nil {
		return nil, fmt.Errorf("decode extended ADR components: %w", err)
	}
	if len(parsed) > len(components) {
		components = append(components, parsed[len(components):]...)
	}
	return components, nil
}

// addressPlaceValue picks the BIRTHPLACE/DEATHPLACE value and reports whether
// it is a URI. A place URI that is not an absolute URI cannot carry VALUE=uri,
// so it falls back to the text form: the free text or label when present,
// otherwise the string itself as text.
func addressPlaceValue(address store.PersonAddress) (string, bool) {
	placeURI := ""
	if address.PlaceURI != nil {
		placeURI = strings.TrimSpace(*address.PlaceURI)
	}
	if placeURI != "" && vcard.IsURIValue(placeURI) {
		return placeURI, true
	}
	for _, value := range []*string{address.FreeText, address.Label} {
		if !isBlank(value) {
			return vcard.EscapeText(*value), false
		}
	}
	if placeURI != "" {
		return vcard.EscapeText(placeURI), false
	}
	return vcard.EscapeText(address.OriginalValue), false
}

func projectDates(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	dates := snapshot.Profile.Dates
	properties := make([]projectedProperty, 0, len(dates))
	retained := make([]retainedOwner, 0)
	for _, date := range dates {
		projected, ok, err := projectDate(date)
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, projected)
		} else {
			retained = append(retained, retainInResource(ownerDate.row(date.Envelope.ID), date.Envelope))
		}
	}
	return properties, retained, nil
}

func projectDate(date store.PersonDate) (projectedProperty, bool, error) {
	propertyName := map[store.PersonDateKind]string{
		store.PersonDateBirthday: "BDAY", store.PersonDateAnniversary: "ANNIVERSARY",
		store.PersonDateDeath: "DEATHDATE",
	}[date.DateKind]
	if propertyName == "" {
		return projectedProperty{}, false, nil
	}
	value := vcardPartialDate(date.Date)
	valueType := "date"
	if !isBlank(date.DateText) {
		value = vcard.EscapeText(*date.DateText)
		valueType = "text"
	}
	if value == "" {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty(propertyName, value, date.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	if err := appendOptionalParameter(&property, "VALUE", &valueType); err != nil {
		return projectedProperty{}, false, err
	}
	if err := appendOptionalParameter(&property, "CALSCALE", date.CalendarScale); err != nil {
		return projectedProperty{}, false, err
	}
	return ownedProjection(ownerDate, date.Envelope, dateOwnedParameters, property), true, nil
}

func vcardPartialDate(date store.PartialDate) string {
	switch {
	case date.Year != nil && date.Month != nil && date.Day != nil:
		return fmt.Sprintf("%04d%02d%02d", *date.Year, *date.Month, *date.Day)
	case date.Year != nil && date.Month != nil:
		return fmt.Sprintf("%04d-%02d", *date.Year, *date.Month)
	case date.Year != nil:
		return fmt.Sprintf("%04d", *date.Year)
	case date.Month != nil && date.Day != nil:
		return fmt.Sprintf("--%02d%02d", *date.Month, *date.Day)
	case date.Month != nil:
		return fmt.Sprintf("--%02d", *date.Month)
	case date.Day != nil:
		return fmt.Sprintf("---%02d", *date.Day)
	default:
		return ""
	}
}

// vcardDateTime renders an RFC 6350 UTC timestamp, which has no fractional
// seconds; sub-second precision is truncated.
func vcardDateTime(value time.Time) string {
	return value.UTC().Format("20060102T150405Z")
}

// projectCategories renders one CATEGORIES occurrence per row. A row is one
// category: its text is escaped as a single list value, so a comma inside it
// is a literal comma. An import mapper that splits a comma-separated
// CATEGORIES must store one row per value.
func projectCategories(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	categories := snapshot.Profile.Categories
	properties := make([]projectedProperty, 0, len(categories))
	for _, category := range categories {
		property, err := newOwnedProperty(
			"CATEGORIES", vcard.EscapeText(category.OriginalValue), category.Envelope,
		)
		if err != nil {
			return nil, nil, err
		}
		properties = append(properties, ownedProjection(ownerCategory, category.Envelope, nil, property))
	}
	return properties, nil, nil
}

func projectMediaItems(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	inline := make(map[int64]store.PersonVCardMediaData, len(snapshot.MediaData))
	for _, data := range snapshot.MediaData {
		inline[data.MediaID] = data
	}
	properties := make([]projectedProperty, 0, len(snapshot.Profile.Media))
	retained := make([]retainedOwner, 0)
	for _, item := range snapshot.Profile.Media {
		projected, ok, err := projectMedia(item, inline[item.Envelope.ID])
		if err != nil {
			return nil, nil, err
		}
		if ok {
			properties = append(properties, projected)
		} else {
			retained = append(retained, retainInResource(ownerMedia.row(item.Envelope.ID), item.Envelope))
		}
	}
	return properties, retained, nil
}

func projectMedia(
	media store.PersonMedia, inline store.PersonVCardMediaData,
) (projectedProperty, bool, error) {
	propertyName := map[store.PersonMediaKind]string{
		store.PersonMediaPhoto: "PHOTO", store.PersonMediaLogo: "LOGO",
		store.PersonMediaSound: "SOUND", store.PersonMediaKey: "KEY",
	}[media.MediaKind]
	value := mediaValue(media, inline)
	if propertyName == "" || value == "" {
		return projectedProperty{}, false, nil
	}
	property, err := newOwnedProperty(propertyName, value, media.Envelope)
	if err != nil {
		return projectedProperty{}, false, err
	}
	if err := appendOptionalParameter(&property, "MEDIATYPE", media.MediaType); err != nil {
		return projectedProperty{}, false, err
	}
	return ownedProjection(ownerMedia, media.Envelope, mediaOwnedParameters, property), true, nil
}

// mediaValue renders inline data as a data: URI, else the row's URI. Media
// properties have no text form in vCard 4, so a reference that is not an
// absolute URI cannot be rendered: "" keeps any imported occurrence as
// residue.
func mediaValue(media store.PersonMedia, inline store.PersonVCardMediaData) string {
	if len(inline.Data) > 0 {
		mediaType := inline.MediaType
		if mediaType == "" {
			mediaType = deref(media.MediaType)
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(inline.Data)
	}
	value := strings.TrimSpace(deref(media.URI))
	if !vcard.IsURIValue(value) {
		return ""
	}
	return value
}

// employmentCarriedParameters are regenerated by the vCard layer for the
// derived ORG, TITLE, and ROLE properties, but an employment has no typed
// field for them, so an imported value is carried across the re-render.
var employmentCarriedParameters = []string{"TYPE", "PREF"}

// projectEmployments renders the primary current employment as ORG, TITLE,
// and ROLE through the same rendering the employment API exposes. Every other
// employment, and any of the three values the primary one lacks, keeps its
// imported occurrence as residue.
func projectEmployments(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	properties := make([]projectedProperty, 0, 3)
	retained := make([]retainedOwner, 0)
	for _, item := range snapshot.Employments {
		employment := Employment{
			OrganizationName: item.Organization.Organization.Name,
			Department:       deref(item.Employment.Department),
			Title:            deref(item.Employment.Title),
			Role:             deref(item.Employment.Role),
		}
		values := []struct {
			name  string
			owner ownerField
			raw   string
		}{
			{"ORG", ownerEmploymentOrg, vcard.JoinStructuredText(OrgComponents(employment))},
			{"TITLE", ownerEmploymentTitle, vcard.EscapeText(Title(employment))},
			{"ROLE", ownerEmploymentRole, vcard.EscapeText(Role(employment))},
		}
		primary := item.Employment.IsCurrent && item.Employment.IsPrimary
		// Only an imported employment's occurrences are card data in their
		// own right: when the row stops rendering, they stay as residue —
		// and only in the book the row was imported from (employment rows
		// record no per-card resource). A row from any other provenance only
		// ever had projector-generated occurrences, and those leave every
		// card with the primacy — retaining them would accumulate the lines
		// of every former primary employment.
		imported := item.Employment.Source == store.ProvenanceVCardImport ||
			item.Employment.Source == store.ProvenanceCardDAVImport
		for _, value := range values {
			owner := value.owner.row(item.Employment.ID)
			if !primary || value.raw == "" {
				if imported {
					retained = append(retained, retainedOwner{
						Owner: owner, SourceRef: item.Employment.SourceRef,
					})
				}
				continue
			}
			property, err := vcard.NewProperty("", value.name, value.raw)
			if err != nil {
				return nil, nil, err
			}
			properties = append(properties, projectedProperty{
				Owner: owner, CarriedParameters: employmentCarriedParameters, Property: property,
			})
		}
	}
	return properties, retained, nil
}

// relatedCarriedParameters: RELATED is a relationship property, so the vCard
// layer regenerates TYPE and PREF on every edit. A relationship row has one
// relationship type and no preference, so PREF is carried across from the
// occurrence. Further TYPE tokens an imported RELATED carried beyond the
// row's own type have no home in the store and are not re-rendered.
var relatedCarriedParameters = []string{"PREF"}

func projectRelationships(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	types := make(map[int64]store.RelationshipType, len(snapshot.RelationshipTypes))
	for _, relationshipType := range snapshot.RelationshipTypes {
		types[relationshipType.ID] = relationshipType
	}
	reviewIdentities := make(map[int64][]scopedIdentity, len(snapshot.AcceptedRelationshipReviews))
	for _, review := range snapshot.AcceptedRelationshipReviews {
		if review.VCardIdentity.IsZero() {
			continue
		}
		reviewIdentities[review.RelationshipID] = append(reviewIdentities[review.RelationshipID],
			scopedIdentity{identity: review.VCardIdentity, sourceRef: review.SourceRef,
				sourceResourceUID: review.SourceResourceUID})
	}
	properties := make([]projectedProperty, 0, len(snapshot.Relationships))
	retained := make([]retainedOwner, 0)
	for _, view := range snapshot.Relationships {
		relationshipType, ok := types[view.Relationship.RelationshipTypeID]
		if !ok {
			return nil, nil, fmt.Errorf("relationship %d has no snapshot type", view.Relationship.ID)
		}
		wireType := relatedWireType(view, relationshipType, types)
		if wireType == "" {
			// Only an edge imported from card data keeps its occurrence when
			// its type loses the wire mapping, and only on the card it came
			// from; a projector-generated RELATED leaves with the mapping.
			imported := view.Relationship.Source == store.ProvenanceVCardImport ||
				view.Relationship.Source == store.ProvenanceCardDAVImport
			if imported {
				retained = append(retained, retainedOwner{
					Owner:             ownerRelationship.row(view.Relationship.ID),
					SourceRef:         view.Relationship.SourceRef,
					SourceResourceUID: view.Relationship.SourceResourceUID,
				})
			}
			continue
		}
		property, err := relatedProperty(view.Relationship, view.CounterpartVCardUID, wireType)
		if err != nil {
			return nil, nil, err
		}
		properties = append(properties, projectedProperty{
			Owner: ownerRelationship.row(view.Relationship.ID),
			Identity: scopedIdentity{
				identity: view.Relationship.VCardIdentity, sourceRef: view.Relationship.SourceRef,
				sourceResourceUID: view.Relationship.SourceResourceUID,
			},
			ReviewIdentities:  reviewIdentities[view.Relationship.ID],
			OwnedParameters:   valueOwnedParameters,
			CarriedParameters: relatedCarriedParameters,
			Property:          property,
		})
	}
	return properties, retained, nil
}

// relatedWireType picks the RELATED TYPE token for one endpoint's view of an
// edge: the type's own token, or the inverse type's token when the edge is
// read from its target end. It returns "" when no token applies.
func relatedWireType(
	view store.PersonRelationshipView, relationshipType store.RelationshipType,
	types map[int64]store.RelationshipType,
) string {
	wireType := relationshipType.VCardRelatedType
	if view.Direction == store.RelationshipDirectionOutgoing && !relationshipType.IsSymmetric {
		wireType = nil
		if relationshipType.InverseTypeID != nil {
			if inverse, exists := types[*relationshipType.InverseTypeID]; exists {
				wireType = inverse.VCardRelatedType
			}
		}
	}
	if wireType == nil {
		return ""
	}
	return strings.TrimSpace(*wireType)
}

func relatedProperty(
	relationship store.PersonRelationship, counterpartUID, wireType string,
) (vcard.Property, error) {
	value := counterpartUID
	if parsed, err := uuid.Parse(value); err == nil && parsed.String() == strings.ToLower(value) {
		value = "urn:uuid:" + value
	}
	property, err := newOwnedProperty(
		"RELATED", value, store.ValueEnvelope{VCard: relationship.VCardIdentity},
	)
	if err != nil {
		return vcard.Property{}, err
	}
	if err := appendOptionalParameter(&property, "TYPE", &wireType); err != nil {
		return vcard.Property{}, err
	}
	// The projection decides between the URI and text forms of the
	// counterpart reference, so it owns VALUE: an imported VALUE=text must
	// not survive onto a urn:uuid: value.
	if !vcard.IsURIValue(value) {
		if err := appendOptionalParameter(&property, "VALUE", new("text")); err != nil {
			return vcard.Property{}, err
		}
	}
	return property, nil
}

func projectAttributes(
	snapshot store.PersonVCardSnapshot,
) ([]projectedProperty, []retainedOwner, error) {
	properties := make([]projectedProperty, 0)
	retained := make([]retainedOwner, 0)
	for _, attribute := range snapshot.Attributes {
		if attribute.Definition.VCardProperty == nil {
			continue
		}
		for _, value := range attribute.Values {
			property, supported, err := portableAttributeProperty(
				*attribute.Definition.VCardProperty, value.Value,
			)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"project person attribute %q: %w", attribute.Definition.Slug, err,
				)
			}
			if !supported {
				retained = append(retained, retainedOwner{
					Owner: ownerAttributeValue.row(value.ID), SourceRef: value.SourceRef,
				})
				continue
			}
			properties = append(properties, projectedProperty{
				Owner: ownerAttributeValue.row(value.ID), OwnedParameters: valueOwnedParameters,
				Property: property,
			})
		}
	}
	return properties, retained, nil
}

func portableAttributeProperty(
	name string, value store.AttributeValue,
) (vcard.Property, bool, error) {
	if value.Type == store.AttributeValueJSON ||
		value.Type == store.AttributeValueRecordReference ||
		vcard.IsReservedProperty(name) {
		return vcard.Property{}, false, nil
	}
	canonical, err := value.CanonicalString()
	if err != nil {
		return vcard.Property{}, false, err
	}
	raw := canonical
	switch value.Type {
	case store.AttributeValueText:
		raw = vcard.EscapeText(canonical)
	case store.AttributeValueDate:
		raw = strings.ReplaceAll(canonical, "-", "")
	case store.AttributeValueTimestamp:
		raw = vcardDateTime(*value.Timestamp)
	default:
		// Other portable scalar types already use their canonical spelling.
	}
	property, err := vcard.NewProperty("", name, raw)
	if err != nil {
		return vcard.Property{}, false, err
	}
	valueType := map[store.AttributeValueType]string{
		store.AttributeValueInteger:   "integer",
		store.AttributeValueReal:      "float",
		store.AttributeValueBoolean:   "boolean",
		store.AttributeValueDate:      "date",
		store.AttributeValueTimestamp: "date-time",
	}[value.Type]
	if err := appendOptionalParameter(&property, "VALUE", &valueType); err != nil {
		return vcard.Property{}, false, err
	}
	return property, true, nil
}

// newOwnedProperty builds a property from the row's envelope: its group,
// TYPE tokens, PREF, and the identity parameters that name its occurrence.
func newOwnedProperty(
	name, rawValue string, envelope store.ValueEnvelope,
) (vcard.Property, error) {
	group := ""
	if envelope.VCard.Group != nil {
		group = *envelope.VCard.Group
	}
	property, err := vcard.NewProperty(group, name, rawValue)
	if err != nil {
		return vcard.Property{}, err
	}
	for _, token := range envelope.TypeTokens {
		if strings.TrimSpace(token) == "" {
			continue
		}
		parameter, err := vcard.NewParameter("TYPE", token)
		if err != nil {
			return vcard.Property{}, err
		}
		property.Parameters = append(property.Parameters, parameter)
	}
	if envelope.Pref != nil {
		parameter, err := vcard.NewParameter("PREF", strconv.Itoa(*envelope.Pref))
		if err != nil {
			return vcard.Property{}, err
		}
		property.Parameters = append(property.Parameters, parameter)
	}
	return appendIdentityParameters(
		property, envelope.VCard.PropID, envelope.VCard.PID, envelope.VCard.AltID,
	)
}

// appendIdentityParameters adds the PROP-ID, PID, and ALTID parameters that
// name one occurrence, skipping the ones that are unset.
func appendIdentityParameters(
	property vcard.Property, propID *string, pid []string, altID *string,
) (vcard.Property, error) {
	for _, parameter := range []struct {
		name   string
		values []string
	}{
		{"PROP-ID", optionalValues(propID)}, {"PID", pid}, {"ALTID", optionalValues(altID)},
	} {
		if len(parameter.values) == 0 {
			continue
		}
		value, err := vcard.NewParameter(parameter.name, parameter.values...)
		if err != nil {
			return vcard.Property{}, err
		}
		property.Parameters = append(property.Parameters, value)
	}
	return property, nil
}

func appendOptionalParameter(property *vcard.Property, name string, value *string) error {
	if isBlank(value) {
		return nil
	}
	parameter, err := vcard.NewParameter(name, strings.TrimSpace(*value))
	if err != nil {
		return err
	}
	property.Parameters = append(property.Parameters, parameter)
	return nil
}

func optionalValues(value *string) []string {
	if value == nil || *value == "" {
		return nil
	}
	return []string{*value}
}

func hasProjectedProperty(properties []projectedProperty, name string) bool {
	for _, property := range properties {
		if strings.EqualFold(property.Property.Name, name) {
			return true
		}
	}
	return false
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func isBlank(value *string) bool {
	return value == nil || strings.TrimSpace(*value) == ""
}

func allBlank(values []string) bool {
	return len(nonBlank(values)) == 0
}

func nonBlank(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}
