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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	for _, source := range []store.Provenance{store.ProvenanceUser, store.ProvenanceSystem} {
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
	second := NewService(st, service.dav().client)
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
	t.Parallel()
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
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{timeout: true}
	service, st, personID, book := seededMutationService(t, fixture)
	service.dav().client.requestTimeout = 250 * time.Millisecond //nolint:kennlint // deadlinetest: the fixture withholds the mutation response until expiry to test recovery without replay
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
	restarted := NewService(reopened, service.dav().client)
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
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Scoped detail")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	account, err := st.GetCardDAVAccountByIDContext(t.Context(), store.DefaultCardDAVAccountID)
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
	t.Parallel()
	body := make([]byte, 32*1024*1024+1)
	require.NoError(t, checkPublicationPreviewSize(body[:len(body)-1]))
	require.ErrorIs(t, checkPublicationPreviewSize(body), ErrCardDAVPreviewTooLarge)
}

func TestCurrentPublicationResolverRetirementRequiresReviewAndRemovesOwnedNote(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	for _, operation := range []string{"commit", "rollback", "throttle"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
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

func TestInferenceReviewTracksDerivedFullNameSelection(t *testing.T) {
	inferredName := func(kind store.PersonNameKind, given string) store.PersonNameInput {
		return store.PersonNameInput{
			NameKind: kind, GivenName: new(given), FamilyName: new("Example"),
			OriginalValue: given + ";Example",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
		}
	}
	tests := []struct {
		name         string
		initial      []store.PersonNameInput
		replaceIndex int
		replacement  store.PersonNameInput
		wantBefore   string
		wantAfter    string
	}{
		{
			name:         "structured name replaced by phonetic name",
			initial:      []store.PersonNameInput{inferredName(store.PersonNameStructured, "Agent")},
			replaceIndex: 0,
			replacement:  inferredName(store.PersonNamePhonetic, "Agent"),
			wantBefore:   "Agent Example",
			wantAfter:    "Local Display",
		},
		{
			name: "structured names reordered",
			initial: []store.PersonNameInput{
				inferredName(store.PersonNameStructured, "First"),
				inferredName(store.PersonNameStructured, "Second"),
			},
			replaceIndex: 0,
			replacement:  inferredName(store.PersonNameStructured, "First"),
			wantBefore:   "First Example",
			wantAfter:    "Second Example",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Display"))
			require.NoError(err)
			for _, input := range tt.initial {
				_, err = st.AddPersonNameContext(t.Context(), personID, input)
				require.NoError(err)
			}

			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.True(preview.ReviewRequired)
			assert.Contains(preview.VCard, "FN:"+tt.wantBefore+"\r\n")
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

			profile, err := st.GetPersonProfileContext(t.Context(), personID)
			require.NoError(err)
			require.Len(profile.Names, len(tt.initial))
			_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
				Names: &store.PersonNamePatch{
					Supersede: []int64{profile.Names[tt.replaceIndex].Envelope.ID},
					Add:       []store.PersonNameInput{tt.replacement},
				},
			})
			require.NoError(err)

			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:"+tt.wantAfter+"\r\n")
			assert.NotContains(preview.VCard, "FN:"+tt.wantBefore+"\r\n")
			assert.True(preview.ReviewRequired)
			require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}

func TestImportConnectionIdentityChangeReviewsExposedInferredFullName(t *testing.T) {
	for _, tc := range []struct {
		name           string
		source         store.Provenance
		reviewRequired bool
	}{
		{"inferred name", store.ProvenanceEnrichment, true},
		{"declared name", store.ProvenanceUser, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, publicationBook := newPullService(t, server, false)
			input := store.CardDAVDiscoveryInput{
				ConnectionName: "import", BaseURL: "https://import.example.test/dav", Username: "before",
				PrincipalURL: "https://import.example.test/principal/", HomeURL: "https://import.example.test/books/",
				Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://import.example.test/books/contacts/", CanCreate: new(false)}},
			}
			account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			require.Len(books, 1)
			importBook := books[0]
			require.NotEqual(publicationBook.AccountID, importBook.AccountID)
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), importBook.ID, store.CardDAVBookRoles{
				IsSubscribed: true, IsLookupSource: true,
			}))
			books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
			require.NoError(err)
			require.Len(books, 1)
			importBook = books[0]
			resource, err := parseRemoteResource(importBook.CanonicalURL+"person.vcf", `"imported"`,
				[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Imported Example\r\nEND:VCARD\r\n"))
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
				AddressBookID: importBook.ID, ConnectionGeneration: account.ConnectionGeneration,
				SyncRevision: importBook.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource},
			})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), importBook.ID, resource.Href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
				NameKind: store.PersonNameStructured, GivenName: new("Agent"), FamilyName: new("Example"),
				OriginalValue: "Agent Example", Envelope: store.ValueEnvelopeInput{Source: tc.source},
			})
			require.NoError(err)
			appendInferenceReviewNote(t, st, personID, "Reviewed detail")
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.Contains(preview.VCard, "FN:Imported Example\r\n")
			require.True(preview.ReviewRequired)
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			require.Equal(new(publicationBook.ID), before.Inference.ApprovedAddressBookID)
			require.Equal(before.Inference.InferenceRevision, before.Inference.ApprovedRevision)

			input.Username = "after"
			_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
			require.NoError(err)
			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:Agent Example\r\n")
			assert.NotContains(preview.VCard, "FN:Imported Example\r\n")
			assert.Equal(tc.reviewRequired, preview.ReviewRequired)
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			if tc.reviewRequired {
				assert.Greater(after.Inference.InferenceRevision, before.Inference.InferenceRevision)
				require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
				fixture.mu.Lock()
				assert.Equal(1, fixture.puts)
				assert.Contains(string(fixture.body), "FN:Imported Example\r\n")
				fixture.mu.Unlock()
				require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			} else {
				assert.Equal(before.Inference, after.Inference)
				require.NoError(service.PublishPerson(t.Context(), personID))
			}
			fixture.mu.Lock()
			assert.Equal(2, fixture.puts)
			assert.Contains(string(fixture.body), "FN:Agent Example\r\n")
			fixture.mu.Unlock()
		})
	}
}

