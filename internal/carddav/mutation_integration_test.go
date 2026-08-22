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

type mutationFixture struct {
	mu                  sync.Mutex
	body                []byte
	etag                string
	puts                int
	deletes             int
	gets                int
	reports             int
	timeout             bool
	throttle            bool
	throttleGets        int
	throttleReport      bool
	reportNoChanges     bool
	reportCurrent       bool
	missingStatus       int
	deleteMissingStatus int
	deleteTimeout       bool
	throttleDeletes     int
	putStatus           int
	deleteStatus        int
	putFailures         int
	href                string
}

func (f *mutationFixture) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			f.puts++
			f.href = r.URL.EscapedPath()
			body, err := io.ReadAll(r.Body)
			assert.NoError(t, err)
			assert.Equal(t, "text/vcard; charset=utf-8", r.Header.Get("Content-Type"))
			assert.NotContains(t, string(body), "PRODID:")
			assert.NotContains(t, string(body), "LAST-MODIFIED:")
			if f.throttle {
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.putFailures > 0 {
				f.putFailures--
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if len(f.body) == 0 {
				assert.Equal(t, "*", r.Header.Get("If-None-Match"))
				assert.Empty(t, r.Header.Get("If-Match"))
			} else {
				assert.Equal(t, f.etag, r.Header.Get("If-Match"))
				assert.Empty(t, r.Header.Get("If-None-Match"))
			}
			if f.putStatus != 0 {
				w.WriteHeader(f.putStatus)
				return
			}
			f.body = append([]byte(nil), body...)
			f.etag = `"remote-` + strings.Repeat("x", f.puts) + `"`
			if f.timeout {
				f.timeout = false
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			f.gets++
			if f.throttleGets > 0 {
				f.throttleGets--
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if len(f.body) == 0 {
				status := f.missingStatus
				if status == 0 {
					status = http.StatusNotFound
				}
				w.WriteHeader(status)
				return
			}
			w.Header().Set("ETag", f.etag)
			_, err := w.Write(append(append([]byte(nil), f.body[:len(f.body)-len("END:VCARD\r\n")]...), []byte("PRODID:-//Server//EN\r\nEND:VCARD\r\n")...))
			assert.NoError(t, err)
		case http.MethodDelete:
			f.deletes++
			assert.Equal(t, f.etag, r.Header.Get("If-Match"))
			if f.throttleDeletes > 0 {
				f.throttleDeletes--
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.deleteStatus != 0 {
				w.WriteHeader(f.deleteStatus)
				return
			}
			f.body = nil
			f.etag = ""
			if f.deleteTimeout {
				f.deleteTimeout = false
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			if f.deleteMissingStatus != 0 {
				w.WriteHeader(f.deleteMissingStatus)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case "REPORT":
			f.reports++
			if f.throttleReport {
				w.Header().Set("Retry-After", "7200")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.reportNoChanges {
				writeDAVXML(t, w, syncResponse("", "token-after-timeout"))
				return
			}
			if f.reportCurrent {
				card := strings.ReplaceAll(string(f.body), "&", "&amp;")
				card = strings.ReplaceAll(card, "<", "&lt;")
				href := f.href
				if href == "" {
					href = "/books/personal/person.vcf"
				}
				writeDAVXML(t, w, syncResponse(cardResponseRaw(
					href, f.etag, card), "token-after-timeout"))
				return
			}
			if len(f.body) == 0 {
				writeDAVXML(t, w, syncResponse("", ""))
				return
			}
			card := strings.ReplaceAll(string(f.body), "&", "&amp;")
			card = strings.ReplaceAll(card, "<", "&lt;")
			href := f.href
			if href == "" {
				href = "/books/personal/person.vcf"
			}
			writeDAVXML(t, w, syncResponse(cardResponseRaw(href, f.etag, card), ""))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func cardResponseRaw(href, etag, card string) string {
	return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` +
		`<D:getetag>` + strings.ReplaceAll(etag, `"`, `&quot;`) + `</D:getetag>` +
		`<C:address-data>` + card + `</C:address-data></D:prop>` +
		`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func seededMutationService(t *testing.T, fixture *mutationFixture) (*Service, *store.Store, int64, store.CardDAVAddressBook) {
	t.Helper()
	server := httptest.NewServer(fixture.handler(t))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	service.client.requestTimeout = 250 * time.Millisecond
	var personID int64
	err := st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "person", "Alice Example").Scan(&personID)
	require.NoError(t, err)
	return service, st, personID, book
}

func TestPublishPersonCreatesConditionallyAndCommitsCanonicalCard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)

	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	assert.Equal(1, fixture.gets)
	assert.Contains(string(fixture.body), "VERSION:3.0")

	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(personID, *resource.PersonID)
	assert.Contains(string(resource.RemoteBody), "PRODID:-//Server//EN")
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)
}

func TestPublishThenSyncAcceptsEscapedUIDChild(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	_, err := st.DB().Exec(st.Rebind(`UPDATE persons SET vcard_uid = ? WHERE id = ?`),
		"a/b?c#d", personID)
	require.NoError(err)

	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal("/books/personal/a%2Fb%3Fc%23d.vcf", fixture.href)
	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)

	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID,
		book.CanonicalURL+"a%2Fb%3Fc%23d.vcf")
	require.NoError(err)
	require.NotNil(resource.PersonID)
	assert.Equal(personID, *resource.PersonID)
}

func TestPublishPersonSkipsSemanticallyUnchangedMappedCard(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	puts, gets := fixture.puts, fixture.gets

	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(puts, fixture.puts)
	assert.Equal(gets, fixture.gets)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, before.Href)
	require.NoError(err)
	assert.Equal(before.MappingRevision, after.MappingRevision)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.True(publication.Desired)
	assert.Empty(publication.PendingOperation)
}

func TestTimedOutUpdatePersistsIntentAndSyncRecoveryDoesNotReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	addProjectedEmail(t, st, personID)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET
		supports_sync_collection = TRUE, sync_token = ? WHERE id = ?`), "token-before-timeout", book.ID)
	require.NoError(err)
	fixture.timeout = true

	err = service.PublishPerson(t.Context(), personID)
	require.Error(err)
	assert.Equal(2, fixture.puts)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(store.CardDAVMutationUpdate, publication.PendingOperation)

	fixture.reportCurrent = true
	_, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(2, fixture.puts, "mapped recovery must be read-only")
	publication, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

func TestTimedOutCreateRecoveryProvesCanonicalResourceWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{timeout: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	_, err := st.DB().Exec(st.Rebind(`UPDATE persons SET display_name = ? WHERE id = ?`), "Alice\nExample", personID)
	require.NoError(err)

	err = service.PublishPerson(t.Context(), personID)
	require.Error(err)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVMutationCreate, publication.PendingOperation)
	assert.Equal(1, fixture.puts)
	fixture.mu.Lock()
	fixture.body = bytes.ReplaceAll(fixture.body, []byte(`\n`), []byte(`\N`))
	fixture.body = bytes.Replace(fixture.body, []byte("FN:"), []byte("FN;VALUE=text:"), 1)
	fixture.mu.Unlock()

	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts, "a semantically equivalent canonical GET proved the timed-out create")
	publication, getErr = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Empty(publication.PendingOperation)
}

func TestTimedOutCreateRecoveryAdoptsResourceMaterializedByPull(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{timeout: true}
	service, st, personID, _ := seededMutationService(t, fixture)

	require.Error(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.puts)
	service.client.requestTimeout = 2 * time.Second
	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixture.puts, "create recovery after pull must not replay PUT")
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

func TestCreateRecoveryAcceptsOneConditional412OnlyAfterCanonicalProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	var body []byte
	puts, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			puts++
			attempted, err := io.ReadAll(r.Body)
			assert.NoError(err)
			assert.Equal("*", r.Header.Get("If-None-Match"))
			if puts == 1 {
				mu.Unlock()
				<-r.Context().Done()
				mu.Lock()
				return
			}
			body = attempted
			w.WriteHeader(http.StatusPreconditionFailed)
		case http.MethodGet:
			gets++
			if len(body) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", `"raced-create"`)
			_, err := w.Write(body)
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	service, st, personID, _ := seededMutationServiceForServer(t, server)
	service.client.requestTimeout = 250 * time.Millisecond

	require.Error(service.PublishPerson(t.Context(), personID))
	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(2, puts)
	assert.Equal(2, gets)
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

func TestCreateRecoveryRetriesAfterRepeatedTransientFailures(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{putFailures: 2}
	service, st, personID, _ := seededMutationService(t, fixture)

	require.Error(service.PublishPerson(t.Context(), personID))
	require.Error(service.PublishPerson(t.Context(), personID))
	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(3, fixture.puts)
	assert.Equal(3, fixture.gets,
		"each recovery attempt must prove absence and a successful retry must read the canonical card")
	publication, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

func TestDefinitiveCreateRejectionClearsPendingIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{putStatus: http.StatusForbidden}
	service, st, personID, _ := seededMutationService(t, fixture)

	err := service.PublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusForbidden, status.StatusCode)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(getErr, store.ErrCardDAVPublicationNotFound)
	assert.Nil(publication)

	fixture.putStatus = 0
	require.NoError(service.PublishPerson(t.Context(), personID))
}

func TestDefinitiveMappedMutationRejectionRestoresFence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *mutationFixture, *Service, *store.Store, int64) error
	}{
		{
			name: "update",
			mutate: func(t *testing.T, fixture *mutationFixture, service *Service, st *store.Store, personID int64) error {
				t.Helper()
				addProjectedEmail(t, st, personID)
				fixture.putStatus = http.StatusMethodNotAllowed
				return service.PublishPerson(t.Context(), personID)
			},
		},
		{
			name: "delete",
			mutate: func(t *testing.T, fixture *mutationFixture, service *Service, _ *store.Store, personID int64) error {
				t.Helper()
				fixture.deleteStatus = http.StatusForbidden
				return service.UnpublishPerson(t.Context(), personID)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := &mutationFixture{}
			service, st, personID, book := seededMutationService(t, fixture)
			require.NoError(service.PublishPerson(t.Context(), personID))
			before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
			require.NoError(err)

			err = test.mutate(t, fixture, service, st, personID)
			var status *StatusError
			require.ErrorAs(err, &status)
			publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(getErr)
			assert.True(publication.Desired)
			assert.Empty(publication.PendingOperation)
			after, getErr := st.GetCardDAVResourceContext(t.Context(), book.ID, before.Href)
			require.NoError(getErr)
			assert.Equal(before.MappingRevision, after.MappingRevision)
		})
	}
}

func TestDefinitiveCreateRecoveryRejectionClearsPendingIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{putFailures: 1, putStatus: http.StatusForbidden}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))

	err := service.PublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusForbidden, status.StatusCode)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(getErr, store.ErrCardDAVPublicationNotFound)
	assert.Nil(publication)
}

