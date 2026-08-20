package vcard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// PropertyIdentity is the identity of one property occurrence in a resource.
//
// Ordinal is assigned once, while the property is parsed, and is never
// recalculated from the property name. The wire identity fields are retained
// as an additional guard for grouped and repeated values. In particular, this
// keeps an itemN.TEL occurrence attached to its itemN.X-ABLABEL residue.
type PropertyIdentity struct {
	Ordinal      int      `json:"ordinal"`
	Group        string   `json:"group,omitempty"`
	OriginalName string   `json:"original_name,omitempty"`
	PropID       *string  `json:"prop_id,omitempty"`
	PID          []string `json:"pid,omitempty"`
	AltID        *string  `json:"altid,omitempty"`
}

// IsZero reports whether the identity has not been assigned.
func (i PropertyIdentity) IsZero() bool {
	return i.Ordinal == 0 && i.Group == "" && i.OriginalName == "" &&
		i.PropID == nil && len(i.PID) == 0 && i.AltID == nil
}

// Key returns a deterministic key suitable for identity maps. JSON is used
// here deliberately: unlike a property name, it includes every wire identity
// component and the immutable occurrence ordinal.
func (i PropertyIdentity) Key() string {
	data, err := json.Marshal(i)
	if err != nil {
		panic(fmt.Sprintf("marshal property identity: %v", err))
	}
	return string(data)
}

// Equal compares every identity component, including the occurrence ordinal.
func (i PropertyIdentity) Equal(other PropertyIdentity) bool {
	return i.Ordinal == other.Ordinal && i.Group == other.Group &&
		i.OriginalName == other.OriginalName &&
		equalStringPointers(i.PropID, other.PropID) &&
		slices.Equal(i.PID, other.PID) &&
		equalStringPointers(i.AltID, other.AltID)
}

