package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentExtractionProfileRequiresExactConsent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	fingerprint := strings.Repeat("a", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: "mistral-ocr-4-0",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"text/csv", "application/pdf", "text/csv"},
		PolicyJSON:        []byte(`{"normalization":1,"chunking":1}`),
	}

	created, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	assert.True(created)
	created, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	assert.False(created)

	changed := profile
	changed.Model = "different-model"
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), changed)
	require.ErrorContains(err, "different immutable policy")

	err = f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	})
	require.NoError(err)
	var enabled bool
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT enabled FROM document_extraction_profiles WHERE id = ?`), profile.ID).Scan(&enabled))
	assert.True(enabled)
	revision, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Zero(revision, "consent cannot invalidate an empty document index")

	err = f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: "zdr", TrainingPosture: profile.TrainingPosture,
	})
	require.ErrorContains(err, "does not match immutable profile")
}

func TestCurrentDocumentIndexStatusScopeUsesOnlySelectedDurableProfile(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	_, _, err := f.Store.GetCurrentDocumentIndexStatusScope(t.Context())
	require.ErrorIs(err, store.ErrDocumentIndexStatusScopeUnavailable)
	fingerprint := strings.Repeat("9", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + fingerprint, Fingerprint: fingerprint,
		Provider: "mistral", Endpoint: "https://api.mistral.ai/v1/ocr",
		Region: "eu", Model: "mistral-ocr-4-0",
		RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"text/csv", "application/pdf", "text/csv"},
		PolicyJSON:        []byte(`{"normalization":1,"chunking":1}`),
	}
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))

	profileID, mediaTypes, err := f.Store.GetCurrentDocumentIndexStatusScope(t.Context())

	require.NoError(err)
	assert.Equal(profile.ID, profileID)
	assert.Equal([]string{"application/pdf", "text/csv"}, mediaTypes)
	retired, err := f.Store.RetireDocumentExtractionProfile(t.Context(), profile.ID)
	require.NoError(err)
	require.True(retired)
	_, _, err = f.Store.GetCurrentDocumentIndexStatusScope(t.Context())
	require.ErrorIs(err, store.ErrDocumentIndexStatusScopeUnavailable)
}

func TestReconcileDocumentOccurrenceUsesTrustedCASAndLiveRole(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-occurrence")
	hash := strings.Repeat("b", 64)
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 128,
		StoragePath:   hash[:2] + "/" + hash,
		Role:          store.AttachmentRoleStandalone,
		RoleSource:    store.AttachmentRoleSourceMIMEDisposition,
		SourcePartKey: "mime:1.2",
	}))
	attachmentID := singleAttachmentID(t, f, messageID)

	occurrence, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 10)
	require.NoError(err)
	assert.True(eligible)
	assert.Equal(hash, occurrence.CanonicalBlobHash)
	assert.True(occurrence.StableSourcePart)
	assert.NotContains(occurrence.OccurrenceKey, "mime:1.2")
	revision, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), revision)

	_, eligible, err = f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 11)
	require.NoError(err)
	assert.True(eligible)
	revision, err = f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), revision, "sequence-only reconciliation must not invalidate search cursors")
	var sourceSequence int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT source_sequence FROM document_occurrences WHERE attachment_id = ?`),
		attachmentID,
	).Scan(&sourceSequence))
	assert.Equal(int64(11), sourceSequence)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, eligible, err = f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 12)
	require.NoError(err)
	assert.False(eligible)
	revision, err = f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(int64(2), revision)
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM document_occurrences`).Scan(&occurrences))
	assert.Zero(occurrences)
}

func TestReconcileDocumentOccurrenceFailsClosedForUnknownRole(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-unknown-role")
	hash := strings.Repeat("c", 64)
	require.NoError(f.Store.UpsertAttachment(
		messageID, "synthetic.pdf", "application/pdf", hash[:2]+"/"+hash, hash, 64,
	))
	attachmentID := singleAttachmentID(t, f, messageID)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	require.NoError(err)
	assert.False(eligible)
	revision, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Zero(revision)
}

func TestListPendingDocumentExtractionsRespectsAuthorityAndRetryState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)

	candidates, err := f.Store.ListPendingDocumentExtractions(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, 10,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(hash, candidates[0].CanonicalBlobHash)
	assert.Equal("application/pdf", candidates[0].MIMEType)

	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-retry", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-retry", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "provider_transient", RetryAt: time.Now().UTC().Add(time.Hour),
	}))
	candidates, err = f.Store.ListPendingDocumentExtractions(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, 10,
	)
	require.NoError(err)
	assert.Empty(candidates)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE document_extractions SET next_retry_at = ? WHERE id = ?`),
		time.Now().UTC().Add(-time.Minute), claim.ExtractionID)
	require.NoError(err)
	candidates, err = f.Store.ListPendingDocumentExtractions(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, 10,
	)
	require.NoError(err)
	assert.Len(candidates, 1)
}

