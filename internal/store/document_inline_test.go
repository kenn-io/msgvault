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

func TestInlineDocumentsRequireExactProfileConsentAndStayInScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	original, hash := seedDocumentPublicationAuthority(t, f)
	attachmentID := seededDocumentAttachmentID(t, f, hash)
	_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE attachments SET attachment_role = 'inline' WHERE id = ?"), attachmentID)
	require.NoError(err)
	occurrence, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 2)
	require.NoError(err)
	require.True(eligible, "catalog retains inline occurrence without granting upload authority")
	assert.Equal(store.AttachmentRoleInline, occurrence.AttachmentRole)
	candidates, err := f.Store.ListDocumentExtractionCandidates(t.Context(), original.ID, "original", original.AllowedMediaTypes, nil, nil, 10)
	require.NoError(err)
	assert.Empty(candidates)
	input := store.DocumentExtractionClaimInput{
		ExtractionID: "inline-extraction", ProfileID: original.ID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original", OccurrenceAttachmentID: attachmentID,
		OccurrenceMIMEType: "application/pdf", OccurrenceMessageType: "email",
		LeaseOwner: "inline-worker", LeaseUntil: time.Now().Add(time.Hour), LocalBytes: 128, SourceSequence: 2,
	}
	_, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.ErrorContains(err, "no eligible occurrence")
	expanded := original
	expanded.ID = "profile-inline"
	expanded.Fingerprint = strings.Repeat("f", 64)
	expanded.IncludeInline = true
	expanded.PolicyJSON = []byte(`{"include_inline":true}`)
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), expanded)
	require.NoError(err)
	input.ProfileID = expanded.ID
	_, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.ErrorContains(err, "exact consent")
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: expanded.ID, ProfileFingerprint: expanded.Fingerprint,
		RetentionPosture: expanded.RetentionPosture, TrainingPosture: expanded.TrainingPosture,
	}))
	candidates, err = f.Store.ListDocumentExtractionCandidates(t.Context(), expanded.ID, "original", expanded.AllowedMediaTypes, nil, nil, 10)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(attachmentID, candidates[0].AttachmentID)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, claim, "inline searchable quasar", strings.Repeat("d", 64))))
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar", PageSize: 10})
	require.NoError(err)
	require.Len(response.Results, 1)
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("8", 64), TargetExtractionProfileID: expanded.ID, EmbeddingProfile: "vector.embeddings", Model: "synthetic-model", Dimension: 768})
	require.NoError(err)
	vectorCandidates, err := f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	require.Len(vectorCandidates, 1, "semantic lane uses the inline serving authority")
	status, err := f.Store.GetDocumentIndexStatusForScope(t.Context(), expanded.ID, "original", expanded.AllowedMediaTypes, nil)
	require.NoError(err)
	assert.Equal(int64(1), status.ReadyOwners)
	assert.Zero(status.IneligibleRoleOccurrences)
	rebuild, err := f.Store.StartDocumentExtractionRebuild(t.Context(), "inline-rebuild", expanded.ID, "original", expanded.AllowedMediaTypes, nil)
	require.NoError(err)
	assert.Equal(int64(1), rebuild.SnapshotOwners)
	remaining, err := f.Store.CountIncompleteDocumentExtractionRebuild(t.Context(), rebuild, expanded.AllowedMediaTypes, nil)
	require.NoError(err)
	assert.Equal(int64(1), remaining)
	gc, err := f.Store.GarbageCollectDocumentDerivatives(t.Context(), time.Now().Add(time.Hour), 10)
	require.NoError(err)
	assert.Zero(gc.CurrentHeadsRemoved, "a live inline owner retains its extraction")
	// Selecting standalone scope must immediately hide expanded fallback heads.
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: original.ID, ProfileFingerprint: original.Fingerprint,
		RetentionPosture: original.RetentionPosture, TrainingPosture: original.TrainingPosture,
	}))
	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar", PageSize: 10})
	require.NoError(err)
	assert.Empty(response.Results)
	vectorCandidates, err = f.Store.ListDocumentVectorChunkCandidates(t.Context(), generation.ID, 0, 10)
	require.NoError(err)
	assert.Empty(vectorCandidates, "narrowing the extraction target also stops vector candidates")
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: expanded.ID, ProfileFingerprint: expanded.Fingerprint,
		RetentionPosture: expanded.RetentionPosture, TrainingPosture: expanded.TrainingPosture,
	}))
	// A role edit invalidates serving before the asynchronous journal is consumed.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE attachments SET attachment_role = 'preview' WHERE id = ?"), attachmentID)
	require.NoError(err)
	response, err = f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar", PageSize: 10})
	require.NoError(err)
	assert.Empty(response.Results)
	retired, err := f.Store.RetireDocumentExtractionProfile(t.Context(), expanded.ID)
	require.NoError(err)
	assert.True(retired)
}

