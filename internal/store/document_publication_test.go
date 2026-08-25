package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbankdocument "go.kenn.io/docbank/document"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentExtractionPublicationRoundTripsNormalizedV3Identity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-normalized-v3", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-normalized-v3", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	policy, err := docbankdocument.NewNormalizePolicy(10_000)
	require.NoError(err)
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{
		Family: "pdf", UnitKind: "page", Units: []docbankdocument.SourceUnit{{
			Index: 0, Markdown: "# Evidence\n\n" + strings.Repeat("stored identity evidence ", 300),
		}},
	}, policy)
	require.NoError(err)
	require.Greater(len(normalized.Chunks), 1)
	publication := publicationFor(t, claim, normalized.Chunks[0].Text, normalized.Chunks[0].Checksum)
	publication.ManifestChecksum = normalized.Checksum
	publication.NormalizationVersion = normalized.PolicyVersion
	publication.DocumentFamily = normalized.Family
	publication.UnitKind = normalized.UnitKind
	publication.NormalizedTruncated = normalized.Truncated
	publication.Units[0] = store.DocumentPublishedUnit{
		Index: 0, Kind: normalized.Units[0].Kind, Text: normalized.Units[0].Text,
		Header: normalized.Units[0].Header, Footer: normalized.Units[0].Footer,
		Width: normalized.Units[0].Dimensions.Width, Height: normalized.Units[0].Dimensions.Height,
		DPI: normalized.Units[0].Dimensions.DPI, Checksum: normalized.Units[0].Checksum,
		CharCount: normalized.Units[0].CharCount, Truncated: normalized.Units[0].Truncated,
		HeadingMarks: normalized.Units[0].HeadingMarks,
	}
	publication.Chunks = make([]store.DocumentPublishedChunk, len(normalized.Chunks))
	for index, chunk := range normalized.Chunks {
		publication.Chunks[index] = store.DocumentPublishedChunk{
			Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text,
			HeadingPath: chunk.HeadingPath, Checksum: chunk.Checksum,
			CharCount: chunk.CharCount, Truncated: chunk.Truncated,
			Spans: []store.DocumentPublishedSpan{{
				UnitIndex: chunk.Spans[0].UnitIndex,
				CharStart: chunk.Spans[0].CharStart,
				CharEnd:   chunk.Spans[0].CharEnd,
			}},
		}
	}
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publication))

	logs := captureAttachmentQueryLogs(t)
	loaded, err := f.Store.LoadNormalizedDocument(t.Context(), claim.ExtractionID)

	require.NoError(err)
	assert.Equal(normalized, loaded)
	assert.Equal(1, strings.Count(logs.String(), "FROM document_chunk_spans"),
		"normalized document loading must fetch every chunk span in one query")
}

