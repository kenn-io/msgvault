package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalidProvenance reports a source value outside the fixed provenance
// vocabulary. ErrConfidenceScope reports a confidence attached to a declared
// value, or one outside [0, 1].
var (
	ErrInvalidProvenance = errors.New("invalid source")
	ErrConfidenceScope   = errors.New("confidence is only meaningful for derived or suggested values")
)

// Provenance records which subsystem asserted a mutable curated fact.
type Provenance string

const (
	ProvenanceUser               Provenance = "user"
	ProvenanceCardDAVImport      Provenance = "carddav_import"
	ProvenanceVCardImport        Provenance = "vcard_import"
	ProvenanceArchiveObservation Provenance = "archive_observation"
	ProvenanceExtraction         Provenance = "extraction"
	ProvenanceEnrichment         Provenance = "enrichment"
	ProvenanceSystem             Provenance = "system"
)

// AllProvenances is the ordered provenance vocabulary.
var AllProvenances = []Provenance{
	ProvenanceUser,
	ProvenanceCardDAVImport,
	ProvenanceVCardImport,
	ProvenanceArchiveObservation,
	ProvenanceExtraction,
	ProvenanceEnrichment,
	ProvenanceSystem,
}

// Valid reports whether p is one of AllProvenances.
func (p Provenance) Valid() bool {
	return slices.Contains(AllProvenances, p)
}

// IsDeclared reports whether a person or curated address book asserted the value.
func (p Provenance) IsDeclared() bool {
	switch p {
	case ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport:
		return true
	default:
		return false
	}
}

// ParseProvenance trims whitespace and parses an exact provenance value.
func ParseProvenance(raw string) (Provenance, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("%w: source is required", ErrInvalidProvenance)
	}
	candidate := Provenance(trimmed)
	if !candidate.Valid() {
		return "", fmt.Errorf("%w: unknown source %q", ErrInvalidProvenance, trimmed)
	}
	return candidate, nil
}

// ProvenanceCheckValues renders the vocabulary for portable SQL CHECK clauses.
func ProvenanceCheckValues() string {
	quoted := make([]string, 0, len(AllProvenances))
	for _, provenance := range AllProvenances {
		quoted = append(quoted, "'"+string(provenance)+"'")
	}
	return strings.Join(quoted, ", ")
}
