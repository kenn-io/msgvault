package storetest

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/msgvault/internal/store"
)

// DocumentRangeCheckpoint builds canonical evidence for a completed PDF range.
func DocumentRangeCheckpoint(t *testing.T) store.DocumentExtractionRange {
	t.Helper()
	source := document.SourceEvidenceV1{ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidenceComplete, Family: "pdf", UnitKind: document.EvidenceUnitPage}
	for page := 1; page <= 50; page++ {
		source.Units = append(source.Units, document.SourceEvidenceUnitV1{Order: page - 1, Text: "saved sentinel", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: int64(page), End: int64(page)}})
	}
	policy, err := document.NewEvidencePolicy(10000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(source, policy)
	require.NoError(t, err)
	encoded, checksum, err := document.MarshalNormalizedEvidenceV1(normalized)
	require.NoError(t, err)
	return store.DocumentExtractionRange{FirstPage: 1, LastPage: 50, TotalPages: 51, Evidence: encoded, EvidenceChecksum: checksum, CharCount: 50 * len("saved sentinel")}
}
