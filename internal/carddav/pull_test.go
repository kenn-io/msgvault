package carddav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestParseSyncPageRecognizesEquivalentAbsoluteCollectionHref(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	collection := mustParseURL(t, "https://contacts.example/books/personal/")
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("203.0.113.9"))
	client, err := NewClient(ClientOptions{CredentialOrigin: collection, Resolver: resolver})
	require.NoError(err)
	service := NewService(nil, client)

	changed, removed, token, truncated, err := service.parseSyncPage(t.Context(), collection, MultiStatus{
		SyncToken: "next-token",
		Responses: []MultiStatusResponse{{
			Href:      "https://CONTACTS.example:443/books/personal",
			PropStats: []PropStat{{StatusCode: http.StatusOK}},
		}},
	})

	require.NoError(err)
	assert.Empty(changed)
	assert.Empty(removed)
	assert.Equal("next-token", token)
	assert.False(truncated)
}

func TestPullBudgetChargesHTTPErrorBodies(t *testing.T) {
	const errorBody = "remote error response body"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, err := w.Write([]byte(errorBody))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	service, _, _ := newPullService(t, server, false)
	budget := &operationBudget{remaining: int64(len(errorBody) + 1)}

	_, err := service.do(t.Context(), Request{Method: http.MethodGet, URL: server.URL + "/missing-one"}, budget)
	require.Error(t, err)
	_, err = service.do(t.Context(), Request{Method: http.MethodGet, URL: server.URL + "/missing-two"}, budget)
	require.ErrorIs(t, err, ErrOperationLimit)
}

func TestPullBudgetChargesPartialBodiesOnResponseLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const responseBody = "abc"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, err := w.Write([]byte(responseBody))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	service, _, _ := newPullService(t, server, false)
	service.client.responseBytes = 2
	service.client.operationBytes = 10
	budget := &operationBudget{remaining: 5}

	response, err := service.do(t.Context(), Request{Method: http.MethodGet, URL: server.URL + "/oversized-one"}, budget)
	require.ErrorIs(err, ErrResponseLimit)
	require.NotNil(response)
	assert.Equal([]byte(responseBody), response.Body)
	assert.Equal(int64(2), budget.remaining)

	response, err = service.do(t.Context(), Request{Method: http.MethodGet, URL: server.URL + "/oversized-two"}, budget)
	require.ErrorIs(err, ErrOperationLimit)
	assert.Nil(response)
	assert.Equal(int64(-1), budget.remaining)
	assert.Equal(2, requests)
}

func TestPullBudgetChargesRedirectResponseBodies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "/done")
			w.WriteHeader(http.StatusFound)
			_, err := w.Write([]byte("12"))
			assert.NoError(err)
			return
		}
		_, err := w.Write([]byte("345"))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	service, _, _ := newPullService(t, server, false)
	budget := &operationBudget{remaining: 4}

	response, err := service.do(t.Context(), Request{Method: http.MethodGet, URL: server.URL + "/start"}, budget)
	require.ErrorIs(err, ErrOperationLimit)
	assert.Nil(response)
	assert.Equal(int64(-1), budget.remaining)
	assert.Equal(2, requests)
}

func TestIndividualMemberFetchTreatsGoneAsMissing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	t.Cleanup(server.Close)
	service, _, book := newPullService(t, server, false)
	collection, err := url.Parse(book.CanonicalURL)
	require.NoError(err)
	href := book.CanonicalURL + "gone.vcf"

	resources, missing, err := service.fetchMembersIndividually(
		t.Context(), collection, []string{href}, &operationBudget{remaining: defaultOperationBytes})
	require.NoError(err)
	assert.Empty(resources)
	assert.Equal([]string{href}, missing)
}

func TestSyncPageTreatsGoneMemberAsRemoved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	service, _, book := newPullService(t, server, false)
	collection, err := url.Parse(book.CanonicalURL)
	require.NoError(err)
	href := book.CanonicalURL + "gone.vcf"

	changed, removed, token, truncated, err := service.parseSyncPage(t.Context(), collection, MultiStatus{
		SyncToken: "next-token",
		Responses: []MultiStatusResponse{{Href: href, StatusCode: http.StatusGone}},
	})
	require.NoError(err)
	assert.Empty(changed)
	assert.Equal([]string{href}, removed)
	assert.Equal("next-token", token)
	assert.False(truncated)
}

