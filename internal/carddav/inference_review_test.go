package carddav

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

func appendInferenceReviewNote(t *testing.T, st *store.Store, personID int64, text string) {
	t.Helper()
	score := 0.99
	_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{
		PersonID: personID, Text: text, Source: store.ProvenanceExtraction, Confidence: &score,
	})
	require.NoError(t, err)
}

func TestPublishPersonRequiresReviewForInferredNotes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Unreviewed extracted detail")
	err := service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVInferenceReviewRequired)
	assert.Zero(fixture.puts)
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("Unreviewed extracted detail", *values[0].Value.Text)
}

func TestSyncSkipsPublishedInferredNoteChanges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{reportCurrent: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	before := string(fixture.body)
	appendInferenceReviewNote(t, st, personID, "Unreviewed extracted detail")
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixture.puts)
	assert.Equal(before, string(fixture.body))
	values, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugNotes})
	require.NoError(err)
	require.Len(values, 1)
	assert.Equal("Unreviewed extracted detail", *values[0].Value.Text)
}

func TestCurrentPublicationReviewApprovesExactCardAndRejectsStale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Unreviewed extracted detail")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.True(preview.ReviewRequired)
	assert.Equal(PublicationReviewCurrent, preview.Kind)
	require.Contains(preview.VCard, "NOTE:Unreviewed extracted detail")
	assert.Len(preview.ApprovalToken, 64)
	view, err := service.PublicationView(t.Context(), personID)
	require.NoError(err)
	assert.True(view.InferenceReviewRequired)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.Equal(preview.VCard, string(fixture.body))
	assert.Equal(1, fixture.puts)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(source.Inference.InferenceRevision, source.Inference.ApprovedRevision)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.False(preview.ReviewRequired)
	appendInferenceReviewNote(t, st, personID, "Later inferred detail")
	require.ErrorIs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), store.ErrCardDAVReviewStale)
	assert.Equal(1, fixture.puts)
	source, err = st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Less(source.Inference.ApprovedRevision, source.Inference.InferenceRevision)
}

func TestCurrentPublicationNoopApprovalAndUnpublishClearsScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	// A declared-to-inferred takeover has unchanged outgoing semantics but creates debt.
	text := "Same detail"
	_, err := st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugNotes,
		Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text}, Source: store.ProvenanceUser,
	})
	require.NoError(err)
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: store.AttributeSlugNotes,
		Value: store.AttributeValue{Type: store.AttributeValueText, Text: &text}, Source: store.ProvenanceExtraction,
	})
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.True(preview.ReviewRequired)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.Equal(1, fixture.puts)
	view, err := service.PublicationView(t.Context(), personID)
	require.NoError(err)
	assert.False(view.InferenceReviewRequired)
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Zero(source.Inference.ApprovedRevision)
	assert.Nil(source.Inference.ApprovedAddressBookID)
	assert.Nil(source.Inference.ApprovedConnectionGeneration)
	assert.Positive(source.Inference.InferenceRevision)
}

func TestReviewedPublicationTransportFailureKeepsExactAuthorization(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Reviewed detail")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Error(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(preview.VCard, string(pending.OutgoingBody))
	require.NotNil(pending.ApprovedBodySHA256)
	assert.Equal(store.CardDAVBodySHA256(pending.OutgoingBody), *pending.ApprovedBodySHA256)
	require.NotNil(pending.ApprovedMutationRevision)
	assert.Equal(pending.MutationRevision, *pending.ApprovedMutationRevision)
	require.NotNil(pending.ApprovedInferenceRevision)
	assert.Equal(int64(1), *pending.ApprovedInferenceRevision)
}

func TestCurrentPublicationRetirementRemovesPreviouslyOwnedEmployment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Example Company", Kind: store.OrganizationKindCompany})
	require.NoError(err)
	input := store.EmploymentInput{PersonID: personID, OrganizationID: organization.ID, Title: new("Engineer"), Source: store.ProvenanceExtraction}
	employment, err := st.AddEmploymentContext(t.Context(), input)
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Contains(preview.VCard, "TITLE:Engineer")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	input.IsCurrent = new(false)
	employment, err = st.UpdateEmploymentContext(t.Context(), employment.ID, employment.Revision, input)
	require.NoError(err)
	require.NoError(st.DeleteEmploymentContext(t.Context(), employment.ID, employment.Revision))
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.NotContains(preview.VCard, "TITLE:")
	assert.NotContains(preview.VCard, "ORG:")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.Equal(preview.VCard, string(fixture.body))
	assert.Equal(2, fixture.puts)
}

func TestCurrentPublicationSupersessionDoesNotDuplicateOwnedNote(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{reportCurrent: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "First detail")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixture.puts)
	appendInferenceReviewNote(t, st, personID, "Second detail")
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(1, fixture.puts)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Equal(1, strings.Count(preview.VCard, "NOTE:"))
	require.Contains(preview.VCard, "Second detail")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.Equal(preview.VCard, string(fixture.body))
}

