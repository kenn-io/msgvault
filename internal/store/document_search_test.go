package store_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
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
	assert.Equal(f.ConvID, result.ConversationID)
	assert.Equal("document-publication", result.SourceMessageID)
	assert.Nil(result.OccurredAt)
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

func TestSearchDocumentsRestrictsOwningMessageCandidates(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "candidate nebula evidence", "search-candidate")
	first, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	requirements.NoError(err)
	requirements.Len(first.Results, 1)

	secondMessageID := f.CreateMessage("document-search-selected-candidate")
	secondAttachmentID := addSearchAttachment(
		t, f, secondMessageID, hash, "selected.pdf", "provider:selected-candidate",
	)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", MessageIDs: []int64{secondMessageID},
	})
	requirements.NoError(err)
	requirements.Len(response.Results, 1)
	checks.Equal(secondMessageID, response.Results[0].MessageID)
	checks.NotEqual(first.Results[0].MessageID, response.Results[0].MessageID)
}

func TestSearchDocumentsFiltersOwningMessagesByResolvedPerson(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "person nebula evidence", "search-person")

	firstParticipant := f.EnsureParticipant("first@example.test", "First", "example.test")
	secondParticipant := f.EnsureParticipant("second@example.test", "Second", "example.test")
	var firstAttachmentID, firstMessageID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id, message_id FROM attachments WHERE content_hash = ?`), hash).
		Scan(&firstAttachmentID, &firstMessageID))
	when := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET sender_id = ?, sent_at = ? WHERE id = ?`),
		firstParticipant, when, firstMessageID)
	requirements.NoError(err)

	secondMessageID := f.CreateMessage("document-search-other-person")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "other.pdf", "provider:other")
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET sender_id = ?, sent_at = ? WHERE id = ?`),
		secondParticipant, when, secondMessageID)
	requirements.NoError(err)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)

	after := when.Add(-time.Hour)
	before := when.Add(time.Hour)
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", After: &after, Before: &before,
		Person: &personscope.Scope{
			ParticipantIDs: []int64{firstParticipant},
			Directions:     []personscope.Direction{personscope.FromPerson},
		},
	})
	requirements.NoError(err)
	requirements.Len(response.Results, 1)
	assertions.Equal(firstMessageID, response.Results[0].MessageID)
	assertions.Equal(&personscope.Provenance{
		ParticipantIDs: []int64{firstParticipant},
		Roles:          []personscope.Role{personscope.RoleFrom},
		Directions:     []personscope.Direction{personscope.FromPerson},
	}, response.Results[0].PersonProvenance)

	var firstConversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT conversation_id FROM messages WHERE id = ?`), firstMessageID).
		Scan(&firstConversationID))
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE conversations SET conversation_type = 'direct_chat' WHERE id = ?`), firstConversationID)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET sender_id = NULL, is_from_me = FALSE WHERE id = ?`), firstMessageID)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`),
		firstMessageID, secondParticipant)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (?, ?)`),
		firstConversationID, firstParticipant)
	requirements.NoError(err)

	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", AttachmentID: firstAttachmentID,
		Person: &personscope.Scope{
			ParticipantIDs: []int64{firstParticipant},
			Directions:     []personscope.Direction{personscope.ToPerson},
		},
	})
	requirements.NoError(err)
	requirements.Len(response.Results, 1,
		"a direct-chat roster member is the inferred recipient when a different sender is known only from the envelope")
	assertions.Equal([]personscope.Direction{personscope.ToPerson},
		response.Results[0].PersonProvenance.Directions)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM message_recipients WHERE message_id = ? AND participant_id = ? AND recipient_type = 'from'`),
		firstMessageID, secondParticipant)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`),
		firstMessageID, firstParticipant)
	requirements.NoError(err)
	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", AttachmentID: firstAttachmentID,
		Person: &personscope.Scope{
			ParticipantIDs: []int64{firstParticipant},
			Directions:     []personscope.Direction{personscope.ToPerson},
		},
	})
	requirements.NoError(err)
	assertions.Empty(response.Results,
		"a person known as the envelope sender cannot also be inferred as the direct recipient")
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM message_recipients WHERE message_id = ? AND participant_id = ? AND recipient_type = 'from'`),
		firstMessageID, firstParticipant)
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')`),
		firstMessageID, secondParticipant)
	requirements.NoError(err)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = ?`), firstConversationID)
	requirements.NoError(err)
	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", AttachmentID: firstAttachmentID,
		Person: &personscope.Scope{
			ParticipantIDs: []int64{firstParticipant},
			Directions:     []personscope.Direction{personscope.Group},
		},
	})
	requirements.NoError(err)
	requirements.Len(response.Results, 1)
	assertions.Equal([]personscope.Role{personscope.RoleConversationMember},
		response.Results[0].PersonProvenance.Roles)
	assertions.Equal([]personscope.Direction{personscope.Group},
		response.Results[0].PersonProvenance.Directions)
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

func TestDocumentOccurrenceCascadeInvalidatesSearchCursor(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "cascade nebula evidence", "search-cascade")

	secondMessageID := f.CreateMessage("document-cascade-page-two")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "second.pdf", "provider:cascade")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)

	first, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1,
	})
	require.NoError(err)
	require.NotEmpty(first.NextCursor)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM attachments WHERE id = ?`), secondAttachmentID)
	require.NoError(err)
	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1, Cursor: first.NextCursor,
	})
	require.ErrorIs(err, store.ErrDocumentSearchCursorStale)
}

