package carddav

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func conflictCard(uid, name string) []byte {
	return []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:" + name + "\r\nEND:VCARD\r\n")
}

func conflictCardWithEmail(uid, name, email string) []byte {
	return []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:" + name +
		"\r\nEMAIL:" + email + "\r\nEND:VCARD\r\n")
}

func escapedCardData(body []byte) string {
	value := strings.ReplaceAll(string(body), "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

func TestPullConflictBlocksOnlyMappingAndAdvancesBookFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	cards := map[string]struct {
		body []byte
		etag string
	}{
		"alice": {body: conflictCard("alice", "Alice Base"), etag: `"alice-1"`},
		"bob":   {body: conflictCard("bob", "Bob Base"), etag: `"bob-1"`},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var events strings.Builder
		for _, uid := range []string{"alice", "bob"} {
			card := cards[uid]
			events.WriteString(cardResponseRaw("/books/personal/"+uid+".vcf", card.etag, escapedCardData(card.body)))
		}
		writeDAVXML(t, w, syncResponse(events.String(), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	beforeRevision := books[0].SyncRevision
	alice, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	require.NotNil(alice.PersonID)
	bob, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/bob.vcf")
	require.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), *alice.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	mu.Lock()
	cards["alice"] = struct {
		body []byte
		etag string
	}{body: conflictCard("alice", "Alice Remote"), etag: `"alice-2"`}
	cards["bob"] = struct {
		body []byte
		etag string
	}{body: conflictCard("bob", "Bob Remote"), etag: `"bob-2"`}
	mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	books, err = st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	assert.Equal(beforeRevision+1, books[0].SyncRevision)

	afterAlice, err := st.GetCardDAVResourceContext(t.Context(), book.ID, alice.Href)
	require.NoError(err)
	assert.Equal(alice.RemoteBody, afterAlice.RemoteBody, "conflicted mapping must retain its last common remote state")
	afterBob, err := st.GetCardDAVResourceContext(t.Context(), book.ID, bob.Href)
	require.NoError(err)
	assert.Equal(cards["bob"].body, afterBob.RemoteBody, "unrelated mapping must continue applying")

	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.Equal(alice.Href, conflicts[0].Href)
	assert.Contains(string(conflicts[0].LocalBody), "EMAIL:alice-local@example.test")
	assert.Equal(cards["alice"].body, conflicts[0].RemoteBody)

	views, err := service.ListConflictViews(t.Context())
	require.NoError(err)
	require.Len(views, 1)
	assert.Equal(book.ID, views[0].AddressBook.ID)
	assert.NotContains(views[0].AddressBook.Name, "http")
	assert.Equal(ConflictSidePresent, views[0].LocalState)
	assert.Equal(ConflictSidePresent, views[0].RemoteState)
	assert.Equal([]ResolutionChoice{ResolutionKeepLocal, ResolutionKeepRemote}, views[0].AllowedResolutions)

	detail, err := service.GetConflictView(t.Context(), conflicts[0].ID)
	require.NoError(err)
	assert.Equal(ConflictSidePresent, detail.Base.State)
	assert.Equal("Alice Base", detail.Base.DisplayName)
	assert.Equal("Alice Remote", detail.Remote.DisplayName)
	assert.Contains(detail.Local.Emails, "alice-local@example.test")
}

func TestPullConflictCapturesLocalEditAgainstRemoteDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	deleted := false
	body := conflictCard("alice", "Alice Base")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			if deleted {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"one"`)
			_, err := w.Write(body)
			assert.NoError(err)
			return
		}
		events := ""
		if !deleted {
			events = cardResponseRaw("/books/personal/alice.vcf", `"one"`, escapedCardData(body))
		}
		writeDAVXML(t, w, syncResponse(events, ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications
		(person_id, desired, address_book_id, href) VALUES (?, TRUE, ?, ?)`),
		*mapping.PersonID, book.ID, mapping.Href)
	require.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), *mapping.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	mu.Lock()
	deleted = true
	mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.True(conflicts[0].RemoteTombstone)
	assert.False(conflicts[0].LocalTombstone)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err, "conflict must retain the mapping")
	require.NoError(service.ResolveConflict(t.Context(), conflicts[0].ID, ResolutionKeepRemote))
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetCardDAVPublicationContext(t.Context(), *mapping.PersonID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestPullConflictCapturesLocalDeleteAgainstRemoteEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	body := conflictCard("alice", "Alice Base")
	etag := `"one"`
	remoteDeleted := false
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case "REPORT":
			events := ""
			if !remoteDeleted {
				events = cardResponseRaw("/books/personal/alice.vcf", etag, escapedCardData(body))
			}
			writeDAVXML(t, w, syncResponse(events, ""))
		case http.MethodGet:
			if remoteDeleted {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", etag)
			_, err := w.Write(body)
			assert.NoError(err)
		case http.MethodDelete:
			deletes++
			assert.Equal(etag, r.Header.Get("If-Match"))
			remoteDeleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	person, err := st.GetPersonContext(t.Context(), *mapping.PersonID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), person.ID, person.Revision))
	mu.Lock()
	body = conflictCard("alice", "Alice Remote")
	etag = `"two"`
	mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)
	assert.True(conflicts[0].LocalTombstone)
	assert.False(conflicts[0].RemoteTombstone)
	assert.Equal(body, conflicts[0].RemoteBody)
	require.NoError(service.ResolveConflict(t.Context(), conflicts[0].ID, ResolutionKeepLocal))
	assert.Equal(1, deletes)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestPullAppliesEquivalentConcurrentOutcomesWithoutConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	body := conflictCard("alice", "Alice Base")
	etag := `"one"`
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "REPORT" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		responses := ""
		if !deleted {
			responses = cardResponseRaw("/books/personal/alice.vcf", etag, escapedCardData(body))
		}
		writeDAVXML(t, w, syncResponse(responses, ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	localBody, _, err := service.renderPublicationCard(t.Context(), *person, book, mapping)
	require.NoError(err)
	mu.Lock()
	body, etag = localBody, `"two"`
	mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	assert.Empty(conflicts)
	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Equal(`"two"`, mapping.RemoteETag)
	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), personID, person.Revision))
	mu.Lock()
	deleted = true
	mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err = service.ListConflicts(t.Context())
	require.NoError(err)
	assert.Empty(conflicts)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestKeepRemoteReimportsSubscribedCardAfterLocalDeleteWithoutPublicationReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	fixture.body = conflictCard("person", "Alice Base")
	fixture.etag = `"remote-base"`
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NoError(st.DeletePersonContext(t.Context(), personID, person.Revision))
	latest := conflictCard("person", "Alice Retained")
	fixture.mu.Lock()
	fixture.body = latest
	fixture.etag = `"remote-retained"`
	fixture.mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)
	require.NoError(service.ResolveConflict(t.Context(), conflicts[0].ID, ResolutionKeepRemote))
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	assert.NotEqual(personID, *mapping.PersonID)
	assert.Equal(store.CardDAVGovernanceRemote, mapping.Governance)
	fixture.mu.Lock()
	requestsBeforeReconcile := fixture.puts + fixture.deletes + fixture.gets + fixture.reports
	fixture.mu.Unlock()
	require.NoError(service.ReconcilePublications(t.Context()))
	fixture.mu.Lock()
	assert.Equal(requestsBeforeReconcile, fixture.puts+fixture.deletes+fixture.gets+fixture.reports)
	fixture.mu.Unlock()
}