func TestMutationRejectsExplicitlyDeniedCapabilityBeforeNetwork(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability string
		prepare    func(*testing.T, *Service, *store.Store, int64)
		mutate     func(*testing.T, *Service, int64) error
	}{
		{
			name: "create", capability: "can_create",
			prepare: func(t *testing.T, _ *Service, _ *store.Store, _ int64) { t.Helper() },
			mutate: func(t *testing.T, service *Service, personID int64) error {
				t.Helper()
				return service.PublishPerson(t.Context(), personID)
			},
		},
		{
			name: "update", capability: "can_update",
			prepare: func(t *testing.T, service *Service, st *store.Store, personID int64) {
				t.Helper()
				require.NoError(t, service.PublishPerson(t.Context(), personID))
				addProjectedEmail(t, st, personID)
			},
			mutate: func(t *testing.T, service *Service, personID int64) error {
				t.Helper()
				return service.PublishPerson(t.Context(), personID)
			},
		},
		{
			name: "delete", capability: "can_delete",
			prepare: func(t *testing.T, service *Service, _ *store.Store, personID int64) {
				t.Helper()
				require.NoError(t, service.PublishPerson(t.Context(), personID))
			},
			mutate: func(t *testing.T, service *Service, personID int64) error {
				t.Helper()
				return service.UnpublishPerson(t.Context(), personID)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := &mutationFixture{}
			service, st, personID, book := seededMutationService(t, fixture)
			test.prepare(t, service, st, personID)
			requestsBefore := fixture.puts + fixture.deletes
			_, err := st.DB().Exec(st.Rebind(
				"UPDATE carddav_address_books SET "+test.capability+" = FALSE WHERE id = ?"), book.ID)
			require.NoError(t, err)

			err = test.mutate(t, service, personID)
			require.ErrorIs(t, err, store.ErrCardDAVReadOnlyAddressBook)
			assert.Equal(t, requestsBefore, fixture.puts+fixture.deletes)
		})
	}
}

