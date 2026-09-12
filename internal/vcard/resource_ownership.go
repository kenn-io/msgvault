package vcard

import (
	"encoding/json"
	"errors"
)

var ErrResourceOwnershipMismatch = errors.New("vCard resource ownership cannot be matched unambiguously")

// RebindResourceOwnership matches equal properties one-to-one onto fresh
// canonical bytes. Unknown and changed properties remain residue. requireAll is
// used for confirmation of an outgoing artifact; an inbound refresh may drop
// ownership whose original value is no longer present or distinguishable.
func RebindResourceOwnership(prepared, canonical ResourceEnvelope, requireAll bool) (ResourceEnvelope, error) {
	preparedByIdentity := make(map[string]PropertyOccurrence)
	preparedPosition := make(map[string]int)
	preparedCount := make(map[string]int)
	canonicalBySemantic := make(map[string][]PropertyOccurrence)
	key := func(version Version, occurrence PropertyOccurrence) string {
		encoded, _ := json.Marshal(NormalizeSemanticProperty(version, occurrence.Property)) //nolint:errchkjson // SemanticProperty holds only strings and string slices
		return string(encoded)
	}
	for _, occurrence := range prepared.PropertyTree {
		preparedByIdentity[occurrence.Identity.Key()] = occurrence
		k := key(prepared.RenderMetadata.StoredVersion, occurrence)
		preparedPosition[occurrence.Identity.Key()] = preparedCount[k]
		preparedCount[k]++
	}
	for _, occurrence := range canonical.PropertyTree {
		k := key(canonical.RenderMetadata.StoredVersion, occurrence)
		canonicalBySemantic[k] = append(canonicalBySemantic[k], occurrence)
	}
	canonical.NativeMappings = nil
	for _, mapping := range prepared.NativeMappings {
		occurrence, ok := preparedByIdentity[mapping.Identity.Key()]
		if !ok {
			return ResourceEnvelope{}, ErrResourceOwnershipMismatch
		}
		k := key(prepared.RenderMetadata.StoredVersion, occurrence)
		matches := canonicalBySemantic[k]
		if len(matches) != preparedCount[k] {
			if requireAll {
				return ResourceEnvelope{}, ErrResourceOwnershipMismatch
			}
			continue
		}
		// The semantic key includes wire identity (group, PID, PROP-ID, ALTID).
		// Equal duplicates retain their occurrence order, independently of the
		// order in which native mappings are stored.
		mapping.Identity = matches[preparedPosition[mapping.Identity.Key()]].Identity
		canonical.NativeMappings = append(canonical.NativeMappings, mapping)
	}
	canonical.Residue = ResidueWithMappings(canonical.PropertyTree, canonical.NativeMappings)
	if _, err := MarshalResourceMetadata(canonical); err != nil {
		return ResourceEnvelope{}, err
	}
	return canonical, nil
}