func TestKeepRemoteRebasesBoundImportedProjectionWithoutPublicationReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{
		body: conflictCardWithEmail(
			"person", "Alice Remote Base", "alice.base@example.test",
		),
		etag: `"remote-1"`,
	}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(
		t.Context(), book.ID, book.CanonicalURL+"person.vcf",
	)
	require.NoError(err)
	require.NotNil(mapping.PersonID)
	personID := *mapping.PersonID
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(
		t.Context(), personID, person.Revision, new("Alice Local Label"),
	)
	require.NoError(err)
	_, err = st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
		NameKind: store.PersonNameSort, SortAs: new("Local Sort Key"),
		OriginalValue: "Local Sort Key",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications
		(person_id, desired, address_book_id, href) VALUES (?, TRUE, ?, ?)`),
		personID, book.ID, mapping.Href)
	require.NoError(err)

	retained := conflictCardWithEmail(
		"person", "Alice Remote Retained", "alice.retained@example.test",
	)
	fixture.mu.Lock()
	fixture.body = retained
	fixture.etag = `"remote-2"`
	fixture.mu.Unlock()
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)

	require.NoError(service.ResolveConflict(
		t.Context(), conflicts[0].ID, ResolutionKeepRemote,
	))
	person, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	require.NotNil(person.DisplayName)
	assert.Equal("Alice Local Label", *person.DisplayName,
		"an explicit local display label must not be overwritten")
	names, err := st.ListPersonNamesContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(names, 2)
	remoteName, userName := names[0], names[1]
	if remoteName.Envelope.Source == store.ProvenanceUser {
		remoteName, userName = userName, remoteName
	}
	require.NotNil(remoteName.Formatted)
	assert.Equal("Alice Remote Retained", *remoteName.Formatted)
	assert.Equal(store.ProvenanceCardDAVImport, remoteName.Envelope.Source)
	require.NotNil(userName.SortAs)
	assert.Equal("Local Sort Key", *userName.SortAs)
	assert.Equal(store.ProvenanceUser, userName.Envelope.Source)
	points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
	require.NoError(err)
	require.Len(points, 1)
	assert.Equal("alice.retained@example.test", points[0].OriginalValue)
	assert.Equal(store.ProvenanceCardDAVImport, points[0].Envelope.Source)

	mapping, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(snapshot.Fingerprint, mapping.LocalHash)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)

	fixture.mu.Lock()
	putsBefore, deletesBefore := fixture.puts, fixture.deletes
	getsBefore, reportsBefore := fixture.gets, fixture.reports
	fixture.mu.Unlock()
	require.NoError(service.ReconcilePublications(t.Context()))
	fixture.mu.Lock()
	assert.Equal(putsBefore, fixture.puts)
	assert.Equal(deletesBefore, fixture.deletes)
	assert.Equal(getsBefore, fixture.gets)
	assert.Equal(reportsBefore, fixture.reports)
	fixture.mu.Unlock()
}

func TestKeepRemoteRebasesImportedPersonCleanupBaseline(t *testing.T) {
	tests := []struct {
		name               string
		makeLocalChange    func(t *testing.T, st *store.Store, personID int64)
		wantPersonRetained bool
	}{
		{
			name: "discarded imported projection edit advances cleanup baseline",
			makeLocalChange: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				points, err := st.ListPersonContactPointsContext(t.Context(), personID, true)
				require.NoError(t, err)
				require.Len(t, points, 1)
				require.NoError(t, st.SupersedePersonContactPointContext(
					t.Context(), personID, points[0].Envelope.ID, nil,
				))
			},
		},
		{
			name: "explicit user state keeps cleanup baseline",
			makeLocalChange: func(t *testing.T, st *store.Store, personID int64) {
				t.Helper()
				_, err := st.AddPersonNameContext(t.Context(), personID, store.PersonNameInput{
					NameKind: store.PersonNameSort, SortAs: new("User Sort Key"),
					OriginalValue: "User Sort Key",
					Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
				})
				require.NoError(t, err)
			},
			wantPersonRetained: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			fixture := &conflictMutationServer{
				body: conflictCardWithEmail(
					"person", "Alice Remote Base", "alice.base@example.test",
				),
				etag: `"remote-1"`,
			}
			server := httptest.NewServer(fixture.handler(t))
			t.Cleanup(server.Close)
			service, st, book := newPullService(t, server, false)

			_, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			mapping, err := st.GetCardDAVResourceContext(
				t.Context(), book.ID, book.CanonicalURL+"person.vcf",
			)
			require.NoError(err)
			require.NotNil(mapping.PersonID)
			personID := *mapping.PersonID
			tt.makeLocalChange(t, st, personID)

			fixture.mu.Lock()
			fixture.body = conflictCardWithEmail(
				"person", "Alice Remote Retained", "alice.retained@example.test",
			)
			fixture.etag = `"remote-2"`
			fixture.mu.Unlock()
			_, err = service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			conflicts, err := service.ListConflicts(t.Context())
			require.NoError(err)
			require.Len(conflicts, 1)
			require.NoError(service.ResolveConflict(
				t.Context(), conflicts[0].ID, ResolutionKeepRemote,
			))

			fixture.mu.Lock()
			fixture.body = nil
			fixture.etag = ""
			fixture.mu.Unlock()
			_, err = service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
			require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
			person, err := st.GetPersonContext(t.Context(), personID)
			if tt.wantPersonRetained {
				require.NoError(err)
				assert.Equal(t, personID, person.ID)
				return
			}
			require.ErrorIs(err, store.ErrPersonNotFound)
		})
	}
}

type conflictMutationServer struct {
	mu                  sync.Mutex
	body                []byte
	etag                string
	force412            bool
	delete412           bool
	deleteStatus        int
	timeoutPut          bool
	timeoutDelete       bool
	puts                int
	deletes             int
	gets                int
	reports             int
	lastPutBody         []byte
	lastIfMatch         string
	syncToken           string
	deleteRaceBody      []byte
	deleteRaceETag      string
	deleteRaceTombstone bool
	getFailures         int
	onGet               func(int)
}

func (f *conflictMutationServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.puts++
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			f.lastPutBody = append([]byte(nil), body...)
			f.lastIfMatch = r.Header.Get("If-Match")
			if f.force412 {
				f.force412 = false
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			if len(f.body) == 0 {
				assert.Equal(t, "*", r.Header.Get("If-None-Match"))
			} else {
				assert.Equal(t, f.etag, r.Header.Get("If-Match"))
			}
			f.body = append([]byte(nil), body...)
			f.etag = `"server-` + string(rune('0'+f.puts)) + `"`
			if f.timeoutPut {
				f.timeoutPut = false
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			f.gets++
			if f.onGet != nil {
				f.onGet(f.gets)
			}
			if f.getFailures > 0 {
				f.getFailures--
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if len(f.body) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", f.etag)
			_, err := w.Write(f.body)
			assert.NoError(t, err)
		case http.MethodDelete:
			f.deletes++
			f.lastIfMatch = r.Header.Get("If-Match")
			if f.deleteStatus != 0 {
				if f.deleteStatus == http.StatusTooManyRequests {
					w.Header().Set("Retry-After", "3600")
				}
				w.WriteHeader(f.deleteStatus)
				return
			}
			if f.delete412 {
				f.delete412 = false
				if f.deleteRaceTombstone {
					f.body = nil
					f.etag = ""
				} else if len(f.deleteRaceBody) > 0 {
					f.body = append([]byte(nil), f.deleteRaceBody...)
					f.etag = f.deleteRaceETag
				}
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			assert.Equal(t, f.etag, r.Header.Get("If-Match"))
			f.body = nil
			f.etag = ""
			if f.timeoutDelete {
				f.timeoutDelete = false
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "REPORT":
			f.reports++
			events := ""
			if len(f.body) > 0 {
				events = cardResponseRaw("/books/personal/person.vcf", f.etag, escapedCardData(f.body))
			}
			writeDAVXML(t, w, syncResponse(events, f.syncToken))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func oversizedConflictCard(uid string) []byte {
	prefix := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Oversized\r\n")
	suffix := []byte("END:VCARD\r\n")
	body := make([]byte, 0, store.MaxCardDAVConflictSnapshotBytes)
	body = append(body, prefix...)
	remaining := store.MaxCardDAVConflictSnapshotBytes - len(prefix) - len(suffix)
	for remaining > 0 {
		const overhead = len("NOTE:\r\n")
		chunk := min(1<<20, remaining-overhead)
		body = append(body, "NOTE:"...)
		body = append(body, bytes.Repeat([]byte("x"), chunk)...)
		body = append(body, '\r', '\n')
		remaining -= overhead + chunk
	}
	body = append(body, suffix...)
	return body
}

func TestConditional412CapturesConflictAndKeepLocalRefetchesCurrentETag(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	service.client.requestTimeout = 250 * time.Millisecond
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote")
	fixture.etag = `"remote-2"`
	fixture.force412 = true
	fixture.mu.Unlock()

	err = service.PublishPerson(t.Context(), personID)
	var conflictErr *ConflictError
	require.ErrorAs(err, &conflictErr)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Empty(publication.PendingOperation)
	conflicts, getErr := service.ListConflicts(t.Context())
	require.NoError(getErr)
	require.Len(conflicts, 1)
	assert.Equal(conflictErr.ID, conflicts[0].ID)
	assert.Contains(string(conflicts[0].LocalBody), "EMAIL:alice-local@example.test")
	assert.Equal(conflictCard("person", "Alice Remote"), conflicts[0].RemoteBody)
	fixture.mu.Lock()
	putsBeforeBlockedRetry := fixture.puts
	fixture.mu.Unlock()
	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, ErrCardDAVConflictPending)
	fixture.mu.Lock()
	assert.Equal(putsBeforeBlockedRetry, fixture.puts, "unresolved mapping must block before network")
	fixture.mu.Unlock()

	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Even Newer")
	fixture.etag = `"remote-3"`
	fixture.timeoutPut = true
	getsBefore := fixture.gets
	fixture.mu.Unlock()
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET
		is_write_target = FALSE, is_subscribed = TRUE, can_update = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)

	require.Error(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	putsAfterTimeout := fixture.puts
	fixture.mu.Unlock()
	require.NoError(service.ResolveConflict(t.Context(), conflictErr.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	assert.GreaterOrEqual(fixture.gets, getsBefore+3, "resolution must preflight and recover with canonical GETs")
	assert.Equal(putsAfterTimeout, fixture.puts, "ambiguous resolution recovery must not replay PUT")
	assert.Equal(`"remote-3"`, fixture.lastIfMatch)
	assert.Equal(conflicts[0].LocalBody, fixture.lastPutBody)
	fixture.mu.Unlock()
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepLocal, resolved.Resolution)
}

func TestSyncSkipsConflictedPublicationAndStillAdvancesToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote")
	fixture.etag = `"remote-2"`
	fixture.force412 = true
	fixture.syncToken = "token-after-conflict"
	fixture.mu.Unlock()
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET supports_sync_collection = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)
	var conflictErr *ConflictError
	require.ErrorAs(service.PublishPerson(t.Context(), personID), &conflictErr)
	fixture.mu.Lock()
	putsBeforeSync := fixture.puts
	deletesBeforeSync := fixture.deletes
	fixture.mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	for _, candidate := range books {
		if candidate.ID == book.ID {
			assert.Equal("token-after-conflict", candidate.SyncToken)
		}
	}
	fixture.mu.Lock()
	assert.Equal(putsBeforeSync, fixture.puts, "reconciliation must not replay a conflicted publication")
	assert.Equal(deletesBeforeSync, fixture.deletes)
	fixture.mu.Unlock()
}

func TestSyncAbortsPullAfterPendingPublicationRecoveryFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	service.client.requestTimeout = 250 * time.Millisecond
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	fixture.mu.Lock()
	fixture.timeoutPut = true
	fixture.mu.Unlock()
	require.Error(service.PublishPerson(t.Context(), personID))
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	require.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)

	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Divergent Remote")
	fixture.etag = `"remote-divergent"`
	fixture.getFailures = 1
	reportsBefore := fixture.reports
	fixture.mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusServiceUnavailable, status.StatusCode)
	fixture.mu.Lock()
	assert.Equal(reportsBefore, fixture.reports, "pull must not advance a mapping after recovery fails")
	fixture.mu.Unlock()
	stillPending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(pending.MappingRevision, stillPending.MappingRevision)
	assert.Equal(pending.PendingOperation, stillPending.PendingOperation)

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	fixture.mu.Lock()
	assert.Greater(fixture.reports, reportsBefore, "a later successful recovery may proceed to pull")
	fixture.mu.Unlock()
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	require.Len(conflicts, 1)
	require.NoError(service.ResolveConflict(t.Context(), conflicts[0].ID, ResolutionKeepLocal))
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflicts[0].ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepLocal, resolved.Resolution)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "EMAIL:alice-local@example.test")
}

func seedLocalTombstoneConflict(
	t *testing.T, fixture *conflictMutationServer,
) (*Service, *store.Store, store.CardDAVAddressBook, *store.CardDAVResource, *store.CardDAVConflict) {
	t.Helper()
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	fixture.body = conflictCard("person", "Alice Base")
	fixture.etag = `"remote-base"`
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(t, err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(t, err)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(t, err)
	require.NoError(t, st.DeletePersonContext(t.Context(), personID, person.Revision))
	remoteBody := conflictCard("person", "Alice Remote")
	fixture.mu.Lock()
	fixture.body = append([]byte(nil), remoteBody...)
	fixture.etag = `"remote-race"`
	fixture.mu.Unlock()
	capture := store.CardDAVConflictCapture{
		AddressBookID: book.ID, Href: mapping.Href,
		ExpectedMappingRevision: mapping.MappingRevision,
		BaseLocalHash:           mapping.LocalHash, LocalHash: mapping.LocalHash,
		BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		RemoteETag: `"remote-race"`, RemoteBody: remoteBody,
		LocalTombstone: true,
	}
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), capture)
	require.NoError(t, err)
	return service, st, book, mapping, conflict
}

func seedUnpublishConflict(
	t *testing.T, fixture *conflictMutationServer,
) (*Service, *store.Store, int64, store.CardDAVAddressBook, *store.CardDAVResource, *store.CardDAVConflict) {
	t.Helper()
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(t, service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote")
	fixture.etag = `"remote-unpublish"`
	fixture.delete412 = true
	fixture.mu.Unlock()

	var conflictErr *ConflictError
	require.ErrorAs(t, service.UnpublishPerson(t.Context(), personID), &conflictErr)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(t, err)
	require.NotNil(t, mapping.PersonID)
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(t, err)
	require.True(t, conflict.LocalTombstone)
	return service, st, personID, book, mapping, conflict
}

func TestKeepLocalTombstoneTimeoutPersistsIntentAndRecoversWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{timeoutDelete: true}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	service.client.requestTimeout = 250 * time.Millisecond

	require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	pending, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVMutationDelete, pending.PendingOperation)
	assert.Equal(conflict.MappingRevision+1, pending.MappingRevision)
	assert.Equal(conflict.MappingRevision, pending.PreviousMappingRevision)
	assert.NotNil(pending.PendingStartedAt)
	fixture.mu.Lock()
	deletesAfterTimeout := fixture.deletes
	fixture.mu.Unlock()

	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	assert.Equal(deletesAfterTimeout, fixture.deletes, "ambiguous tombstone recovery must not replay DELETE")
	fixture.mu.Unlock()
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
}

func TestPullTombstoneCompletesTimedOutKeepLocalWithoutDeleteReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{timeoutDelete: true, syncToken: "token-after-tombstone"}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	service.client.requestTimeout = 250 * time.Millisecond
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET supports_sync_collection = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)

	require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	deletesAfterTimeout := fixture.deletes
	fixture.mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	fixture.mu.Lock()
	assert.Equal(deletesAfterTimeout, fixture.deletes, "pull proof must not replay DELETE")
	fixture.mu.Unlock()
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepLocal, resolved.Resolution)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal("token-after-tombstone", books[0].SyncToken)
}

func TestKeepLocalTombstoneDefinitiveRejectionRestoresFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{deleteStatus: http.StatusForbidden}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)

	err = service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusForbidden, status.StatusCode)
	refreshed, getErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(getErr)
	assert.Empty(refreshed.PendingOperation)
	assert.Zero(refreshed.PreviousMappingRevision)
	after, getErr := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(getErr)
	assert.Equal(before.MappingRevision, after.MappingRevision)

	fixture.deleteStatus = 0
	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
}

func TestKeepLocalTombstoneThrottleClearsIntentAndPersistsGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{deleteStatus: http.StatusTooManyRequests}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	started := time.Now()

	err = service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	refreshed, getErr := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(getErr)
	assert.Empty(refreshed.PendingOperation)
	after, getErr := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(getErr)
	assert.Equal(before.MappingRevision, after.MappingRevision)
	gate, getErr := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(getErr)
	require.NotNil(gate)
	assert.WithinDuration(started.Add(time.Hour), *gate, 5*time.Second)
}

func TestKeepLocalUnpublishConflictRetainsPersonAndClearsPublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	service, st, personID, book, mapping, conflict := seedUnpublishConflict(t, fixture)

	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	_, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepLocal, resolved.Resolution)
}

func TestPullTombstoneCompletesTimedOutUnpublishAndRetainsPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{timeoutDelete: true, syncToken: "token-after-unpublish"}
	service, st, personID, book, mapping, conflict := seedUnpublishConflict(t, fixture)
	service.client.requestTimeout = 250 * time.Millisecond
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET supports_sync_collection = TRUE WHERE id = ?`), book.ID)
	require.NoError(err)

	require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	pending, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	require.Equal(store.CardDAVMutationDelete, pending.PendingOperation)
	fixture.mu.Lock()
	deletesAfterTimeout := fixture.deletes
	fixture.mu.Unlock()

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	fixture.mu.Lock()
	assert.Equal(deletesAfterTimeout, fixture.deletes, "pull proof must not replay DELETE")
	fixture.mu.Unlock()
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictResolved, resolved.Status)
	assert.Equal(store.CardDAVResolutionKeepLocal, resolved.Resolution)
	books, err := st.ListCardDAVAddressBooksContext(t.Context())
	require.NoError(err)
	require.Len(books, 1)
	assert.Equal("token-after-unpublish", books[0].SyncToken)
}

