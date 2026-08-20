package vcard

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ResourceMetadataFormatVersion identifies the persisted JSON contract for
// the normalized parts of a resource envelope. Raw and stored bytes, resource
// identifiers, hashes, and the store revision live in ordinary table columns.
const ResourceMetadataFormatVersion = 1

// resourceMetadata is the versioned document MarshalResourceMetadata writes.
// The nested types serialize through their own json tags, which are part of
// the persisted contract.
type resourceMetadata struct {
	FormatVersion         int                  `json:"format_version"`
	NextOccurrenceOrdinal int                  `json:"next_occurrence_ordinal"`
	PropertyTree          []PropertyOccurrence `json:"property_tree"`
	NativeMappings        []NativeMapping      `json:"native_mappings"`
	Residue               []PropertyOccurrence `json:"residue"`
	Render                RenderMetadata       `json:"render"`
}

// MarshalResourceMetadata serializes only the normalized envelope state using
// a versioned, stable JSON shape.
func MarshalResourceMetadata(envelope ResourceEnvelope) ([]byte, error) {
	if err := validateResourceMetadata(envelope); err != nil {
		return nil, err
	}
	data, err := json.Marshal(resourceMetadata{
		FormatVersion:         ResourceMetadataFormatVersion,
		NextOccurrenceOrdinal: envelope.NextOccurrenceOrdinal,
		PropertyTree:          envelope.PropertyTree,
		NativeMappings:        envelope.NativeMappings,
		Residue:               envelope.Residue,
		Render:                envelope.RenderMetadata,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal vCard resource metadata: %w", err)
	}
	return data, nil
}

// UnmarshalResourceMetadata decodes the normalized portion of an envelope.
// The caller supplies resource identifiers, bodies, hashes, and store revision
// from their dedicated columns.
func UnmarshalResourceMetadata(data []byte) (ResourceEnvelope, error) {
	var metadata resourceMetadata
	if err := decodeStrictJSON(data, &metadata); err != nil {
		return ResourceEnvelope{}, fmt.Errorf("decode vCard resource metadata: %w", err)
	}
	if metadata.FormatVersion != ResourceMetadataFormatVersion {
		return ResourceEnvelope{}, fmt.Errorf(
			"unsupported vCard resource metadata format %d", metadata.FormatVersion,
		)
	}
	envelope := ResourceEnvelope{
		NextOccurrenceOrdinal: metadata.NextOccurrenceOrdinal,
		PropertyTree:          metadata.PropertyTree,
		NativeMappings:        metadata.NativeMappings,
		Residue:               metadata.Residue,
		RenderMetadata:        metadata.Render,
	}
	if err := validateResourceMetadata(envelope); err != nil {
		return ResourceEnvelope{}, err
	}
	return envelope, nil
}

// decodeStrictJSON decodes exactly one JSON value into target and rejects
// unknown fields and trailing content.
func decodeStrictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing content: %w", err)
	}
	return errors.New("multiple JSON values")
}

// propertyJSON is the wire form of Property: its json-tagged fields plus a
// base64 spelling of RawValue. RawValue may hold bytes that are not valid
// UTF-8 (a CHARSET-declared vCard 2.1/3.0 value the decoder admits verbatim);
// encoding/json would replace those with U+FFFD, so such a value travels as
// raw_value_base64 instead.
type propertyJSON struct {
	propertyFields

	RawValueBase64 string `json:"raw_value_base64,omitempty"`
}

// propertyFields is Property without its methods, so the wire form can embed
// it without recursing into MarshalJSON.
type propertyFields Property

// MarshalJSON implements json.Marshaler; see propertyJSON.
func (p Property) MarshalJSON() ([]byte, error) {
	wire := propertyJSON{propertyFields: propertyFields(p)}
	if !utf8.ValidString(p.RawValue) {
		wire.RawValue = ""
		wire.RawValueBase64 = base64.StdEncoding.EncodeToString([]byte(p.RawValue))
	}
	return json.Marshal(wire)
}

// UnmarshalJSON implements json.Unmarshaler; see propertyJSON.
func (p *Property) UnmarshalJSON(data []byte) error {
	var wire propertyJSON
	if err := decodeStrictJSON(data, &wire); err != nil {
		return err
	}
	if wire.RawValueBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(wire.RawValueBase64)
		if err != nil {
			return fmt.Errorf("decode raw_value_base64: %w", err)
		}
		wire.RawValue = string(raw)
	}
	*p = Property(wire.propertyFields)
	return nil
}

