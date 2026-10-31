package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseProvenance(t *testing.T) {
	for _, provenance := range AllProvenances {
		parsed, err := ParseProvenance("  " + string(provenance) + "  ")
		require.NoError(t, err)
		assert.Equal(t, provenance, parsed)
	}

	for _, raw := range []string{"", "   ", "User", "guessed", "USER"} {
		_, err := ParseProvenance(raw)
		require.ErrorIs(t, err, ErrInvalidProvenance)
	}
}

func TestProvenanceIsDeclaredSplitsDeclaredFromDerived(t *testing.T) {
	for _, declared := range []Provenance{
		ProvenanceUser, ProvenanceCardDAVImport, ProvenanceVCardImport,
	} {
		assert.True(t, declared.IsDeclared(), "%s is declared data", declared)
	}
	for _, derived := range []Provenance{
		ProvenanceArchiveObservation, ProvenanceExtraction,
		ProvenanceEnrichment, ProvenanceSystem,
	} {
		assert.False(t, derived.IsDeclared(), "%s is derived or suggested", derived)
	}
}

func TestProvenanceCheckValuesCoversEveryVocabularyEntry(t *testing.T) {
	clause := ProvenanceCheckValues()
	for _, provenance := range AllProvenances {
		assert.Contains(t, clause, "'"+string(provenance)+"'")
	}
	assert.Equal(t,
		"'user', 'carddav_import', 'vcard_import', 'archive_observation', "+
			"'extraction', 'enrichment', 'system'", clause)
}