func TestKeepRemoteFullNameRemovalReviewsInferredNameInAnotherBook(t *testing.T) {
	for _, tc := range []struct {
		name           string
		source         store.Provenance
		reviewRequired bool
	}{
		{"inferred name", store.ProvenanceEnrichment, true},
		{"declared name", store.ProvenanceUser, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			publication := &mutationFixture{exactGet: true}
			imported := &mutationFixture{
				body: conflictCard("person", "Imported Example"), etag: `"imported"`,
				href: "/books/subscribed/person.vcf", exactGet: true,
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/books/subscribed/") {
					imported.handler(t)(w, r)
				} else {
					publication.handler(t)(w, r)
				}
			}))
			t.Cleanup(server.Close)
			service, st, publicationBook := newPullService(t, server, false)
			_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
				BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/",
				Books: []store.CardDAVDiscoveredBook{
					{CanonicalURL: publicationBook.CanonicalURL, CanCreate: new(true)},
					{CanonicalURL: server.URL + "/books/subscribed/", CanCreate: new(false)},
				},
			})
			require.NoError(err)
			require.Len(books, 2)
			importBook := books[1]
			require.NotEqual(publicationBook.ID, importBook.ID)
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), importBook.ID, store.CardDAVBookRoles{IsSubscribed: true}))
			_, err = service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), importBook.ID, server.URL+imported.href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
				NameKind: store.PersonNameStructured, GivenName: new("Agent"), FamilyName: new("Example"),
				OriginalValue: "Agent Example", Envelope: store.ValueEnvelopeInput{Source: tc.source},
			})
			require.NoError(err)
			appendInferenceReviewNote(t, st, personID, "Reviewed detail")
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.Contains(preview.VCard, "FN:Imported Example\r\n")
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			require.Equal(new(publicationBook.ID), before.Inference.ApprovedAddressBookID)
			require.Equal(before.Inference.InferenceRevision, before.Inference.ApprovedRevision)

			imported.mu.Lock()
			imported.body = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nEND:VCARD\r\n")
			imported.etag = `"removed-name"`
			imported.mu.Unlock()
			_, err = service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			conflicts, err := service.ListConflicts(t.Context())
			require.NoError(err)
			require.Len(conflicts, 1)
			require.Equal(mapping.Href, conflicts[0].Href)
			require.NoError(service.ResolveConflict(t.Context(), conflicts[0].ID, ResolutionKeepRemote))

			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:Agent Example\r\n")
			assert.NotContains(preview.VCard, "FN:Imported Example\r\n")
			assert.Equal(tc.reviewRequired, preview.ReviewRequired)
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			if tc.reviewRequired {
				assert.Greater(after.Inference.InferenceRevision, before.Inference.InferenceRevision)
				require.NoError(service.ReconcilePublications(t.Context()))
				publication.mu.Lock()
				assert.Equal(1, publication.puts)
				assert.Contains(string(publication.body), "FN:Imported Example\r\n")
				publication.mu.Unlock()
				require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
				require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			} else {
				assert.Equal(before.Inference, after.Inference)
				require.NoError(service.PublishPerson(t.Context(), personID))
			}
			publication.mu.Lock()
			assert.Equal(2, publication.puts)
			assert.Contains(string(publication.body), "FN:Agent Example\r\n")
			publication.mu.Unlock()
		})
	}
}