func TestOrdinaryPublicationUpdatesManualAndDeterministicNotes(t *testing.T) {
	for _, source := range []store.Provenance{store.ProvenanceUser, store.ProvenanceSystem} {
		t.Run(string(source), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			require.NoError(service.PublishPerson(t.Context(), personID))
			_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "Ordinary detail", Source: source})
			require.NoError(err)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(2, fixture.puts)
			assert.Contains(string(fixture.body), "NOTE:Ordinary detail")
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.False(preview.ReviewRequired)
		})
	}
}

func TestTwoServicesCancelQueuedPublishBeforeNetwork(t *testing.T) {
	require := require.New(t)
	fixture := &mutationFixture{}
	reached, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	releaseHTTP := func() { resumeOnce.Do(func() { close(resume) }) }
	defer releaseHTTP()
	handler := fixture.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			close(reached)
			select {
			case <-resume:
			case <-r.Context().Done():
				return
			}
		}
		handler(w, r)
	}))
	defer server.Close()
	service, st, _ := newPullService(t, server, false)
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid, display_name) VALUES ('queued', 'Queued Person') RETURNING id`).Scan(&personID))
	first := make(chan error, 1)
	go func() { first <- service.PublishPerson(t.Context(), personID) }()
	<-reached
	second := NewService(st, service.client)
	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan error, 1)
	go func() { queued <- second.PublishPerson(ctx, personID) }()
	cancel()
	require.ErrorIs(<-queued, context.Canceled)
	releaseHTTP()
	require.NoError(<-first)
	assert.Equal(t, 1, fixture.puts)
}

func TestSyncSkipsReviewDebtAndPublishesAnotherPersonsOrdinaryChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixtures := map[string]*mutationFixture{"first.vcf": {}, "second.vcf": {}}
	handlers := map[string]http.HandlerFunc{}
	for key, fixture := range fixtures {
		handlers[key] = fixture.handler(t)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "REPORT" {
			var events strings.Builder
			for _, name := range []string{"first.vcf", "second.vcf"} {
				f := fixtures[name]
				f.mu.Lock()
				events.WriteString(cardResponseRaw("/books/personal/"+name, f.etag, string(f.body)))
				f.mu.Unlock()
			}
			writeDAVXML(t, w, syncResponse(events.String(), ""))
			return
		}
		handlers[path.Base(r.URL.Path)](w, r)
	}))
	defer server.Close()
	service, st, _ := newPullService(t, server, false)
	ids := []int64{}
	for _, uid := range []string{"first", "second"} {
		var id int64
		require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?) RETURNING id`), uid, uid).Scan(&id))
		ids = append(ids, id)
		require.NoError(service.PublishPerson(t.Context(), id))
	}
	appendInferenceReviewNote(t, st, ids[0], "Unreviewed")
	_, err := st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: ids[1], Text: "Ordinary", Source: store.ProvenanceUser})
	require.NoError(err)
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixtures["first.vcf"].puts)
	assert.Equal(2, fixtures["second.vcf"].puts)
	assert.NotContains(string(fixtures["first.vcf"].body), "Unreviewed")
	assert.Contains(string(fixtures["second.vcf"].body), "NOTE:Ordinary")
}

func TestReviewedPublicationRestartRecoversPersistedOwnershipWithoutAnotherPut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{timeout: true}
	service, st, personID, book := seededMutationService(t, fixture)
	if st.IsPostgreSQL() {
		t.Skip("SQLite backup is used to reopen an independent on-disk archive; PostgreSQL intent reopen is covered by store tests")
	}
	appendInferenceReviewNote(t, st, personID, "Persisted detail")
	beforePins, err := st.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Error(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	databasePath := filepath.Join(t.TempDir(), "restart.db")
	require.NoError(st.BackupDatabase(databasePath))
	reopened, err := store.Open(databasePath)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.NoError(reopened.InitSchema())
	restarted := NewService(reopened, service.client)
	require.NoError(restarted.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	envelope, err := reopened.GetVCardResourceEnvelopeContext(t.Context(), fmt.Sprintf("carddav:%d", book.ID), book.CanonicalURL+"person.vcf")
	require.NoError(err)
	assert.NotEmpty(envelope.NativeMappings)
	afterPins, err := reopened.ListPersonFactPinsContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(beforePins, afterPins)
	next, err := restarted.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Equal(preview.VCard, next.VCard)
	assert.False(next.ReviewRequired)
}

func TestCurrentPublicationApprovalScopeChangesAfterUnpublishAndNewTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Scoped detail")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	allowed := true
	updatedAccount, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: account.BaseURL, Username: account.Username, PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL,
		CredentialsChanged: true,
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: book.CanonicalURL, DisplayName: "Personal", CanCreate: &allowed},
			{CanonicalURL: account.BaseURL + "/books/other/", DisplayName: "Other", CanCreate: &allowed},
		},
	})
	require.NoError(err)
	require.Greater(updatedAccount.ConnectionGeneration, account.ConnectionGeneration)
	require.Len(books, 2)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{IsWriteTarget: true, IsSubscribed: true}))
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	require.ErrorIs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), store.ErrCardDAVReviewStale)
	fresh, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.True(fresh.ReviewRequired)
	assert.NotEqual(preview.ApprovalToken, fresh.ApprovalToken)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, fresh.ApprovalToken))
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(source.Inference.ApprovedAddressBookID)
	assert.Equal(books[1].ID, *source.Inference.ApprovedAddressBookID)
	assert.Equal(2, fixture.puts)
}

