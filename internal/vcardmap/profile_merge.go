package vcardmap

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vcard"
)

// ProjectPersonEnvelope merges one semantic snapshot into an envelope and
// returns a complete prepared vCard 4 value. The caller commits it with the
// snapshot fingerprint through the store's revision-qualified API.
func ProjectPersonEnvelope(
	snapshot store.PersonVCardSnapshot,
	envelope vcard.ResourceEnvelope,
) (vcard.ResourceEnvelope, error) {
	// Canonicalize an imported legacy envelope before anything reads its
	// tree. Canonicalization can synthesize occurrences — a derived FN for a
	// card with only N — which advances the ordinal counter and adds an FN
	// the fallback logic must see; binding, ordinal accounting, and the merge
	// all have to run against that tree, not the legacy one.
	if envelope.RenderMetadata.StoredVersion != vcard.Version40 {
		canonical, err := envelope.PrepareCanonicalRender()
		if err != nil {
			return vcard.ResourceEnvelope{}, fmt.Errorf("canonicalize envelope: %w", err)
		}
		envelope = canonical
	}
	desired, retained, err := projectPersonProperties(snapshot)
	if err != nil {
		return vcard.ResourceEnvelope{}, err
	}
	envelope = rebindAcceptedReviewMappings(envelope, snapshot.AcceptedRelationshipReviews)
	return mergeProjectedProperties(envelope, desired, retained)
}

// rebindAcceptedReviewMappings hands each occurrence a review stood for to
// the edge that satisfied the review, in this envelope only. Ownership of an
// occurrence is per resource: the same edge sits on both endpoints' cards and
// on every card a person has, so it is the envelope's own mapping, not the
// edge's single vCard identity, that says which occurrence here was the
// review's. With the owner rebound, the merge updates that occurrence in
// place under the edge instead of retiring it and appending a fresh RELATED.
func rebindAcceptedReviewMappings(
	envelope vcard.ResourceEnvelope, accepted []store.PersonVCardAcceptedReview,
) vcard.ResourceEnvelope {
	if len(accepted) == 0 || len(envelope.NativeMappings) == 0 {
		return envelope
	}
	edgeByReview := make(map[int64]int64, len(accepted))
	for _, binding := range accepted {
		edgeByReview[binding.ReviewID] = binding.RelationshipID
	}
	// An edge that already owns an occurrence here keeps it: the review's
	// occurrence is then a duplicate of it, and stays a review mapping so the
	// merge retires it rather than leaving two owners for one edge.
	edgeMapped := ownedRowsInEnvelope(envelope, ownerRelationship)
	mappings := make([]vcard.NativeMapping, 0, len(envelope.NativeMappings))
	for _, mapping := range envelope.NativeMappings {
		edgeID, accepted := edgeByReview[mapping.RowID]
		_, taken := edgeMapped[edgeID]
		if accepted && !taken && mappingOwnedBy(envelope, mapping, ownerReview) {
			mapping.Table, mapping.RowID = ownerRelationship.table, edgeID
			edgeMapped[edgeID] = struct{}{}
		}
		mappings = append(mappings, mapping)
	}
	envelope.NativeMappings = mappings
	return envelope
}

// ownedRowsInEnvelope lists the row IDs of the given owner field that hold a
// mapping recorded for this envelope's own source.
func ownedRowsInEnvelope(envelope vcard.ResourceEnvelope, field ownerField) map[int64]struct{} {
	rows := make(map[int64]struct{})
	for _, mapping := range envelope.NativeMappings {
		if mappingOwnedBy(envelope, mapping, field) {
			rows[mapping.RowID] = struct{}{}
		}
	}
	return rows
}

func mappingOwnedBy(
	envelope vcard.ResourceEnvelope, mapping vcard.NativeMapping, field ownerField,
) bool {
	return mapping.SourceRef == envelope.SourceRef &&
		mapping.Table == field.table && mapping.Field == field.field
}

