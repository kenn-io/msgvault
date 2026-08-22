package carddav

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func integrationRemoteResource(href, uid, etag string) store.CardDAVRemoteResource {
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:" + uid + "\r\nEMAIL:" + uid + "@example.test\r\nEND:VCARD\r\n")
	return store.CardDAVRemoteResource{
		Href: href, RemoteUID: uid, RemoteETag: etag, RemoteBody: body,
		SemanticHash: "semantic-" + uid, DisplayName: uid, Emails: []string{uid + "@example.test"},
	}
}

func TestSnapshot507NeverTombstonesOmittedResources(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := syncResponse(`<D:response><D:href>/books/personal/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`, "")
		writeDAVXML(t, w, body)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	seed := integrationRemoteResource(server.URL+"/books/personal/keep.vcf", "keep", `"seed"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{seed},
	})
	require.NoError(err)

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(err, ErrTruncatedSnapshot)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, seed.Href)
	require.NoError(err)
}

func TestSnapshotHTTP507NeverTombstonesOmittedResources(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	account, err := st.GetCardDAVAccountContext(t.Context())
	require.NoError(err)
	seed := integrationRemoteResource(server.URL+"/books/personal/keep.vcf", "keep", `"seed"`)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, ReplaceAll: true, Upserts: []store.CardDAVRemoteResource{seed},
	})
	require.NoError(err)

	_, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(err, ErrTruncatedSnapshot)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, seed.Href)
	require.NoError(err)
}

func TestSnapshotAcceptsEquivalentCollectionURLWithTrailingSlash(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		events := `<D:response><D:href>/books/personal/</D:href><D:propstat><D:prop/>` +
			`<D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
			cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice")
		writeDAVXML(t, w, syncResponse(events, ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET canonical_url = ? WHERE id = ?`), server.URL+"/books/personal", book.ID)
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(t, 1, result.Created)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID,
		server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
}

func TestSnapshotMovesResourceByAddressBookScopedRemoteUID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	phase := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		href, etag := "/books/personal/old.vcf", `&quot;one&quot;`
		if phase == 2 {
			href, etag = "/books/personal/new.vcf", `&quot;two&quot;`
		}
		writeDAVXML(t, w, syncResponse(cardResponse(href, etag, "stable-uid"), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	oldHref := server.URL + "/books/personal/old.vcf"
	newHref := server.URL + "/books/personal/new.vcf"

	first, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, first.Created)
	oldMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.NoError(err)
	require.NotNil(oldMapping.PersonID)
	personID := *oldMapping.PersonID

	phase = 2
	second, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(0, second.Created)
	assert.Equal(1, second.Updated)
	assert.Equal(0, second.Removed)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	newMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, newHref)
	require.NoError(err)
	require.NotNil(newMapping.PersonID)
	assert.Equal(personID, *newMapping.PersonID)
	resources, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	assert.Len(resources, 1)
	_, err = st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", oldHref)
	require.ErrorIs(err, store.ErrVCardResourceNotFound)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", newHref)
	require.NoError(err)
	assert.Equal(personID, envelope.PersonID)
}

func TestSnapshotDuplicateRemoteUIDClaimsPreviousHrefOnlyOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	phase := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		events := cardResponse("/books/personal/old.vcf", `&quot;one&quot;`, "stable-uid")
		if phase == 2 {
			events = cardResponse("/books/personal/new-a.vcf", `&quot;two-a&quot;`, "stable-uid") +
				cardResponse("/books/personal/new-b.vcf", `&quot;two-b&quot;`, "stable-uid")
		}
		writeDAVXML(t, w, syncResponse(events, ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)
	oldHref := server.URL + "/books/personal/old.vcf"

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	oldMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.NoError(err)
	require.NotNil(oldMapping.PersonID)
	personID := *oldMapping.PersonID

	phase = 2
	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, result.Created)
	assert.Equal(1, result.Updated)
	assert.Zero(result.Removed)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, oldHref)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	resources, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	require.Len(resources, 2)
	for _, resource := range resources {
		require.NotNil(resource.PersonID)
		assert.Equal(personID, *resource.PersonID)
	}
}

func TestSyncRefetchesOneStalePlanThenAborts(t *testing.T) {
	requests := 0
	var st *store.Store
	var bookID int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		writeDAVXML(t, w, syncResponse("", ""))
		if st != nil {
			// This independent write completing inside the handler proves the
			// service has not opened its apply transaction around network I/O.
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision = sync_revision + 1 WHERE id = ?`), bookID)
			assert.NoError(t, err)
		}
	}))
	t.Cleanup(server.Close)
	service, opened, book := newPullService(t, server, false)
	st, bookID = opened, book.ID

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(t, err, store.ErrCardDAVStalePlan)
	assert.Equal(t, 2, requests)
}

