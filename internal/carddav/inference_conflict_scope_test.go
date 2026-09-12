package carddav

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQueuedConflictPreviewRejectsPersonReboundByMerge(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	service, st, oldID, book := seededMutationServiceForServer(t, server)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	body := conflictCard("person", "Person")
	hash, err := SemanticHash(body)
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: hash}}})
	require.NoError(err)
	appendInferenceReviewNote(t, st, oldID, "Local inference")
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	remote := store.CardDAVRemoteResource{Href: mapping.Href, RemoteETag: `"changed"`, RemoteBody: conflictCard("person", "Changed"), SemanticHash: "changed"}
	capture, needed, err := service.prepareMappingConflict(t.Context(), books[0], *mapping, &remote, false)
	require.NoError(err)
	require.True(needed)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	var survivorID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons(vcard_uid,display_name) VALUES ('survivor','Survivor') RETURNING id`).Scan(&survivorID))
	old, err := st.GetPersonContext(t.Context(), oldID)
	require.NoError(err)
	survivor, err := st.GetPersonContext(t.Context(), survivorID)
	require.NoError(err)
	oldRelease, err := st.AcquireCardDAVPersonOperation(t.Context(), oldID)
	require.NoError(err)
	newRelease, err := st.AcquireCardDAVPersonOperation(t.Context(), survivorID)
	require.NoError(err)
	defer newRelease()
	reached, resume := make(chan struct{}), make(chan struct{})
	service.conflictOperationMappingReadHook = func() {
		close(reached)
		select {
		case <-resume:
		case <-t.Context().Done():
		}
	}
	done := make(chan error, 1)
	go func() { _, err := service.PreviewConflictPublication(t.Context(), conflict.ID); done <- err }()
	<-reached
	_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivorID, AbsorbedID: oldID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: old.Revision, Actor: "test", IdempotencyKey: "queued-conflict-merge"})
	require.NoError(err)
	oldRelease()
	close(resume)
	assert.ErrorIs(t, <-done, store.ErrCardDAVReviewStale)
}

func TestNonWriteConflictPreservesOrdinaryPublicationAcrossRecovery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart_%t", restart), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixtures := map[string]*mutationFixture{"/books/personal/": {}, "/books/subscribed/": {}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for prefix, f := range fixtures {
					if strings.HasPrefix(r.URL.Path, prefix) {
						f.handler(t)(w, r)
						return
					}
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(server.Close)
			service, st, personID, w := seededMutationServiceForServer(t, server)
			if restart && st.IsPostgreSQL() {
				t.Skip("real file reopen uses SQLite backup; PostgreSQL durable intent reopening covered by store contracts")
			}
			appendInferenceReviewNote(t, st, personID, "Initially approved")
			preview, err := service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			allowed := true
			account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: w.CanonicalURL, DisplayName: "Write", CanCreate: &allowed}, {CanonicalURL: server.URL + "/books/subscribed/", DisplayName: "Subscribed", CanCreate: &allowed}}})
			require.NoError(err)
			s := books[1]
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), s.ID, store.CardDAVBookRoles{IsSubscribed: true}))
			books, err = st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			s = books[1]
			body := conflictCard("person", "Remote base")
			hash, err := SemanticHash(body)
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: s.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: s.SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: s.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: hash}}})
			require.NoError(err)
			appendInferenceReviewNote(t, st, personID, "Later inferred change")
			mapping, err := st.GetCardDAVResourceContext(t.Context(), s.ID, s.CanonicalURL+"person.vcf")
			require.NoError(err)
			books, err = st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			s = books[1]
			remoteBody := conflictCard("person", "Remote changed")
			remoteHash, err := SemanticHash(remoteBody)
			require.NoError(err)
			remote := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: "person", RemoteETag: `"changed"`, RemoteBody: remoteBody, SemanticHash: remoteHash}
			capture, needed, err := service.prepareMappingConflict(t.Context(), s, *mapping, &remote, false)
			require.NoError(err)
			require.True(needed)
			conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
			require.NoError(err)
			sf := fixtures["/books/subscribed/"]
			sf.body, sf.etag = remoteBody, remote.RemoteETag
			ordinary, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			preview, err = service.PreviewConflictPublication(t.Context(), conflict.ID)
			require.NoError(err)
			require.NoError(service.ApproveConflictPublication(t.Context(), conflict.ID, preview.ApprovalToken))
			if restart {
				sf.timeout = true
				service.client.requestTimeout = 250 * time.Millisecond
				require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
				during, err := st.GetCardDAVPublicationContext(t.Context(), personID)
				require.NoError(err)
				assert.Equal(ordinary, during)
				// Another pull can advance the book after the server accepted the
				// PUT. Recovery must confirm it without replaying the request or
				// approving the ordinary publication.
				source, err := st.LoadCardDAVConflictReviewSourceContext(t.Context(), conflict.ID)
				require.NoError(err)
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
					AddressBookID: s.ID, ConnectionGeneration: source.ConnectionGeneration,
					SyncRevision: source.Book.SyncRevision, NextSyncToken: "advanced-before-recovery",
				})
				require.NoError(err)
				databasePath := filepath.Join(t.TempDir(), "scoped-recovery.db")
				require.NoError(st.BackupDatabase(databasePath))
				reopened, err := store.Open(databasePath)
				require.NoError(err)
				t.Cleanup(func() { _ = reopened.Close() })
				require.NoError(reopened.InitSchema())
				st = reopened
				service = NewService(reopened, service.client)
				_, err = service.Sync(t.Context(), SyncOptions{Full: true})
				require.NoError(err)
			} else {
				require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
			}
			after, err := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			assert.Equal(ordinary, after)
			source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
			require.NoError(err)
			assert.Equal(before.Inference, source.Inference)
			require.NoError(service.ReconcilePublications(t.Context()))
			assert.Equal(1, fixtures["/books/personal/"].puts)
			assert.Equal(1, sf.puts)
			preview, err = service.PreviewPublication(t.Context(), personID)
			require.NoError(err)
			require.NoError(service.PublishReviewedPerson(t.Context(), personID, preview.ApprovalToken))
			assert.Equal(2, fixtures["/books/personal/"].puts)
			assert.Equal(1, sf.puts)
		})
	}
}

func TestLegacyNonWriteConflictRecoveryDoesNotEnrollPublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	appendInferenceReviewNote(t, st, personID, "Approved local inference")
	mapping, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	book = books[0]
	remote := store.CardDAVRemoteResource{Href: mapping.Href, RemoteUID: "person", RemoteETag: `"changed"`, RemoteBody: conflictCard("person", "Changed remote")}
	remote.SemanticHash, err = SemanticHash(remote.RemoteBody)
	require.NoError(err)
	capture, needed, err := service.prepareMappingConflict(t.Context(), book, *mapping, &remote, false)
	require.NoError(err)
	require.True(needed)
	c, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(err)
	preview, err := service.PreviewConflictPublication(t.Context(), c.ID)
	require.NoError(err)
	require.NoError(service.ApproveConflictPublication(t.Context(), c.ID, preview.ApprovalToken))
	c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
	require.NoError(err)
	hash, err := SemanticHash(c.LocalBody)
	require.NoError(err)
	pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteETag: c.RemoteETag, OutgoingSemanticHash: hash})
	require.NoError(err)
	require.False(pending.ConflictOwned)
	// This persisted ordinary row represents the pre-fix non-write conflict
	// recovery format. A canonical GET proves its historical write succeeded.
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET is_write_target=FALSE,is_subscribed=TRUE WHERE id=?`), book.ID)
	require.NoError(err)
	fixture.mu.Lock()
	fixture.body = pending.OutgoingBody
	fixture.etag = `"canonical"`
	puts := fixture.puts
	fixture.mu.Unlock()
	require.NoError(service.ResolveConflict(t.Context(), c.ID, ResolutionKeepLocal))
	assert.Equal(puts, fixture.puts)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	require.NoError(service.ReconcilePublications(t.Context()))
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(puts, fixture.puts)
}

