package store_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchDocumentsReturnsCurrentExactOccurrences(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "old quasar evidence", "search-old")
	publishSearchDocument(t, f, profile, hash, "new nebula evidence", "search-new")

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(t, err)
	assert.Empty(response.Results, "an immutable superseded chunk must not remain searchable")

	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(t, err)
	require.Len(t, response.Results, 1)
	result := response.Results[0]
	assert.Equal("synthetic.pdf", result.Filename)
	assert.Equal(hash, result.CanonicalBlobHash)
	assert.Equal(profile.ID, result.ProfileID)
	assert.Equal("mistral", result.Provider)
	assert.Equal("mistral-ocr-4-0", result.Model)
	assert.Equal([]string{"content"}, result.MatchedSignals)
	assert.Equal("new nebula evidence", result.Excerpt)
	assert.Equal(4, result.HighlightStart)
	assert.Equal(10, result.HighlightEnd)
	assert.Zero(result.OtherLiveCopies)
	assert.Equal(1, result.Rank)
}

func TestSearchDocumentsFailsCleanlyWithoutFTS(t *testing.T) {
	f := storetest.New(t)
	store.SetFTS5AvailableForTest(f.Store, false)

	_, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "evidence"})
	require.ErrorIs(t, err, store.ErrDocumentSearchUnavailable)
}

func TestSearchDocumentsPreservesWinningOccurrenceAndFilenameSignal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "shared nebula evidence", "search-shared")

	secondMessageID := f.CreateMessage("document-search-copy")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "invoice-nebula.xlsx", "provider:copy")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "invoice", AttachmentID: secondAttachmentID,
	})
	require.NoError(err)
	require.Len(response.Results, 1)
	assert.Equal(secondAttachmentID, response.Results[0].AttachmentID)
	assert.Equal(secondMessageID, response.Results[0].MessageID)
	assert.Equal([]string{"filename"}, response.Results[0].MatchedSignals)
	assert.Equal(1, response.Results[0].OtherLiveCopies,
		"copy count is global even when the request is scoped to one occurrence")

	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	require.Len(response.Results, 2)
	assert.Contains([]int64{response.Results[0].AttachmentID, response.Results[1].AttachmentID}, secondAttachmentID)
	for _, result := range response.Results {
		var expectedAttachmentID int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
			`SELECT id FROM attachments WHERE message_id = ?`), result.MessageID).Scan(&expectedAttachmentID))
		assert.Equal(expectedAttachmentID, result.AttachmentID,
			"a hash-level hit must not be broadcast onto a different occurrence")
		assert.Equal(1, result.OtherLiveCopies)
		assert.NotZero(result.AttachmentID)
		assert.NotZero(result.MessageID)
	}
}

func TestSearchDocumentsRejectsStaleOrMismatchedCursor(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "cursor nebula evidence", "search-cursor")

	secondMessageID := f.CreateMessage("document-search-page-two")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "second.pdf", "provider:second")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)

	first, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula", PageSize: 1})
	require.NoError(err)
	require.Len(first.Results, 1)
	require.NotEmpty(first.NextCursor)

	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "different", PageSize: 1, Cursor: first.NextCursor,
	})
	require.ErrorIs(err, store.ErrDocumentSearchInvalidCursor)

	thirdMessageID := f.CreateMessage("document-search-page-three")
	thirdAttachmentID := addSearchAttachment(t, f, thirdMessageID, hash, "third.pdf", "provider:third")
	_, eligible, err = f.Store.ReconcileDocumentOccurrence(t.Context(), thirdAttachmentID, 3)
	require.NoError(err)
	require.True(eligible)
	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1, Cursor: first.NextCursor,
	})
	require.ErrorIs(err, store.ErrDocumentSearchCursorStale)
}

func TestRetireDocumentExtractionProfileHidesResultsAndInvalidatesCursor(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "retirement nebula evidence", "search-retirement")

	secondMessageID := f.CreateMessage("document-retirement-page-two")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "second.pdf", "provider:retirement")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)
	first, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula", PageSize: 1})
	require.NoError(err)
	require.NotEmpty(first.NextCursor)

	changed, err := f.Store.RetireDocumentExtractionProfile(t.Context(), profile.ID)
	require.NoError(err)
	assert.True(t, changed)
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	assert.Empty(t, response.Results)
	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1, Cursor: first.NextCursor,
	})
	require.ErrorIs(err, store.ErrDocumentSearchCursorStale)
}

