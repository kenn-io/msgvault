package carddav

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMultipleConnectionsSyncUsesOwningCredentials(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	var mu sync.Mutex
	var principals []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		assertions.True(ok)
		assertions.Contains([]string{"personal", "work"}, username)
		assertions.Equal(username+"-synthetic-secret", password)
		mu.Lock()
		principals = append(principals, username)
		mu.Unlock()
		assertions.Equal("/shared/", r.URL.Path)
		writeDAVXML(t, w, syncResponse(cardResponse("/shared/person.vcf", `"one"`, "example-"+username), ""))
	}))
	t.Cleanup(server.Close)
	st := testutil.NewTestStore(t)
	input := store.CardDAVDiscoveryInput{BaseURL: server.URL, Username: "personal",
		PrincipalURL: server.URL + "/principal/", HomeURL: server.URL + "/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: server.URL + "/shared/", DisplayName: "Personal", CanCreate: new(true)}}}
	_, personalBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	input.ConnectionName, input.Username = "work", "work"
	workAccount, workBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	require.NoError(st.SetCardDAVBookRolesContext(t.Context(), workBooks[0].ID, store.CardDAVBookRoles{IsSubscribed: true, IsLookupSource: true}))
	newService := func(name, username string) *Service {
		client, err := NewClient(ClientOptions{CredentialOrigin: mustParseURL(t, server.URL), Username: username,
			Password: username + "-synthetic-secret", AllowInsecureCredentials: true})
		require.NoError(err)
		client.allowPrivateOrigin = true
		return NewService(st, client).ForConnection(name, 1)
	}
	personal, work := newService("default", "personal"), newService("work", "work")
	result, err := personal.Sync(t.Context(), SyncOptions{Full: true})
	require.NoError(err)
	assertions.Equal(1, result.Books)
	mu.Lock()
	assertions.Equal([]string{"personal"}, principals)
	principals = nil
	mu.Unlock()
	var abandonedPersonID int64
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`), "example-abandoned-unbound").Scan(&abandonedPersonID)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_publications (person_id, desired) VALUES (?, FALSE)`), abandonedPersonID)
	require.NoError(err)
	var workRunID int64
	result, err = work.Sync(t.Context(), SyncOptions{Full: true, OnRunStarted: func(id int64) { workRunID = id }})
	require.NoError(err)
	assertions.Equal(1, result.Books)
	mu.Lock()
	assertions.Equal([]string{"work"}, principals)
	mu.Unlock()
	personalMapping, err := st.GetCardDAVResourceContext(t.Context(), personalBooks[0].ID, server.URL+"/shared/person.vcf")
	require.NoError(err)
	workMapping, err := st.GetCardDAVResourceContext(t.Context(), workBooks[0].ID, server.URL+"/shared/person.vcf")
	require.NoError(err)
	assertions.Equal("example-personal", personalMapping.RemoteUID)
	assertions.Equal("example-work", workMapping.RemoteUID)
	require.NotNil(personalMapping.PersonID)
	require.NotNil(workMapping.PersonID)
	assertions.NotEqual(*personalMapping.PersonID, *workMapping.PersonID)
	runs, err := st.ListCardDAVSyncRunsContext(t.Context(), 25, nil, workAccount.ID)
	require.NoError(err)
	require.Len(runs, 1)
	assertions.Equal(runs[0].ID, workRunID)
	assertions.Equal(store.CardDAVSyncRunSucceeded, runs[0].State)
	// Retry gates are selected independently even on the same origin.
	require.NoError(st.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(time.Hour), workAccount.ID))
	_, err = work.Sync(t.Context(), SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	_, err = personal.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	_, err = st.GetCardDAVPublicationContext(t.Context(), abandonedPersonID)
	require.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
	// Explicit mutations must reject a foreign owner before touching its pending
	// intent or issuing a request, even though both books have identical URLs.
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), *personalMapping.PersonID)
	require.NoError(err)
	pending, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{
		PersonID: *personalMapping.PersonID, Desired: true, AddressBookID: personalBooks[0].ID, Href: personalMapping.Href,
		OutgoingBody: personalMapping.RemoteBody, OutgoingSemanticHash: "synthetic-change", LocalHash: snapshot.Fingerprint})
	require.NoError(err)
	mu.Lock()
	before := len(principals)
	mu.Unlock()
	err = work.PublishPerson(t.Context(), *personalMapping.PersonID)
	require.ErrorIs(err, ErrConnectionMismatch)
	mu.Lock()
	assertions.Len(principals, before)
	mu.Unlock()
	after, err := st.GetCardDAVPublicationContext(t.Context(), *personalMapping.PersonID)
	require.NoError(err)
	assertions.Equal(pending, after)
	_, err = work.PreviewPublication(t.Context(), *personalMapping.PersonID)
	require.ErrorIs(err, ErrConnectionMismatch)
	// A client retained across a credential replacement must never fetch using
	// the new generation, even when both the URL and username are unchanged.
	input.CredentialsChanged = true
	_, _, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM carddav_retry_gate WHERE account_id = ?`), workAccount.ID)
	require.NoError(err, "remove the fixture's throttle so the generation fence is the only admission condition")
	mu.Lock()
	before = len(principals)
	mu.Unlock()
	_, err = work.Sync(t.Context(), SyncOptions{})
	require.ErrorIs(err, store.ErrCardDAVStalePlan)
	mu.Lock()
	assertions.Len(principals, before)
	mu.Unlock()
}

func TestMultipleConnectionsRejectForeignConflictBeforeNetwork(t *testing.T) {
	assertions := assert.New(t)
	require := require.New(t)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { requests++; w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(server.Close)
	personal, st, book := newPullService(t, server, false)
	resource, err := parseRemoteResource(book.CanonicalURL+"person.vcf", `"one"`, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:example-person\r\nFN:Example Person\r\nEMAIL:person@example.com\r\nEND:VCARD\r\n"))
	require.NoError(err)
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: 1,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{resource}})
	require.NoError(err)
	mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, resource.Href)
	require.NoError(err)
	conflict, err := st.RecordCardDAVConflictContext(t.Context(), store.CardDAVConflictCapture{AddressBookID: book.ID,
		Href: resource.Href, ExpectedMappingRevision: mapping.MappingRevision, BaseLocalHash: mapping.LocalHash,
		LocalHash: mapping.LocalHash, BaseRemoteHash: mapping.RemoteSemanticHash, BaseRemoteETag: mapping.RemoteETag,
		RemoteETag: `"two"`, LocalBody: resource.RemoteBody, RemoteBody: resource.RemoteBody})
	require.NoError(err)
	work := personal.ForConnection("work", 0)
	err = work.ResolveConflict(t.Context(), conflict.ID, ResolutionKeepRemote)
	require.ErrorIs(err, ErrConnectionMismatch)
	assertions.Zero(requests)
	after, err := st.GetCardDAVConflictContext(t.Context(), conflict.ID)
	require.NoError(err)
	assertions.Equal(conflict, after)
}