func validateResourceMetadata(envelope ResourceEnvelope) error {
	if envelope.NextOccurrenceOrdinal < nextPropertyOrdinal(envelope.PropertyTree) {
		return errors.New("vCard resource occurrence high-water mark is stale")
	}
	if err := validateRenderMetadata(envelope.RenderMetadata); err != nil {
		return err
	}
	// The merge, reconcile, and residue logic all key on the ordinal alone,
	// and every producer derives the identity and classification from the
	// property. Stored metadata that violates any of that would silently
	// attach mappings or residue to the wrong occurrence, so it is refused.
	present := make(map[string]PropertyOccurrence, len(envelope.PropertyTree))
	ordinals := make(map[int]struct{}, len(envelope.PropertyTree))
	for _, occurrence := range envelope.PropertyTree {
		ordinal := occurrence.Identity.Ordinal
		if ordinal < 0 {
			return fmt.Errorf("negative vCard occurrence ordinal %d", ordinal)
		}
		if _, duplicate := ordinals[ordinal]; duplicate {
			return fmt.Errorf("duplicate vCard occurrence ordinal %d", ordinal)
		}
		ordinals[ordinal] = struct{}{}
		if !occurrence.Identity.Equal(propertyIdentity(ordinal, occurrence.Property)) {
			return fmt.Errorf(
				"vCard occurrence %d identity does not match its property", ordinal,
			)
		}
		if occurrence.Classification != classifyProperty(occurrence.Property) {
			return fmt.Errorf(
				"vCard occurrence %d classification does not match its property", ordinal,
			)
		}
		present[occurrence.Identity.Key()] = occurrence
	}
	if err := validateNativeMappings(envelope.NativeMappings, present); err != nil {
		return err
	}
	return validateResidue(envelope.Residue, present)
}

func validateRenderMetadata(metadata RenderMetadata) error {
	if metadata.CanonicalVersion != Version40 {
		return fmt.Errorf(
			"vCard resource canonical version must be 4.0, got %q", metadata.CanonicalVersion,
		)
	}
	switch metadata.StoredVersion {
	case Version21, Version30, Version40:
		return nil
	default:
		return fmt.Errorf("unsupported stored vCard version %q", metadata.StoredVersion)
	}
}

func validateNativeMappings(
	mappings []NativeMapping, present map[string]PropertyOccurrence,
) error {
	mappedIdentities := make(map[string]struct{}, len(mappings))
	mappedOwners := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		identityKey := mapping.Identity.Key()
		if _, ok := present[identityKey]; !ok {
			return errors.New("vCard native mapping references a missing occurrence")
		}
		if mapping.Table == "" || mapping.RowID <= 0 || mapping.Field == "" ||
			!validHandlingStrategy(mapping.Kind) {
			return errors.New("vCard native mapping has an invalid owner or handling strategy")
		}
		if _, duplicate := mappedIdentities[identityKey]; duplicate {
			return errors.New("vCard property occurrence is claimed by multiple native mappings")
		}
		mappedIdentities[identityKey] = struct{}{}
		ownerKey := strings.Join([]string{
			mapping.SourceRef, mapping.Table, strconv.FormatInt(mapping.RowID, 10),
			mapping.Field,
		}, "\x1f")
		if _, duplicate := mappedOwners[ownerKey]; duplicate {
			return errors.New("vCard native owner has multiple occurrence mappings")
		}
		mappedOwners[ownerKey] = struct{}{}
	}
	return nil
}

func validateResidue(residue []PropertyOccurrence, present map[string]PropertyOccurrence) error {
	residueIdentities := make(map[string]struct{}, len(residue))
	for _, occurrence := range residue {
		key := occurrence.Identity.Key()
		treeOccurrence, ok := present[key]
		if !ok {
			return errors.New("vCard residue references a missing occurrence")
		}
		if _, duplicate := residueIdentities[key]; duplicate {
			return errors.New("duplicate vCard residue occurrence")
		}
		residueIdentities[key] = struct{}{}
		if !reflect.DeepEqual(treeOccurrence, occurrence) {
			return errors.New("vCard residue occurrence does not match the property tree")
		}
	}
	return nil
}
