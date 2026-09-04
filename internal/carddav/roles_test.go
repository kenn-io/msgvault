package carddav

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestListBooksAndSetBookRolesUseServiceContract(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	service, _, book := newPullService(t, server, false)

	books, err := service.ListBooks(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal(t, book.ID, books[0].ID)

	require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{
		Subscribed: true, LookupSource: true, WriteTarget: true,
	}))
}

func TestPublicationViewProjectsProspectivePublishedAndConflictState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDAVXML(t, w, syncResponse(cardResponse(
			"/books/personal/alice.vcf", `&quot;one&quot;`, "alice",
		), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	_, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"alice.vcf")
	require.NoError(err)
	require.NotNil(mapping.PersonID)

	view, err := service.PublicationView(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Equal(PublicationUnpublished, view.State)
	assert.False(view.Desired)
	require.NotNil(view.AddressBook)
	assert.Equal(book.ID, view.AddressBook.ID)
	assert.Equal("Personal", view.AddressBook.Name)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE, is_subscribed = FALSE WHERE id = ?`), book.ID)
	require.NoError(err)
	view, err = service.PublicationView(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Nil(view.AddressBook, "an unpublished person has no prospective book when no target is eligible")
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = TRUE, is_subscribed = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)
	_, err = service.PublicationView(t.Context(), *mapping.PersonID+10_000)
	require.ErrorIs(err, store.ErrPersonNotFound)

	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	_, err = st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *mapping.PersonID, Desired: true, AddressBookID: book.ID, Href: mapping.Href,
		OutgoingBody: mapping.RemoteBody, OutgoingSemanticHash: mapping.RemoteSemanticHash,
		LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	view, err = service.PublicationView(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Equal(PublicationPublished, view.State)

	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE, is_subscribed = FALSE WHERE id = ?`), book.ID)
	require.NoError(err)
	view, err = service.PublicationView(t.Context(), *mapping.PersonID)
	require.NoError(err)
	require.NotNil(view.AddressBook, "the publication row remains authoritative after role changes")
	assert.Equal(book.ID, view.AddressBook.ID)

	conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{
		AddressBookID: book.ID, Href: mapping.Href,
		ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash:           mapping.LocalHash, LocalHash: mapping.LocalHash,
		BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		RemoteETag: `"two"`, LocalBody: mapping.RemoteBody, RemoteBody: conflictCard("alice", "Remote"),
	})
	require.NoError(err)
	view, err = service.PublicationView(t.Context(), *mapping.PersonID)
	require.NoError(err)
	assert.Equal(PublicationConflict, view.State)
	require.NotNil(view.ConflictID)
	assert.Equal(conflict.ID, *view.ConflictID)
}

func TestPublicationViewStatePrecedence(t *testing.T) {
	for _, operation := range []store.CardDAVMutationOperation{
		store.CardDAVMutationCreate, store.CardDAVMutationUpdate, store.CardDAVMutationDelete,
	} {
		t.Run(string(operation), func(t *testing.T) {
			view := publicationViewFromSource(&store.CardDAVPublicationStateSource{
				PersonID: 7, HasPublication: true,
				Desired: operation != store.CardDAVMutationDelete, PendingOperation: operation,
				AddressBookID: 2, AddressBookName: "Personal", ConflictID: 9,
			})
			assert.Equal(t, PublicationPending, view.State, "pending wins over a matching conflict")
			assert.Equal(t, operation, view.PendingOperation)
			assert.Nil(t, view.ConflictID)
		})
	}

	conflict := publicationViewFromSource(&store.CardDAVPublicationStateSource{
		PersonID: 7, HasPublication: true, Desired: true,
		AddressBookID: 2, AddressBookName: "Personal", ConflictID: 9,
	})
	assert.Equal(t, PublicationConflict, conflict.State)
	require.NotNil(t, conflict.ConflictID)
	assert.Equal(t, int64(9), *conflict.ConflictID)
}