func TestSyncContinuesAfterOneBookFailsAndReconcilesPublications(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/books/broken/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeDAVXML(t, w, syncResponse("", ""))
	}))
	t.Cleanup(server.Close)

	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/",
		HomeURL: server.URL + "/books/",
		Books: []store.CardDAVDiscoveredBook{
			{CanonicalURL: server.URL + "/books/broken/", DisplayName: "Broken", DiscoveryIndex: 0,
				CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed},
			{CanonicalURL: server.URL + "/books/healthy/", DisplayName: "Healthy", DiscoveryIndex: 1,
				CanCreate: &allowed, CanUpdate: &allowed, CanDelete: &allowed},
		},
	})
	require.NoError(err)
	require.Len(books, 2)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), books[1].ID, store.CardDAVBookRoles{
		IsWriteTarget: true, IsSubscribed: true,
	}))
	var personID int64
	require.NoError(st.DB().QueryRow(`INSERT INTO persons (vcard_uid) VALUES ('local-only') RETURNING id`).Scan(&personID))
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO carddav_publications (person_id, desired) VALUES (?, FALSE)`), personID)
	require.NoError(err)

	origin := mustParseURL(t, server.URL)
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin, Username: "alice", Password: "secret", AllowInsecureCredentials: true,
	})
	require.NoError(err)
	client.allowPrivateOrigin = true
	service := NewService(st, client)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.Error(err)
	assert.Equal(1, result.Books)
	assert.Equal([]string{"/books/broken/", "/books/healthy/"}, requestedPaths)
	_, publicationErr := st.GetCardDAVPublicationContext(t.Context(), personID)
	require.ErrorIs(publicationErr, store.ErrCardDAVPublicationNotFound,
		"publication reconciliation must still run after an independent book failure")
	runs, listErr := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(listErr)
	require.Len(runs, 1)
	assert.Equal(store.CardDAVSyncRunPartial, runs[0].State)
	assert.Equal(int64(result.Books), runs[0].Books)
	assert.Equal("upstream_failed", runs[0].ErrorCode)
	assert.Equal("CardDAV server request failed.", runs[0].ErrorMessage)
}

func newPullService(t *testing.T, server *httptest.Server, supportsSync bool) (*Service, *store.Store, store.CardDAVAddressBook) {
	t.Helper()
	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: server.URL, Username: "alice", PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: server.URL + "/books/personal/", DisplayName: "Personal", SupportsSyncCollection: supportsSync, SupportsMultiget: true, CanCreate: &allowed}},
	})
	require.NoError(t, err)
	origin := mustParseURL(t, server.URL)
	client, err := NewClient(ClientOptions{
		CredentialOrigin: origin, Username: "alice", Password: "secret",
		AllowInsecureCredentials: true,
	})
	require.NoError(t, err)
	client.allowPrivateOrigin = true
	return NewService(st, client), st, books[0]
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}

func readRequestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return string(body)
}

func requestedHrefs(body string) []string {
	decoder := xml.NewDecoder(strings.NewReader(body))
	var hrefs []string
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != davNamespace || start.Name.Local != "href" {
			continue
		}
		var href string
		if decoder.DecodeElement(&href, &start) == nil {
			hrefs = append(hrefs, href)
		}
	}
	return hrefs
}

func syncRequestToken(body string) string {
	decoder := xml.NewDecoder(strings.NewReader(body))
	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Space != davNamespace || start.Name.Local != "sync-token" {
			continue
		}
		var value string
		_ = decoder.DecodeElement(&value, &start)
		return value
	}
}

func syncResponse(events, token string) string {
	return `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` + events + `<D:sync-token>` + token + `</D:sync-token></D:multistatus>`
}

func changedResponse(href, etag string) string {
	return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop><D:getetag>` + etag + `</D:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func cardResponse(href, etag, uid string) string {
	return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop><D:getetag>` + etag + `</D:getetag><C:address-data>BEGIN:VCARD&#13;
VERSION:4.0&#13;
UID:` + uid + `&#13;
FN:` + uid + `&#13;
EMAIL:` + uid + `@example.test&#13;
END:VCARD&#13;
</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}

func writeDAVXML(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
}

func TestSyncRecordsOneSucceededManualRunWithExactCounters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			writeDAVXML(t, w, syncResponse(
				changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`), "token-1",
			))
			return
		}
		writeDAVXML(t, w, syncResponse(
			cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice"), "",
		))
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(SyncResult{Books: 1, Created: 1}, result)

	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(err)
	require.Len(runs, 1)
	assert.Equal(store.CardDAVSyncTriggerManual, runs[0].Trigger)
	assert.True(runs[0].Full)
	assert.Equal(store.CardDAVSyncRunSucceeded, runs[0].State)
	assert.NotNil(runs[0].FinishedAt)
	assert.Equal(int64(result.Books), runs[0].Books)
	assert.Equal(int64(result.Created), runs[0].Created)
	assert.Equal(int64(result.Updated), runs[0].Updated)
	assert.Equal(int64(result.Removed), runs[0].Removed)
}