// projectedBinding is one desired owner and the occurrence it will edit. A
// zero identity means the property is appended as a new occurrence.
type projectedBinding struct {
	property projectedProperty
	identity vcard.PropertyIdentity
}

func (b projectedBinding) appended() bool {
	return b.identity.IsZero()
}

// mergeProjectedProperties binds every desired owner to an occurrence, retires
// the occurrences of owners that no longer project anything, applies the
// edits, and rebuilds the envelope's mappings and residue over the result.
func mergeProjectedProperties(
	envelope vcard.ResourceEnvelope, desired []projectedProperty,
	retained []retainedOwner,
) (vcard.ResourceEnvelope, error) {
	retainedOwners := make(map[string]struct{}, len(retained))
	for _, entry := range retained {
		if !entry.appliesToResource(envelope.SourceRef, envelope.SourceResourceUID) {
			continue
		}
		retainedOwners[ownerKey(envelope.SourceRef, entry.Owner)] = struct{}{}
	}
	bindings, err := bindProjectedProperties(envelope, desired, retainedOwners)
	if err != nil {
		return vcard.ResourceEnvelope{}, err
	}
	stale := staleMappedOccurrences(envelope, bindings, retainedOwners)
	bindings = dropRedundantFullName(envelope.PropertyTree, bindings, stale)
	edits, err := projectedEdits(envelope, bindings)
	if err != nil {
		return vcard.ResourceEnvelope{}, err
	}
	for _, identity := range stale {
		edits = append(edits, vcard.PropertyEdit{Identity: identity, Delete: true})
	}
	merged, err := envelope.MergeProperties(edits)
	if err != nil {
		return vcard.ResourceEnvelope{}, err
	}
	bindings, err = locateAppendedOccurrences(
		merged.PropertyTree, envelope.NextOccurrenceOrdinal, bindings,
	)
	if err != nil {
		return vcard.ResourceEnvelope{}, err
	}
	merged.NativeMappings = rebuildMappings(envelope, merged.PropertyTree, bindings, retainedOwners)
	merged.Residue = vcard.ResidueWithMappings(merged.PropertyTree, merged.NativeMappings)
	return merged.PrepareCanonicalRender()
}

// bindProjectedProperties decides which occurrence each desired owner edits.
// Occurrences that mappings outside this projection will keep holding — other
// sources' mappings, unmanaged fields, and retained owners — are claimed
// before anything else, so no fallback can put a second owner on them. Every
// desired owner with a live persisted mapping is bound next, so a durable
// mapping always wins; only then do the remaining owners try their scoped
// identity fallbacks against the occurrences still unclaimed. Without the
// passes, an earlier row's fallback could take the occurrence a later row's
// mapping points to.
func bindProjectedProperties(
	envelope vcard.ResourceEnvelope, desired []projectedProperty,
	retainedOwners map[string]struct{},
) ([]projectedBinding, error) {
	claimed := heldOccurrences(envelope, desired, retainedOwners)
	bindings, err := bindPersistedMappings(envelope, desired, claimed)
	if err != nil {
		return nil, err
	}
	for index := range bindings {
		if !bindings[index].appended() {
			continue
		}
		identity := matchScopedIdentity(envelope, bindings[index].property, claimed)
		if !identity.IsZero() {
			bindings[index].identity = identity
			claimed[identity.Key()] = struct{}{}
		}
	}
	return bindings, nil
}

// heldOccurrences lists the occurrence keys that mappings outside this
// projection keep: mappings recorded for other sources, mappings on fields the
// projection does not manage, and mappings of owners retained as residue. None
// of those is replaced or retired by the merge (see survivingMappings), so a
// desired owner may not bind to their occurrences; it is appended instead.
func heldOccurrences(
	envelope vcard.ResourceEnvelope, desired []projectedProperty,
	retainedOwners map[string]struct{},
) map[string]struct{} {
	desiredOwners := make(map[string]struct{}, len(desired))
	for _, projected := range desired {
		desiredOwners[ownerKey(envelope.SourceRef, projected.Owner)] = struct{}{}
	}
	held := make(map[string]struct{})
	for _, mapping := range envelope.NativeMappings {
		owner := ownerKey(mapping.SourceRef, mappingOwner(mapping))
		if _, replaced := desiredOwners[owner]; replaced {
			continue
		}
		_, retained := retainedOwners[owner]
		if retained || mapping.SourceRef != envelope.SourceRef ||
			!managedProjectionField(mapping.Table, mapping.Field) {
			held[mapping.Identity.Key()] = struct{}{}
		}
	}
	return held
}