func TestListDocumentExtractionCandidatesIncludesCurrentOnlyForFullRebuild(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "current document evidence", "current-for-rebuild")

	pending, err := f.Store.ListDocumentExtractionCandidates(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil, nil, 10,
	)
	require.NoError(err)
	assert.Empty(t, pending)

	rebuild, err := f.Store.StartDocumentExtractionRebuild(
		t.Context(), "rebuild-current", profile.ID, "original", []string{"application/pdf"}, nil,
	)
	require.NoError(err)
	rebuildCandidates, err := f.Store.ListDocumentExtractionCandidates(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil, &rebuild, 10,
	)
	require.NoError(err)
	require.Len(rebuildCandidates, 1)
	assert.Equal(t, hash, rebuildCandidates[0].CanonicalBlobHash)
}

func TestDocumentExtractionRebuildKeepsTerminalTargetsIncompleteUntilRetry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "current document evidence", "current-before-terminal-rebuild")
	rebuild, err := f.Store.StartDocumentExtractionRebuild(
		t.Context(), "rebuild-terminal", profile.ID, "original", []string{"application/pdf"}, nil,
	)
	require.NoError(err)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "terminal-rebuild-attempt", ProfileID: profile.ID, RebuildID: rebuild.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-terminal-rebuild", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "unsupported_provider_response", Terminal: true,
	}))
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "current",
	})
	require.NoError(err)
	require.Len(response.Results, 1,
		"a failed replacement must preserve the previously published current head")
	assert.Equal("current document evidence", response.Results[0].Excerpt)

	remaining, err := f.Store.CountIncompleteDocumentExtractionRebuild(
		t.Context(), rebuild, []string{"application/pdf"}, nil,
	)
	require.NoError(err)
	assert.Equal(int64(1), remaining)
	candidates, err := f.Store.ListDocumentExtractionCandidates(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil, &rebuild, 10,
	)
	require.NoError(err)
	assert.Empty(candidates, "terminal targets require an explicit retry")

	changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	assert.True(changed)
	candidates, err = f.Store.ListDocumentExtractionCandidates(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil, &rebuild, 10,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(hash, candidates[0].CanonicalBlobHash)
}

func TestDocumentIndexScopedStatusClassifiesOwnersAndRoleExclusions(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "status evidence", "status-current")
	messageID := f.CreateMessage("document-status-exclusions")
	unknownHash := strings.Repeat("c", 64)
	require.NoError(t, f.Store.UpsertAttachment(
		messageID, "unknown.pdf", "application/pdf", unknownHash[:2]+"/"+unknownHash,
		unknownHash, 64,
	))
	inlineHash := strings.Repeat("d", 64)
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "inline.pdf", MIMEType: "application/pdf", Size: 32,
		StoragePath: inlineHash[:2] + "/" + inlineHash, ContentHash: inlineHash,
		Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		SourcePartKey: "mime:inline",
	}))

	status, err := f.Store.GetDocumentIndexStatusForScope(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil,
	)
	require.NoError(t, err)
	assert.Equal(int64(1), status.EligibleOccurrences)
	assert.Equal(int64(1), status.EligibleOwners)
	assert.Equal(int64(128), status.EligibleBytes)
	assert.Equal(int64(1), status.ReadyOwners)
	assert.Zero(status.MissingOwners)
	assert.Equal(int64(1), status.UnknownRoleOccurrences)
	assert.Equal(int64(1), status.IneligibleRoleOccurrences)
	assert.Equal(int64(1), status.StoredPlaintextChunks)
	assert.Equal(int64(1), status.ExtractionAttempts)
	assert.Equal(int64(1), status.SuccessfulAttempts)
	assert.Zero(status.FailedAttempts)
	assert.Equal(int64(1), status.ProviderRequests)
	assert.Zero(status.ProviderRetries)
	assert.Equal(int64(25), status.ProviderLatencyMillis)
	assert.InDelta(25, status.AverageProviderLatencyMS, 0.001)
	assert.Equal(int64(128), status.VerifiedUploadBytes)
	assert.Equal(int64(1), status.ProcessedProviderUnits)
	assert.Zero(status.ReportedProviderBytes)
	assert.Equal(int64(1), status.MissingProviderByteReports)
}