func TestSearchDocumentsRotatesProfilesAfterEachReplacementIsReady(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	oldProfile, firstHash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, oldProfile, firstHash, "old quasar first", "search-old-first")

	secondHash := strings.Repeat("c", 64)
	secondMessageID := f.CreateMessage("document-profile-rotation-second")
	secondAttachmentID := addSearchAttachment(
		t, f, secondMessageID, secondHash, "second.pdf", "provider:rotation-second",
	)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)
	publishSearchDocument(t, f, oldProfile, secondHash, "old quasar second", "search-old-second")

	before, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar", PageSize: 1})
	require.NoError(err)
	require.Len(before.Results, 1)
	require.NotEmpty(before.NextCursor)

	newProfile := oldProfile
	newProfile.Fingerprint = strings.Repeat("e", 64)
	newProfile.ID = "profile-" + newProfile.Fingerprint
	newProfile.Model = "mistral-ocr-latest"
	newProfile.PolicyJSON = []byte(`{"policy":2}`)
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), newProfile)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: newProfile.ID, ProfileFingerprint: newProfile.Fingerprint,
		RetentionPosture: newProfile.RetentionPosture, TrainingPosture: newProfile.TrainingPosture,
	}))

	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "quasar", PageSize: 1, Cursor: before.NextCursor,
	})
	require.ErrorIs(err, store.ErrDocumentSearchCursorStale)
	whilePending, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(whilePending.Results, 2, "old heads remain visible until each target replacement is ready")

	publishSearchDocument(t, f, newProfile, firstHash, "new nebula first", "search-new-first")
	oldResults, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(oldResults.Results, 1)
	assert.Equal(secondHash, oldResults.Results[0].CanonicalBlobHash)
	assert.Equal(oldProfile.ID, oldResults.Results[0].ProfileID)

	newResults, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	require.Len(newResults.Results, 1)
	assert.Equal(firstHash, newResults.Results[0].CanonicalBlobHash)
	assert.Equal(newProfile.ID, newResults.Results[0].ProfileID)
	assert.Equal("mistral-ocr-latest", newResults.Results[0].Model)

	changed, err := f.Store.RetireDocumentExtractionProfile(t.Context(), newProfile.ID)
	require.NoError(err)
	require.True(changed)
	fallback, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	assert.Len(fallback.Results, 2, "retiring the target restores the newest eligible fallback heads")

	err = f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: newProfile.ID, ProfileFingerprint: newProfile.Fingerprint,
		RetentionPosture: newProfile.RetentionPosture, TrainingPosture: newProfile.TrainingPosture,
	})
	require.ErrorContains(err, "retired")
}

func TestSearchDocumentsSuppressesFallbackAfterTargetProfileTerminalFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	oldProfile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, oldProfile, hash, "old quasar evidence", "search-terminal-old")

	newProfile := oldProfile
	newProfile.Fingerprint = strings.Repeat("e", 64)
	newProfile.ID = "profile-" + newProfile.Fingerprint
	newProfile.PolicyJSON = []byte(`{"policy":2}`)
	_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), newProfile)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: newProfile.ID, ProfileFingerprint: newProfile.Fingerprint,
		RetentionPosture: newProfile.RetentionPosture, TrainingPosture: newProfile.TrainingPosture,
	}))
	before, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(before.Results, 1, "the old profile remains a fallback while the target is pending")
	revisionBefore, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)

	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "search-terminal-target", ProfileID: newProfile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "search-terminal-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1, RequireNoHead: true,
	}))
	require.NoError(err)
	require.NoError(f.Store.FailDocumentExtraction(t.Context(), store.DocumentExtractionFailure{
		Claim: claim, ReasonCode: "provider_rejected", Terminal: true,
	}))

	after, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	assert.Empty(after.Results, "a terminal decision for the desired policy suppresses older profile fallback")
	revisionAfter, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter, "terminal suppression must invalidate search cursors")

	changed, err := f.Store.RetryDocumentExtraction(t.Context(), newProfile.ID, hash)
	require.NoError(err)
	require.True(changed)
	retried, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar"})
	require.NoError(err)
	require.Len(retried.Results, 1, "retrying the desired policy restores eligible fallback evidence")
	assert.Equal(oldProfile.ID, retried.Results[0].ProfileID)
	revisionRetried, err := f.Store.GetDocumentIndexRevision(t.Context())
	require.NoError(err)
	assert.Equal(revisionAfter+1, revisionRetried, "restoring fallback must invalidate search cursors")
}