func TestDocumentExtractionPublicationKeepsOldHeadUntilAtomicSwitch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)

	firstClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-first", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-first", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	assert.Equal(int64(1), firstClaim.LeaseFence)

	_, err = f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-concurrent", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-concurrent", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.ErrorIs(err, store.ErrDocumentExtractionClaimed)
	var concurrentRows int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM document_extractions WHERE id = ?`), "extraction-concurrent").Scan(&concurrentRows))
	assert.Zero(concurrentRows, "failed owner claims must roll back their staging revision")

	wrongFence := firstClaim
	wrongFence.LeaseFence++
	err = f.Store.RenewDocumentExtractionClaim(t.Context(), wrongFence, time.Now().UTC().Add(15*time.Minute))
	require.ErrorIs(err, store.ErrDocumentExtractionFenceLost)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t,
		firstClaim, "old searchable quasar evidence", strings.Repeat("d", 64),
	)))
	assert.Equal(1, documentFTSMatchCount(t, f.Store, "quasar"))
	assert.Equal([]string{"old searchable quasar evidence"}, currentDocumentTexts(t, f, profile.ID, hash))

	secondClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-second", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-second", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	assert.Equal([]string{"old searchable quasar evidence"}, currentDocumentTexts(t, f, profile.ID, hash),
		"a staging replacement must not hide the ready head")

	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t,
		secondClaim, "new searchable nebula evidence", strings.Repeat("e", 64),
	)))
	assert.Equal([]string{"new searchable nebula evidence"}, currentDocumentTexts(t, f, profile.ID, hash))
	assert.Equal(1, documentFTSMatchCount(t, f.Store, "quasar"),
		"old immutable derivatives may remain until GC but are unreachable through the head")
	assert.Equal(1, documentFTSMatchCount(t, f.Store, "nebula"))

	revision, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(int64(3), revision, "one occurrence and two head publications")
}

func TestDocumentExtractionPublicationRejectsInvalidSpanBeforeMutation(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-invalid", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-invalid", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	publication := publicationFor(t, claim, "short text", strings.Repeat("f", 64))
	publication.Chunks[0].Spans[0].CharEnd = 100
	require.ErrorContains(f.Store.PublishDocumentExtraction(t.Context(), publication), "span 0 is invalid")

	var units int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM document_units WHERE extraction_id = ?`), claim.ExtractionID).Scan(&units))
	assert.Zero(t, units)
	var state string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT state FROM document_extractions WHERE id = ?`), claim.ExtractionID).Scan(&state))
	assert.Equal(t, "staging", state)
}

func TestDocumentExtractionPublicationRequiresNormalizedIdentity(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-missing-normalized-identity", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-missing-normalized-identity", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	requirements.NoError(err)

	for name, clearIdentity := range map[string]func(*store.DocumentExtractionPublication){
		"normalization version": func(publication *store.DocumentExtractionPublication) { publication.NormalizationVersion = 0 },
		"document family":       func(publication *store.DocumentExtractionPublication) { publication.DocumentFamily = "" },
		"unit kind":             func(publication *store.DocumentExtractionPublication) { publication.UnitKind = "" },
	} {
		t.Run(name, func(t *testing.T) {
			publication := publicationFor(t, claim, "normalized identity", strings.Repeat("f", 64))
			clearIdentity(&publication)
			require.ErrorContains(t, f.Store.PublishDocumentExtraction(t.Context(), publication), "normalized identity")
		})
	}
}

func TestDocumentExtractionClaimRequiresAuthoritativeRoleProvenance(t *testing.T) {
	tests := []struct {
		name   string
		update string
	}{
		{
			name:   "occurrence",
			update: `UPDATE document_occurrences SET role_source = 'unknown' WHERE attachment_id = ?`,
		},
		{
			name:   "current attachment",
			update: `UPDATE attachments SET role_source = 'unknown' WHERE id = ?`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := storetest.New(t)
			profile, hash := seedDocumentPublicationAuthority(t, f)
			input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
				ExtractionID: "extraction-untrusted-role", ProfileID: profile.ID,
				CanonicalBlobHash: hash, ExtractionInputKey: "original",
				LeaseOwner: "worker-untrusted-role", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
				LocalBytes: 128, SourceSequence: 1,
			})
			_, err := f.Store.DB().Exec(f.Store.Rebind(test.update), input.OccurrenceAttachmentID)
			require.NoError(t, err)

			_, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
			require.ErrorContains(t, err, "no eligible occurrence")
		})
	}
}

func TestDocumentExtractionPublicationRechecksClaimedOccurrenceScope(t *testing.T) {
	tests := []struct {
		name   string
		update string
	}{
		{
			name:   "occurrence provenance",
			update: `UPDATE document_occurrences SET role_source = 'unknown' WHERE attachment_id = ?`,
		},
		{
			name:   "current attachment provenance",
			update: `UPDATE attachments SET role_source = 'unknown' WHERE id = ?`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			profile, hash := seedDocumentPublicationAuthority(t, f)
			claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
				ExtractionID: "extraction-scope-change", ProfileID: profile.ID,
				CanonicalBlobHash: hash, ExtractionInputKey: "original",
				LeaseOwner: "worker-scope-change", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
				LocalBytes: 128, SourceSequence: 1,
			}))
			require.NoError(err)

			otherMessageID := f.CreateMessage("document-publication-other-scope")
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), otherMessageID, store.AttachmentWrite{
				Filename: "other.pdf", MIMEType: "application/pdf", Size: 128,
				StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
				Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
				SourcePartKey: "mime:other",
			}))
			otherAttachmentID := singleAttachmentID(t, f, otherMessageID)
			_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), otherAttachmentID, 2)
			require.NoError(err)
			require.True(eligible)
			_, err = f.Store.DB().Exec(f.Store.Rebind(test.update), claim.OccurrenceAttachmentID)
			require.NoError(err)

			err = f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t,
				claim, "must not publish", strings.Repeat("f", 64),
			))
			require.ErrorContains(err, "claimed occurrence is no longer eligible")
		})
	}
}

func TestDocumentExtractionPublicationAcceptsTrustedHashlessCASAlias(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	input := documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-hashless-alias", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-hashless-alias", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	})
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE attachments SET content_hash = '' WHERE id = ?`), input.OccurrenceAttachmentID)
	require.NoError(err)

	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t,
		claim, "hashless alias evidence", strings.Repeat("a", 64),
	)))
}