func TestSyncRecordsExplicitScheduledTrigger(t *testing.T) {
	require := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeDAVXML(t, w, syncResponse("", "token-1"))
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)

	_, err := service.Sync(t.Context(), SyncOptions{Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(err)
	require.Len(runs, 1)
	assert.Equal(t, store.CardDAVSyncTriggerScheduled, runs[0].Trigger)
}

func TestSyncCancellationFinishesRunWithUncancelledCleanupContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := service.Sync(ctx, SyncOptions{})
		done <- err
	}()
	<-requestStarted
	cancel()

	err := <-done
	close(releaseRequest)
	require.ErrorIs(err, context.Canceled)
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(err)
	require.Len(runs, 1)
	assert.Equal(store.CardDAVSyncRunCancelled, runs[0].State)
	assert.Equal("cancelled", runs[0].ErrorCode)
	assert.NotNil(runs[0].FinishedAt)
}

func TestSyncActiveClaimPreventsNetworkAndSecondRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)
	_, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{Trigger: store.CardDAVSyncTriggerManual})
	require.NoError(err)

	_, err = service.Sync(t.Context(), SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVSyncActive)
	assert.Zero(requests)
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(err)
	assert.Len(runs, 1)
}

func TestSyncTotalFailureRecordsSafeFailedTerminalState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.Error(err)
	assert.Equal(SyncResult{}, result)
	assert.Equal("CardDAV server request failed.", err.Error())
	runs, listErr := st.ListCardDAVSyncRunsContext(t.Context(), 10, nil)
	require.NoError(listErr)
	require.Len(runs, 1)
	assert.Equal(store.CardDAVSyncRunFailed, runs[0].State)
	assert.Equal("upstream_failed", runs[0].ErrorCode)
	assert.Equal("CardDAV server request failed.", runs[0].ErrorMessage)
}

func TestSyncFailureProjectionRejectsPrivateMaterial(t *testing.T) {
	assert := assert.New(t)
	privateErr := errors.New("Authorization: Bearer synthetic-secret BEGIN:VCARD https://private.invalid/dav")
	finish := cardDAVSyncRunFinish(SyncResult{}, privateErr)
	returned := publicCardDAVSyncError(privateErr)
	assert.Equal("sync_failed", finish.ErrorCode)
	assert.Equal("CardDAV sync failed.", finish.ErrorMessage)
	require.Error(t, returned)
	assert.Equal("CardDAV sync failed.", returned.Error())
	for _, private := range []string{"synthetic-secret", "BEGIN:VCARD", "private.invalid"} {
		assert.NotContains(finish.ErrorMessage, private)
		assert.NotContains(returned.Error(), private)
	}
	assert.ErrorIs(returned, privateErr, "safe projection must preserve machine-readable cause semantics")
}

