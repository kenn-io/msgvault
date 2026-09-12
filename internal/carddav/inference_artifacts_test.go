package carddav

import (
	"bytes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestLegacyPendingCreateRequiresExactApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET approved_body_sha256=NULL, approved_inference_revision=NULL, approved_mutation_revision=NULL WHERE person_id=?`), personID)
	require.NoError(err)
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(1, fixture.puts)
	assert.Equal(1, fixture.gets)
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(store.CardDAVMutationCreate, pending.PendingOperation)
}

func TestUnpublishCancelsAbsentPendingCreate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	assert.Zero(fixture.deletes)
	_, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestPendingPreviewApprovesImmutableBytesLeavingNewInference(t *testing.T) {
	for _, kind := range []string{"captured", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{putFailures: 2}
			service, st, personID, _ := seededMutationService(t, fixture)
			appendInferenceReviewNote(t, st, personID, "Previously reviewed detail")
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.Error(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotNil(pending.ApprovedInferenceRevision)
			approvedRevision := *pending.ApprovedInferenceRevision
			if kind == "legacy" {
				_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET approved_body_sha256=NULL, approved_inference_revision=NULL, approved_mutation_revision=NULL WHERE person_id=?`), personID)
				require.NoError(err)
				approvedRevision = 0
			}
			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Equal(kind == "legacy", preview.ReviewRequired)
			appendInferenceReviewNote(t, st, personID, "Later private detail")
			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.Equal(PublicationReviewPending, preview.Kind)
			assert.Equal(string(pending.OutgoingBody), preview.VCard)
			assert.NotContains(preview.VCard, "Later private detail")
			assert.True(preview.ReviewRequired)
			require.Error(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			approved, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			require.NotNil(approved.ApprovedInferenceRevision)
			assert.Equal(approvedRevision, *approved.ApprovedInferenceRevision)
			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			assert.True(preview.ReviewRequired)
			if !st.IsPostgreSQL() {
				databasePath := filepath.Join(t.TempDir(), "pending-restart.db")
				require.NoError(st.BackupDatabase(databasePath))
				reopened, err := store.Open(databasePath)
				require.NoError(err)
				t.Cleanup(func() { _ = reopened.Close() })
				require.NoError(reopened.InitSchema())
				service = NewService(reopened, service.client)
			}
			require.NoError(service.PublishPerson(t.Context(), personID))
			assert.Equal(string(pending.OutgoingBody), string(fixture.body))
			view, err := service.PublicationView(t.Context(), personID)
			require.NoError(err)
			assert.True(view.InferenceReviewRequired)
			require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
		})
	}
}

func TestKeepLocalRequiresInferenceArtifactApproval(t *testing.T) {
	require := require.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "Extracted local detail")
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Remote Person")
	fixture.etag = `"remote-other"`
	fixture.force412 = true
	fixture.mu.Unlock()
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), &conflictErr)
	puts := fixture.puts
	require.ErrorIs(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal), store.ErrCardDAVInferenceReviewRequired)
	assert.Equal(t, puts, fixture.puts)
}

func TestConflictPublicationPreviewRefreshesWithoutResolving(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "First local detail")
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Remote Person")
	fixture.etag = `"remote-other"`
	fixture.force412 = true
	fixture.mu.Unlock()
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), &conflictErr)
	appendInferenceReviewNote(t, st, personID, "Second local detail")
	preview, err = service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.Equal(PublicationReviewConflict, preview.Kind)
	assert.Contains(preview.VCard, "Second local detail")
	puts := fixture.puts
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	assert.Equal(puts, fixture.puts)
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, conflict.Status)
	assert.Equal(preview.VCard, string(conflict.LocalBody))
	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	assert.Equal(puts+1, fixture.puts)
	assert.Equal(preview.VCard, string(fixture.body))
}

func TestLegacyPendingCanonicalSuccessDoesNotApproveNewInference(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{timeout: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET approved_body_sha256=NULL, approved_inference_revision=NULL, approved_mutation_revision=NULL WHERE person_id=?`), personID)
	require.NoError(err)
	appendInferenceReviewNote(t, st, personID, "New private detail")
	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	view, err := service.PublicationView(t.Context(), personID)
	require.NoError(err)
	assert.True(view.InferenceReviewRequired)
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(pending.PendingOperation)
}

func TestLegacyPendingReviewFlagAndBothRecoveryLoops(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET approved_body_sha256=NULL, approved_inference_revision=NULL, approved_mutation_revision=NULL WHERE person_id=?`), personID)
	require.NoError(err)
	view, err := service.PublicationView(t.Context(), personID)
	require.NoError(err)
	assert.True(view.InferenceReviewRequired)
	_, err = service.recoverPendingPublications(t.Context())
	require.NoError(err)
	require.NoError(service.ReconcilePublications(t.Context()))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixture.puts)
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(store.CardDAVMutationCreate, pending.PendingOperation)
}

func TestPendingRecoveryAndCancellationShareHTTPBarrierAcrossServices(t *testing.T) {
	server := httptest.NewTestServer(t, nil)
	// The in-memory transport accepts any address; use a literal to avoid DNS.
	server.URL = "http://127.0.0.1"
	// PostgreSQL's shared admin connection lives for the whole test process.
	// Create the database outside the bubble so it does not keep it alive.
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		fixture := &mutationFixture{putFailures: 2}
		entered, resume := make(chan struct{}), make(chan struct{})
		var block atomic.Bool
		handler := fixture.handler(t)
		server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && block.CompareAndSwap(true, false) {
				close(entered)
				select {
				case <-resume:
				case <-r.Context().Done():
					return
				}
			}
			handler(w, r)
		})
		t.Cleanup(server.Close)
		transport, ok := server.Client().Transport.(*http.Transport)
		require.True(ok)
		service.client.dialContext = transport.DialContext
		second := NewService(st, service.client)
		require.Error(service.PublishPerson(t.Context(), personID))
		block.Store(true)
		recoveryDone := make(chan error, 1)
		go func() { recoveryDone <- service.PublishPerson(t.Context(), personID) }()
		<-entered
		cancelDone := make(chan error, 1)
		go func() { cancelDone <- second.UnpublishPerson(t.Context(), personID) }()
		synctest.Wait()
		select {
		case err := <-cancelDone:
			require.FailNow("cancellation returned while prior replay could still PUT", "%v", err)
		default:
		}
		close(resume)
		require.Error(<-recoveryDone)
		require.NoError(<-cancelDone)
		require.NoError(service.ReconcilePublications(t.Context()))
		fixture.mu.Lock()
		assert.Equal(2, fixture.puts)
		assert.Zero(fixture.deletes)
		fixture.mu.Unlock()
		_, err := st.GetCardDAVPublicationContext(t.Context(), personID)
		require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	})
}

func TestConflictReviewSupportsTwoSubscribedNonWriteBooks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixtures := map[string]*mutationFixture{
		"/books/personal/": {}, "/books/second/": {},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for prefix, fixture := range fixtures {
			if strings.HasPrefix(r.URL.Path, prefix) {
				fixture.handler(t)(w, r)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	service, st, personID, first := seededMutationServiceForServer(t, server)
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: first.CanonicalURL, DisplayName: "Personal", CanCreate: &allowed}, {CanonicalURL: server.URL + "/books/second/", DisplayName: "Second", CanCreate: &allowed}},
	})
	require.NoError(err)
	require.Len(books, 2)
	for _, book := range books {
		require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true}))
	}
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	for _, book := range books {
		body := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:person\r\nFN:Shared Person\r\nEND:VCARD\r\n")
		hash, err := SemanticHash(body)
		require.NoError(err)
		_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "shared.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: hash}}})
		require.NoError(err)
		mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"shared.vcf")
		require.NoError(err)
		require.Equal(&personID, mapping.PersonID)
	}
	appendInferenceReviewNote(t, st, personID, "Shared inferred detail")
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	var previews []*PublicationPreview
	for _, book := range books {
		mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"shared.vcf")
		require.NoError(err)
		body := bytes.Replace(mapping.RemoteBody, []byte("Shared Person"), []byte("Remote Person"), 1)
		hash, err := SemanticHash(body)
		require.NoError(err)
		remote := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: "person", RemoteETag: `"changed"`, RemoteBody: body, SemanticHash: hash}
		capture, needed, err := service.prepareMappingConflict(t.Context(), book, *mapping, &remote, false)
		require.NoError(err)
		require.True(needed)
		conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
		require.NoError(err)
		fixture := fixtures[strings.TrimPrefix(book.CanonicalURL, server.URL)]
		fixture.body, fixture.etag = body, remote.RemoteETag
		preview, err := service.PreviewConflictPublication(t.Context(), conflict.ID)
		require.NoError(err)
		assert.Equal(book.ID, preview.AddressBook.ID)
		assert.Equal(personID, preview.PersonID)
		require.NoError(service.ApproveConflictPublication(t.Context(), conflict.ID, preview.ApprovalToken))
		previews = append(previews, preview)
	}
	// Both independent artifacts are approved before either publication exists.
	for _, preview := range previews {
		require.NoError(service.ResolveConflict(t.Context(), *preview.ConflictID, ResolutionKeepLocal))
		conflict, err := st.GetCardDAVConflictContext(t.Context(), *preview.ConflictID)
		require.NoError(err)
		assert.Equal(store.CardDAVConflictResolved, conflict.Status)
		fixture := fixtures["/books/personal/"]
		if preview.AddressBook.ID != first.ID {
			fixture = fixtures["/books/second/"]
		}
		assert.Equal(1, fixture.puts)
		assert.Equal(preview.VCard, string(fixture.body))
	}
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound, "standalone conflicts must not enroll ordinary publication")
	require.NoError(service.ReconcilePublications(t.Context()))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
}

func TestPendingCreateCollisionWithNewInferenceRemainsReviewable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "New local inference")
	fixture.body = conflictCard("other", "Remote collision")
	fixture.etag = `"collision"`
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflictErr)
	assert.Equal(1, fixture.puts)
	preview, err := service.PreviewConflictPublication(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Contains(preview.VCard, "New local inference")
	require.NoError(service.ApproveConflictPublication(t.Context(), conflictErr.ID, preview.ApprovalToken))
	assert.Equal(1, fixture.puts)
}

func TestPendingApprovalUsesCanonicalGETBeforeStaleToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{putFailures: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	appendInferenceReviewNote(t, st, personID, "Changed after preview")
	gets := fixture.gets
	require.ErrorIs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), store.ErrCardDAVReviewStale)
	assert.Equal(gets+1, fixture.gets)
	assert.Equal(1, fixture.puts)
}

func TestConflictKeepRemoteDoesNotGrantArtifactInferenceApproval(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	addProjectedEmail(t, st, personID)
	fixture.body = conflictCard("person", "Remote Person")
	fixture.etag = `"changed"`
	fixture.force412 = true
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflictErr)
	appendInferenceReviewNote(t, st, personID, "Unpublished inferred note")
	preview, err := service.PreviewConflictPublication(t.Context(), conflictErr.ID)
	require.NoError(err)
	require.NoError(service.ApproveConflictPublication(t.Context(), conflictErr.ID, preview.ApprovalToken))
	before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	puts := fixture.puts
	require.NoError(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepRemote))
	assert.Equal(puts, fixture.puts)
	after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(before.Inference.ApprovedRevision, after.Inference.ApprovedRevision)
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Nil(conflict.ApprovedConflictRevision)
	assert.Nil(conflict.ApprovedLocalBodySHA256)
	assert.Empty(conflict.LocalEnvelopeMetadata)
}

func TestCancelAbsentPendingCreateAfterPullMaterializedMapping(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{timeout: true}
	service, st, personID, book := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	remote, absent, err := service.fetchCanonical(t.Context(), pending.Href)
	require.NoError(err)
	require.False(absent)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, pending.Href)
	require.NoError(err)
	require.Equal(&personID, mapping.PersonID)
	fixture.body = nil
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	assert.Zero(fixture.deletes)
	assert.Equal(1, fixture.puts)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, pending.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestApprovedConflictRemoteChangeRequiresRefreshBeforeResolution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	addProjectedEmail(t, st, personID)
	fixture.body = conflictCard("person", "Remote Person")
	fixture.etag = `"changed"`
	fixture.force412 = true
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflictErr)
	appendInferenceReviewNote(t, st, personID, "Inferred note")
	preview, err := service.PreviewConflictPublication(t.Context(), conflictErr.ID)
	require.NoError(err)
	require.NoError(service.ApproveConflictPublication(t.Context(), conflictErr.ID, preview.ApprovalToken))
	fixture.body = conflictCard("person", "Newest Remote Person")
	fixture.etag = `"newest"`
	puts := fixture.puts
	require.ErrorIs(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal), store.ErrCardDAVReviewStale)
	assert.Equal(puts, fixture.puts)
	refreshed, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Equal(fixture.etag, refreshed.RemoteETag)
	assert.Nil(refreshed.ApprovedConflictRevision)
	preview, err = service.PreviewConflictPublication(t.Context(), conflictErr.ID)
	require.NoError(err)
	require.NoError(service.ApproveConflictPublication(t.Context(), conflictErr.ID, preview.ApprovalToken))
	require.NoError(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal))
	assert.Equal(puts+1, fixture.puts)
}

func TestConflictUnpublishClearsInferenceApprovalScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	appendInferenceReviewNote(t, st, personID, "Approved local note")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
	fixture.body = conflictCard("person", "Remote Person")
	fixture.etag = `"changed"`
	fixture.delete412 = true
	var conflictErr *ConflictError
	require.ErrorAs(service.UnpublishPerson(t.Context(), personID), &conflictErr)
	require.NoError(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal))
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
	require.NoError(err)
	assert.Nil(source.Inference.ApprovedAddressBookID)
	assert.Nil(source.Inference.ApprovedConnectionGeneration)
	assert.Zero(source.Inference.ApprovedRevision)
	require.ErrorIs(service.PublishPerson(t.Context(), personID), store.ErrCardDAVInferenceReviewRequired)
}

func TestPublicationReviewStaysStaleWhenTargetIsRemoved(t *testing.T) {
	require := require.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	appendInferenceReviewNote(t, st, personID, "Reviewed candidate")
	preview, err := service.PreviewPublication(t.Context(), personID)
	require.NoError(err)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{}))
	require.ErrorIs(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken), store.ErrCardDAVReviewStale)
	assert.Zero(t, fixture.puts)
}