// sameWireIdentity reports whether two identities name the same wire
// occurrence regardless of ordinal. Groups are case-insensitive (RFC 6350
// §3.3), so item1.TEL and ITEM1.TEL are the same occurrence.
func (i PropertyIdentity) sameWireIdentity(other PropertyIdentity) bool {
	return strings.EqualFold(i.Group, other.Group) &&
		equalStringPointers(i.PropID, other.PropID) &&
		slices.Equal(i.PID, other.PID) &&
		equalStringPointers(i.AltID, other.AltID)
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// PropertyOccurrence is one ordered property and its envelope identity.
type PropertyOccurrence struct {
	Identity       PropertyIdentity `json:"identity"`
	Property       Property         `json:"property"`
	Classification HandlingStrategy `json:"classification"`
}

// PropertyEdit replaces or removes exactly one property occurrence. A zero
// identity creates a new occurrence at the end of the ordered tree; a delete
// always names an existing occurrence.
//
// OwnedParameters names the parameters the semantic field behind this edit
// represents, beyond the ones a managed property always implies. An owned
// parameter is replaced when Property carries it and removed when Property
// omits it, so clearing a typed field also clears the parameter it rendered.
// Every unowned parameter keeps its source spelling, order, and value.
type PropertyEdit struct {
	Identity        PropertyIdentity `json:"identity"`
	Property        Property         `json:"property"`
	OwnedParameters []string         `json:"owned_parameters,omitempty"`
	Delete          bool             `json:"delete,omitempty"`
}

// NativeMapping records which typed/native record owns one property. The
// envelope stores the mapping, while the typed profile tables remain the
// semantic source of truth.
type NativeMapping struct {
	Identity  PropertyIdentity `json:"identity"`
	SourceRef string           `json:"source_ref,omitempty"`
	Table     string           `json:"table,omitempty"`
	RowID     int64            `json:"row_id,omitempty"`
	Field     string           `json:"field,omitempty"`
	Kind      HandlingStrategy `json:"kind,omitempty"`
}

// RenderMetadata describes the stored and canonical wire versions. A view
// render never changes these fields; PrepareCanonicalRender does.
type RenderMetadata struct {
	CanonicalVersion Version `json:"canonical_version"`
	StoredVersion    Version `json:"stored_version"`
	RenderRequired   bool    `json:"render_required,omitempty"`
	Revision         int64   `json:"revision"`
}

// ResourceEnvelope is the blob-authoritative representation of one native
// vCard resource. OriginalRawBytes is immutable import evidence. StoredBody
// is the exact body used for responses and for ETag calculation. PropertyTree,
// NativeMappings, and Residue are ordered representations above those bytes.
type ResourceEnvelope struct {
	CanonicalPersonUID string `json:"canonical_person_uid,omitempty"`
	SourceResourceUID  string `json:"source_resource_uid"`
	SourceRef          string `json:"source_ref,omitempty"`
	Href               string `json:"href,omitempty"`

	OriginalRawBytes []byte `json:"original_raw_bytes"`
	StoredBody       []byte `json:"stored_body"`

	PropertyTree   []PropertyOccurrence `json:"property_tree"`
	NativeMappings []NativeMapping      `json:"native_mappings,omitempty"`
	Residue        []PropertyOccurrence `json:"residue,omitempty"`
	RenderMetadata RenderMetadata       `json:"render_metadata"`
	// NextOccurrenceOrdinal is a high-water mark. It prevents a deleted final
	// occurrence's identity from being reused by a later appended property.
	NextOccurrenceOrdinal int `json:"next_occurrence_ordinal"`

	ContentHash string `json:"content_hash"`
	ETag        string `json:"etag"`
}

// ContentHash returns the lowercase SHA-256 digest of body.
func ContentHash(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

// ETagForBody returns a strong quoted ETag derived from the exact body bytes.
func ETagForBody(body []byte) string {
	return `"` + ContentHash(body) + `"`
}

// ParseResourceEnvelope decodes exactly one vCard resource and records its
// ordered property tree. The decoder returns an error for malformed content
// lines; no partial card is accepted.
func ParseResourceEnvelope(raw []byte) (ResourceEnvelope, error) {
	if len(raw) == 0 {
		return ResourceEnvelope{}, errors.New("vCard resource body is empty")
	}
	doc, err := Decode(bytes.NewReader(raw))
	if err != nil {
		return ResourceEnvelope{}, fmt.Errorf("decode vCard resource: %w", err)
	}
	if len(doc.Cards) != 1 {
		return ResourceEnvelope{}, fmt.Errorf(
			"vCard resource must contain exactly one card, found %d", len(doc.Cards),
		)
	}
	card := doc.Cards[0]
	version, err := card.Version()
	if err != nil {
		return ResourceEnvelope{}, fmt.Errorf("vCard resource version: %w", err)
	}
	propertyTree := make([]PropertyOccurrence, 0, len(card.Properties))
	for ordinal, property := range card.Properties {
		if err := validatePropertyEncoding(property, version); err != nil {
			return ResourceEnvelope{}, err
		}
		propertyTree = append(propertyTree, PropertyOccurrence{
			Identity:       propertyIdentity(ordinal, property),
			Property:       property,
			Classification: classifyProperty(property),
		})
	}

	return ResourceEnvelope{
		OriginalRawBytes:      append([]byte(nil), raw...),
		StoredBody:            append([]byte(nil), raw...),
		PropertyTree:          propertyTree,
		Residue:               residueProperties(propertyTree),
		NextOccurrenceOrdinal: len(propertyTree),
		RenderMetadata: RenderMetadata{
			CanonicalVersion: Version40,
			StoredVersion:    version,
			Revision:         1,
		},
		ContentHash: ContentHash(raw),
		ETag:        ETagForBody(raw),
	}, nil
}

// MergeProperties applies edits by exact occurrence identity. It does not
// render or mutate StoredBody; callers must commit a rendered body separately.
func (e ResourceEnvelope) MergeProperties(edits []PropertyEdit) (ResourceEnvelope, error) {
	merged := cloneResourceEnvelope(e)
	if len(edits) == 0 {
		return merged, nil
	}
	// Edits carry vCard 4 wire values. A tree still in its imported legacy
	// form is rendered canonical first, so the legacy decoding a later render
	// applies to the whole tree — 2.1 backslash escaping above all — never
	// reads a projected "\n" as legacy text. Occurrence identities keep their
	// ordinals through canonicalization, so the edits still resolve.
	if e.RenderMetadata.StoredVersion != Version40 {
		canonical, err := e.PrepareCanonicalRender()
		if err != nil {
			return ResourceEnvelope{}, fmt.Errorf("canonicalize before merge: %w", err)
		}
		merged = canonical
	}
	byKey, err := indexPropertyEdits(edits)
	if err != nil {
		return ResourceEnvelope{}, err
	}
	properties, seen := applyPropertyEdits(merged.PropertyTree, byKey)
	for _, edit := range edits {
		if !edit.Identity.IsZero() {
			if !seen[edit.Identity.Key()] {
				return ResourceEnvelope{}, fmt.Errorf(
					"property edit identity not found: %s", edit.Identity.Key(),
				)
			}
			continue
		}
		property := cloneProperty(edit.Property)
		properties = append(properties, PropertyOccurrence{
			Identity:       propertyIdentity(merged.NextOccurrenceOrdinal, property),
			Property:       property,
			Classification: classifyProperty(property),
		})
		merged.NextOccurrenceOrdinal++
	}
	merged.PropertyTree = properties
	if len(merged.NativeMappings) > 0 {
		merged.NativeMappings = mappingsForTree(merged.NativeMappings, properties)
	}
	merged.Residue = mergeResidueOccurrences(
		residueProperties(properties), merged.Residue, properties,
	)
	merged.RenderMetadata.RenderRequired = true
	return merged, nil
}

// applyPropertyEdits replaces or removes the occurrences named by byKey and
// reports which identities it found.
func applyPropertyEdits(
	tree []PropertyOccurrence, byKey map[string]PropertyEdit,
) ([]PropertyOccurrence, map[string]bool) {
	seen := make(map[string]bool, len(byKey))
	properties := make([]PropertyOccurrence, 0, len(tree)+len(byKey))
	for _, occurrence := range tree {
		edit, ok := byKey[occurrence.Identity.Key()]
		if !ok {
			properties = append(properties, occurrence)
			continue
		}
		seen[occurrence.Identity.Key()] = true
		if edit.Delete {
			continue
		}
		occurrence.Property = mergeEditedProperty(
			occurrence.Property, cloneProperty(edit.Property), edit.OwnedParameters,
		)
		occurrence.Classification = classifyProperty(edit.Property)
		properties = append(properties, occurrence)
	}
	return properties, seen
}

// indexPropertyEdits validates every edit up front and indexes the ones that
// address an existing occurrence, so a malformed batch changes nothing.
func indexPropertyEdits(edits []PropertyEdit) (map[string]PropertyEdit, error) {
	byKey := make(map[string]PropertyEdit, len(edits))
	for _, edit := range edits {
		if edit.Delete {
			if edit.Identity.IsZero() {
				return nil, errors.New("delete edit requires an occurrence identity")
			}
		} else if !validToken(edit.Property.Name) {
			if edit.Property.Name == "" {
				return nil, errors.New("property edit has an empty property name")
			}
			return nil, fmt.Errorf(
				"property edit has an invalid property name %q", edit.Property.Name,
			)
		}
		if edit.Identity.IsZero() {
			continue
		}
		key := edit.Identity.Key()
		if _, exists := byKey[key]; exists {
			return nil, fmt.Errorf("duplicate property edit identity %s", key)
		}
		byKey[key] = edit
	}
	return byKey, nil
}

// RenderView returns the requested wire-version view without mutating the
// envelope. A no-op view returns StoredBody byte-for-byte.
func (e ResourceEnvelope) RenderView(version Version) ([]byte, error) {
	if version == "" {
		version = e.RenderMetadata.StoredVersion
	}
	if version == Version21 {
		return nil, errors.New("vCard 2.1 is read-only and cannot be emitted")
	}
	if !e.RenderMetadata.RenderRequired && version == e.RenderMetadata.StoredVersion {
		return append([]byte(nil), e.StoredBody...), nil
	}
	if version != Version30 && version != Version40 {
		return nil, fmt.Errorf("unsupported vCard render version %q", version)
	}
	card, err := e.cardForRender(version)
	if err != nil {
		return nil, fmt.Errorf("prepare vCard %s view: %w", version, err)
	}
	if err := validateRenderedCard(card, version); err != nil {
		return nil, fmt.Errorf("validate vCard %s view: %w", version, err)
	}
	body, err := Marshal(Document{Cards: []Card{card}})
	if err != nil {
		return nil, fmt.Errorf("render vCard %s view: %w", version, err)
	}
	return body, nil
}

// PrepareCanonicalRender renders and installs a canonical vCard 4 body in a
// complete envelope value. Callers persist that value atomically with its
// mappings and residue; there is deliberately no exported body-only commit
// operation.
func (e ResourceEnvelope) PrepareCanonicalRender() (ResourceEnvelope, error) {
	body, err := e.RenderView(Version40)
	if err != nil {
		return ResourceEnvelope{}, err
	}
	return e.commitRenderedBody(body)
}

// commitRenderedBody installs a rendered vCard 4 body. The wire tree parsed
// from the body becomes the property tree, keeping the stable ordinal of every
// occurrence it still contains; mappings and residue follow those ordinals.
func (e ResourceEnvelope) commitRenderedBody(body []byte) (ResourceEnvelope, error) {
	parsed, err := ParseResourceEnvelope(body)
	if err != nil {
		return ResourceEnvelope{}, fmt.Errorf("validate rendered vCard body: %w", err)
	}
	if parsed.RenderMetadata.StoredVersion != Version40 {
		return ResourceEnvelope{}, fmt.Errorf(
			"rendered vCard VERSION %q is not the canonical %s",
			parsed.RenderMetadata.StoredVersion, Version40,
		)
	}
	committed := cloneResourceEnvelope(e)
	unchanged := e.RenderMetadata.StoredVersion == Version40 && bytes.Equal(body, e.StoredBody)
	if unchanged && !e.RenderMetadata.RenderRequired {
		return committed, nil
	}
	committed.StoredBody = append([]byte(nil), body...)
	committed.PropertyTree = reconcilePropertyTree(committed.PropertyTree, parsed.PropertyTree)
	committed.NextOccurrenceOrdinal = max(
		committed.NextOccurrenceOrdinal, nextPropertyOrdinal(committed.PropertyTree),
	)
	committed.NativeMappings = mappingsForTree(committed.NativeMappings, committed.PropertyTree)
	committed.Residue = mergeResidueOccurrences(nil, e.Residue, committed.PropertyTree)
	committed.ContentHash = ContentHash(body)
	committed.ETag = ETagForBody(body)
	committed.RenderMetadata.StoredVersion = Version40
	committed.RenderMetadata.RenderRequired = false
	// Edits that render to the stored bytes (a re-projection of unchanged
	// semantic data) settle the render-required state without producing a new
	// revision: nothing a reader can observe has changed.
	if unchanged {
		return committed, nil
	}
	committed.RenderMetadata.Revision++
	if committed.RenderMetadata.Revision <= 0 {
		committed.RenderMetadata.Revision = 1
	}
	return committed, nil
}

// validatePropertyEncoding is the version-aware half of the decoder's
// content-line check. vCard 3 and 4 values are UTF-8 unless a CHARSET
// parameter declares otherwise; vCard 2.1 producers wrote bare Latin-1, which
// the render-time legacy decoding reads through its ISO-8859-1 fallback.
func validatePropertyEncoding(property Property, version Version) error {
	if version == Version21 || utf8.ValidString(property.RawValue) ||
		propertyCharset(property) != "" {
		return nil
	}
	return fmt.Errorf(
		"vCard %s property %s value is not valid UTF-8 and declares no charset",
		version, property.Name,
	)
}

func (e ResourceEnvelope) cardForRender(version Version) (Card, error) {
	sourceVersion := e.RenderMetadata.StoredVersion
	properties := make([]Property, 0, len(e.PropertyTree)+1)
	for _, occurrence := range e.PropertyTree {
		property := cloneProperty(occurrence.Property)
		if strings.EqualFold(property.Name, "VERSION") {
			property.RawValue = string(version)
		}
		property, omit, err := normalizePropertyForVersion(property, sourceVersion, version)
		if err != nil {
			return Card{}, err
		}
		if !omit {
			properties = append(properties, property)
		}
	}
	properties, err := ensureRenderedFullName(properties, version)
	if err != nil {
		return Card{}, err
	}
	properties, err = placeVersionProperty(properties, version)
	if err != nil {
		return Card{}, err
	}
	return Card{Properties: properties}, nil
}

// placeVersionProperty adds a missing VERSION and, for vCard 4.0, moves it to
// the front where RFC 6350 requires it.
func placeVersionProperty(properties []Property, version Version) ([]Property, error) {
	versionIndex := slices.IndexFunc(properties, func(property Property) bool {
		return strings.EqualFold(property.Name, "VERSION")
	})
	if versionIndex < 0 {
		versionProperty, err := NewProperty("", "VERSION", string(version))
		if err != nil {
			return nil, err
		}
		return append([]Property{versionProperty}, properties...), nil
	}
	if version == Version40 && versionIndex != 0 {
		versionProperty := properties[versionIndex]
		properties = slices.Delete(properties, versionIndex, versionIndex+1)
		return append([]Property{versionProperty}, properties...), nil
	}
	return properties, nil
}

// ensureRenderedFullName derives the mandatory FN from N when the card has
// none. DERIVED (RFC 6350 §5.9) marks the synthesized value in vCard 4.0; the
// parameter does not exist in 3.0, whose FN is simply the derived text.
func ensureRenderedFullName(properties []Property, version Version) ([]Property, error) {
	for _, property := range properties {
		if strings.EqualFold(property.Name, "FN") && strings.TrimSpace(property.RawValue) != "" {
			return properties, nil
		}
	}
	for index, property := range properties {
		if !strings.EqualFold(property.Name, "N") {
			continue
		}
		fullName, ok, err := fullNameFromStructuredName(property, version)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		return slices.Insert(properties, index+1, fullName), nil
	}
	return properties, nil
}

// fullNameFromStructuredName builds FN from the components of N and reports
// false when N has no non-empty component to derive it from.
func fullNameFromStructuredName(name Property, version Version) (Property, bool, error) {
	components, err := SplitStructuredText(name.RawValue)
	if err != nil {
		return Property{}, false, fmt.Errorf("derive FN from N: %w", err)
	}
	ordered := make([]string, 0, len(components))
	for _, componentIndex := range []int{3, 1, 2, 0, 4} {
		if componentIndex >= len(components) {
			continue
		}
		if component := strings.TrimSpace(components[componentIndex]); component != "" {
			ordered = append(ordered, component)
		}
	}
	if len(ordered) == 0 {
		return Property{}, false, nil
	}
	fullName, err := NewProperty(name.Group, "FN", EscapeText(strings.Join(ordered, " ")))
	if err != nil {
		return Property{}, false, err
	}
	if version == Version40 {
		derived, err := NewParameter("DERIVED", "true")
		if err != nil {
			return Property{}, false, err
		}
		fullName.Parameters = append(fullName.Parameters, derived)
	}
	return fullName, true, nil
}

// validateRenderedCard checks the rendered card's cross-property semantics
// and that no transfer declaration describing the source bytes survived
// normalization. vCard 3.0 keeps ENCODING=b for inline media, its own inline
// binary spelling.
func validateRenderedCard(card Card, version Version) error {
	if err := Validate(Document{Cards: []Card{card}}); err != nil {
		return err
	}
	for _, property := range card.Properties {
		for _, parameter := range property.Parameters {
			if version == Version30 && isV3InlineBinaryEncoding(property, parameter) {
				continue
			}
			if isTransferParameter(parameter) {
				return fmt.Errorf("legacy parameter %s remains on %s", parameter.Name, property.Name)
			}
		}
	}
	return nil
}

func isV3InlineBinaryEncoding(property Property, parameter Parameter) bool {
	if !isInlineMediaName(property.Name) || !strings.EqualFold(parameter.Name, "ENCODING") {
		return false
	}
	return len(parameter.Values) == 1 &&
		strings.EqualFold(strings.TrimSpace(parameter.Values[0].Decoded), "b")
}

func propertyIdentity(ordinal int, property Property) PropertyIdentity {
	identity := PropertyIdentity{
		Ordinal: ordinal, Group: property.Group,
		OriginalName: property.OriginalName,
	}
	for _, parameter := range property.Parameters {
		if len(parameter.Values) == 0 {
			continue
		}
		values := make([]string, 0, len(parameter.Values))
		for _, value := range parameter.Values {
			values = append(values, value.Decoded)
		}
		switch strings.ToUpper(parameter.Name) {
		case "PROP-ID":
			identity.PropID = new(values[0])
		case "PID":
			identity.PID = append(identity.PID, values...)
		case "ALTID":
			identity.AltID = new(values[0])
		}
	}
	return identity
}

func classifyProperty(property Property) HandlingStrategy {
	if handling, ok := PropertyHandling(property.Name); ok {
		return handling.Strategy
	}
	return HandlingPreserve
}

func residueProperties(properties []PropertyOccurrence) []PropertyOccurrence {
	residue := make([]PropertyOccurrence, 0)
	for _, property := range properties {
		if property.Classification == HandlingPreserve {
			residue = append(residue, property)
		}
	}
	return residue
}

// ResidueWithMappings returns the preserved portion of a tree after native
// mappings have been attached. An occurrence with no mapping is residue unless
// it is a runtime property owned by the resource layer. This keeps old
// derived/relationship values reviewable instead of silently treating them as
// regenerated semantics.
func ResidueWithMappings(
	properties []PropertyOccurrence, mappings []NativeMapping,
) []PropertyOccurrence {
	mapped := make(map[string]HandlingStrategy, len(mappings))
	for _, mapping := range mappings {
		if !mapping.Identity.IsZero() {
			mapped[mapping.Identity.Key()] = mapping.Kind
		}
	}
	residue := make([]PropertyOccurrence, 0)
	for _, property := range properties {
		if kind, ok := mapped[property.Identity.Key()]; ok {
			if kind == HandlingPreserve {
				residue = append(residue, property)
			}
			continue
		}
		if property.Classification == HandlingPreserve {
			residue = append(residue, property)
			continue
		}
		if isRuntimeProperty(property.Property.Name) {
			continue
		}
		if property.Classification == HandlingFraming {
			continue
		}
		residue = appendUniqueOccurrence(residue, property)
	}
	return residue
}

// mergeResidueOccurrences returns, in tree order, the tree occurrences whose
// ordinal appears in base or extra. Ordinals rather than full identities are
// compared because a commit refreshes the wire identity fields of an edited
// occurrence while keeping its ordinal.
func mergeResidueOccurrences(
	base, extra, properties []PropertyOccurrence,
) []PropertyOccurrence {
	wanted := make(map[int]struct{}, len(base)+len(extra))
	for _, residue := range base {
		wanted[residue.Identity.Ordinal] = struct{}{}
	}
	for _, residue := range extra {
		wanted[residue.Identity.Ordinal] = struct{}{}
	}
	merged := make([]PropertyOccurrence, 0, len(wanted))
	for _, property := range properties {
		if _, ok := wanted[property.Identity.Ordinal]; ok {
			merged = append(merged, property)
		}
	}
	return merged
}

func appendUniqueOccurrence(
	properties []PropertyOccurrence, occurrence PropertyOccurrence,
) []PropertyOccurrence {
	for _, existing := range properties {
		if existing.Identity.Equal(occurrence.Identity) {
			return properties
		}
	}
	return append(properties, occurrence)
}

// mappingsForTree drops the mappings whose occurrence left the tree and
// refreshes the identity of every other one from the tree, so a mapping
// follows its occurrence through an edit that changed the wire identity.
func mappingsForTree(mappings []NativeMapping, properties []PropertyOccurrence) []NativeMapping {
	present := make(map[int]PropertyIdentity, len(properties))
	for _, property := range properties {
		present[property.Identity.Ordinal] = property.Identity
	}
	kept := make([]NativeMapping, 0, len(mappings))
	for _, mapping := range mappings {
		identity, ok := present[mapping.Identity.Ordinal]
		if !ok {
			continue
		}
		mapping.Identity = clonePropertyIdentity(identity)
		kept = append(kept, mapping)
	}
	return kept
}

// IsReservedProperty reports the property names no semantic layer may
// project a value onto: the card framing (BEGIN, END), VERSION, and the
// runtime singletons the renderer and store own (UID, PRODID, REV, CREATED,
// SOURCE). A second VERSION fails validation outright; a stray BEGIN or END
// produces a body that no longer parses as one card.
func IsReservedProperty(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "BEGIN", "END":
		return true
	}
	return isRuntimeProperty(name)
}

func isRuntimeProperty(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "VERSION", "UID", "PRODID", "REV", "CREATED", "SOURCE":
		return true
	default:
		return false
	}
}