func TestSyncJoinsExecutionAndFinishFailuresWithoutReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION fail_carddav_run_finish_fn() RETURNS trigger AS $$
			BEGIN
				IF OLD.state = 'running' THEN
					RAISE EXCEPTION 'injected finish failure';
				END IF;
				RETURN NEW;
			END $$ LANGUAGE plpgsql`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER fail_carddav_run_finish
			BEFORE UPDATE OF state ON carddav_sync_runs
			FOR EACH ROW EXECUTE FUNCTION fail_carddav_run_finish_fn()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER fail_carddav_run_finish
			BEFORE UPDATE OF state ON carddav_sync_runs
			WHEN OLD.state = 'running'
			BEGIN SELECT RAISE(ABORT, 'injected finish failure'); END`)
	}
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.Error(err)
	assert.Equal(SyncResult{}, result)
	var statusErr *StatusError
	require.ErrorAs(err, &statusErr, "execution error must remain inspectable")
	require.ErrorContains(err, "injected finish failure")
	assert.Equal(1, requests, "finish failure must not replay network work")
}

func TestSyncEmptyTokenReturnsMembersAndAdvancesToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		requests = append(requests, body)
		if strings.Contains(body, "sync-collection") {
			assert.Empty(syncRequestToken(body))
			writeDAVXML(t, w, syncResponse(changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`), "token-1"))
			return
		}
		writeDAVXML(t, w, syncResponse(cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice"), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, result.Created)
	assert.Len(requests, 2)
	resource, err := st.GetCardDAVResourceContext(t.Context(), book.ID, server.URL+"/books/personal/alice.vcf")
	require.NoError(err)
	assert.Equal(`"one"`, resource.RemoteETag)
}

func TestSyncMultigetUsesBatchesOfOneHundred(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	var batchSizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			assert.Equal("token-before", syncRequestToken(body))
			var events strings.Builder
			for index := range 205 {
				events.WriteString(changedResponse(fmt.Sprintf("/books/personal/%03d.vcf", index), `&quot;e&quot;`))
			}
			writeDAVXML(t, w, syncResponse(events.String(), "token-205"))
			return
		}
		hrefs := requestedHrefs(body)
		count := len(hrefs)
		mu.Lock()
		batchSizes = append(batchSizes, count)
		mu.Unlock()
		var cards strings.Builder
		for _, href := range hrefs {
			uid := strings.TrimSuffix(path.Base(href), ".vcf")
			cards.WriteString(cardResponse(href, `&quot;e&quot;`, uid))
		}
		writeDAVXML(t, w, syncResponse(cards.String(), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_token = ? WHERE id = ?`), "token-before", book.ID)
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(205, result.Created)
	assert.Equal([]int{100, 100, 5}, batchSizes)
}

func TestMultigetMatchesEquivalentAbsoluteHref(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var responseHref string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/books/personal/", r.URL.Path)
		writeDAVXML(t, w, syncResponse(cardResponse(responseHref, `&quot;one&quot;`, "alice"), ""))
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(err)
	origin := mustParseURL(t, "http://contacts.example:"+serverURL.Port())
	responseHref = "http://CONTACTS.example:" + serverURL.Port() + "/books/personal/alice.vcf"
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("127.0.0.1"))
	client, err := NewClient(ClientOptions{CredentialOrigin: origin, Resolver: resolver})
	require.NoError(err)
	client.allowPrivateOrigin = true
	service := NewService(testutil.NewTestStore(t), client)
	collection := mustParseURL(t, origin.String()+"/books/personal/")
	requestedHref := origin.String() + "/books/personal/alice.vcf"

	resources, missing, err := service.fetchMultiget(t.Context(), collection,
		[]string{requestedHref}, &operationBudget{remaining: defaultOperationBytes})

	require.NoError(err)
	assert.Empty(missing)
	require.Len(resources, 1)
	assert.Equal(requestedHref, resources[0].Href)
}