func TestGarbageCollectDocumentDerivativesKeepsCurrentUntilFinalOccurrenceIsGone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "old quasar evidence", "gc-old")
	publishSearchDocument(t, f, profile, hash, "current nebula evidence", "gc-current")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE document_extractions SET updated_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-48*time.Hour), "gc-old")
	require.NoError(err)

	result, err := f.Store.GarbageCollectDocumentDerivatives(
		t.Context(), time.Now().UTC().Add(-24*time.Hour), 10,
	)
	require.NoError(err)
	assert.Equal(store.DocumentDerivativeGCResult{ExtractionsRemoved: 1}, result)
	assert.Equal([]string{"current nebula evidence"}, currentDocumentTexts(t, f, profile.ID, hash))
	var oldExtractions int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM document_extractions WHERE id = ?`), "gc-old").Scan(&oldExtractions))
	assert.Zero(oldExtractions)

	var attachmentID, messageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_id, message_id FROM document_occurrences
		WHERE canonical_blob_hash = ?`), hash).Scan(&attachmentID, &messageID))
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 2)
	require.NoError(err)
	assert.False(eligible)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE document_extractions SET updated_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-48*time.Hour), "gc-current")
	require.NoError(err)

	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	result, err = f.Store.GarbageCollectDocumentDerivatives(
		t.Context(), time.Now().UTC().Add(-24*time.Hour), 10,
	)
	require.NoError(err)
	assert.Equal(store.DocumentDerivativeGCResult{
		ExtractionsRemoved: 1, CurrentHeadsRemoved: 1,
	}, result)
	var heads, chunks int
	require.NoError(f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM document_extraction_heads`).Scan(&heads))
	require.NoError(f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM document_chunks`).Scan(&chunks))
	assert.Zero(heads)
	assert.Zero(chunks)
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter)
}

func TestGarbageCollectDocumentDerivativesKeepsTerminalSuppressionUntilFinalOccurrenceIsGone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "gc-terminal", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "gc-terminal-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "provider_rejected", Terminal: true,
	}))
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE document_extractions SET updated_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-48*time.Hour), claim.ExtractionID)
	require.NoError(err)
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)

	result, err := f.Store.GarbageCollectDocumentDerivatives(
		t.Context(), time.Now().UTC().Add(-24*time.Hour), 10,
	)
	require.NoError(err)
	assert.Equal(store.DocumentDerivativeGCResult{}, result)
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBefore, revisionAfter)

	var attachmentID, messageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT attachment_id, message_id FROM document_occurrences
		WHERE canonical_blob_hash = ?`), hash).Scan(&attachmentID, &messageID))
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 2)
	require.NoError(err)
	assert.False(eligible)
	revisionBeforeCollection, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)

	result, err = f.Store.GarbageCollectDocumentDerivatives(
		t.Context(), time.Now().UTC().Add(-24*time.Hour), 10,
	)
	require.NoError(err)
	assert.Equal(store.DocumentDerivativeGCResult{ExtractionsRemoved: 1}, result)
	revisionAfter, err = f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBeforeCollection+1, revisionAfter,
		"removing a terminal marker may restore older-profile fallback evidence")
}

func TestPurgeDocumentDerivedByHashKeepsOccurrenceReadyForRebuild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "purge nebula evidence", "purge-current")
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)

	result, err := f.Store.PurgeDocumentDerivedByHash(t.Context(), hash)
	require.NoError(err)
	assert.Equal(store.DocumentDerivedPurgeResult{ExtractionsRemoved: 1, HeadsRemoved: 1}, result)
	var occurrences, heads, chunks int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM document_occurrences`).Scan(&occurrences))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM document_extraction_heads`).Scan(&heads))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM document_chunks`).Scan(&chunks))
	assert.Equal(1, occurrences)
	assert.Zero(heads)
	assert.Zero(chunks)
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter)

	candidates, err := f.Store.ListPendingDocumentExtractions(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, 10,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(hash, candidates[0].CanonicalBlobHash)
}