// mergeEditedProperty applies a semantic replacement without throwing away
// parameters that the mapper does not own. Parameter names are the boundary:
// a replacement may deliberately change or drop a parameter it owns (for
// example TYPE, PREF, or a name's SORT-AS), while an unmentioned unowned
// parameter, including an unknown vendor parameter and its source spelling,
// remains at its original position.
//
// Transfer declarations are the exception to "unowned survives": CHARSET,
// ENCODING, and the bare vCard 2.1 encoding tokens describe the original wire
// bytes, and the replacement value is plain UTF-8 vCard text. Whoever owns
// them, they never apply to the new value, so they are dropped for every edit
// — from the original and from a replacement that was cloned from a legacy
// occurrence and had only its value changed.
func mergeEditedProperty(original, replacement Property, owned []string) Property {
	replacement.Group = original.Group
	replacement.Parameters = withoutTransferParameters(replacement.Parameters)
	if len(original.Parameters) == 0 {
		return replacement
	}
	declared := declaredParameterNames(owned)
	if len(replacement.Parameters) == 0 {
		replacement.Parameters = unownedParameters(original, declared)
		return replacement
	}
	replacement.Parameters = mergeParameterLists(original, replacement.Parameters, declared)
	return replacement
}

// unownedParameters returns the original parameters an edit leaves alone:
// neither transfer declarations nor parameters the edit owns.
func unownedParameters(original Property, declared map[string]struct{}) []Parameter {
	kept := make([]Parameter, 0, len(original.Parameters))
	for _, parameter := range original.Parameters {
		if isTransferParameter(parameter) ||
			editOwnsParameter(original.Name, declared, parameter.Name) {
			continue
		}
		kept = append(kept, parameter)
	}
	return kept
}