func TestInferenceReviewTracksRenderedPhoneticFullNameFallback(t *testing.T) {
	phoneticName := func(given string, ordinal int) store.PersonNameInput {
		return store.PersonNameInput{
			NameKind: store.PersonNamePhonetic, GivenName: new(given), FamilyName: new("Example"),
			OriginalValue: given + ";Example",
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceEnrichment, Ordinal: &ordinal,
			},
		}
	}

	t.Run("clearing display name exposes phonetic fallback", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		fixture := &mutationFixture{}
		service, st, personID, _ := seededMutationService(t, fixture)
		person, err := st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Local Display"))
		require.NoError(err)
		_, err = st.AddPersonNameContext(t.Context(), personID, phoneticName("Agent", 0))
		require.NoError(err)

		preview, err := service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		require.Contains(preview.VCard, "FN:Local Display\r\n")
		require.True(preview.ReviewRequired)
		require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

		person, err = st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
		require.NoError(err)

		preview, err = service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		assert.Contains(preview.VCard, "FN:Agent Example\r\n")
		assert.NotContains(preview.VCard, "FN:Local Display\r\n")
		assert.True(preview.ReviewRequired)
		require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
		fixture.mu.Lock()
		assert.Equal(1, fixture.puts)
		fixture.mu.Unlock()
	})

	t.Run("replacing first phonetic name changes fallback ordering", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		fixture := &mutationFixture{}
		service, st, personID, _ := seededMutationService(t, fixture)
		person, err := st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
		require.NoError(err)
		first, err := st.AddPersonNameContext(t.Context(), personID, phoneticName("First", 0))
		require.NoError(err)
		_, err = st.AddPersonNameContext(t.Context(), personID, phoneticName("Second", 1))
		require.NoError(err)

		preview, err := service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		require.Contains(preview.VCard, "FN:First Example\r\n")
		require.True(preview.ReviewRequired)
		require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

		profile, err := st.GetPersonProfileContext(t.Context(), personID)
		require.NoError(err)
		require.Len(profile.Names, 2)
		_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
			Names: &store.PersonNamePatch{
				Supersede: []int64{first.Envelope.ID},
				Add:       []store.PersonNameInput{phoneticName("First", 2)},
			},
		})
		require.NoError(err)

		preview, err = service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		assert.Contains(preview.VCard, "FN:Second Example\r\n")
		assert.NotContains(preview.VCard, "FN:First Example\r\n")
		assert.True(preview.ReviewRequired)
		require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
		fixture.mu.Lock()
		assert.Equal(1, fixture.puts)
		fixture.mu.Unlock()
	})
}