func TestInlineFallbackSurvivesStandaloneHeadForSharedHash(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	original, hash := seedDocumentPublicationAuthority(t, f)
	standaloneID := seededDocumentAttachmentID(t, f, hash)
	msg := f.CreateMessage("shared-inline")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), msg, store.AttachmentWrite{Filename: "inline.pdf", MIMEType: "application/pdf", Size: 128, StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceImporterSemantics}))
	inlineID := singleAttachmentID(t, f, msg)
	_, eligible, err := f.Store.ReconcileDocumentOccurrence(t.Context(), inlineID, 1)
	require.NoError(err)
	require.True(eligible)
	expanded := original
	expanded.ID = "expanded-old"
	expanded.Fingerprint = strings.Repeat("f", 64)
	expanded.IncludeInline = true
	expanded.PolicyJSON = []byte(`{"include_inline":true}`)
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), expanded)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{ProfileID: expanded.ID, ProfileFingerprint: expanded.Fingerprint, RetentionPosture: expanded.RetentionPosture, TrainingPosture: expanded.TrainingPosture}))
	input := store.DocumentExtractionClaimInput{ExtractionID: "expanded-head", ProfileID: expanded.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", OccurrenceAttachmentID: inlineID, OccurrenceMIMEType: "application/pdf", OccurrenceMessageType: "email", LeaseOwner: "mixed-worker", LeaseUntil: time.Now().Add(time.Hour), LocalBytes: 128, SourceSequence: 1}
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, claim, "quasar inline fallback", strings.Repeat("d", 64))))
	// A more recent standalone head must not hide an inline occurrence when a
	// new expanded target has not produced its own head yet.
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{ProfileID: original.ID, ProfileFingerprint: original.Fingerprint, RetentionPosture: original.RetentionPosture, TrainingPosture: original.TrainingPosture}))
	input.ProfileID = original.ID
	input.ExtractionID = "standalone-head"
	input.OccurrenceAttachmentID = standaloneID
	claim, err = f.Store.ClaimDocumentExtraction(t.Context(), input)
	require.NoError(err)
	require.NoError(f.Store.PublishDocumentExtraction(t.Context(), publicationFor(t, claim, "quasar standalone", strings.Repeat("e", 64))))
	target := expanded
	target.ID = "expanded-target"
	target.Fingerprint = strings.Repeat("9", 64)
	target.PolicyJSON = []byte(`{"include_inline":true,"revision":2}`)
	_, err = f.Store.EnsureDocumentExtractionProfile(t.Context(), target)
	require.NoError(err)
	require.NoError(f.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{ProfileID: target.ID, ProfileFingerprint: target.Fingerprint, RetentionPosture: target.RetentionPosture, TrainingPosture: target.TrainingPosture}))
	// Deterministic consent precedence independent of database clock granularity.
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_provider_consents SET consented_at = ? WHERE profile_id = ?"), time.Now().Add(-2*time.Hour), expanded.ID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind("UPDATE document_provider_consents SET consented_at = ? WHERE profile_id = ?"), time.Now().Add(-time.Hour), original.ID)
	require.NoError(err)
	response, err := f.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "quasar", PageSize: 10})
	require.NoError(err)
	require.Len(response.Results, 2)
	attachments := []int64{response.Results[0].AttachmentID, response.Results[1].AttachmentID}
	assert.ElementsMatch([]int64{inlineID, standaloneID}, attachments)
}