// mergeParameterLists places each replacement parameter at the position of
// the original parameter of the same name (or at the end when the original
// had none), keeps unowned originals in place, and drops the rest.
func mergeParameterLists(
	original Property, replacements []Parameter, declared map[string]struct{},
) []Parameter {
	byName := make(map[string][]Parameter)
	for _, parameter := range replacements {
		name := strings.ToUpper(parameter.Name)
		byName[name] = append(byName[name], parameter)
	}
	merged := make([]Parameter, 0, len(original.Parameters)+len(replacements))
	emitted := make(map[string]bool, len(byName))
	for _, parameter := range original.Parameters {
		name := strings.ToUpper(parameter.Name)
		if replacementParameters, ok := byName[name]; ok {
			if !emitted[name] {
				merged = append(merged, replacementParameters...)
				emitted[name] = true
			}
			continue
		}
		if isTransferParameter(parameter) || editOwnsParameter(original.Name, declared, name) {
			continue
		}
		merged = append(merged, parameter)
	}
	for _, parameter := range replacements {
		name := strings.ToUpper(parameter.Name)
		if emitted[name] {
			continue
		}
		merged = append(merged, parameter)
		emitted[name] = true
	}
	return merged
}

func declaredParameterNames(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(names))
	for _, name := range names {
		declared[strings.ToUpper(strings.TrimSpace(name))] = struct{}{}
	}
	return declared
}

