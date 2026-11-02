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

func TestDocumentRangesCharacterLimit(t *testing.T) {
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "range-char-limit", ProfileID: profile.ID, CanonicalBlobHash: hash,
		ExtractionInputKey: "original", LeaseOwner: "worker", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1,
	})
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(t, err)
	policy, err := document.NewEvidencePolicy(100)
	require.NoError(t, err)
	for page := 1; page <= 3; page++ {
		source := document.SourceEvidenceV1{
			ContractVersion: document.SourceEvidenceContractV1, Completeness: document.EvidencePartial, Family: "pdf", UnitKind: document.EvidenceUnitPage,
			Omissions: []document.SourceEvidenceOmissionV1{{Kind: document.EvidenceOmissionField, Field: "page_range", Reason: "requested page selection"}},
			Units:     []document.SourceEvidenceUnitV1{{Order: 0, Text: "éééééééé", Locator: document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: int64(page), End: int64(page)}}},
		}
		if page > 1 {
			source.Omissions = append(source.Omissions, document.SourceEvidenceOmissionV1{Kind: document.EvidenceOmissionUnit, Reason: "pages precede the requested range", Locator: &document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorPage, IndexOrigin: document.EvidenceIndexOriginOne, Start: 1, End: int64(page - 1)}})
		}
		evidence, err := document.NormalizeEvidenceV1(source, policy)
		require.NoError(t, err)
		encoded, checksum, err := document.MarshalNormalizedEvidenceV1(evidence)
		require.NoError(t, err)
		checkpoint := store.DocumentExtractionRange{FirstPage: page, LastPage: page, TotalPages: 3, Evidence: encoded, EvidenceChecksum: checksum, CharCount: 8}
		err = f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20, 16)
		if page == 3 {
			require.ErrorIs(t, err, store.ErrDocumentRangeChars)
		} else {
			require.NoError(t, err)
			require.NoError(t, f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20, 16), "saving the same range twice must not consume more characters")
		}
	}
	ranges, err := f.Store.LoadDocumentExtractionRanges(t.Context(), claim, 1<<20)
	require.NoError(t, err)
	require.Len(t, ranges, 2)
	assert.Equal(t, 1, ranges[0].FirstPage)
	assert.Equal(t, 2, ranges[1].FirstPage)
	var chars int
	require.NoError(t, f.Store.DB().QueryRow("SELECT SUM(char_count) FROM document_extraction_ranges").Scan(&chars))
	assert.Equal(t, 16, chars)
}

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
	checkpoint := storetest.DocumentRangeCheckpoint(t)
	requirements.ErrorIs(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1, 1<<20), store.ErrDocumentRangeBytes)
	requirements.NoError(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20, 1<<20))
	status, err := f.Store.GetDocumentIndexStatusForScope(t.Context(), profile.ID, "original", profile.AllowedMediaTypes, nil)
	requirements.NoError(err)
	assertions.Equal(int64(1), status.StoredPlaintextRanges)
	stale := claim
	stale.LeaseFence++
	requirements.ErrorIs(f.Store.SaveDocumentExtractionRange(t.Context(), stale, checkpoint, 1<<20, 1<<20), store.ErrDocumentExtractionFenceLost)
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extraction_claims SET lease_until = ? WHERE extraction_id = ?"), time.Now().UTC().Add(-time.Minute), claim.ExtractionID)
	requirements.NoError(err)
	requirements.ErrorIs(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20, 1<<20), store.ErrDocumentExtractionFenceLost)
	requirements.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{Claim: claim, ReasonCode: "provider_timeout", Terminal: true}))
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
	requirements.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{Claim: rebuilt, ReasonCode: "provider_timeout", Terminal: true}))
	input.RebuildID = ""
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