func TestMultigetCanonicalizesEquivalentMissingHref(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var responseHref string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/books/personal/", r.URL.Path)
		missing := `<D:response><D:href>` + responseHref +
			`</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`
		writeDAVXML(t, w, syncResponse(missing, ""))
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	require.NoError(err)
	origin := mustParseURL(t, "http://contacts.example:"+serverURL.Port())
	responseHref = "http://CONTACTS.example:" + serverURL.Port() + "/books/personal/alice.vcf"
	resolver, _ := newFixtureResolver(t, netip.MustParseAddr("127.0.0.1"))
	client, err := NewClient(ClientOptions{CredentialOrigin: origin, Resolver: resolver})
	require.NoError(err)
	client.allowPrivateOrigin = true
	service := NewService(testutil.NewTestStore(t), client)
	collection := mustParseURL(t, origin.String()+"/books/personal/")
	requestedHref := origin.String() + "/books/personal/alice.vcf"

	resources, missing, err := service.fetchMultiget(t.Context(), collection,
		[]string{requestedHref}, &operationBudget{remaining: defaultOperationBytes})

	require.NoError(err)
	assert.Empty(resources)
	assert.Equal([]string{requestedHref}, missing)
}

func TestSyncContinuesTruncated507PageWithNextToken(t *testing.T) {
	syncRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			syncRequests++
			if syncRequests == 1 {
				events := changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`) +
					`<D:response><D:href>/books/personal/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`
				writeDAVXML(t, w, syncResponse(events, "page-2"))
				return
			}
			assert.Equal(t, "page-2", syncRequestToken(body))
			writeDAVXML(t, w, syncResponse(changedResponse("/books/personal/bob.vcf", `&quot;two&quot;`), "final"))
			return
		}
		var cards strings.Builder
		for _, href := range requestedHrefs(body) {
			cards.WriteString(cardResponse(href, `&quot;card&quot;`, strings.TrimSuffix(path.Base(href), ".vcf")))
		}
		writeDAVXML(t, w, syncResponse(cards.String(), ""))
	}))
	t.Cleanup(server.Close)
	service, _, _ := newPullService(t, server, true)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(t, err)
	assert.Equal(t, 2, result.Created)
	assert.Equal(t, 2, syncRequests)
}

func TestInitialSyncSendsEmptyTokenAndFallsBackToIndividualGET(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	reports, gets := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			reports++
			body := readRequestBody(t, r)
			if strings.Contains(body, "addressbook-multiget") {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			assert.Contains(body, "sync-token")
			assert.Empty(syncRequestToken(body))
			writeDAVXML(t, w, syncResponse(
				changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`), "next",
			))
		case http.MethodGet:
			gets++
			w.Header().Set("ETag", `"one"`)
			_, err := w.Write(conflictCard("alice", "Alice"))
			assert.NoError(err)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET supports_multiget = FALSE WHERE id = ?`),
		book.ID)
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(1, result.Created)
	assert.Equal(1, reports)
	assert.Equal(1, gets)
}

func TestSyncFallsBackWhenAdvertisedMultigetIsUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			reports, gets := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "REPORT":
					reports++
					body := readRequestBody(t, r)
					if strings.Contains(body, "addressbook-multiget") {
						w.WriteHeader(status)
						return
					}
					writeDAVXML(t, w, syncResponse(
						changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`), "next",
					))
				case http.MethodGet:
					gets++
					w.Header().Set("ETag", `"one"`)
					_, err := w.Write(conflictCard("alice", "Alice"))
					assert.NoError(err)
				default:
					w.WriteHeader(http.StatusMethodNotAllowed)
				}
			}))
			t.Cleanup(server.Close)
			service, _, _ := newPullService(t, server, true)

			result, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(err)
			assert.Equal(1, result.Created)
			assert.Equal(2, reports)
			assert.Equal(1, gets)
		})
	}
}