func TestConditionalCreateCollisionRecordsResolvableConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	remoteBody := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-owner\r\nFN:Remote Owner\r\nEND:VCARD\r\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			assert.Equal("*", r.Header.Get("If-None-Match"))
			w.WriteHeader(http.StatusPreconditionFailed)
		case http.MethodGet:
			w.Header().Set("ETag", `"remote-owner"`)
			_, err := w.Write(remoteBody)
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	service, st, personID, book := seededMutationServiceForServer(t, server)

	err := service.PublishPerson(t.Context(), personID)
	var conflictErr *ConflictError
	require.ErrorAs(err, &conflictErr)
	assert.Positive(conflictErr.ID)
	conflicts, listErr := service.ListConflicts(t.Context())
	require.NoError(listErr)
	require.Len(conflicts, 1)
	assert.Contains(string(conflicts[0].RemoteBody), "UID:remote-owner")
	assert.NotEmpty(conflicts[0].LocalBody)
	mapping, getErr := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(getErr)
	require.NotNil(mapping.PersonID)
	assert.Equal(personID, *mapping.PersonID)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Empty(publication.PendingOperation)
}

func TestTimedOutCreateRecoveryRoutesDivergentCanonicalCardToConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := &mutationFixture{timeout: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	remoteBody := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person\r\nFN:Remote Owner\r\nEND:VCARD\r\n")
	fixture.mu.Lock()
	fixture.body = remoteBody
	fixture.etag = `"remote-owner"`
	fixture.mu.Unlock()

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	conflicts, listErr := service.ListConflicts(t.Context())
	require.NoError(listErr)
	require.Len(conflicts, 1)
	assert.Contains(string(conflicts[0].RemoteBody), "FN:Remote Owner")
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Empty(publication.PendingOperation)
}