// bindPersistedMappings binds each desired owner to the occurrence its
// persisted mapping names, when that occurrence still exists and nothing
// holds it yet, adding each bound occurrence to claimed.
func bindPersistedMappings(
	envelope vcard.ResourceEnvelope, desired []projectedProperty,
	claimed map[string]struct{},
) ([]projectedBinding, error) {
	occurrences := occurrencesByKey(envelope.PropertyTree)
	mappings := make(map[string]vcard.NativeMapping, len(envelope.NativeMappings))
	for _, mapping := range envelope.NativeMappings {
		mappings[ownerKey(mapping.SourceRef, mappingOwner(mapping))] = mapping
	}
	owners := make(map[string]struct{}, len(desired))
	bindings := make([]projectedBinding, 0, len(desired))
	for _, projected := range desired {
		owner := ownerKey(envelope.SourceRef, projected.Owner)
		if _, duplicate := owners[owner]; duplicate {
			return nil, fmt.Errorf("duplicate projected vCard owner %s", owner)
		}
		owners[owner] = struct{}{}
		binding := projectedBinding{property: projected}
		if mapping, ok := mappings[owner]; ok {
			key := mapping.Identity.Key()
			_, present := occurrences[key]
			_, taken := claimed[key]
			if present && !taken {
				binding.identity = mapping.Identity
				claimed[key] = struct{}{}
			}
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// staleMappedOccurrences lists the occurrences of this source's managed
// owners that no longer project anything and are not retained as residue. An
// occurrence a desired owner already claimed through its identity (a replaced
// row, or a review's edge once the review is accepted) changes owner in place
// instead; deleting it would collide with that owner's edit.
func staleMappedOccurrences(
	envelope vcard.ResourceEnvelope, bindings []projectedBinding,
	retainedOwners map[string]struct{},
) []vcard.PropertyIdentity {
	occurrences := occurrencesByKey(envelope.PropertyTree)
	desired := desiredOwnerKeys(envelope.SourceRef, bindings)
	claimed := claimedKeys(bindings)
	stale := make([]vcard.PropertyIdentity, 0)
	for _, mapping := range envelope.NativeMappings {
		owner := ownerKey(mapping.SourceRef, mappingOwner(mapping))
		if _, stillDesired := desired[owner]; stillDesired {
			continue
		}
		if _, keepAsResidue := retainedOwners[owner]; keepAsResidue {
			continue
		}
		if mapping.SourceRef != envelope.SourceRef ||
			!managedProjectionField(mapping.Table, mapping.Field) {
			continue
		}
		key := mapping.Identity.Key()
		_, present := occurrences[key]
		_, reclaimed := claimed[key]
		if present && !reclaimed {
			stale = append(stale, mapping.Identity)
		}
	}
	return stale
}

// dropRedundantFullName removes the derived FN when the card keeps a
// non-empty FN of its own that no projected owner claims and that is not
// about to be retired: the mandatory FN is then the imported one, and deriving
// another would render two. A derived FN the projection already owns is bound
// through its persisted mapping and is updated in place instead.
func dropRedundantFullName(
	tree []vcard.PropertyOccurrence, bindings []projectedBinding,
	stale []vcard.PropertyIdentity,
) []projectedBinding {
	taken := claimedKeys(bindings)
	for _, identity := range stale {
		taken[identity.Key()] = struct{}{}
	}
	kept := make([]projectedBinding, 0, len(bindings))
	for _, binding := range bindings {
		if binding.property.FullNameFallback && binding.appended() &&
			hasUnclaimedFullName(tree, taken) {
			continue
		}
		kept = append(kept, binding)
	}
	return kept
}

func hasUnclaimedFullName(tree []vcard.PropertyOccurrence, taken map[string]struct{}) bool {
	for _, occurrence := range tree {
		if _, used := taken[occurrence.Identity.Key()]; used {
			continue
		}
		if strings.EqualFold(occurrence.Property.Name, "FN") &&
			strings.TrimSpace(occurrence.Property.RawValue) != "" {
			return true
		}
	}
	return false
}

// projectedEdits turns bindings into property edits. A bound property takes
// its occurrence's identity parameters and carries the parameters its owner
// has no field for; an appended one drops a PROP-ID the card already uses on
// the same property, since PROP-ID must be unique per property name.
func projectedEdits(
	envelope vcard.ResourceEnvelope, bindings []projectedBinding,
) ([]vcard.PropertyEdit, error) {
	occurrences := occurrencesByKey(envelope.PropertyTree)
	usedPropIDs := propIDsInUse(envelope.PropertyTree)
	edits := make([]vcard.PropertyEdit, 0, len(bindings))
	for _, binding := range bindings {
		property := binding.property.Property
		if binding.appended() {
			property = withoutUsedPropID(property, usedPropIDs)
		} else {
			identified, err := propertyWithOccurrenceIdentity(property, binding.identity)
			if err != nil {
				return nil, err
			}
			original := occurrences[binding.identity.Key()].Property
			property = withCarriedParameters(identified, original, binding.property.CarriedParameters)
		}
		edits = append(edits, vcard.PropertyEdit{
			Identity: binding.identity, Property: property,
			OwnedParameters: binding.property.OwnedParameters,
		})
	}
	return edits, nil
}

func propIDsInUse(tree []vcard.PropertyOccurrence) map[string]struct{} {
	used := make(map[string]struct{})
	for _, occurrence := range tree {
		if occurrence.Identity.PropID != nil {
			used[propIDKey(occurrence.Property.Name, *occurrence.Identity.PropID)] = struct{}{}
		}
	}
	return used
}

func propIDKey(propertyName, propID string) string {
	return strings.ToUpper(propertyName) + "\x1f" + propID
}

func withoutUsedPropID(property vcard.Property, used map[string]struct{}) vcard.Property {
	parameters := make([]vcard.Parameter, 0, len(property.Parameters))
	for _, parameter := range property.Parameters {
		if strings.EqualFold(parameter.Name, "PROP-ID") && len(parameter.Values) == 1 {
			key := propIDKey(property.Name, parameter.Values[0].Decoded)
			if _, taken := used[key]; taken {
				continue
			}
			used[key] = struct{}{}
		}
		parameters = append(parameters, parameter)
	}
	property.Parameters = parameters
	return property
}

func withCarriedParameters(property, original vcard.Property, names []string) vcard.Property {
	for _, name := range names {
		if len(property.ParametersNamed(name)) > 0 {
			continue
		}
		property.Parameters = append(property.Parameters, original.ParametersNamed(name)...)
	}
	return property
}

// locateAppendedOccurrences resolves the identity of every appended binding
// from the merged tree. MergeProperties appends in edit order from the
// envelope's next ordinal, so the ordinals are consecutive.
func locateAppendedOccurrences(
	tree []vcard.PropertyOccurrence, nextOrdinal int, bindings []projectedBinding,
) ([]projectedBinding, error) {
	byOrdinal := make(map[int]vcard.PropertyIdentity, len(tree))
	for _, occurrence := range tree {
		byOrdinal[occurrence.Identity.Ordinal] = occurrence.Identity
	}
	for index := range bindings {
		if !bindings[index].appended() {
			continue
		}
		identity, ok := byOrdinal[nextOrdinal]
		if !ok {
			return nil, fmt.Errorf("locate appended vCard occurrence %d", nextOrdinal)
		}
		bindings[index].identity = identity
		nextOrdinal++
	}
	return bindings, nil
}

// rebuildMappings carries the mappings that still apply over from the source
// envelope and records each binding's owner with the registry's handling for
// the property, ordered as their occurrences appear in the merged tree.
func rebuildMappings(
	envelope vcard.ResourceEnvelope, tree []vcard.PropertyOccurrence,
	bindings []projectedBinding, retainedOwners map[string]struct{},
) []vcard.NativeMapping {
	mappings := survivingMappings(envelope, tree, bindings, retainedOwners)
	for _, binding := range bindings {
		mappings = append(mappings, vcard.NativeMapping{
			Identity: binding.identity, SourceRef: envelope.SourceRef,
			Table: binding.property.Owner.Table, RowID: binding.property.Owner.RowID,
			Field: binding.property.Owner.Field, Kind: mappingKind(binding.property.Property.Name),
		})
	}
	order := make(map[string]int, len(tree))
	for index, occurrence := range tree {
		order[occurrence.Identity.Key()] = index
	}
	slices.SortStableFunc(mappings, func(left, right vcard.NativeMapping) int {
		return order[left.Identity.Key()] - order[right.Identity.Key()]
	})
	return mappings
}

// survivingMappings keeps every source mapping that still names an occurrence
// and is not superseded by a binding: retained owners become preserve
// mappings, this source's managed owners that no longer project anything are
// dropped, and mappings recorded for other sources pass through.
func survivingMappings(
	envelope vcard.ResourceEnvelope, tree []vcard.PropertyOccurrence,
	bindings []projectedBinding, retainedOwners map[string]struct{},
) []vcard.NativeMapping {
	desired := desiredOwnerKeys(envelope.SourceRef, bindings)
	present := occurrencesByKey(tree)
	mappings := make([]vcard.NativeMapping, 0, len(envelope.NativeMappings)+len(bindings))
	for _, mapping := range envelope.NativeMappings {
		owner := ownerKey(mapping.SourceRef, mappingOwner(mapping))
		if _, replaced := desired[owner]; replaced {
			continue
		}
		if _, exists := present[mapping.Identity.Key()]; !exists {
			continue
		}
		if _, keepAsResidue := retainedOwners[owner]; keepAsResidue {
			mapping.Kind = vcard.HandlingPreserve
			mappings = append(mappings, mapping)
			continue
		}
		if mapping.SourceRef == envelope.SourceRef &&
			managedProjectionField(mapping.Table, mapping.Field) {
			continue
		}
		mappings = append(mappings, mapping)
	}
	return mappings
}

// mappingKind records the registry's handling for the property an owner
// renders. A property the registry does not treat as owned semantics (an
// unregistered X- property, for one) is native to the row that renders it.
func mappingKind(propertyName string) vcard.HandlingStrategy {
	handling, ok := vcard.PropertyHandling(propertyName)
	if !ok {
		return vcard.HandlingNative
	}
	switch handling.Strategy {
	case vcard.HandlingDerived, vcard.HandlingRelationship:
		return handling.Strategy
	default:
		return vcard.HandlingNative
	}
}

func desiredOwnerKeys(sourceRef string, bindings []projectedBinding) map[string]struct{} {
	keys := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		keys[ownerKey(sourceRef, binding.property.Owner)] = struct{}{}
	}
	return keys
}

func claimedKeys(bindings []projectedBinding) map[string]struct{} {
	keys := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if !binding.appended() {
			keys[binding.identity.Key()] = struct{}{}
		}
	}
	return keys
}

func occurrencesByKey(tree []vcard.PropertyOccurrence) map[string]vcard.PropertyOccurrence {
	occurrences := make(map[string]vcard.PropertyOccurrence, len(tree))
	for _, occurrence := range tree {
		occurrences[occurrence.Identity.Key()] = occurrence
	}
	return occurrences
}

func mappingOwner(mapping vcard.NativeMapping) projectedOwner {
	return projectedOwner{Table: mapping.Table, RowID: mapping.RowID, Field: mapping.Field}
}

func ownerKey(sourceRef string, owner projectedOwner) string {
	return strings.Join(
		[]string{sourceRef, owner.Table, strconv.FormatInt(owner.RowID, 10), owner.Field}, "\x1f",
	)
}

// matchScopedIdentity tries the row's own vCard identity and then any review
// identities it inherited, each only where it applies, and returns the first
// unclaimed occurrence one of them names.
func matchScopedIdentity(
	envelope vcard.ResourceEnvelope, projected projectedProperty,
	claimed map[string]struct{},
) vcard.PropertyIdentity {
	candidates := append([]scopedIdentity{projected.Identity}, projected.ReviewIdentities...)
	for _, candidate := range candidates {
		if !identityAppliesToResource(candidate, envelope.SourceRef, envelope.SourceResourceUID) {
			continue
		}
		identity := matchReducedIdentity(envelope.PropertyTree, candidate.identity, claimed)
		if !identity.IsZero() {
			return identity
		}
	}
	return vcard.PropertyIdentity{}
}

// identityAppliesToResource reports whether a scoped identity may claim an
// occurrence in the given resource: only when its complete resource pair
// matches, or when it records no resource at all. A partial pair (a source
// without a resource UID, or the reverse) names an address book but not a
// card in it, so it claims nothing: the row is appended and any occurrence it
// was read from stays as residue. Rows the import path writes carry both.
func identityAppliesToResource(
	candidate scopedIdentity, sourceRef, sourceResourceUID string,
) bool {
	if isBlank(candidate.sourceRef) && isBlank(candidate.sourceResourceUID) {
		return true
	}
	return candidate.sourceRef != nil && candidate.sourceResourceUID != nil &&
		*candidate.sourceRef == sourceRef && *candidate.sourceResourceUID == sourceResourceUID
}

// matchReducedIdentity finds the first unclaimed occurrence the identity
// names.
func matchReducedIdentity(
	properties []vcard.PropertyOccurrence,
	identity store.VCardIdentity,
	claimed map[string]struct{},
) vcard.PropertyIdentity {
	if identity.Property == "" {
		return vcard.PropertyIdentity{}
	}
	for _, occurrence := range properties {
		if _, used := claimed[occurrence.Identity.Key()]; used {
			continue
		}
		if identityNamesOccurrence(identity, occurrence) {
			return occurrence.Identity
		}
	}
	return vcard.PropertyIdentity{}
}

// identityNamesOccurrence reports whether the property name, group, PROP-ID,
// PID, and ALTID all match. Names and groups are case-insensitive on the wire
// (RFC 6350 section 3.3); IDs are not.
func identityNamesOccurrence(
	identity store.VCardIdentity, occurrence vcard.PropertyOccurrence,
) bool {
	return strings.EqualFold(identity.Property, occurrence.Property.Name) &&
		strings.EqualFold(deref(identity.Group), occurrence.Identity.Group) &&
		deref(identity.PropID) == deref(occurrence.Identity.PropID) &&
		slices.Equal(identity.PID, occurrence.Identity.PID) &&
		deref(identity.AltID) == deref(occurrence.Identity.AltID)
}

// propertyWithOccurrenceIdentity re-addresses a projected property to the
// occurrence it binds to: the occurrence's group and identity parameters
// replace whatever the row's own identity rendered.
func propertyWithOccurrenceIdentity(
	property vcard.Property, identity vcard.PropertyIdentity,
) (vcard.Property, error) {
	property.Group = identity.Group
	parameters := make([]vcard.Parameter, 0, len(property.Parameters))
	for _, parameter := range property.Parameters {
		switch strings.ToUpper(parameter.Name) {
		case "PROP-ID", "PID", "ALTID":
			continue
		default:
			parameters = append(parameters, parameter)
		}
	}
	property.Parameters = parameters
	return appendIdentityParameters(property, identity.PropID, identity.PID, identity.AltID)
}