func TestPurgeDocumentDerivedByHashInvalidatesTerminalSuppression(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "purge-terminal", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "purge-terminal-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "provider_rejected", Terminal: true,
	}))
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)

	result, err := f.Store.PurgeDocumentDerivedByHash(t.Context(), hash)
	require.NoError(err)
	assert.Equal(store.DocumentDerivedPurgeResult{ExtractionsRemoved: 1}, result)
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter)
}

func seedDocumentPublicationAuthority(
	t *testing.T,
	f *storetest.Fixture,
) (store.DocumentExtractionProfile, string) {
	t.Helper()
	require := require.New(t)
	fingerprint := strings.Repeat("a", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: "mistral-ocr-4-0",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	messageID := f.CreateMessage("document-publication")
	hash := strings.Repeat("b", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		SourcePartKey: "mime:1.2",
	}))
	attachmentID := singleAttachmentID(t, f, messageID)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	require.NoError(err)
	require.True(eligible)
	return profile, hash
}

func publicationFor(
	t *testing.T,
	claim store.DocumentExtractionClaim,
	text string,
	checksum string,
) store.DocumentExtractionPublication {
	t.Helper()
	policy, err := docbankdocument.NewNormalizePolicy(1)
	require.NoError(t, err)
	return store.DocumentExtractionPublication{
		ExtractionID: claim.ExtractionID, ProfileID: claim.ProfileID,
		CanonicalBlobHash: claim.CanonicalBlobHash, ExtractionInputKey: claim.ExtractionInputKey,
		OccurrenceAttachmentID: claim.OccurrenceAttachmentID,
		OccurrenceMIMEType:     claim.OccurrenceMIMEType,
		OccurrenceMessageType:  claim.OccurrenceMessageType,
		LeaseOwner:             claim.LeaseOwner, LeaseFence: claim.LeaseFence,
		ReturnedModel: "mistral-ocr-4-0", UnitsProcessed: 1,
		RequestCount: 1, ProviderLatencyMS: 25,
		ManifestChecksum: strings.Repeat("c", 64), NormalizationVersion: policy.Identity().Version,
		DocumentFamily: "pdf", UnitKind: "page",
		Units: []store.DocumentPublishedUnit{{
			Index: 0, Kind: "page", Text: text, Checksum: checksum, CharCount: len([]rune(text)),
		}},
		Chunks: []store.DocumentPublishedChunk{{
			Key: "chunk-0", Ordinal: 0, Text: text, FirstUnitIndex: 0, LastUnitIndex: 0,
			Checksum: checksum, CharCount: len([]rune(text)),
			Spans: []store.DocumentPublishedSpan{{UnitIndex: 0, CharStart: 0, CharEnd: len([]rune(text))}},
		}},
	}
}

func documentClaimInputForHash(
	t *testing.T,
	f *storetest.Fixture,
	input store.DocumentExtractionClaimInput,
) store.DocumentExtractionClaimInput {
	t.Helper()
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT o.attachment_id, COALESCE(o.mime_type, ''), COALESCE(m.message_type, '')
		FROM document_occurrences o
		JOIN messages m ON m.id = o.message_id
		WHERE o.canonical_blob_hash = ? AND o.attachment_role = 'standalone'
		ORDER BY o.occurrence_key
		LIMIT 1`), input.CanonicalBlobHash).Scan(
		&input.OccurrenceAttachmentID, &input.OccurrenceMIMEType, &input.OccurrenceMessageType,
	))
	return input
}

func currentDocumentTexts(t *testing.T, f *storetest.Fixture, profileID, hash string) []string {
	t.Helper()
	require := require.New(t)
	rows, err := f.Store.DB().Query(f.Store.Rebind(`
		SELECT c.text
		FROM document_extraction_heads h
		JOIN document_chunks c ON c.extraction_id = h.extraction_id
		JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
		JOIN messages m ON m.id = o.message_id
		WHERE h.profile_id = ? AND h.canonical_blob_hash = ?
		  AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL
		ORDER BY c.ordinal`), profileID, hash)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var texts []string
	for rows.Next() {
		var text string
		require.NoError(rows.Scan(&text))
		texts = append(texts, text)
	}
	require.NoError(rows.Err())
	return texts
}