func TestSyncUsesOneInvalidTokenReconcileAcrossStaleRefetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	var st *store.Store
	var bookID int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1, 3:
			assert.Equal("stale-token", syncRequestToken(readRequestBody(t, r)))
			w.WriteHeader(http.StatusForbidden)
			_, err := w.Write([]byte(`<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`))
			assert.NoError(err)
		case 2:
			assert.Empty(syncRequestToken(readRequestBody(t, r)))
			writeDAVXML(t, w, syncResponse("", "fresh-token"))
			_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
				SET sync_revision = sync_revision + 1 WHERE id = ?`), bookID)
			assert.NoError(err)
		default:
			assert.Empty(syncRequestToken(readRequestBody(t, r)))
			writeDAVXML(t, w, syncResponse("", "unexpected-second-reconcile"))
		}
	}))
	t.Cleanup(server.Close)
	service, opened, book := newPullService(t, server, true)
	st, bookID = opened, book.ID
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET sync_token = ? WHERE id = ?`), "stale-token", book.ID)
	require.NoError(err)

	_, err = service.Sync(t.Context(), SyncOptions{})
	var status *StatusError
	require.ErrorAs(err, &status)
	assert.Equal(http.StatusForbidden, status.StatusCode)
	assert.Equal(3, requests)
}

func TestETagOnlyChurnUpdatesLedgerWithoutDuplicatingPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	etag := `&quot;one&quot;`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			writeDAVXML(t, w, syncResponse(changedResponse("/books/personal/alice.vcf", etag), "next"))
			return
		}
		writeDAVXML(t, w, syncResponse(cardResponse("/books/personal/alice.vcf", etag, "alice"), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	before, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	etag = `&quot;two&quot;`
	_, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	after, err := st.GetCardDAVResourceContext(t.Context(), book.ID, before.Href)
	require.NoError(err)
	assert.Equal(`"two"`, after.RemoteETag)
	assert.Equal(before.PersonID, after.PersonID)
	assert.Equal(before.MappingRevision+1, after.MappingRevision)
}

func TestSyncCanonicalizesEquivalentHrefSpellingsForUpdatesAndTombstones(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	phase := 1
	canonicalBase := ""
	equivalentBase := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			if phase == 1 {
				writeDAVXML(t, w, syncResponse(
					changedResponse(canonicalBase+"/books/personal/update.vcf", `&quot;one&quot;`)+
						changedResponse(canonicalBase+"/books/personal/delete.vcf", `&quot;one&quot;`),
					"token-one"))
				return
			}
			removed := `<D:response><D:href>` + equivalentBase + `/books/personal/delete.vcf` +
				`</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`
			writeDAVXML(t, w, syncResponse(
				changedResponse(equivalentBase+"/books/personal/update.vcf", `&quot;two&quot;`)+removed,
				"token-two"))
			return
		}
		if phase == 1 {
			writeDAVXML(t, w, syncResponse(
				cardResponse(canonicalBase+"/books/personal/update.vcf", `&quot;one&quot;`, "update")+
					cardResponse(canonicalBase+"/books/personal/delete.vcf", `&quot;one&quot;`, "delete"), ""))
			return
		}
		writeDAVXML(t, w, syncResponse(
			cardResponse(equivalentBase+"/books/personal/update.vcf", `&quot;two&quot;`, "update"), ""))
	}))
	t.Cleanup(server.Close)
	serverURL := mustParseURL(t, server.URL)
	canonicalBase = "http://contacts.example:" + serverURL.Port()
	equivalentBase = "http://CONTACTS.example:" + serverURL.Port()
	origin := mustParseURL(t, canonicalBase)
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("127.0.0.1"))
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin, Username: "alice", Password: "secret",
		AllowInsecureCredentials: true, Resolver: resolver,
	})
	require.NoError(err)
	client.allowPrivateOrigin = true
	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: canonicalBase, Username: "alice", PrincipalURL: canonicalBase + "/principal/",
		HomeURL: canonicalBase + "/books/", Books: []store.CardDAVDiscoveredBook{{
			CanonicalURL: canonicalBase + "/books/personal/", DisplayName: "Personal",
			SupportsSyncCollection: true, SupportsMultiget: true, CanCreate: &allowed,
		}},
	})
	require.NoError(err)
	require.Len(books, 1)
	service := NewService(st, client)
	book := books[0]
	updateHref := canonicalBase + "/books/personal/update.vcf"
	deleteHref := canonicalBase + "/books/personal/delete.vcf"

	first, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, first.Created)

	phase = 2
	second, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(1, second.Updated)
	assert.Equal(1, second.Removed)
	updated, err := st.GetCardDAVResourceContext(t.Context(), book.ID, updateHref)
	require.NoError(err)
	assert.Equal(`"two"`, updated.RemoteETag)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, deleteHref)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	resources, err := st.ListCardDAVResourcesContext(t.Context(), book.ID)
	require.NoError(err)
	require.Len(resources, 1)
	assert.Equal(updateHref, resources[0].Href)
}