func TestConflictOwnedAbsentCreateRecovery(t *testing.T) {
	for _, change := range []string{"inference", "book", "generation"} {
		t.Run(change, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := &mutationFixture{}
			service, st, personID, book := seededMutationService(t, fixture)
			body := conflictCard("person", "Remote base")
			hash, err := SemanticHash(body)
			require.NoError(err)
			account, err := st.GetCardDAVAccountContext(t.Context())
			require.NoError(err)
			_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{{Href: book.CanonicalURL + "person.vcf", RemoteUID: "person", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: hash}}})
			require.NoError(err)
			require.NoError(st.SetCardDAVBookRolesContext(t.Context(), book.ID, store.CardDAVBookRoles{IsSubscribed: true}))
			appendInferenceReviewNote(t, st, personID, "First inference")
			mapping, err := st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
			require.NoError(err)
			books, err := st.ListCardDAVAddressBooksContext(t.Context())
			require.NoError(err)
			book = books[0]
			capture, needed, err := service.prepareMappingConflict(t.Context(), book, *mapping, nil, true)
			require.NoError(err)
			require.True(needed)
			c, err := st.RecordCardDAVConflictContext(t.Context(), capture)
			require.NoError(err)
			preview, err := service.PreviewConflictPublication(t.Context(), c.ID)
			require.NoError(err)
			require.NoError(service.ApproveConflictPublication(t.Context(), c.ID, preview.ApprovalToken))
			c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			hash, err = SemanticHash(c.LocalBody)
			require.NoError(err)
			pending, err := st.PrepareCardDAVConflictLocalContext(t.Context(), store.CardDAVConflictLocalPlan{ConflictID: c.ID, ExpectedMappingRevision: c.MappingRevision, RemoteTombstone: true, OutgoingSemanticHash: hash})
			require.NoError(err)
			require.True(pending.ConflictOwned)
			appendInferenceReviewNote(t, st, personID, "New inference after captured intent")
			if change != "inference" {
				switch change {
				case "book":
					_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: pending.ConnectionGeneration, SyncRevision: pending.BookSyncRevision, NextSyncToken: "advanced"})
				case "generation":
					_, err = st.DB().Exec(`UPDATE carddav_accounts SET connection_generation=connection_generation+1`)
				}
				require.NoError(err)
				require.ErrorIs(service.ResolveConflict(t.Context(), c.ID, ResolutionKeepLocal), store.ErrCardDAVReviewStale)
				assert.Equal(1, fixture.gets)
				assert.Zero(fixture.puts)
				c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
				require.NoError(err)
				assert.Empty(c.LocalMutationIntent)
				mapping, err = st.GetCardDAVResourceForPersonContext(t.Context(), book.ID, personID)
				require.NoError(err)
				assert.Equal(pending.PreviousMappingRevision, mapping.MappingRevision)
				preview, err = service.PreviewConflictPublication(t.Context(), c.ID)
				require.NoError(err)
				require.NoError(service.ApproveConflictPublication(t.Context(), c.ID, preview.ApprovalToken))
				require.NoError(service.ResolveConflict(t.Context(), c.ID, ResolutionKeepLocal))
				assert.Equal(1, fixture.puts)
				assert.Contains(string(fixture.body), "New inference after captured intent")
				return
			}
			require.NoError(service.ResolveConflict(t.Context(), c.ID, ResolutionKeepLocal))
			assert.Equal(1, fixture.puts)
			assert.Equal(0, fixture.deletes)
			assert.Equal(pending.OutgoingBody, fixture.body)
			assert.NotContains(string(fixture.body), "New inference after captured intent")
			var inferenceRevision, approvedRevision int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT inference_revision,approved_revision FROM person_carddav_inference_state WHERE person_id=?`), personID).Scan(&inferenceRevision, &approvedRevision))
			assert.Greater(inferenceRevision, *pending.ApprovedInferenceRevision)
			assert.Equal(int64(0), approvedRevision)
			c, err = st.GetCardDAVConflictContext(t.Context(), c.ID)
			require.NoError(err)
			assert.Empty(c.LocalMutationIntent)
			assert.Equal(store.CardDAVConflictResolved, c.Status)
			_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
			require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
		})
	}
}