// withoutTransferParameters drops the legacy parameters that describe how a
// value was encoded on the wire rather than what it means: CHARSET, ENCODING,
// and the bare 2.1 tokens (QUOTED-PRINTABLE, BASE64, B).
func withoutTransferParameters(parameters []Parameter) []Parameter {
	kept := parameters[:0:0]
	for _, parameter := range parameters {
		if !isTransferParameter(parameter) {
			kept = append(kept, parameter)
		}
	}
	return kept
}

func isTransferParameter(parameter Parameter) bool {
	switch strings.ToUpper(parameter.Name) {
	case "CHARSET", "ENCODING":
		return true
	}
	return isLegacyEncodingToken(parameter)
}

// editOwnsParameter reports whether an edit may rewrite or drop a parameter.
// Ownership is the union of the names the edit declares for its semantic field
// and the names a managed property always implies.
func editOwnsParameter(
	propertyName string, declared map[string]struct{}, parameterName string,
) bool {
	if _, ok := declared[strings.ToUpper(strings.TrimSpace(parameterName))]; ok {
		return true
	}
	return ownsMapperParameter(propertyName, parameterName)
}

// ownsMapperParameter reports the parameters every projection of a managed
// property regenerates, whichever semantic field produced the replacement.
func ownsMapperParameter(propertyName, parameterName string) bool {
	handling, managed := PropertyHandling(propertyName)
	if !managed || handling.Strategy == HandlingPreserve || handling.Strategy == HandlingFraming {
		return false
	}
	switch strings.ToUpper(parameterName) {
	case "PREF", parameterTypeName:
		return true
	case "GEO", "TZ":
		return strings.EqualFold(propertyName, "ADR")
	case "MEDIATYPE", "VALUE":
		return isInlineMediaName(propertyName)
	default:
		return false
	}
}