func TestDocumentRangesCleanup(t *testing.T) {
	for _, action := range []string{"superseding rebuild", "purge-derived", "source removed", "target changed", "retryable failure"} {
		t.Run(action, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			profile, hash := seedDocumentPublicationAuthority(t, f)
			input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{ExtractionID: "range-gc", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", LeaseOwner: "worker", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1})
			claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
			requirements.NoError(err)
			checkpoint := storetest.DocumentRangeCheckpoint(t)
			requirements.NoError(f.Store.SaveDocumentExtractionRange(t.Context(), claim, checkpoint, 1<<20, 1<<20))
			failure := store.DocumentExtractionFailure{Claim: claim, ReasonCode: "provider_timeout", Terminal: action != "retryable failure"}
			if action == "retryable failure" {
				failure.RetryAt = time.Now().UTC().Add(time.Minute)
			}
			requirements.NoError(f.Store.FailDocumentExtraction(t.Context(), failure))
			if action == "retryable failure" {
				_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET updated_at = ? WHERE id = ?"), time.Now().UTC().Add(-8*24*time.Hour), claim.ExtractionID)
				requirements.NoError(err)
				_, err = f.Store.GarbageCollectDocumentRangeCheckpoints(t.Context(), time.Now().UTC().Add(-7*24*time.Hour), 100)
				requirements.NoError(err)
				_, err = f.Store.GarbageCollectDocumentDerivatives(t.Context(), time.Now().UTC().Add(-7*24*time.Hour), 100)
				requirements.NoError(err)
				var count int
				requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
				assertions.Equal(1, count, "retain checkpoints for the latest retryable attempt on the live target")
				return
			}
			_, err = f.Store.GarbageCollectDocumentRangeCheckpoints(t.Context(), time.Now().UTC().Add(time.Hour), 100)
			requirements.NoError(err)
			_, err = f.Store.GarbageCollectDocumentDerivatives(t.Context(), time.Now().UTC().Add(time.Hour), 100)
			requirements.NoError(err)
			var count int
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
			assertions.Equal(1, count, "retain the latest unfinished live ranges")
			if action == "target changed" {
				other := profile
				other.ID = "range-new-target"
				other.Fingerprint = strings.Repeat("f", 64)
				_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), other)
				requirements.NoError(err)
				requirements.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
					ProfileID: other.ID, ProfileFingerprint: other.Fingerprint,
					RetentionPosture: other.RetentionPosture, TrainingPosture: other.TrainingPosture,
				}))
				cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
				_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET updated_at = ? WHERE id = ?"), time.Now().UTC().Add(-8*24*time.Hour), claim.ExtractionID)
				requirements.NoError(err)
				rangesRemoved, err := f.Store.GarbageCollectDocumentRangeCheckpoints(t.Context(), cutoff, 100)
				requirements.NoError(err)
				assertions.Equal(1, rangesRemoved)
				collected, err := f.Store.GarbageCollectDocumentDerivatives(t.Context(), cutoff, 100)
				requirements.NoError(err)
				assertions.Zero(collected.ExtractionsRemoved)
				requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
				assertions.Zero(count, "collect checkpoints after their profile stops being the target")
				requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM document_extractions WHERE id = ? AND state = 'terminal'"), claim.ExtractionID).Scan(&count))
				assertions.Equal(1, count, "retain terminal suppression while the source is live")
				return
			}
			if action == "purge-derived" {
				purged, err := f.Store.PurgeDocumentDerivedByHash(t.Context(), hash)
				requirements.NoError(err)
				assertions.Equal(1, purged.ExtractionsRemoved)
				requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
				assertions.Zero(count)
				return
			}
			if action == "source removed" {
				_, _, err = f.Store.RemoveSourceSerialized(t.Context(), f.Source.ID)
				requirements.NoError(err)
				requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
				assertions.Equal(1, count)
				_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET updated_at = ? WHERE id = ?"), time.Now().UTC().Add(-8*24*time.Hour), claim.ExtractionID)
				requirements.NoError(err)
				_, err = f.Store.GarbageCollectDocumentRangeCheckpoints(t.Context(), time.Now().UTC().Add(-7*24*time.Hour), 100)
				requirements.NoError(err)
				_, err = f.Store.GarbageCollectDocumentDerivatives(t.Context(), time.Now().UTC().Add(-7*24*time.Hour), 100)
				requirements.NoError(err)
				requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
				assertions.Zero(count)
				return
			}
			_, err = f.Store.StartDocumentExtractionRebuild(t.Context(), "range-rebuild", profile.ID, "original", []string{"application/pdf"}, nil)
			requirements.NoError(err)
			input.ExtractionID = "range-rebuild-attempt"
			input.RebuildID = "range-rebuild"
			rebuilt, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
			requirements.NoError(err)
			requirements.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, rebuilt, "rebuilt sentinel", strings.Repeat("e", 64))))
			cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
			_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_extractions SET updated_at = ? WHERE id = ?"), time.Now().UTC().Add(-8*24*time.Hour), claim.ExtractionID)
			requirements.NoError(err)
			_, err = f.Store.GarbageCollectDocumentRangeCheckpoints(t.Context(), cutoff, 100)
			requirements.NoError(err)
			_, err = f.Store.GarbageCollectDocumentDerivatives(t.Context(), cutoff, 100)
			requirements.NoError(err)
			requirements.NoError(f.Store.DB().QueryRow("SELECT COUNT(*) FROM document_extraction_ranges").Scan(&count))
			assertions.Zero(count, "published replacement makes old terminal ranges collectible")
			requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM document_extractions WHERE id = ? AND state = 'terminal'"), claim.ExtractionID).Scan(&count))
			assertions.Equal(1, count, "retain terminal suppression while the source is live")
		})
	}
}