func TestSnapshotKeepsCardWithNonE164PhoneWithoutAbortingOtherImports(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	localCard := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:local-number\r\nFN:Local Number\r\nTEL:12345\r\nEND:VCARD\r\n")
	validCard := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:valid-contact\r\nFN:Valid Contact\r\nEMAIL:valid@example.test\r\nEND:VCARD\r\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDAVXML(t, w, syncResponse(
			cardResponseRaw("/books/personal/local.vcf", `"local"`, escapedCardData(localCard))+
				cardResponseRaw("/books/personal/valid.vcf", `"valid"`, escapedCardData(validCard)), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, result.Created)
	localHref := server.URL + "/books/personal/local.vcf"
	local, err := st.GetCardDAVResourceContext(t.Context(), book.ID, localHref)
	require.NoError(err)
	require.NotNil(local.PersonID)
	points, err := st.ListPersonContactPointsContext(t.Context(), *local.PersonID, true)
	require.NoError(err)
	assert.Empty(points)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", localHref)
	require.NoError(err)
	assert.Equal(localCard, envelope.StoredBody)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/valid.vcf")
	require.NoError(err)
}

func TestSyncRebasesUntouchedRemoteProjectionAndPreservesUserOwnedPersonOnTombstone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	phase := 0
	cards := func() string {
		switch phase {
		case 0:
			return cardResponseRaw("/books/personal/alice.vcf", `"alice-one"`, escapedCardData(
				conflictCardWithEmail("alice", "Alice Initial", "alice-initial@example.test"))) +
				cardResponseRaw("/books/personal/bob.vcf", `"bob-one"`, escapedCardData(
					conflictCardWithEmail("bob", "Bob Initial", "bob-initial@example.test")))
		case 1:
			return cardResponseRaw("/books/personal/alice.vcf", `"alice-two"`, escapedCardData(
				conflictCardWithEmail("alice", "Alice Remote", "alice-remote@example.test"))) +
				cardResponseRaw("/books/personal/bob.vcf", `"bob-two"`, escapedCardData(
					conflictCardWithEmail("bob", "Bob Remote", "bob-remote@example.test")))
		default:
			return ""
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeDAVXML(t, w, syncResponse(cards(), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, result.Created)
	aliceHref := server.URL + "/books/personal/alice.vcf"
	bobHref := server.URL + "/books/personal/bob.vcf"
	aliceMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, aliceHref)
	require.NoError(err)
	require.NotNil(aliceMapping.PersonID)
	bobMapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, bobHref)
	require.NoError(err)
	require.NotNil(bobMapping.PersonID)

	var noteID int64
	err = st.DB().QueryRow(st.Rebind(`INSERT INTO daily_note_entries (local_date, ordinal, body)
		VALUES (?, ?, ?) RETURNING id`), "2026-08-19", 1, "synthetic note").Scan(&noteID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO daily_note_entry_persons (entry_id, person_id)
		VALUES (?, ?)`), noteID, *bobMapping.PersonID)
	require.NoError(err)

	phase = 1
	result, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, result.Updated)
	alice, err := st.GetPersonContext(t.Context(), *aliceMapping.PersonID)
	require.NoError(err)
	require.NotNil(alice.DisplayName)
	assert.Equal("Alice Remote", *alice.DisplayName)
	alicePoints, err := st.ListPersonContactPointsContext(t.Context(), *aliceMapping.PersonID, true)
	require.NoError(err)
	require.Len(alicePoints, 1)
	assert.Equal("alice-remote@example.test", alicePoints[0].OriginalValue)
	bob, err := st.GetPersonContext(t.Context(), *bobMapping.PersonID)
	require.NoError(err)
	require.NotNil(bob.DisplayName)
	assert.Equal("Bob Initial", *bob.DisplayName,
		"user-owned state must keep the local projection while the remote ledger advances")
	bobEnvelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", bobHref)
	require.NoError(err)
	assert.Contains(string(bobEnvelope.StoredBody), "FN:Bob Remote")

	phase = 2
	result, err = service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, result.Removed)
	_, err = st.GetPersonContext(t.Context(), *aliceMapping.PersonID)
	require.ErrorIs(err, store.ErrPersonNotFound,
		"the untouched rebased import must remain eligible for later tombstone cleanup")
	bob, err = st.GetPersonContext(t.Context(), *bobMapping.PersonID)
	require.NoError(err)
	require.NotNil(bob.DisplayName)
	assert.Equal("Bob Initial", *bob.DisplayName)
	var links int
	err = st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM daily_note_entry_persons
		WHERE entry_id = ? AND person_id = ?`), noteID, *bobMapping.PersonID).Scan(&links)
	require.NoError(err)
	assert.Equal(1, links)
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, bobHref)
	require.ErrorIs(err, store.ErrCardDAVResourceNotFound)
	_, err = st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", bobHref)
	require.ErrorIs(err, store.ErrVCardResourceNotFound)
}