func TestUnpublishPersonDeletesConditionallyButKeepsPerson(t *testing.T) {
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))

	require.NoError(service.UnpublishPerson(t.Context(), personID))
	assert.Equal(t, 1, fixture.deletes)
	_, err := st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
}

func TestUnpublishRemoteImportWithoutPublicationPreservesSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	remoteBody := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:remote-import\r\nFN:Remote Import\r\nEND:VCARD\r\n")
	fixture := &mutationFixture{body: remoteBody, etag: `"remote-import"`}
	service, st, _, book := seededMutationService(t, fixture)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	href := book.CanonicalURL + "remote-import.vcf"
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true,
		Upserts: []store.CardDAVRemoteResource{{
			Href: href, RemoteUID: "remote-import", RemoteETag: fixture.etag,
			RemoteBody: remoteBody, SemanticHash: "semantic-remote-import",
			DisplayName: "Remote Import",
		}},
	})
	require.NoError(err)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(resource.PersonID)
	personID := *resource.PersonID
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)

	require.NoError(service.UnpublishPerson(t.Context(), personID))
	assert.Zero(fixture.deletes)
	preserved, err := st.GetCardDAVResourceContext(t.Context(), book.ID, href)
	require.NoError(err)
	require.NotNil(preserved.PersonID)
	assert.Equal(personID, *preserved.PersonID)
	assert.Equal(remoteBody, preserved.RemoteBody)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestUnpublishPersonTreatsAbsentRemoteAsComplete(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			fixture := &mutationFixture{}
			service, _, personID, _ := seededMutationService(t, fixture)
			require.NoError(t, service.PublishPerson(t.Context(), personID))
			fixture.deleteMissingStatus = status

			require.NoError(t, service.UnpublishPerson(t.Context(), personID))
			assert.Equal(t, 1, fixture.deletes)
		})
	}
}

