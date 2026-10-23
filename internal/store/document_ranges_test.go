package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentRangesSurviveRetryAndFenceWrites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "range-first", ProfileID: profile.ID, CanonicalBlobHash: hash,
		ExtractionInputKey: "original", LeaseOwner: "range-worker", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1,
	})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	requirements.NoError(err)
	checkpoint := documentRangeCheckpoint(t)
	requirements.NoError(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20))
	stale := claim
	stale.LeaseFence++
	requirements.ErrorIs(f.Store.SaveDocumentExtractionRange(t.Context(), stale, checkpoint, 1<<20), store.ErrDocumentExtractionFenceLost)
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extraction_claims SET lease_until = ? WHERE extraction_id = ?"), time.Now().UTC().Add(-time.Minute), claim.ExtractionID)
	requirements.NoError(err)
	requirements.ErrorIs(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20), store.ErrDocumentExtractionFenceLost)
	requirements.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{Claim: claim, ReasonCode: "provider_timeout", Terminal: true}))
	_, err = f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	requirements.NoError(err)
	input.ExtractionID = "range-retry"
	next, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	requirements.NoError(err)
	ranges, err := f.Store.LoadDocumentExtractionRanges(t.Context(), next, 1<<20)
	requirements.NoError(err)
	assertions.Equal([]store.DocumentExtractionRange{checkpoint}, ranges)
	var count int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges WHERE extraction_id = 'range-first'").Scan(&count))
	assertions.Zero(count, "checkpoint plaintext belongs only to the replacement attempt")
	assertions.Zero(documentFTSMatchCount(t, f.Store, "sentinel"))
	requirements.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, next, "published sentinel", strings.Repeat("e", 64))))
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
	assertions.Zero(count)
}

func TestDocumentRangesGCRebuildAndPurge(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{ExtractionID: "range-gc", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", LeaseOwner: "worker", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	requirements.NoError(err)
	checkpoint := documentRangeCheckpoint(t)
	requirements.ErrorContains(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1), "byte bound")
	requirements.NoError(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20))
	requirements.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{Claim: claim, ReasonCode: "provider_timeout", Terminal: true}))
	_, err = f.Store.GarbageCollectDocumentDerivatives(t.Context(), time.Now().UTC().Add(time.Hour), 100)
	requirements.NoError(err)
	var count int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
	assertions.Equal(1, count, "retain the latest unfinished live ranges")
	other := profile
	other.ID = "range-other-profile"
	other.Fingerprint = strings.Repeat("f", 64)
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), other)
	requirements.NoError(err)
	requirements.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{ProfileID: other.ID, ProfileFingerprint: other.Fingerprint, RetentionPosture: other.RetentionPosture, TrainingPosture: other.TrainingPosture}))
	otherInput := input
	otherInput.ProfileID = other.ID
	otherInput.ExtractionID = "range-other-attempt"
	otherClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), otherInput)
	requirements.NoError(err)
	otherRanges, err := f.Store.LoadDocumentExtractionRanges(t.Context(), otherClaim, 1<<20)
	requirements.NoError(err)
	assertions.Empty(otherRanges)
	_, err = f.Store.StartDocumentExtractionRebuild(t.Context(), "range-rebuild", profile.ID, "original", []string{"application/pdf"}, nil)
	requirements.NoError(err)
	input.ExtractionID = "range-rebuild-attempt"
	input.RebuildID = "range-rebuild"
	rebuilt, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	requirements.NoError(err)
	saved, err := f.Store.LoadDocumentExtractionRanges(t.Context(), rebuilt, 1<<20)
	requirements.NoError(err)
	assertions.Empty(saved)
	_, err = f.Store.PurgeDocumentDerivedByHash(t.Context(), hash)
	requirements.NoError(err)
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
	assertions.Zero(count)
}

func TestSchemaVersionDocumentRangesUpgrade(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().Exec("DROP TABLE document_extraction_ranges")
	requirements.NoError(err)
	_, err = f.Store.DB().Exec("UPDATE archive_metadata SET value='11' WHERE key='schema_version'")
	requirements.NoError(err)
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	version, err := f.Store.SchemaVersionContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(11, version)
	var count int
	requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
	assertions.Zero(count)
}

func documentRangeCheckpoint(t *testing.T) store.DocumentExtractionRange {
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
	return store.DocumentExtractionRange{FirstPage: 1, LastPage: 50, TotalPages: 51, Evidence: encoded, EvidenceChecksum: checksum, Markdown: "saved sentinel", CharCount: 714}
}