func TestSnapshotPreservesLiteralCRLFAddressDataBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const href = "/books/personal/line-endings.vcf"
	const card = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:line-endings\r\nFN:Line Endings\r\nEND:VCARD\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
			`<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` +
			`<D:getetag>&quot;literal-crlf&quot;</D:getetag><C:address-data>` + card +
			`</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status>` +
			`</D:propstat></D:response></D:multistatus>`
		writeDAVXML(t, w, body)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	resourceHref := server.URL + href
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, resourceHref)
	require.NoError(err)
	assert.Equal([]byte(card), resource.RemoteBody)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", resourceHref)
	require.NoError(err)
	assert.Equal([]byte(card), envelope.StoredBody)
}

func TestSyncCarriesRedirectedCollectionURLIntoRelativeHrefsAndMultiget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	redirects := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/books/personal" {
			redirects++
			http.Redirect(w, r, "/books/personal/", http.StatusMovedPermanently)
			return
		}
		if !assert.Equal("/books/personal/", r.URL.Path) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			if r.Header.Get("Depth") != "1" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeDAVXML(t, w, syncResponse(changedResponse("alice.vcf", `&quot;one&quot;`), "next"))
			return
		}
		writeDAVXML(t, w, syncResponse(cardResponse("alice.vcf", `&quot;one&quot;`, "alice"), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET canonical_url = ? WHERE id = ?`),
		server.URL+"/books/personal", book.ID)
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(1, result.Created)
	assert.Equal(1, redirects, "multiget must use the effective collection URL")
	_, err = st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
}

func TestSnapshotPreservesCDATAAddressDataBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const href = "/books/personal/cdata.vcf"
	const card = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:cdata\r\nFN:CDATA\r\nNOTE:literal &amp; text\r\nEND:VCARD\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
			`<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` +
			`<D:getetag>&quot;cdata&quot;</D:getetag><C:address-data><![CDATA[` + card +
			`]]></C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status>` +
			`</D:propstat></D:response></D:multistatus>`
		writeDAVXML(t, w, body)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	resourceHref := server.URL + href
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, resourceHref)
	require.NoError(err)
	assert.Equal([]byte(card), resource.RemoteBody)
	envelope, err := st.GetVCardResourceEnvelopeContext(t.Context(), "carddav:1", resourceHref)
	require.NoError(err)
	assert.Equal([]byte(card), envelope.StoredBody)
}