func TestPublicationPreviewSizeAcceptsExactLimitAndRejectsOverflow(t *testing.T) {
	body := make([]byte, 32*1024*1024+1)
	require.NoError(t, checkPublicationPreviewSize(body[:len(body)-1]))
	require.ErrorIs(t, checkPublicationPreviewSize(body), ErrCardDAVPreviewTooLarge)
}

func TestCurrentPublicationResolverRetirementRequiresReviewAndRemovesOwnedNote(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	_, err := st.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(err)
	var target personfacts.TargetDescriptor
	for _, candidate := range catalog.Targets {
		if candidate.Slug == store.AttributeSlugNotes {
			target = candidate
		}
	}
	require.NotEmpty(target.Key)
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	generation := personfacts.GenerationInput{
		PersonID: personID, SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "seed", End: "seed-end"}},
		ProgramID: "publication-fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64),
		CatalogFingerprint: "catalog-fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: now, Policy: personfacts.PolicyContext{AllowSensitive: true, ProviderPolicyFingerprint: "policy-v1"},
		Claims: []personfacts.ProposedClaim{{Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(`"Resolved inferred detail"`), Origin: personfacts.OriginExtraction,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{PersonID: personID, SourceClass: personfacts.EvidencePublic,
				Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
				SourceURL: "https://example.test/statement", SubjectPersonID: &personID, SubjectRef: "synthetic-person",
				Excerpt: "Synthetic note statement", SourceVersion: "source-v1", EventTime: now.Add(-time.Hour), RecordedTime: now, IdentityScore: 990}},
		}},
	}
	applied, err := st.ApplyPersonFactGenerationContext(t.Context(), generation, nil)
	require.NoError(err)
	require.NotEmpty(applied.Projections)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Contains(preview.VCard, "NOTE:Resolved inferred detail")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{Limit: 10})
	require.NoError(err)
	require.Len(evidence, 1)
	generation.Claims = nil
	generation.SourceCursors[0].Start, generation.SourceCursors[0].End = "retire", "retire-end"
	generation.ResolvedAt = now.Add(time.Hour)
	generation.EvidenceStatusChanges = []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: "source-v1", Supported: false, Reason: personfacts.EvidenceStatusSourceDeleted}}
	_, err = st.ApplyPersonFactGenerationContext(t.Context(), generation, nil)
	require.NoError(err)
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.NotContains(preview.VCard, "NOTE:")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.NotContains(string(fixture.body), "NOTE:")
	assert.Equal(2, fixture.puts)
}

func TestReviewedPublicationIntentMetadataClearsWithBody(t *testing.T) {
	for _, operation := range []string{"commit", "rollback", "throttle"} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			service, st, personID, _ := seededMutationService(t, &mutationFixture{})
			_, plan, err := service.currentPublicationPlan(t.Context(), personID)
			require.NoError(err)
			pending, err := st.PrepareCardDAVPublicationContext(t.Context(), plan)
			require.NoError(err)
			require.NotEmpty(pending.OutgoingEnvelopeMetadata)
			require.NotNil(pending.ApprovedBodySHA256)
			switch operation {
			case "commit":
				err = st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{
					Publication: *pending, Remote: store.CardDAVRemoteResource{Href: pending.Href,
						RemoteBody: pending.OutgoingBody, SemanticHash: pending.OutgoingSemanticHash, RemoteETag: `"canonical"`},
				})
			case "rollback":
				err = st.RollbackCardDAVPublicationContext(t.Context(), pending)
			case "throttle":
				err = st.RollbackCardDAVPublicationThrottleContext(t.Context(), pending, time.Now().Add(time.Minute))
			}
			require.NoError(err)
			after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			if operation == "rollback" {
				require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
				return
			}
			require.NoError(err)
			assert.Empty(after.OutgoingBody)
			assert.Empty(after.OutgoingEnvelopeMetadata)
			assert.Nil(after.ApprovedBodySHA256)
			assert.Nil(after.ApprovedInferenceRevision)
			assert.Nil(after.ApprovedMutationRevision)
		})
	}
}