func TestTimedOutDeleteRecoveryAcceptsPullTombstoneWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.deleteTimeout = true

	err := service.UnpublishPerson(t.Context(), personID)
	require.Error(err)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVMutationDelete, publication.PendingOperation)
	assert.Equal(1, fixture.deletes)

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, fixture.deletes, "delete recovery must be read-only")
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	_, err = st.GetPersonContext(t.Context(), personID)
	require.NoError(err)
}

func TestTimedOutDeleteRecoveryAcceptsCanonicalGoneWithoutReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{missingStatus: http.StatusGone}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	fixture.deleteTimeout = true

	require.Error(service.UnpublishPerson(t.Context(), personID))
	assert.Equal(1, fixture.deletes)
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(1, fixture.deletes, "410 recovery must prove absence without replaying DELETE")
	_, err := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestPublishPersonUsesV4ForV4OnlyWriteTarget(t *testing.T) {
	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET supported_vcard_versions = ? WHERE id = ?`), `["4.0"]`, book.ID)
	require.NoError(t, err)

	require.NoError(t, service.PublishPerson(t.Context(), personID))
	assert.Contains(t, string(fixture.body), "VERSION:4.0")
}

func TestPublishPersonRejectsWildcardStoredETagBeforeNetwork(t *testing.T) {
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_resources
		SET remote_etag = ? WHERE address_book_id = ? AND person_id = ?`), " * ", book.ID, personID)
	require.NoError(err)

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVInvalidPlan)
	assert.Equal(t, 1, fixture.puts)
}

func TestRetryAfterRollsBackIntentAndClampsGateToOneHour(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{throttle: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	before := time.Now()

	err := service.PublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Empty(publication.PendingOperation)
	gate, getErr := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(getErr)
	require.NotNil(gate)
	assert.WithinDuration(before.Add(time.Hour), *gate, 5*time.Second)

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Equal(1, fixture.puts)
}

func TestThrottledUnpublishRetriesAfterGateExpires(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{throttleDeletes: 1}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))

	err := service.UnpublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.False(publication.Desired)
	assert.Empty(publication.PendingOperation)
	assert.Equal(1, fixture.deletes)

	err = service.ReconcilePublications(t.Context())
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Equal(1, fixture.deletes)

	setRetryGate(t, st, time.Now().Add(-time.Minute))
	require.NoError(service.ReconcilePublications(t.Context()))
	assert.Equal(2, fixture.deletes)
	_, err = st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
}

func TestActiveRetryGateStopsSyncBeforeAnyRequest(t *testing.T) {
	fixture := &mutationFixture{}
	service, st, _, _ := seededMutationService(t, fixture)
	setRetryGate(t, st, time.Now().Add(time.Hour))

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(t, err, store.ErrCardDAVRetryAfter)
	assert.Equal(t, 0, fixture.puts+fixture.gets+fixture.deletes+fixture.reports)
}

func TestActiveRetryGateStopsPendingRecoveryBeforeAnyRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{timeout: true}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.Error(service.PublishPerson(t.Context(), personID))
	setRetryGate(t, st, time.Now().Add(time.Hour))
	requestsBefore := fixture.puts + fixture.gets + fixture.deletes + fixture.reports

	err := service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Equal(requestsBefore, fixture.puts+fixture.gets+fixture.deletes+fixture.reports)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVMutationCreate, publication.PendingOperation)
}

func TestCanonicalGet429PersistsGateAndPreservesAmbiguousUpdateIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, book := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	addProjectedEmail(t, st, personID)
	beforeResource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(err)
	fixture.throttleGets = 1
	before := time.Now()

	err = service.PublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	assert.Equal(2, fixture.puts)
	assert.Equal(2, fixture.gets)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVMutationUpdate, publication.PendingOperation)
	assert.NotEmpty(publication.OutgoingBody)
	assert.Equal(beforeResource.MappingRevision, publication.PreviousMappingRevision)
	resource, getErr := st.GetCardDAVResourceContext(t.Context(), book.ID, book.CanonicalURL+"person.vcf")
	require.NoError(getErr)
	assert.Equal(beforeResource.MappingRevision+1, resource.MappingRevision)
	gate, getErr := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(getErr)
	require.NotNil(gate)
	assert.WithinDuration(before.Add(time.Hour), *gate, 5*time.Second)
	requestsBefore := fixture.puts + fixture.gets + fixture.deletes + fixture.reports

	err = service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	assert.Equal(requestsBefore, fixture.puts+fixture.gets+fixture.deletes+fixture.reports)
}