func TestSearchDocumentsFailsClosedOnLiveAuthorityAndInvalidScopes(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "authority nebula evidence", "search-authority")

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	require.Len(response.Results, 1)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`),
		response.Results[0].MessageID)
	require.NoError(err)
	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	assert.Empty(t, response.Results, "serving must recheck live authority before reconciliation catches up")

	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula", SourceIDs: []int64{0}})
	require.ErrorContains(err, "source IDs must be positive")
	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula", MessageTypes: []string{""}})
	require.ErrorContains(err, "message types must be nonempty")
}

func TestSearchDocumentsAppliesCandidateLimitAfterOccurrenceDeduplication(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, crowdedHash := seedDocumentPublicationAuthority(t, f)
	publishManySearchChunks(t, f, profile, crowdedHash, 201)

	secondHash := strings.Repeat("c", 64)
	secondMessageID := f.CreateMessage("document-search-not-crowded-out")
	secondAttachmentID := addSearchAttachment(
		t, f, secondMessageID, secondHash, "second.docx", "provider:second-owner",
	)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)
	publishSearchDocument(t, f, profile, secondHash, "second nebula evidence", "search-second-owner")

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	require.NoError(err)
	require.Len(response.Results, 2,
		"many matching chunks from one occurrence must not crowd out another attachment")
	assert.Contains(t, []int64{response.Results[0].AttachmentID, response.Results[1].AttachmentID}, secondAttachmentID)
}

func TestSearchDocumentsExpandsCandidateWindowAcrossPagination(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "window nebula evidence", "search-window")

	const copies = 201
	for index := 1; index < copies; index++ {
		messageID := f.CreateMessage("document-search-window-" + strconv.Itoa(index))
		attachmentID := addSearchAttachment(
			t, f, messageID, hash, "copy-"+strconv.Itoa(index)+".pdf",
			"provider:window:"+strconv.Itoa(index),
		)
		_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, int64(index+1))
		require.NoError(err)
		require.True(eligible)
	}

	cursor := ""
	seen := make(map[int64]struct{}, copies)
	for {
		response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
			Query: "nebula", PageSize: 10, Cursor: cursor,
		})
		require.NoError(err)
		for _, result := range response.Results {
			seen[result.AttachmentID] = struct{}{}
		}
		if response.NextCursor == "" {
			require.False(response.Truncated)
			break
		}
		cursor = response.NextCursor
	}
	require.Len(seen, copies, "pagination must expand beyond the initial 200-candidate window")
}

func publishSearchDocument(
	t *testing.T,
	f *storetest.Fixture,
	profile store.DocumentExtractionProfile,
	hash string,
	text string,
	extractionID string,
) {
	t.Helper()
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: extractionID, ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: extractionID + "-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(t, err)
	require.NoError(t, f.Store.PublishDocumentExtraction(t.Context(), publicationFor(
		claim, text, strings.Repeat("d", 64),
	)))
}

func addSearchAttachment(
	t *testing.T,
	f *storetest.Fixture,
	messageID int64,
	hash string,
	filename string,
	sourcePartKey string,
) int64 {
	t.Helper()
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: filename, MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: sourcePartKey,
	}))
	return singleAttachmentID(t, f, messageID)
}

func publishManySearchChunks(
	t *testing.T,
	f *storetest.Fixture,
	profile store.DocumentExtractionProfile,
	hash string,
	count int,
) {
	t.Helper()
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "search-crowded", ProfileID: profile.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "search-crowded-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	require.NoError(t, err)
	text := "crowded nebula evidence"
	publication := publicationFor(claim, text, strings.Repeat("d", 64))
	publication.Chunks = make([]store.DocumentPublishedChunk, count)
	for index := range count {
		publication.Chunks[index] = store.DocumentPublishedChunk{
			Key: "crowded-" + strconv.Itoa(index), Ordinal: index, Text: text,
			FirstUnitIndex: 0, LastUnitIndex: 0, Checksum: strings.Repeat("d", 64),
			CharCount: len([]rune(text)),
			Spans:     []store.DocumentPublishedSpan{{UnitIndex: 0, CharStart: 0, CharEnd: len([]rune(text))}},
		}
	}
	require.NoError(t, f.Store.PublishDocumentExtraction(t.Context(), publication))
}