func TestPullRefreshSupersedesTimedOutTombstoneIntentBeforeFreshDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{timeoutDelete: true}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	service.client.requestTimeout = 250 * time.Millisecond
	require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	pending, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	require.Equal(store.CardDAVMutationDelete, pending.PendingOperation)

	latest := conflictCard("person", "Alice Updated After Timeout")
	fixture.mu.Lock()
	fixture.body = append([]byte(nil), latest...)
	fixture.etag = `"remote-after-timeout"`
	deletesBeforeSync := fixture.deletes
	fixture.mu.Unlock()
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	fixture.mu.Lock()
	assert.Equal(deletesBeforeSync, fixture.deletes, "pull must not replay the ambiguous DELETE")
	fixture.mu.Unlock()

	refreshed, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, refreshed.Status)
	assert.Empty(refreshed.PendingOperation)
	assert.Zero(refreshed.PreviousMappingRevision)
	assert.Nil(refreshed.PendingStartedAt)
	assert.Greater(refreshed.MappingRevision, pending.MappingRevision)
	assert.Equal(`"remote-after-timeout"`, refreshed.RemoteETag)
	assert.Equal(latest, refreshed.RemoteBody)
	afterPull, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Equal(refreshed.MappingRevision, afterPull.MappingRevision)

	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	assert.Equal(deletesBeforeSync+1, fixture.deletes, "explicit retry must issue one freshly fenced DELETE")
	assert.Equal(`"remote-after-timeout"`, fixture.lastIfMatch)
	fixture.mu.Unlock()
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestDirectConflictRefreshDoesNotSupersedePendingTombstoneIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{timeoutDelete: true}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	service.client.requestTimeout = 250 * time.Millisecond
	require.Error(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	pending, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	currentMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	latest := conflictCard("person", "Direct Refresh")

	refreshed, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{
		AddressBookID: book.ID, Href: mapping.Href,
		ExpectedMappingRevision: currentMapping.MappingRevision,
		BaseLocalHash:           currentMapping.LocalHash, LocalHash: pending.LocalHash,
		BaseRemoteHash: currentMapping.RemoteSemanticHash, BaseRemoteETag: currentMapping.RemoteETag,
		RemoteETag: `"direct-refresh"`, RemoteBody: latest, LocalTombstone: true,
	})
	require.NoError(err)
	assert.Equal(store.CardDAVMutationDelete, refreshed.PendingOperation)
	assert.Equal(pending.PreviousMappingRevision, refreshed.PreviousMappingRevision)
	assert.NotNil(refreshed.PendingStartedAt)
}