func TestSyncContinuesTruncated507PageAcrossEquivalentCollectionURLs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	syncRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		if strings.Contains(body, "sync-collection") {
			syncRequests++
			if syncRequests == 1 {
				events := changedResponse("/books/personal/alice.vcf", `&quot;one&quot;`) +
					`<D:response><D:href>/books/personal/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`
				writeDAVXML(t, w, syncResponse(events, "page-2"))
				return
			}
			assert.Equal("page-2", syncRequestToken(body))
			writeDAVXML(t, w, syncResponse(changedResponse("/books/personal/bob.vcf", `&quot;two&quot;`), "final"))
			return
		}
		var cards strings.Builder
		for _, href := range requestedHrefs(body) {
			cards.WriteString(cardResponse(href, `&quot;card&quot;`, strings.TrimSuffix(path.Base(href), ".vcf")))
		}
		writeDAVXML(t, w, syncResponse(cards.String(), ""))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET canonical_url = ? WHERE id = ?`), server.URL+"/books/personal", book.ID)
	require.NoError(err)

	result, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assert.Equal(2, result.Created)
	assert.Equal(2, syncRequests)
}

func TestSyncRejectsContinuationTokenCycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		events := `<D:response><D:href>/books/personal/</D:href><D:status>HTTP/1.1 507 Insufficient Storage</D:status></D:response>`
		writeDAVXML(t, w, syncResponse(events, "cycle"))
	}))
	t.Cleanup(server.Close)
	service, _, _ := newPullService(t, server, true)

	_, err := service.Sync(t.Context(), SyncOptions{Full: true})
	require.ErrorIs(t, err, ErrSyncTokenCycle)
}

func TestSyncReconcilesInvalidSyncTokenOnce(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readRequestBody(t, r)
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, err := w.Write([]byte(`<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>`))
			assert.NoError(t, err)
			return
		}
		assert.Empty(t, syncRequestToken(body))
		writeDAVXML(t, w, syncResponse("", "fresh-token"))
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, true)
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_token = ? WHERE id = ?`), "stale-token", book.ID)
	require.NoError(t, err)

	_, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, requests)
}

func TestManualFullSyncMarksSnapshotAsCompleteReconciliation(t *testing.T) {
	for _, supportsSync := range []bool{false, true} {
		t.Run(fmt.Sprintf("sync-collection=%t", supportsSync), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeDAVXML(t, w, syncResponse("", "fresh-token"))
			}))
			t.Cleanup(server.Close)
			service, st, book := newPullService(t, server, supportsSync)
			account, err := st.GetCardDAVAccountContext(t.Context())
			require.NoError(err)
			require.NotNil(account)

			plan, err := service.fetchBookPlan(t.Context(), *account, book, SyncOptions{Full: true},
				&operationBudget{remaining: defaultOperationBytes}, &bookSyncState{})
			require.NoError(err)
			assert.True(plan.CompletesFullReconcile)
		})
	}
}

func TestSyncDowngradesUnsupportedSyncCollectionToSnapshot(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readRequestBody(t, r)
				requests++
				if strings.Contains(body, "sync-collection") {
					w.WriteHeader(status)
					return
				}
				writeDAVXML(t, w, syncResponse(cardResponse("/books/personal/alice.vcf", `&quot;one&quot;`, "alice"), ""))
			}))
			t.Cleanup(server.Close)
			service, _, _ := newPullService(t, server, true)

			result, err := service.Sync(t.Context(), SyncOptions{Full: true})
			require.NoError(t, err)
			assert.Equal(t, 1, result.Created)
			assert.Equal(t, 2, requests)
		})
	}
}

func TestParseRemoteResourceStripsContactSchemesCaseInsensitively(t *testing.T) {
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:schemes\r\nFN:Schemes\r\n" +
		"EMAIL:MAILTO:Alice@Example.test\r\nTEL:TeL:+1-202-555-0100\r\nEND:VCARD\r\n")

	resource, err := parseRemoteResource("https://contacts.example/schemes.vcf", `"one"`, body)
	require.NoError(t, err)
	assert.Equal(t, []string{"Alice@Example.test"}, resource.Emails)
	assert.Equal(t, []string{"+1-202-555-0100"}, resource.Phones)
}

func TestParseRemoteResourceDecodesTextContactValues(t *testing.T) {
	assert := assert.New(t)
	body := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:escaped\r\n" +
		`FN:Doe\, Jane` + "\r\n" +
		`EMAIL:local\,tag@example.test` + "\r\n" +
		`TEL:+1\,202` + "\r\nEND:VCARD\r\n")

	resource, err := parseRemoteResource("https://contacts.example/escaped.vcf", `"one"`, body)
	require.NoError(t, err)
	assert.Equal("Doe, Jane", resource.DisplayName)
	assert.Equal([]string{"local,tag@example.test"}, resource.Emails)
	assert.Equal([]string{"+1,202"}, resource.Phones)
}