func TestInferenceReviewTracksRenderedContactFullNameFallback(t *testing.T) {
	tests := []struct {
		name        string
		kind        store.ContactAddressKind
		firstValue  string
		secondValue string
		firstFN     string
		secondFN    string
	}{
		{
			name: "email fallback", kind: store.ContactAddressEmail,
			firstValue: "first@example.test", secondValue: "second@example.test",
			firstFN: "first@example.test", secondFN: "second@example.test",
		},
		{
			name: "telephone fallback", kind: store.ContactAddressPhone,
			firstValue: "+12025550101", secondValue: "+12025550102",
			firstFN: "+12025550101", secondFN: "+12025550102",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
			require.NoError(err)
			firstOrdinal, secondOrdinal := 0, 1
			first, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: tt.kind, OriginalValue: tt.firstValue,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment, Ordinal: &firstOrdinal},
			})
			require.NoError(err)
			_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: tt.kind, OriginalValue: tt.secondValue,
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment, Ordinal: &secondOrdinal},
			})
			require.NoError(err)

			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.True(preview.ReviewRequired)
			assert.Contains(preview.VCard, "FN:"+tt.firstFN+"\r\n")
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

			profile, err := st.GetPersonProfileContext(t.Context(), personID)
			require.NoError(err)
			require.Len(profile.ContactPoints, 2)
			replacementOrdinal := 2
			_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
				ContactPoints: &store.PersonContactPointPatch{
					Supersede: []int64{first.Envelope.ID},
					Add: []store.PersonContactPointInput{{
						AddressKind: tt.kind, OriginalValue: tt.firstValue,
						Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment, Ordinal: &replacementOrdinal},
					}},
				},
			})
			require.NoError(err)

			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:"+tt.secondFN+"\r\n")
			assert.NotContains(preview.VCard, "FN:"+tt.firstFN+"\r\n")
			assert.True(preview.ReviewRequired)
			require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}

func TestInferenceReviewTracksEmploymentRenderedFullNameFallback(t *testing.T) {
	t.Run("adding organization removes inferred contact fallback without new debt", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		fixture := &mutationFixture{}
		service, st, personID, _ := seededMutationService(t, fixture)
		person, err := st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
		require.NoError(err)
		_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "agent@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
		})
		require.NoError(err)
		preview, err := service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		require.Contains(preview.VCard, "FN:agent@example.test\r\n")
		require.True(preview.ReviewRequired)
		require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

		organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{
			Name: "Example Company", Kind: store.OrganizationKindCompany,
		})
		require.NoError(err)
		_, err = st.AddEmploymentContext(t.Context(), store.EmploymentInput{
			PersonID: personID, OrganizationID: organization.ID,
			IsCurrent: new(true), IsPrimary: new(true), Source: store.ProvenanceUser,
		})
		require.NoError(err)

		preview, err = service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		assert.Contains(preview.VCard, "FN:Example Company\r\n")
		require.False(preview.ReviewRequired)
		require.NoError(service.PublishPerson(t.Context(), personID))
		fixture.mu.Lock()
		assert.Equal(2, fixture.puts)
		fixture.mu.Unlock()
	})

	for _, operation := range []string{"update", "end", "delete"} {
		t.Run("removing primary organization by "+operation+" reveals contact fallback", func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
			require.NoError(err)
			_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "agent@example.test",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
			})
			require.NoError(err)
			organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{
				Name: "Example Company", Kind: store.OrganizationKindCompany,
			})
			require.NoError(err)
			employment, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{
				PersonID: personID, OrganizationID: organization.ID,
				IsCurrent: new(true), IsPrimary: new(true), Source: store.ProvenanceUser,
			})
			require.NoError(err)

			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.Contains(preview.VCard, "FN:Example Company\r\n")
			require.True(preview.ReviewRequired)
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

			switch operation {
			case "update":
				_, err = st.UpdateEmploymentContext(t.Context(), employment.ID, employment.Revision, store.EmploymentInput{
					PersonID: personID, OrganizationID: organization.ID, IsCurrent: new(false),
					IsPrimary: new(false), Source: store.ProvenanceUser,
				})
			case "end":
				err = func() error {
					_, err := st.EndEmploymentContext(t.Context(), employment.ID, employment.Revision, store.PartialDate{
						Year: new(2026), Month: new(10),
					})
					return err
				}()
			case "delete":
				err = st.DeleteEmploymentContext(t.Context(), employment.ID, employment.Revision)
			}
			require.NoError(err)

			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:agent@example.test\r\n")
			assert.NotContains(preview.VCard, "FN:Example Company\r\n")
			assert.True(preview.ReviewRequired)
			require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}