func TestNonWriteTargetPendingMutationNeverReachesServer(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	service, _, book := newPullService(t, server, false)
	require.NoError(t, service.SetBookRoles(t.Context(), book.ID, BookRoles{
		Subscribed: true, LookupSource: true,
	}))

	err := service.executeMutation(t.Context(), &store.CardDAVPublication{
		PersonID: 1, Desired: true, AddressBookID: book.ID,
		Href: book.CanonicalURL + "person.vcf", PendingOperation: store.CardDAVMutationUpdate,
		OutgoingBody: []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Person\r\nEND:VCARD\r\n"),
		RemoteETag:   `"one"`,
	})
	require.ErrorIs(t, err, store.ErrCardDAVNoWriteTarget)
	assert.Zero(t, requests)
}

func TestRoleWideningForcesOneFullReconcile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		tokens = append(tokens, syncRequestToken(body))
		writeDAVXML(t, w, syncResponse("", "next-token"))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE, is_subscribed = FALSE, is_lookup_source = FALSE,
		    sync_token = ? WHERE id = ?`), "incremental-token", book.ID)
	require.NoError(err)

	require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{LookupSource: true}))
	_, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal([]string{""}, tokens)
	books, err := service.ListBooks(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	assert.False(books[0].NeedsFullReconcile)
	assert.Equal("next-token", books[0].SyncToken)
}

func TestRoleWideningReconcilesUnchangedUnboundResources(t *testing.T) {
	tests := []struct {
		name             string
		prepare          func(t *testing.T, service *Service, st *store.Store, book store.CardDAVAddressBook) int64
		wantSamePerson   bool
		wantRequestCount int
	}{
		{
			name: "lookup-only to subscribed materializes",
			prepare: func(t *testing.T, service *Service, _ *store.Store, book store.CardDAVAddressBook) int64 {
				t.Helper()
				require.NoError(t, service.SetBookRoles(t.Context(), book.ID, BookRoles{
					Subscribed: true, LookupSource: true,
				}))
				require.NoError(t, service.SetBookRoles(t.Context(), book.ID, BookRoles{LookupSource: true}))
				return 0
			},
			wantRequestCount: 3,
		},
		{
			name: "demoted subscription rebinds preserved person",
			prepare: func(t *testing.T, service *Service, st *store.Store, book store.CardDAVAddressBook) int64 {
				t.Helper()
				_, err := service.Sync(t.Context(), SyncOptions{})
				require.NoError(t, err)
				mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"alice.vcf")
				require.NoError(t, err)
				require.NotNil(t, mapping.PersonID)
				person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
				require.NoError(t, err)
				_, err = st.UpdatePersonDisplayNameContext(t.Context(), person.ID, person.Revision, new("User preserved"))
				require.NoError(t, err)
				require.NoError(t, service.SetBookRoles(t.Context(), book.ID, BookRoles{
					Subscribed: true, LookupSource: true,
				}))
				require.NoError(t, service.SetBookRoles(t.Context(), book.ID, BookRoles{LookupSource: true}))
				return person.ID
			},
			wantSamePerson:   true,
			wantRequestCount: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			requests := 0
			failNext := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if failNext {
					failNext = false
					http.Error(w, "temporary failure", http.StatusServiceUnavailable)
					return
				}
				writeDAVXML(t, w, syncResponse(
					cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice"), "",
				))
			}))
			t.Cleanup(server.Close)
			service, st, book := newPullService(t, server, false)
			preparedPersonID := tt.prepare(t, service, st, book)

			_, err := service.Sync(t.Context(), SyncOptions{})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"alice.vcf")
			require.NoError(err)
			assert.Nil(mapping.PersonID)

			require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{
				Subscribed: true, LookupSource: true,
			}))
			failNext = true
			_, err = service.Sync(t.Context(), SyncOptions{})
			require.Error(err)
			books, listErr := service.ListBooks(t.Context())
			require.NoError(listErr)
			require.Len(books, 1)
			assert.True(books[0].NeedsFullReconcile)

			_, err = service.Sync(t.Context(), SyncOptions{})
			require.NoError(err)
			mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"alice.vcf")
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			if tt.wantSamePerson {
				assert.Equal(preparedPersonID, *mapping.PersonID)
			}
			people, err := st.ListPersonsContext(t.Context())
			require.NoError(err)
			assert.Len(people, 1, "role widening must materialize or rebind exactly once")
			books, err = service.ListBooks(t.Context())
			require.NoError(err)
			assert.False(books[0].NeedsFullReconcile)
			assert.Equal(tt.wantRequestCount, requests)
		})
	}
}

func TestRoleWideningPreservesMappedLocalTombstones(t *testing.T) {
	tests := []struct {
		name          string
		changeRemote  bool
		wantConflicts int
	}{
		{name: "unchanged remote stays deleted"},
		{name: "changed remote captures conflict", changeRemote: true, wantConflicts: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			remoteBody := conflictCard("alice", "Alice Base")
			remoteETag := `"one"`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeDAVXML(t, w, syncResponse(cardResponseRaw(
					"/books/personal/alice.vcf", remoteETag, escapedCardData(remoteBody),
				), ""))
			}))
			t.Cleanup(server.Close)
			service, st, book := newPullService(t, server, false)
			require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{
				Subscribed: true, LookupSource: true,
			}))

			_, err := service.Sync(t.Context(), SyncOptions{})
			require.NoError(err)
			href := book.CanonicalURL + "alice.vcf"
			mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
			require.NoError(err)
			require.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))

			deleted, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			require.NoError(err)
			assert.Nil(deleted.PersonID)
			assert.Equal(store.CardDAVMappingMapped, deleted.MappingStatus)

			require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{LookupSource: true}))
			demoted, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			require.NoError(err)
			assert.Nil(demoted.PersonID)
			assert.Equal(store.CardDAVMappingMapped, demoted.MappingStatus,
				"role demotion must retain the local tombstone distinction")

			require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{
				Subscribed: true, LookupSource: true,
			}))
			if tt.changeRemote {
				remoteBody = conflictCard("alice", "Alice Remote")
				remoteETag = `"two"`
			}
			_, err = service.Sync(t.Context(), SyncOptions{})
			require.NoError(err)

			after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
			require.NoError(err)
			assert.Nil(after.PersonID)
			assert.Equal(store.CardDAVMappingMapped, after.MappingStatus)
			if !tt.changeRemote {
				assert.Equal(demoted.MappingRevision, after.MappingRevision,
					"unchanged full reconciliation must not rewrite the tombstone")
				assert.Equal(demoted.RemoteBody, after.RemoteBody)
				assert.Equal(demoted.RemoteETag, after.RemoteETag)
			}
			people, err := st.ListPersonsContext(t.Context())
			require.NoError(err)
			assert.Empty(people)
			conflicts, err := service.ListConflicts(t.Context())
			require.NoError(err)
			assert.Len(conflicts, tt.wantConflicts)
			if tt.changeRemote {
				require.Len(conflicts, 1)
				assert.True(conflicts[0].LocalTombstone)
				assert.Equal(remoteBody, conflicts[0].RemoteBody)
			}
			books, err := service.ListBooks(t.Context())
			require.NoError(err)
			require.Len(books, 1)
			assert.False(books[0].NeedsFullReconcile)
		})
	}
}

func TestSyncStopsWhenBookBecomesIgnoredBetweenFetchAndApply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	var service *Service
	var book store.CardDAVAddressBook
	var roleErr error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			roleErr = service.SetBookRoles(t.Context(), book.ID, BookRoles{})
		}
		writeDAVXML(t, w, syncResponse(
			cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice"), "",
		))
	}))
	t.Cleanup(server.Close)
	var st *store.Store
	service, st, book = newPullService(t, server, false)
	require.NoError(service.SetBookRoles(t.Context(), book.ID, BookRoles{
		Subscribed: true, LookupSource: true,
	}))

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(roleErr)
	require.NoError(err)
	assert.Zero(result.Books)
	assert.Equal(1, requests, "the stale retry must not refetch an ignored book")
	ledger, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	assert.Empty(ledger)
}