func nextPropertyOrdinal(properties []PropertyOccurrence) int {
	next := 0
	for _, property := range properties {
		if property.Identity.Ordinal >= next {
			next = property.Identity.Ordinal + 1
		}
	}
	return next
}

func cloneResourceEnvelope(e ResourceEnvelope) ResourceEnvelope {
	e.OriginalRawBytes = append([]byte(nil), e.OriginalRawBytes...)
	e.StoredBody = append([]byte(nil), e.StoredBody...)
	e.PropertyTree = clonePropertyOccurrences(e.PropertyTree)
	e.Residue = clonePropertyOccurrences(e.Residue)
	if e.NativeMappings != nil {
		mappings := make([]NativeMapping, len(e.NativeMappings))
		for index, mapping := range e.NativeMappings {
			mapping.Identity = clonePropertyIdentity(mapping.Identity)
			mappings[index] = mapping
		}
		e.NativeMappings = mappings
	}
	return e
}

// reconcilePropertyTree returns the parsed wire tree in wire order. Every wire
// occurrence that matches a stable occurrence keeps that occurrence's ordinal
// (each at most once) while taking the wire identity fields the rendered body
// now carries; the rest receive ordinals after every identity in the stable
// tree.
func reconcilePropertyTree(stable, wire []PropertyOccurrence) []PropertyOccurrence {
	used := make([]bool, len(stable))
	nextOrdinal := nextPropertyOrdinal(stable)
	reconciled := make([]PropertyOccurrence, 0, len(wire))
	for _, wireOccurrence := range wire {
		match := matchingStableOccurrence(stable, used, wireOccurrence, true)
		if match < 0 {
			// A semantic edit can change a value or target-version syntax while
			// retaining its wire identity. Exact syntax is preferred above so
			// repeated and reordered values stay attached to the right owner.
			match = matchingStableOccurrence(stable, used, wireOccurrence, false)
		}
		candidate := clonePropertyOccurrence(wireOccurrence)
		if match >= 0 {
			used[match] = true
			candidate.Identity.Ordinal = stable[match].Identity.Ordinal
		} else {
			candidate.Identity.Ordinal = nextOrdinal
			nextOrdinal++
		}
		reconciled = append(reconciled, candidate)
	}
	return reconciled
}