func TestInferenceReviewTracksDeclaredOrganizationFullNameFallback(t *testing.T) {
	t.Run("supersede", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		fixture := &mutationFixture{}
		service, st, personID, _ := seededMutationService(t, fixture)
		person, err := st.GetPersonContext(t.Context(), personID)
		require.NoError(err)
		_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
		require.NoError(err)
		_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: "agent@example.test",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
		})
		require.NoError(err)
		preview, err := service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		require.Contains(preview.VCard, "FN:agent@example.test\r\n")
		require.True(preview.ReviewRequired)
		require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

		definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{
			UniversalID:   "test-person-organization-fallback",
			ObjectType:    store.AttributeObjectPerson,
			Slug:          "organization",
			Label:         "Organization",
			ValueType:     store.AttributeValueText,
			FieldType:     store.AttributeFieldText,
			Cardinality:   store.AttributeCardinalitySingle,
			Ownership:     store.AttributeOwnershipUser,
			UICreatable:   true,
			UIEditable:    true,
			APIMutable:    true,
			IsAudited:     true,
			IsDeletable:   true,
			VCardProperty: new("ORG"),
		})
		require.NoError(err)
		organization := "Example Company"
		_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: definition.Slug,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &organization},
			Source: store.ProvenanceUser,
		})
		require.NoError(err)
		preview, err = service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		assert.Contains(preview.VCard, "FN:Example Company\r\n")
		assert.False(preview.ReviewRequired, "declaring ORG removes the inferred email name fallback")
		require.NoError(service.PublishPerson(t.Context(), personID))

		_, err = st.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{
			PersonID: personID, DefinitionSlug: definition.Slug,
		})
		require.NoError(err)
		preview, err = service.PreviewPublication(t.Context(), personID)
		require.NoError(err)
		assert.Contains(preview.VCard, "FN:agent@example.test\r\n")
		assert.True(preview.ReviewRequired, "removing ORG exposes the inferred email as FN again")
		require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
		fixture.mu.Lock()
		assert.Equal(2, fixture.puts)
		fixture.mu.Unlock()
	})
}

func TestSupersededEnrichmentNameCannotBypassReviewAfterMergeAndSplit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, survivorID, _ := seededMutationService(t, fixture)
	absorbed, err := st.CreateStandalonePersonContext(t.Context(), store.PersonCreateInput{
		Name: "Alex Agent Copy", Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)
	names, err := st.ListPersonNamesContext(t.Context(), absorbed.ID, true)
	require.NoError(err)
	require.Len(names, 1)
	require.NoError(st.SupersedePersonNameContext(
		t.Context(), absorbed.ID, names[0].Envelope.ID, nil,
	))

	survivor, err := st.GetPersonContext(t.Context(), survivorID)
	require.NoError(err)
	absorbed, err = st.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "merge-retired-enrichment-name", Actor: "test",
	})
	require.NoError(err)
	split, err := st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: survivor.ID, MergeID: merged.Merge.ID,
		ExpectedSourceRevision: merged.Person.Revision,
		IdempotencyKey:         "split-retired-enrichment-name", Actor: "test",
	})
	require.NoError(err)

	preview, err := service.PreviewPublication(t.Context(), split.NewPerson.ID)
	require.NoError(err)
	if strings.Contains(preview.VCard, "FN:Alex Agent Copy\r\n") {
		require.True(preview.ReviewRequired, "a restored inferred name must require publication review")
		require.ErrorIs(service.PublishPerson(t.Context(), split.NewPerson.ID), store.ErrCardDAVInferenceReviewRequired)
		assert.Zero(fixture.puts)
		return
	}
	require.NoError(service.PublishPerson(t.Context(), split.NewPerson.ID))
	assert.NotContains(string(fixture.body), "FN:Alex Agent Copy\r\n")
}