func TestKeepLocalTombstone412RefreshesConflictBeforeExplicitRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	latest := conflictCard("person", "Alice Raced Again")
	fixture := &conflictMutationServer{
		delete412: true, deleteRaceBody: latest, deleteRaceETag: `"remote-latest"`,
	}
	service, st, _, _, conflict := seedLocalTombstoneConflict(t, fixture)

	var conflictErr *ConflictError
	require.ErrorAs(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal), &conflictErr)
	assert.Equal(conflict.ID, conflictErr.ID)
	refreshed, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, refreshed.Status)
	assert.Empty(refreshed.PendingOperation)
	assert.Equal(`"remote-latest"`, refreshed.RemoteETag)
	assert.Equal(latest, refreshed.RemoteBody)

	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal))
	fixture.mu.Lock()
	assert.Equal(2, fixture.deletes)
	assert.Equal(`"remote-latest"`, fixture.lastIfMatch)
	fixture.mu.Unlock()
}

func TestKeepLocalTombstoneCanonicalCommitHonorsBookFence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	service, st, book, mapping, conflict := seedLocalTombstoneConflict(t, fixture)
	fixture.mu.Lock()
	fencedGet := fixture.gets + 2
	fixture.onGet = func(gets int) {
		if gets != fencedGet {
			return
		}
		_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
			SET sync_revision = sync_revision + 1 WHERE id = ?`), book.ID)
		require.NoError(err)
	}
	fixture.mu.Unlock()

	err := service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepLocal)
	require.ErrorIs(err, store.ErrCardDAVStalePlan)
	pending, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, pending.Status)
	assert.Equal(store.CardDAVMutationDelete, pending.PendingOperation)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
}

func TestOversized412RestoresMappingFenceAndClearsKnownUnappliedIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "oversized-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	fixture.mu.Lock()
	fixture.body = oversizedConflictCard("person")
	fixture.etag = `"oversized"`
	fixture.force412 = true
	fixture.mu.Unlock()

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVConflictTooLarge)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
	require.NoError(err)
	assert.Equal(mapping.MappingRevision, after.MappingRevision)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestOversizedCreateCollisionCanBeCanceledWithoutDeletingRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	remoteBody := oversizedConflictCard("remote-owner")
	putCount := 0
	deleteCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putCount++
			w.WriteHeader(http.StatusPreconditionFailed)
		case http.MethodGet:
			w.Header().Set("ETag", `"remote-owner"`)
			_, err := w.Write(remoteBody)
			assert.NoError(err)
		case http.MethodDelete:
			deleteCount++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)

	err := service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVConflictTooLarge)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	assert.Equal(remoteBody, resource.RemoteBody)
	assert.Nil(resource.PersonID)
	assert.Equal(store.CardDAVMappingUnbound, resource.MappingStatus)
	assert.Equal(store.CardDAVGovernanceNone, resource.Governance)
	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationMismatch)
	assert.Equal(1, putCount)
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	assert.Equal(1, putCount)
	assert.Zero(deleteCount)
}

func TestOversizedAmbiguousRecoveryRebasesAndRetainsReadOnlyIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	service.client.requestTimeout = 250 * time.Millisecond
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "recovery-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	fixture.mu.Lock()
	fixture.timeoutPut = true
	fixture.mu.Unlock()
	require.Error(service.PublishPerson(t.Context(), personID))
	pending, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	require.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
	fixture.mu.Lock()
	fixture.body = oversizedConflictCard("person")
	fixture.etag = `"oversized-recovery"`
	putsBeforeRecovery := fixture.puts
	fixture.mu.Unlock()

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVConflictTooLarge)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	assert.Equal(pending.PreviousMappingRevision, after.MappingRevision)
	rebased, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(pending.PendingOperation, rebased.PendingOperation)
	assert.Equal(pending.OutgoingBody, rebased.OutgoingBody)
	assert.Equal(pending.RemoteETag, rebased.RemoteETag)
	assert.Equal(pending.MutationRevision, rebased.MutationRevision)
	assert.Equal(pending.PreviousMappingRevision, rebased.MappingRevision)
	assert.Equal(pending.PreviousMappingRevision, rebased.PreviousMappingRevision)

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVConflictTooLarge)
	fixture.mu.Lock()
	assert.Equal(putsBeforeRecovery, fixture.puts, "oversized ambiguous recovery must remain read-only")
	fixture.mu.Unlock()
}

func TestResolveConflictRejectsChoicesOutsideExactContract(t *testing.T) {
	service := &Service{}
	err := service.ResolveConflict(t.Context(), 1, ResolutionChoice("merge"))
	require.ErrorIs(t, err, ErrInvalidResolutionChoice)
}

func TestConditionalDelete412KeepRemoteRevalidatesCanonicalCard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote")
	fixture.etag = `"remote-2"`
	fixture.delete412 = true
	fixture.mu.Unlock()

	err := service.UnpublishPerson(t.Context(), personID)
	var conflictErr *ConflictError
	require.ErrorAs(err, &conflictErr)
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.True(conflict.LocalTombstone)
	assert.False(conflict.RemoteTombstone)
	assert.Equal(conflictCard("person", "Alice Remote"), conflict.RemoteBody)
	fixture.mu.Lock()
	requestsBefore := fixture.puts + fixture.gets + fixture.deletes
	fixture.body = conflictCard("person", "Alice Newer Remote")
	fixture.etag = `"remote-3"`
	fixture.mu.Unlock()

	require.ErrorIs(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote),
		store.ErrCardDAVConflictStale)
	unresolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVConflictUnresolved, unresolved.Status)
	fixture.mu.Lock()
	fixture.body = conflict.RemoteBody
	fixture.etag = conflict.RemoteETag
	fixture.mu.Unlock()
	require.NoError(service.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote))
	fixture.mu.Lock()
	assert.Equal(requestsBefore+2, fixture.puts+fixture.gets+fixture.deletes)
	fixture.mu.Unlock()
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	assert.Equal(conflict.RemoteBody, mapping.RemoteBody)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	resolved, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assert.Equal(store.CardDAVResolutionKeepRemote, resolved.Resolution)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)
	fixture.mu.Lock()
	deletesBeforeReconcile := fixture.deletes
	fixture.mu.Unlock()
	require.NoError(service.ReconcilePublications(t.Context()))
	fixture.mu.Lock()
	assert.Equal(deletesBeforeReconcile, fixture.deletes, "keep-remote must cancel the stale unpublish")
	fixture.mu.Unlock()
}

func TestPullRefreshPreservesUnpublishTombstoneUntilKeepRemoteCancelsIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote")
	fixture.etag = `"remote-2"`
	fixture.delete412 = true
	fixture.mu.Unlock()

	var conflictErr *ConflictError
	require.ErrorAs(service.UnpublishPerson(t.Context(), personID), &conflictErr)
	person, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(t.Context(), personID, person.Revision, new("Alice Local After Unpublish"))
	require.NoError(err)
	latest := conflictCard("person", "Alice Remote Refreshed")
	fixture.mu.Lock()
	fixture.body = latest
	fixture.etag = `"remote-3"`
	fixture.mu.Unlock()
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)

	refreshed, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.True(refreshed.LocalTombstone)
	assert.Equal(latest, refreshed.RemoteBody)
	require.NoError(service.ResolveConflict(t.Context(), refreshed.ID, ResolutionKeepRemote))
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)
	fixture.mu.Lock()
	deletesBeforeReconcile := fixture.deletes
	fixture.mu.Unlock()
	require.NoError(service.ReconcilePublications(t.Context()))
	fixture.mu.Lock()
	assert.Equal(deletesBeforeReconcile, fixture.deletes)
	fixture.mu.Unlock()
}

func TestConditionalDelete412WithCanonicalTombstoneCommitsCleanup(t *testing.T) {
	require := require.New(t)

	fixture := &conflictMutationServer{}
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.mu.Lock()
	fixture.delete412 = true
	fixture.deleteRaceTombstone = true
	fixture.mu.Unlock()

	require.NoError(service.UnpublishPerson(t.Context(), personID))
	_, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	conflicts, err := service.ListConflicts(t.Context())
	require.NoError(err)
	assert.Empty(t, conflicts)
}

func TestAmbiguousUpdateRecoveryCapturesConflictWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice-local@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	fixture.timeout = true
	require.Error(service.PublishPerson(t.Context(), personID))
	putsBeforeRecovery := fixture.puts
	fixture.mu.Lock()
	fixture.body = conflictCard("person", "Alice Remote After Timeout")
	fixture.etag = `"remote-after-timeout"`
	fixture.mu.Unlock()

	err = service.PublishPerson(t.Context(), personID)
	var conflictErr *ConflictError
	require.ErrorAs(err, &conflictErr)
	assert.Equal(putsBeforeRecovery, fixture.puts, "ambiguous mapped recovery must remain read-only")
	conflict, err := st.GetCardDAVConflictContext(t.Context(), conflictErr.ID)
	require.NoError(err)
	assert.Contains(string(conflict.RemoteBody), "FN:Alice Remote After Timeout")
	assert.Contains(string(conflict.RemoteBody), "PRODID:-//Server//EN")
	assert.Contains(string(conflict.LocalBody), "EMAIL:alice-local@example.test")
}