func TestDocumentMessageTypeChangeInvalidatesSearchCursor(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "message type nebula evidence", "search-message-type")

	secondMessageID := f.CreateMessage("document-message-type-page-two")
	secondAttachmentID := addSearchAttachment(t, f, secondMessageID, hash, "second.pdf", "provider:message-type")
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), secondAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)

	first, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1, MessageTypes: []string{"email"},
	})
	require.NoError(err)
	require.NotEmpty(first.NextCursor)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = ? WHERE id = ?`), "chat", secondMessageID)
	require.NoError(err)
	_, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", PageSize: 1, MessageTypes: []string{"email"}, Cursor: first.NextCursor,
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

func TestSearchDocumentsHonorsExplicitCandidateLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "bounded nebula evidence", "search-bounded")
	messageID := f.CreateMessage("document-search-bounded-copy")
	attachmentID := addSearchAttachment(
		t, f, messageID, hash, "bounded-copy.pdf", "provider:bounded-copy",
	)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)

	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", CandidateLimit: 1,
	})
	requirements.NoError(err)
	assertions.Len(response.Results, 1)
	assertions.True(response.Truncated)
}

func TestSearchDocumentsPaginationUsesStableRankingSet(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, "window nebula evidence", "search-window")

	const copies = 1001
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
	nextRank := 1
	for {
		response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{
			Query: "nebula", PageSize: 100, Cursor: cursor,
		})
		require.NoError(err)
		for _, result := range response.Results {
			_, duplicate := seen[result.AttachmentID]
			require.False(duplicate, "a stable cursor must not repeat an occurrence")
			require.Equal(nextRank, result.Rank)
			nextRank++
			seen[result.AttachmentID] = struct{}{}
		}
		if response.NextCursor == "" {
			require.False(response.Truncated)
			break
		}
		cursor = response.NextCursor
	}
	require.Len(seen, copies, "pagination must cover the fixed ranked candidate set")
}

func TestResolveDocumentVectorSearchOccurrencesExpandsAndBoundsAfterOccurrenceDeduplication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, generation := seedDocumentVectorGenerationWithChunks(t, 2)
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	claims := readyAllDocumentVectorChunks(t, f, generation, now)
	require.Len(claims, 2)

	copyMessageID := f.CreateMessage("semantic-search-copy")
	copyAttachmentID := addSearchAttachment(
		t, f, copyMessageID, claims[0].CanonicalBlobHash, "semantic-copy.pdf", "provider:semantic-copy",
	)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET sent_at = ? WHERE id = ?`), now, copyMessageID)
	require.NoError(err)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), copyAttachmentID, 2)
	require.NoError(err)
	require.True(eligible)
	require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))

	hits := []store.DocumentVectorSearchHit{
		{Token: claims[0].Token, Score: .8, Rank: 10},
		{Token: claims[1].Token, Score: .9, Rank: 2},
	}
	results, resultsMore, err := f.Store.ResolveDocumentVectorSearchOccurrences(
		t.Context(), generation.ID, hits, store.DocumentSearchRequest{}, 10,
	)
	require.NoError(err)
	assert.False(resultsMore)
	require.Len(results, 2, "two chunks must expand to each occurrence, then collapse by occurrence")
	assert.Less(results[0].OccurrenceKey, results[1].OccurrenceKey)
	for _, result := range results {
		assert.Equal(claims[1].Token, result.VectorToken)
		assert.Equal(2, result.SemanticRank)
		assert.InDelta(.9, result.SemanticScore, 1e-12)
		assert.Equal(generation.ID, result.VectorGenerationID)
		assert.Equal(generation.Fingerprint, result.VectorGenerationFingerprint)
		assert.Equal(generation.EmbeddingProfile, result.VectorEmbeddingProfile)
		assert.Equal(generation.Model, result.VectorModel)
		assert.Equal(generation.Dimension, result.VectorDimension)
		assert.Equal(claims[1].ChunkKey, result.ChunkKey)
		assert.Equal(claims[1].ChunkOrdinal, result.ChunkOrdinal)
		assert.Equal(claims[1].ExtractionID, result.ExtractionID)
		assert.Equal(claims[1].ExtractionProfileID, result.ProfileID)
		assert.Equal([]string{"semantic"}, result.MatchedSignals)
		if result.AttachmentID == copyAttachmentID {
			require.NotNil(result.OccurredAt)
			assert.True(now.Equal(*result.OccurredAt))
		}
	}

	bounded, boundedMore, err := f.Store.ResolveDocumentVectorSearchOccurrences(
		t.Context(), generation.ID, hits, store.DocumentSearchRequest{}, 1,
	)
	require.NoError(err)
	require.Len(bounded, 1)
	assert.True(boundedMore)
	assert.Equal(results[0].OccurrenceKey, bounded[0].OccurrenceKey)

	scoped, scopedMore, err := f.Store.ResolveDocumentVectorSearchOccurrences(
		t.Context(), generation.ID, hits, store.DocumentSearchRequest{AttachmentID: copyAttachmentID}, 10,
	)
	require.NoError(err)
	require.Len(scoped, 1)
	assert.False(scopedMore)
	assert.Equal(copyAttachmentID, scoped[0].AttachmentID)

	participantID := f.EnsureParticipant("semantic@example.test", "Semantic", "example.test")
	var originalMessageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT message_id FROM attachments WHERE content_hash = ? AND id <> ?`),
		claims[0].CanonicalBlobHash, copyAttachmentID).Scan(&originalMessageID))
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET sender_id = ? WHERE id = ?`), participantID, originalMessageID)
	require.NoError(err)
	personResults, personMore, err := f.Store.ResolveDocumentVectorSearchOccurrences(
		t.Context(), generation.ID, hits, store.DocumentSearchRequest{Person: &personscope.Scope{
			ParticipantIDs: []int64{participantID}, Directions: []personscope.Direction{personscope.FromPerson},
		}}, 10,
	)
	require.NoError(err)
	require.Len(personResults, 1)
	assert.False(personMore)
	assert.Equal(&personscope.Provenance{
		ParticipantIDs: []int64{participantID}, Roles: []personscope.Role{personscope.RoleFrom},
		Directions: []personscope.Direction{personscope.FromPerson},
	}, personResults[0].PersonProvenance)
}