func TestInferenceReviewTracksRemovingAttributeFullNameBeforeEmailFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
	require.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "agent@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceEnrichment},
	})
	require.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{
		UniversalID: "test-person-fullname-attribute", ObjectType: store.AttributeObjectPerson,
		Slug: "test_fullname_attribute", Label: "Preferred full name",
		ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
		Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser,
		UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true,
		IsDeletable: true, VCardProperty: new("FN"),
	})
	require.NoError(err)
	preferredName := "Curated Name"
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: definition.Slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &preferredName},
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Contains(preview.VCard, "FN:Curated Name\r\n")
	require.True(preview.ReviewRequired)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

	_, err = st.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{
		PersonID: personID, DefinitionSlug: definition.Slug,
	})
	require.NoError(err)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Contains(preview.VCard, "FN:agent@example.test\r\n")
	assert.True(preview.ReviewRequired, "revealing an inferred contact as FN requires fresh review")
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
}

// The renderer derives a missing FN from the first EMAIL or phonetic N in the
// merged card, and the merge keeps a persisted occurrence in place while it
// appends a replacement row. Replacing a declared value with an identical row
// at the same ordinal therefore moves an inferred value into FN.
func TestInferenceReviewTracksFallbackReplacementOrder(t *testing.T) {
	email := func(source store.Provenance, value string, ordinal *int) store.PersonContactPointInput {
		return store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: value,
			Envelope: store.ValueEnvelopeInput{Source: source, Ordinal: ordinal},
		}
	}
	phonetic := func(source store.Provenance, given string, ordinal *int) store.PersonNameInput {
		return store.PersonNameInput{
			NameKind: store.PersonNamePhonetic, GivenName: new(given), FamilyName: new("Example"),
			OriginalValue: given + ";Example",
			Envelope:      store.ValueEnvelopeInput{Source: source, Ordinal: ordinal},
		}
	}
	tests := []struct {
		name         string
		declared     string
		inferred     string
		addDeclared  func(t *testing.T, st *store.Store, personID int64)
		addInferred  func(t *testing.T, st *store.Store, personID int64)
		replaceFirst func(t *testing.T, st *store.Store, personID int64)
	}{
		{
			name: "email", declared: "declared@example.test", inferred: "inferred@example.test",
			addDeclared: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				_, err := st.AddPersonContactPointContext(t.Context(), personID,
					email(store.ProvenanceUser, "declared@example.test", nil))
				require.NoError(t, err)
			},
			addInferred: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				_, err := st.AddPersonContactPointContext(t.Context(), personID,
					email(store.ProvenanceEnrichment, "inferred@example.test", nil))
				require.NoError(t, err)
			},
			replaceFirst: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				profile, err := st.GetPersonProfileContext(t.Context(), personID)
				require.NoError(t, err)
				first := profile.ContactPoints[0]
				require.Equal(t, "declared@example.test", first.OriginalValue)
				_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
					ContactPoints: &store.PersonContactPointPatch{
						Supersede: []int64{first.Envelope.ID},
						Add: []store.PersonContactPointInput{
							email(store.ProvenanceUser, "declared@example.test", new(first.Envelope.Ordinal)),
						},
					},
				})
				require.NoError(t, err)
			},
		},
		{
			name: "phonetic name", declared: "Declared Example", inferred: "Inferred Example",
			addDeclared: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				_, err := st.AddPersonNameContext(t.Context(), personID, phonetic(store.ProvenanceUser, "Declared", nil))
				require.NoError(t, err)
			},
			addInferred: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				_, err := st.AddPersonNameContext(t.Context(), personID, phonetic(store.ProvenanceEnrichment, "Inferred", nil))
				require.NoError(t, err)
			},
			replaceFirst: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				profile, err := st.GetPersonProfileContext(t.Context(), personID)
				require.NoError(t, err)
				first := profile.Names[0]
				require.Equal(t, "Declared", *first.GivenName)
				_, err = st.ApplyPersonProfilePatchContext(t.Context(), personID, profile.Person.Revision, store.PersonProfilePatch{
					Names: &store.PersonNamePatch{
						Supersede: []int64{first.Envelope.ID},
						Add: []store.PersonNameInput{
							phonetic(store.ProvenanceUser, "Declared", new(first.Envelope.Ordinal)),
						},
					},
				})
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, _ := seededMutationService(t, fixture)
			person, err := st.GetPersonContext(t.Context(), personID)
			require.NoError(err)
			_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
			require.NoError(err)
			tt.addDeclared(t, st, personID)
			tt.addInferred(t, st, personID)

			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:"+tt.declared+"\r\n")
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

			tt.replaceFirst(t, st, personID)

			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Contains(preview.VCard, "FN:"+tt.inferred+"\r\n")
			assert.True(preview.ReviewRequired)
			require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
			fixture.mu.Lock()
			assert.Equal(1, fixture.puts)
			fixture.mu.Unlock()
		})
	}
}