func TestRecoveryGet429PersistsGateAndPreservesPendingIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{}
	service, st, personID, _ := seededMutationService(t, fixture)
	require.NoError(service.PublishPerson(t.Context(), personID))
	addProjectedEmail(t, st, personID)
	fixture.timeout = true
	require.Error(service.PublishPerson(t.Context(), personID))
	fixture.throttleGets = 1
	before := time.Now()
	putsBefore := fixture.puts

	err := service.PublishPerson(t.Context(), personID)
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	assert.Equal(putsBefore, fixture.puts, "mapped recovery must remain read-only")
	assert.Equal(2, fixture.gets)
	publication, getErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(getErr)
	assert.Equal(store.CardDAVMutationUpdate, publication.PendingOperation)
	assert.NotEmpty(publication.OutgoingBody)
	gate, getErr := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(getErr)
	require.NotNil(gate)
	assert.WithinDuration(before.Add(time.Hour), *gate, 5*time.Second)
}

func TestPull429PersistsAccountRetryGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	fixture := &mutationFixture{throttleReport: true}
	service, st, _, _ := seededMutationService(t, fixture)
	before := time.Now()

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusTooManyRequests, status.StatusCode)
	assert.Equal(1, fixture.reports)
	gate, getErr := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(getErr)
	require.NotNil(gate)
	assert.WithinDuration(before.Add(time.Hour), *gate, 5*time.Second)
}

func TestConcurrent429sPreserveLongestAccountRetryGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	shortStarted := make(chan struct{})
	releaseShort := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseShort) }) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/short.vcf"):
			close(shortStarted)
			<-releaseShort
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
		case strings.HasSuffix(r.URL.Path, "/long.vcf"):
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	shortResult := make(chan error, 1)
	go func() {
		_, err := service.doRequest(t.Context(), Request{
			Method: http.MethodGet, URL: book.CanonicalURL + "short.vcf",
		})
		shortResult <- err
	}()
	<-shortStarted
	before := time.Now()

	_, longErr := service.doRequest(t.Context(), Request{
		Method: http.MethodGet, URL: book.CanonicalURL + "long.vcf",
	})
	var longStatus *StatusError
	require.ErrorAs(longErr, &longStatus)
	assert.Equal(http.StatusTooManyRequests, longStatus.StatusCode)
	releaseOnce.Do(func() { close(releaseShort) })
	shortErr := <-shortResult
	var shortStatus *StatusError
	require.ErrorAs(shortErr, &shortStatus)
	assert.Equal(http.StatusTooManyRequests, shortStatus.StatusCode)

	gate, err := st.GetCardDAVRetryAfterContext(t.Context())
	require.NoError(err)
	require.NotNil(gate)
	assert.WithinDuration(before.Add(time.Hour), *gate, 5*time.Second)
}

func setRetryGate(t *testing.T, st *store.Store, retryAfter time.Time) {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(`INSERT INTO carddav_retry_gate (account_id, retry_after_at, updated_at)
		VALUES (1, ?, CURRENT_TIMESTAMP) ON CONFLICT(account_id) DO UPDATE SET
		retry_after_at = excluded.retry_after_at, updated_at = CURRENT_TIMESTAMP`), retryAfter.UTC())
	require.NoError(t, err)
}

func seededMutationServiceForServer(t *testing.T, server *httptest.Server) (*Service, *store.Store, int64, store.CardDAVAddressBook) {
	t.Helper()
	service, st, book := newPullService(t, server, false)
	var personID int64
	err := st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "person", "Alice Example").Scan(&personID)
	require.NoError(t, err)
	return service, st, personID, book
}

func addProjectedEmail(t *testing.T, st *store.Store, personID int64) {
	t.Helper()
	_, err := st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice.updated@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
}