func TestResolveDocumentVectorSearchOccurrencesBoundsUnicodeExcerpt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	publishSearchDocument(t, f, profile, hash, strings.Repeat("界", 400), "semantic-excerpt")
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 3,
	})
	require.NoError(err)
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	claims := readyAllDocumentVectorChunks(t, f, generation, now)
	require.Len(claims, 1)
	require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))

	results, truncated, err := f.Store.ResolveDocumentVectorSearchOccurrences(t.Context(), generation.ID, []store.DocumentVectorSearchHit{
		{Token: claims[0].Token, Score: .9, Rank: 1},
	}, store.DocumentSearchRequest{}, 10)
	require.NoError(err)
	require.Len(results, 1)
	assert.False(truncated)
	assert.Equal(strings.Repeat("界", 320), results[0].Excerpt)
	assert.Zero(results[0].HighlightStart)
	assert.Zero(results[0].HighlightEnd)
}

func TestResolveDocumentVectorSearchOccurrencesHidesStaleAuthority(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *storetest.Fixture, store.DocumentVectorChunkClaim)
	}{
		{
			name: "attachment replacement",
			mutate: func(t *testing.T, f *storetest.Fixture, claim store.DocumentVectorChunkClaim) {
				t.Helper()
				attachmentID := documentVectorAttachmentID(t, f, claim.CanonicalBlobHash)
				var messageID int64
				require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT message_id FROM attachments WHERE id = ?`), attachmentID).Scan(&messageID))
				require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
					Filename: "replacement.pdf", MIMEType: "application/pdf", Size: 128,
					StoragePath: "ee/" + strings.Repeat("e", 64), ContentHash: strings.Repeat("e", 64),
					Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
					SourcePartKey: "mime:1.2",
				}))
			},
		},
		{
			name: "occurrence deletion",
			mutate: func(t *testing.T, f *storetest.Fixture, claim store.DocumentVectorChunkClaim) {
				t.Helper()
				attachmentID := documentVectorAttachmentID(t, f, claim.CanonicalBlobHash)
				_, err := f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM attachments WHERE id = ?`), attachmentID)
				require.NoError(t, err)
			},
		},
		{
			name: "role change",
			mutate: func(t *testing.T, f *storetest.Fixture, claim store.DocumentVectorChunkClaim) {
				t.Helper()
				attachmentID := documentVectorAttachmentID(t, f, claim.CanonicalBlobHash)
				var messageID int64
				require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(
					`SELECT message_id FROM attachments WHERE id = ?`), attachmentID).Scan(&messageID))
				require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
					Filename: "inline.pdf", MIMEType: "application/pdf", Size: 128,
					StoragePath: claim.CanonicalBlobHash[:2] + "/" + claim.CanonicalBlobHash,
					ContentHash: claim.CanonicalBlobHash, Role: store.AttachmentRoleInline,
					RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "mime:1.2",
				}))
			},
		},
		{
			name: "message lifecycle deletion",
			mutate: func(t *testing.T, f *storetest.Fixture, claim store.DocumentVectorChunkClaim) {
				t.Helper()
				var messageID int64
				require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
					SELECT message_id FROM document_occurrences WHERE canonical_blob_hash = ?`),
					claim.CanonicalBlobHash).Scan(&messageID))
				_, err := f.Store.DB().Exec(f.Store.Rebind(
					`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "target profile rotation",
			mutate: func(t *testing.T, f *storetest.Fixture, _ store.DocumentVectorChunkClaim) {
				t.Helper()
				profile := rotatedDocumentVectorProfile()
				_, err := f.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
				require.NoError(t, err)
				require.NoError(t, f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
					ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
					RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
				}))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
			now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
			claims := readyAllDocumentVectorChunks(t, f, generation, now)
			require.Len(claims, 1)
			require.NoError(f.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(time.Second)))
			test.mutate(t, f, claims[0])

			results, truncated, err := f.Store.ResolveDocumentVectorSearchOccurrences(t.Context(), generation.ID, []store.DocumentVectorSearchHit{
				{Token: claims[0].Token, Score: .9, Rank: 1},
			}, store.DocumentSearchRequest{}, 10)
			require.NoError(err)
			assert.Empty(results)
			assert.False(truncated)
			var publications int
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
				SELECT COUNT(*) FROM document_vector_publications WHERE generation_id = ? AND token = ?`),
				generation.ID, claims[0].Token).Scan(&publications))
			assert.Equal(1, publications, "authority changes hide but do not erase the token ledger")
		})
	}
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
	require.NoError(t, f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t,
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
	publication := publicationFor(t, claim, text, strings.Repeat("d", 64))
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