func TestDocumentIndexScopedStatusUsesCurrentTerminalAttempt(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	transientClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-transient-attempt", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "status-transient-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: transientClaim, ReasonCode: "provider_transient",
		RetryAt: time.Now().UTC().Add(time.Hour),
	}))
	terminalClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-terminal-attempt", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "status-terminal-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: terminalClaim, ReasonCode: "provider_rejected", Terminal: true,
	}))

	status, err := f.Store.GetDocumentIndexStatusForScope(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, nil,
	)
	require.NoError(err)
	assert.Zero(status.RetryOwners)
	assert.Equal(int64(1), status.TerminalOwners)
	var retryConsumed bool
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT next_retry_at IS NULL FROM document_extractions WHERE id = ?`),
		transientClaim.ExtractionID,
	).Scan(&retryConsumed))
	assert.True(retryConsumed)
}

func TestDocumentIndexStatusCountsCompletedAttemptOutcomes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	failedClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-failed-attempt", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "status-failed-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: failedClaim, ReasonCode: "provider_transient",
		RetryAt:      time.Now().UTC().Add(time.Minute),
		RequestCount: 2, RetryCount: 1, ProviderLatencyMS: 50,
	}))
	publishSearchDocument(t, f, profile, hash, "successful retry", "status-successful-retry")

	status, err := f.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Equal(int64(2), status.ExtractionAttempts)
	assert.Equal(int64(1), status.SuccessfulAttempts)
	assert.Equal(int64(1), status.FailedAttempts)
	assert.Equal(int64(3), status.ProviderRequests)
	assert.Equal(int64(1), status.ProviderRetries)
	assert.Equal(int64(75), status.ProviderLatencyMillis)
	assert.InDelta(25, status.AverageProviderLatencyMS, 0.001)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE document_extractions SET attempt_count = 1 WHERE id = ?`),
		"status-successful-retry")
	require.NoError(err)
	status, err = f.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Equal(int64(3), status.ExtractionAttempts,
		"a successful row retains any failed attempts recorded before publication")
	assert.Equal(int64(1), status.SuccessfulAttempts)
	assert.Equal(int64(2), status.FailedAttempts)

	staging, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-in-flight-attempt", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "replacement",
		LeaseOwner: "status-in-flight-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	assert.NotEmpty(staging.ExtractionID)
	status, err = f.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Equal(int64(3), status.ExtractionAttempts,
		"in-flight work is reported separately from completed outcomes")
	assert.Equal(int64(1), status.StagingOwners)
}

func TestDocumentIndexStatusCountsBytesOnlyAfterProviderRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)

	preparationClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-preparation-failure", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "preparation-failure",
		LeaseOwner: "status-preparation-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: preparationClaim, ReasonCode: "invalid_local_source", Terminal: true,
	}))

	providerClaim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "status-provider-failure", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "provider-failure",
		LeaseOwner: "status-provider-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 256, SourceSequence: 1,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: providerClaim, ReasonCode: "provider_rejected", Terminal: true,
		RequestCount: 1,
	}))

	status, err := f.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	require.NoError(err)
	assert.Equal(int64(1), status.ProviderRequests)
	assert.Equal(int64(256), status.VerifiedUploadBytes)
}

func TestRetryDocumentExtractionReschedulesOnlyTerminalOwner(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-terminal", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "worker-terminal", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "unsupported_provider_response", Terminal: true,
	}))

	changed, err := f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.NoError(err)
	assert.True(t, changed)
	candidates, err := f.Store.ListPendingDocumentExtractions(
		t.Context(), profile.ID, "original", []string{"application/pdf"}, 10,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(t, hash, candidates[0].CanonicalBlobHash)

	publishSearchDocument(t, f, profile, hash, "current evidence", "retry-current")
	_, err = f.Store.RetryDocumentExtraction(t.Context(), profile.ID, hash)
	require.ErrorContains(err, "already current")
}

func TestListDocumentAttachmentIDsAfterIsBoundedAndResumable(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("document-bootstrap")
	for i := range 3 {
		hash := strings.Repeat(string(rune('d'+i)), 64)
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: 10,
			StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
			SourcePartKey: "part:" + string(rune('1'+i)),
		}))
	}
	first, err := f.Store.ListDocumentAttachmentIDsAfter(t.Context(), 0, 2)
	require.NoError(err)
	require.Len(first, 2)
	second, err := f.Store.ListDocumentAttachmentIDsAfter(t.Context(), first[1], 2)
	require.NoError(err)
	require.Len(second, 1)
	assert.Greater(t, second[0], first[1])
}