// An attribute mapped to EMAIL renders as an EMAIL occurrence, so the renderer
// can derive FN from it once a declared ORG no longer supplies the name.
func TestInferenceReviewTracksAttributeEmailFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
	require.NoError(err)
	setAttribute := func(slug, property, value string, source store.Provenance) {
		t.Helper()
		definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{
			UniversalID: "test-" + slug, ObjectType: store.AttributeObjectPerson,
			Slug: slug, Label: slug,
			ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
			Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser,
			UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true,
			IsDeletable: true, VCardProperty: new(property),
		})
		require.NoError(err)
		_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
			PersonID: personID, DefinitionSlug: definition.Slug,
			Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &value},
			Source: source,
		})
		require.NoError(err)
	}
	setAttribute("test_org_attribute", "ORG", "Example Org", store.ProvenanceUser)
	setAttribute("test_email_attribute", "EMAIL", "agent@example.test", store.ProvenanceEnrichment)

	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Contains(preview.VCard, "FN:Example Org\r\n")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

	_, err = st.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{
		PersonID: personID, DefinitionSlug: "test_org_attribute",
	})
	require.NoError(err)
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Contains(preview.VCard, "FN:agent@example.test\r\n")
	assert.True(preview.ReviewRequired, "revealing an inferred EMAIL attribute as FN requires fresh review")
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
}

// The renderer derives a missing FN from the first ORG before EMAIL or TEL.
// Deleting a declared employment can therefore expose an inferred ORG
// attribute as FN.
func TestInferenceReviewTracksOrganizationFallback(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, nil)
	require.NoError(err)
	organization, err := st.CreateOrganizationContext(t.Context(), store.OrganizationInput{Name: "Declared Org"})
	require.NoError(err)
	employment, err := st.AddEmploymentContext(t.Context(), store.EmploymentInput{
		PersonID: personID, OrganizationID: organization.ID,
		IsCurrent: new(true), IsPrimary: new(true), Source: store.ProvenanceUser,
	})
	require.NoError(err)
	definition, err := st.CreateAttributeDefinitionContext(t.Context(), store.AttributeDefinitionInput{
		UniversalID: "test-org-attribute", ObjectType: store.AttributeObjectPerson,
		Slug: "test_org_attribute", Label: "Organization",
		ValueType: store.AttributeValueText, FieldType: store.AttributeFieldText,
		Cardinality: store.AttributeCardinalitySingle, Ownership: store.AttributeOwnershipUser,
		UICreatable: true, UIEditable: true, APIMutable: true, IsAudited: true,
		IsDeletable: true, VCardProperty: new("ORG"),
	})
	require.NoError(err)
	inferredOrg := "Inferred Org"
	_, err = st.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: personID, DefinitionSlug: definition.Slug,
		Value:  store.AttributeValue{Type: store.AttributeValueText, Text: &inferredOrg},
		Source: store.ProvenanceEnrichment,
	})
	require.NoError(err)

	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Contains(preview.VCard, "FN:Declared Org\r\n")
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))

	require.NoError(st.DeleteEmploymentContext(t.Context(), employment.ID, employment.Revision))
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	assert.Contains(preview.VCard, "FN:Inferred Org\r\n")
	assert.True(preview.ReviewRequired, "revealing an inferred ORG as FN requires fresh review")
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
}