func matchingStableOccurrence(
	stable []PropertyOccurrence, used []bool, want PropertyOccurrence, exactSyntax bool,
) int {
	for index, candidate := range stable {
		if used[index] {
			continue
		}
		if exactSyntax {
			if samePropertySyntax(candidate.Property, want.Property) {
				return index
			}
			continue
		}
		if sameRenderedIdentity(candidate.Property, want.Property) {
			return index
		}
	}
	return -1
}

func samePropertySyntax(left, right Property) bool {
	if !strings.EqualFold(left.Group, right.Group) || left.Name != right.Name ||
		left.OriginalName != right.OriginalName || left.RawValue != right.RawValue ||
		len(left.Parameters) != len(right.Parameters) {
		return false
	}
	for index, leftParameter := range left.Parameters {
		if !sameParameterSyntax(leftParameter, right.Parameters[index]) {
			return false
		}
	}
	return true
}

func sameParameterSyntax(left, right Parameter) bool {
	return left.Name == right.Name && left.OriginalName == right.OriginalName &&
		left.Bare == right.Bare && slices.Equal(left.Values, right.Values)
}

// sameRenderedIdentity compares the wire identity two properties currently
// carry (group, PROP-ID, PID, ALTID), not the identity recorded when they
// were parsed: an edit may have changed those parameters since.
func sameRenderedIdentity(left, right Property) bool {
	return strings.EqualFold(left.Name, right.Name) &&
		propertyIdentity(0, left).sameWireIdentity(propertyIdentity(0, right))
}

func clonePropertyOccurrences(properties []PropertyOccurrence) []PropertyOccurrence {
	if properties == nil {
		return nil
	}
	cloned := make([]PropertyOccurrence, len(properties))
	for index, property := range properties {
		cloned[index] = clonePropertyOccurrence(property)
	}
	return cloned
}

func clonePropertyOccurrence(occurrence PropertyOccurrence) PropertyOccurrence {
	occurrence.Identity = clonePropertyIdentity(occurrence.Identity)
	occurrence.Property = cloneProperty(occurrence.Property)
	return occurrence
}

func clonePropertyIdentity(identity PropertyIdentity) PropertyIdentity {
	identity.PropID = cloneStringPointer(identity.PropID)
	identity.PID = append([]string(nil), identity.PID...)
	identity.AltID = cloneStringPointer(identity.AltID)
	return identity
}

func cloneProperty(property Property) Property {
	if property.Parameters == nil {
		return property
	}
	parameters := make([]Parameter, len(property.Parameters))
	for index, parameter := range property.Parameters {
		parameter.Values = append([]ParameterValue(nil), parameter.Values...)
		parameters[index] = parameter
	}
	property.Parameters = parameters
	return property
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
