package mscontacts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

// fakeGraph applies contact writes the way Graph does: PATCH checks If-Match,
// DELETE ignores it, and delta rejects $expand.
type fakeGraph struct {
	mu       sync.Mutex
	parents  map[string]string // folder ID -> parent folder ID
	names    map[string]string
	contacts map[string]*contact
	changes  []fakeChange
	version  int
	next     int
	posts    int
	patches  int
	deletes  int
	lists    int
	dropPost bool
	// dropPatch applies the next PATCH and then answers 503.
	dropPatch bool
	// rejectPatch answers the next PATCH with 503 without applying it.
	rejectPatch bool
	// onLookup runs when a UID lookup arrives.
	onLookup func()
	expire   bool
	failWith int // status for every request, when set
}

type fakeChange struct {
	version   int
	folder    string
	contactID string
	removed   bool
}

func newFakeGraph() *fakeGraph {
	return &fakeGraph{
		parents:  map[string]string{"root": "", "test": "root"},
		names:    map[string]string{"root": "Contacts", "test": "test"},
		contacts: map[string]*contact{},
	}
}

func (f *fakeGraph) record(folder, id string, removed bool) {
	f.version++
	f.changes = append(f.changes, fakeChange{version: f.version, folder: folder, contactID: id, removed: removed})
}

func (f *fakeGraph) put(folder string, c contact) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	c.ID, c.ParentFolderID = "c"+strconv.Itoa(f.next), folder
	c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
	f.contacts[c.ID] = &c
	f.record(folder, c.ID, false)
	return c.ID
}

func (f *fakeGraph) edit(id string, change func(*contact)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.contacts[id]
	change(c)
	c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
	f.record(c.ParentFolderID, id, false)
}

func (f *fakeGraph) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(f.contacts[id].ParentFolderID, id, true)
	delete(f.contacts, id)
}

// published returns the contacts that msgvault created for the person "alice".
func (f *fakeGraph) published() []*contact {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []*contact
	for _, c := range f.contacts {
		if c.uid() == "alice" {
			found = append(found, c)
		}
	}
	return found
}

// stored applies Graph's write defaults: an email without a name gets its
// address as the name.
func stored(c *contact) {
	for i := range c.EmailAddresses {
		if c.EmailAddresses[i].Name == "" {
			c.EmailAddresses[i].Name = c.EmailAddresses[i].Address
		}
	}
}

func (f *fakeGraph) view(c contact, expand bool) contact {
	if !expand {
		c.Properties = nil
	}
	return c
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/v1.0")
	query := r.URL.Query()
	expand := query.Has("$expand")
	reply := func(code int, value any) {
		w.WriteHeader(code)
		if value != nil {
			_ = json.MarshalWrite(w, value)
		}
	}
	fail := func(code int, graphCode string) {
		reply(code, map[string]any{"error": map[string]string{"code": graphCode}})
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i := range segments {
		segments[i], _ = url.PathUnescape(segments[i])
	}
	if f.failWith != 0 {
		w.Header().Set("Retry-After", "0")
		fail(f.failWith, "Failure")
		return
	}
	switch {
	case path == "/me/contactFolders/contacts":
		reply(http.StatusOK, folder{ID: "root", DisplayName: "Contacts"})
	case len(segments) == 4 && segments[3] == "childFolders":
		var children []folder
		for id, parent := range f.parents {
			if parent == segments[2] {
				children = append(children, folder{ID: id, DisplayName: f.names[id]})
			}
		}
		reply(http.StatusOK, map[string]any{"value": children})
	case len(segments) == 5 && segments[4] == "delta":
		if expand {
			fail(http.StatusBadRequest, "BadRequest")
			return
		}
		if f.expire && query.Has("v") {
			fail(http.StatusGone, "syncStateNotFound")
			return
		}
		since, _ := strconv.Atoi(query.Get("v"))
		items := []map[string]any{}
		for _, change := range f.changes {
			if change.folder != segments[2] || change.version <= since {
				continue
			}
			item := map[string]any{"id": change.contactID}
			if change.removed {
				item["@removed"] = map[string]string{"reason": "deleted"}
			}
			items = append(items, item)
		}
		link := "http://" + r.Host + "/v1.0" + path + "?v=" + strconv.Itoa(f.version)
		reply(http.StatusOK, map[string]any{"value": items, "@odata.deltaLink": link})
	case len(segments) == 4 && segments[3] == "contacts" && r.Method == http.MethodGet:
		f.lists++
		values := []contact{}
		filter := query.Get("$filter")
		if filter != "" && f.onLookup != nil {
			f.onLookup()
		}
		for _, c := range f.contacts {
			if c.ParentFolderID != segments[2] {
				continue
			}
			if _, uid, ok := strings.Cut(filter, "ep/value eq '"); ok && c.uid() != strings.TrimSuffix(uid, "')") {
				continue
			}
			values = append(values, f.view(*c, expand))
		}
		reply(http.StatusOK, map[string]any{"value": values})
	case len(segments) == 4 && segments[3] == "contacts" && r.Method == http.MethodPost:
		var c contact
		if err := json.UnmarshalRead(r.Body, &c); err != nil {
			fail(http.StatusBadRequest, "BadRequest")
			return
		}
		if overLimit(c) {
			fail(http.StatusBadRequest, "ErrorInvalidProperty")
			return
		}
		stored(&c)
		f.posts++
		f.next++
		c.ID, c.ParentFolderID = "c"+strconv.Itoa(f.next), segments[2]
		c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
		f.contacts[c.ID] = &c
		f.record(c.ParentFolderID, c.ID, false)
		if f.dropPost {
			f.dropPost = false
			reply(http.StatusServiceUnavailable, nil)
			return
		}
		reply(http.StatusCreated, c)
	case len(segments) == 3 && segments[1] == "contacts":
		c, ok := f.contacts[segments[2]]
		if !ok {
			fail(http.StatusNotFound, "ErrorItemNotFound")
			return
		}
		switch r.Method {
		case http.MethodGet:
			reply(http.StatusOK, f.view(*c, expand))
		case http.MethodPatch:
			if f.rejectPatch {
				f.rejectPatch = false
				reply(http.StatusServiceUnavailable, nil)
				return
			}
			if r.Header.Get("If-Match") != c.ETag {
				fail(http.StatusPreconditionFailed, "ErrorIrresolvableConflict")
				return
			}
			var next contact
			if err := json.UnmarshalRead(r.Body, &next); err != nil {
				fail(http.StatusBadRequest, "BadRequest")
				return
			}
			if overLimit(next) {
				fail(http.StatusBadRequest, "ErrorInvalidProperty")
				return
			}
			stored(&next)
			f.patches++
			properties := c.Properties
			for _, update := range next.Properties {
				properties = slices.DeleteFunc(slices.Clone(properties), func(p singleValueExtendedProperty) bool { return p.ID == update.ID })
				properties = append(properties, update)
			}
			next.ID, next.ParentFolderID, next.Properties = c.ID, c.ParentFolderID, properties
			next.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
			f.contacts[c.ID] = &next
			f.record(c.ParentFolderID, c.ID, false)
			if f.dropPatch {
				f.dropPatch = false
				reply(http.StatusServiceUnavailable, nil)
				return
			}
			reply(http.StatusOK, next)
		case http.MethodDelete:
			f.deletes++
			f.record(c.ParentFolderID, c.ID, true)
			delete(f.contacts, c.ID)
			reply(http.StatusNoContent, nil)
		}
	default:
		reply(http.StatusNotFound, nil)
	}
}

type fixture struct {
	fake    *fakeGraph
	service *carddav.Service
	store   *store.Store
	book    store.CardDAVAddressBook
	// tokenErr, when set, is returned in place of an access token.
	tokenErr error
}

// newFixture discovers the fake account and makes the "test" folder the
// subscribed write target. The default Contacts folder gets no role.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	fake := newFakeGraph()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	st := testutil.NewTestStore(t)
	f := &fixture{}
	remote := NewRemote(server.URL+"/v1.0", func(context.Context) (string, error) {
		if f.tokenErr != nil {
			return "", f.tokenErr
		}
		return "token", nil
	})
	service := carddav.NewRemoteService(st, remote)
	discovery, err := service.DiscoverConnection(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, discovery.Books, 2)
	require.NoError(t, service.PersistDiscovery(t.Context(), server.URL+"/v1.0", "mike@example.test", discovery, false))
	books, err := st.ListCardDAVAddressBooksContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(t, err)
	var book store.CardDAVAddressBook
	for _, candidate := range books {
		require.NoError(t, service.SetBookRoles(t.Context(), candidate.ID, carddav.BookRoles{}))
		if candidate.DisplayName == "test" {
			book = candidate
		}
	}
	require.NotZero(t, book.ID)
	require.NoError(t, service.SetBookRoles(t.Context(), book.ID, carddav.BookRoles{WriteTarget: true, Subscribed: true}))
	f.fake, f.service, f.store, f.book = fake, service, st, book
	return f
}

func (f *fixture) sync(t *testing.T) carddav.SyncResult {
	t.Helper()
	result, err := f.service.Sync(t.Context(), carddav.SyncOptions{})
	require.NoError(t, err)
	return result
}

// alice adds the person "Alice Local", whom the tests publish.
func (f *fixture) alice(t *testing.T) int64 {
	t.Helper()
	var id int64
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "alice", "Alice Local").Scan(&id))
	return id
}

func TestSyncFollowsOutlookAddEditAndDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	bob := f.fake.put("test", contact{DisplayName: "Bob Outlook", EmailAddresses: []emailAddress{{Address: "bob@example.test"}}})
	f.fake.put("root", contact{DisplayName: "Not synced"})

	assert.Equal(1, f.sync(t).Created)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+bob)
	require.NoError(err)
	assert.Equal(bob, resource.RemoteUID)
	assert.Contains(string(resource.RemoteBody), "FN:Bob Outlook")
	assert.Contains(string(resource.RemoteBody), "EMAIL:bob@example.test")

	lists := f.fake.lists
	assert.Equal(carddav.SyncResult{Books: 1}, f.sync(t))
	assert.Equal(lists, f.fake.lists, "an unchanged folder is not listed again")

	f.fake.edit(bob, func(c *contact) { c.DisplayName = "Bob Edited" })
	assert.Equal(1, f.sync(t).Updated)
	resource, err = f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+bob)
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "FN:Bob Edited")

	f.fake.remove(bob)
	assert.Equal(1, f.sync(t).Removed)
}

func TestExpiredDeltaTokenFallsBackToSnapshot(t *testing.T) {
	assert := assert.New(t)
	f := newFixture(t)
	f.fake.put("test", contact{DisplayName: "Bob"})
	f.sync(t)
	f.fake.expire = true
	lists := f.fake.lists

	assert.Equal(carddav.SyncResult{Books: 1}, f.sync(t))
	assert.Equal(lists+1, f.fake.lists, "an expired token lists the folder again")
}

func TestPublishCreatesContactWithUIDAndSettles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)

	require.NoError(f.service.PublishPerson(t.Context(), personID))
	created := f.fake.published()
	require.Len(created, 1)
	assert.Equal("Alice Local", created[0].DisplayName)
	assert.Equal("test", created[0].ParentFolderID)
	publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(f.book.CanonicalURL+"/uid/alice", publication.Href)
	assert.Empty(publication.PendingOperation)

	f.sync(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	assert.Equal(1, f.fake.posts)
	assert.Equal(0, f.fake.patches, "a settled publication sends nothing")
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)

	require.NoError(f.service.UnpublishPerson(t.Context(), personID))
	assert.Empty(f.fake.published())
}

func TestOutlookEditBeforeUpdateBecomesConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.fake.edit(f.fake.published()[0].ID, func(c *contact) { c.JobTitle = "Edited in Outlook" })
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	err = f.service.PublishPerson(t.Context(), personID)
	var conflict *carddav.ConflictError
	require.ErrorAs(err, &conflict)
	assert.Equal(0, f.fake.patches)
	assert.Equal("Edited in Outlook", f.fake.published()[0].JobTitle)
}

func TestLostCreateResponseIsRecoveredWithoutDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	f.fake.dropPost = true

	require.Error(f.service.PublishPerson(t.Context(), personID))
	assert.Len(f.fake.published(), 1)

	f.sync(t)
	assert.Equal(1, f.fake.posts, "the create is not sent again")
	assert.Len(f.fake.published(), 1)
	publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

func TestContactMapsToVCardAndBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	birthday := "1990-05-04T00:00:00Z"
	original := contact{
		DisplayName: "Dr. Ada M. Lovelace, Jr.", GivenName: "Ada", MiddleName: "M.", Surname: "Lovelace",
		Title: "Dr.", Generation: "Jr.", NickName: "Ada; the first",
		EmailAddresses: []emailAddress{{Address: "ada@example.test"}, {Address: "ada@work.test"}},
		BusinessPhones: []string{"+1 555 0100"}, HomePhones: []string{"+1 555 0101"}, MobilePhone: "+1 555 0102",
		CompanyName: "Analytical, Ltd", Department: "Engines", JobTitle: "Programmer",
		Birthday: &birthday, PersonalNotes: "line one\nline two",
		HomeAddress:     physicalAddress{Street: "1 Home St", City: "London", PostalCode: "N1", CountryOrRegion: "UK"},
		BusinessAddress: physicalAddress{Street: "2 Work Rd", City: "London"},
		OtherAddress:    physicalAddress{City: "Paris"},
	}
	body, err := original.toVCard("uid-1")
	require.NoError(err)
	assert.Contains(string(body), "UID:uid-1")

	mapped, err := contactFromVCard(body)
	require.NoError(err)
	assert.Equal(original, mapped)
}

func TestUnmappedVCardPropertiesAreDropped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mapped, err := contactFromVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:x\r\nFN:Ann\r\n" +
		"X-CUSTOM:kept nowhere\r\nRELATED:urn:uuid:y\r\nTEL;TYPE=cell:1\r\nTEL;TYPE=cell:2\r\nBDAY:--0504\r\nEND:VCARD\r\n"))
	require.NoError(err)
	assert.Equal("Ann", mapped.DisplayName)
	assert.Equal("1", mapped.MobilePhone)
	assert.Equal([]string{"2"}, mapped.BusinessPhones, "a second mobile number becomes a business phone")
	assert.Nil(mapped.Birthday, "Graph needs a year")

	mapped, err = contactFromVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ann\r\nTEL:tel:+1 555 0100\r\nTEL:+1 555 0199\r\nEND:VCARD\r\n"))
	require.NoError(err)
	placePhones(&mapped, &contact{HomePhones: []string{"+15550100"}})
	assert.Equal([]string{"+1 555 0100"}, mapped.HomePhones, "an untyped number keeps its Outlook field")
	assert.Equal([]string{"+1 555 0199"}, mapped.BusinessPhones)
}

func TestPublishWithUnmappedPropertySettles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	for kind, value := range map[store.ContactAddressKind]string{
		store.ContactAddressEmail: "alice@example.test",
		store.ContactAddressPhone: "+1 555 0100",
		store.ContactAddressURL:   "https://alice.example.test",
	} {
		_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: kind, OriginalValue: value, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}

	require.NoError(f.service.PublishPerson(t.Context(), personID))
	created := f.fake.published()
	require.Len(created, 1)
	assert.Equal([]emailAddress{{Name: "alice@example.test", Address: "alice@example.test"}}, created[0].EmailAddresses)
	f.sync(t)
	f.sync(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))

	assert.Equal(1, f.fake.posts)
	assert.Equal(0, f.fake.patches, "the URL that Graph drops does not cause a write on every publish")
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestOutlookEditKeepsPropertiesThatGraphCannotHold(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://alice.example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	f.fake.edit(f.fake.published()[0].ID, func(c *contact) { c.JobTitle = "Edited in Outlook" })
	f.sync(t)

	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "TITLE:Edited in Outlook")
	assert.Contains(string(resource.RemoteBody), "https://alice.example.test")
}

func TestGraphFailuresReachTheRetryGateAndRunHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)

	f.fake.failWith = http.StatusUnauthorized
	_, err := f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, _ := carddav.SyncFailure(err)
	assert.Equal("authentication_failed", code)

	f.fake.failWith = 0
	f.tokenErr = errors.Join(carddav.ErrMicrosoftAuthorizationRequired, &carddav.StatusError{StatusCode: http.StatusUnauthorized})
	_, err = f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, message := carddav.SyncFailure(err)
	assert.Equal("microsoft_authorization_required", code)
	assert.Contains(message, "msgvault carddav authorize-microsoft")
	f.tokenErr = nil

	f.fake.failWith = http.StatusTooManyRequests
	_, err = f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, _ = carddav.SyncFailure(err)
	assert.Equal("retry_after", code)
	gate, err := f.store.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.NotNil(gate, "a 429 that outlasts Graph's retries pauses the connection")
}

// An Outlook contact that msgvault later updates is written in msgvault's
// form, which Graph stores with losses: a phone without TYPE becomes a
// business phone. The update must read back as sent, not as a conflict.
func TestUpdatedOutlookContactReadsBackWithoutConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	f.fake.put("test", contact{
		DisplayName: "Bob Outlook", MobilePhone: "+1 555 0100",
		EmailAddresses: []emailAddress{{Name: "Bob at home", Address: "bob@home.test"}},
	})
	f.sync(t)
	var personID int64
	require.NoError(f.store.DB().QueryRow(`SELECT id FROM persons WHERE display_name = 'Bob Outlook'`).Scan(&personID))
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "bob@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	assert.Equal(1, f.fake.patches)
	updated := f.fake.contacts[f.fake.changes[0].contactID]
	assert.Equal("+15550100", updated.MobilePhone, "the number stays in its Outlook field")
	assert.Empty(updated.BusinessPhones)
	assert.Contains(updated.EmailAddresses, emailAddress{Name: "Bob at home", Address: "bob@home.test"}, "the Outlook email name is kept")
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestLostUpdateResponseIsRecoveredWithoutConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	f.fake.dropPatch = true

	require.Error(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	assert.Equal(1, f.fake.patches, "the applied update is not sent again")
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
	publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)
}

// publishedAliceWithNewEmail publishes alice, then gives her a new email, so
// the next publish is an update.
func publishedAliceWithNewEmail(t *testing.T, f *fixture) int64 {
	t.Helper()
	personID := f.alice(t)
	require.NoError(t, f.service.PublishPerson(t.Context(), personID))
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	return personID
}

func TestUpdateRejectedBeforeApplyIsSentAgain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := publishedAliceWithNewEmail(t, f)
	f.fake.rejectPatch = true

	require.NoError(f.service.PublishPerson(t.Context(), personID))

	assert.Equal(1, f.fake.patches)
	assert.Contains(f.fake.published()[0].EmailAddresses, emailAddress{Name: "alice@example.test", Address: "alice@example.test"})
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestRetryGateSetDuringLookupStopsTheWrite(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	personID := publishedAliceWithNewEmail(t, f)
	var gateErr error
	f.fake.onLookup = func() {
		f.fake.onLookup = nil
		gateErr = f.store.SetCardDAVRetryAfterContext(context.Background(), time.Now().Add(time.Hour), store.DefaultCardDAVAccountID)
	}

	err := f.service.PublishPerson(t.Context(), personID)

	require.NoError(gateErr)
	require.ErrorIs(err, store.ErrCardDAVRetryAfter)
	require.Equal(0, f.fake.patches, "the write after the lookup waits for the gate")
}

// After an Outlook edit, a saved line that Outlook still holds comes back as
// saved, except PREF and the home, work or cell type that Outlook decides. A
// saved line that Outlook never held comes back unchanged.
func TestOutlookEditKeepsSavedLines(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	saved := "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:alice\r\nFN:Alice\r\n" +
		"EMAIL;TYPE=work;PREF=1:alice@work.test\r\nEMAIL;TYPE=home:alice@old.test\r\n" +
		"EMAIL:alice@third.test\r\nEMAIL:alice@fourth.test\r\n" +
		"TEL;VALUE=uri;TYPE=voice,home:tel:+15550100\r\nTEL;TYPE=home:+15550200\r\nTEL;TYPE=home:+15550300\r\n" +
		"ADR;TYPE=home:PO 7;;1 Main;Springfield;;;\r\nTITLE:Engineer\r\n" +
		"RELATED;VALUE=text;X-LABEL=friend:Bob\r\nBDAY:--0315\r\nEND:VCARD\r\n"
	c, err := contactFromVCard([]byte(saved))
	require.NoError(err)
	placePhones(&c, nil)
	require.Len(c.EmailAddresses, 3, "Graph holds three emails")
	require.Len(c.HomePhones, 2, "Graph holds two home phones")
	c.Properties = []singleValueExtendedProperty{{ID: vcardProperty, Value: saved}}
	c.JobTitle = "Edited in Outlook"
	c.EmailAddresses[1].Address = "alice@new.test"
	c.BusinessPhones, c.HomePhones = c.HomePhones[:1], c.HomePhones[1:]

	body, err := c.body("alice")
	require.NoError(err)
	document, err := vcard.Decode(strings.NewReader(string(body)))
	require.NoError(err)
	card := document.Cards[0]
	emails := card.PropertiesNamed("EMAIL")
	require.Len(emails, 4)
	assert.Equal([]string{"work"}, typeValues(emails[0]))
	assert.Empty(emails[0].ParametersNamed("PREF"))
	assert.Equal("alice@new.test", emails[1].RawValue)
	assert.Empty(emails[1].Parameters, "a changed address drops its saved type")
	assert.Equal("alice@fourth.test", emails[3].RawValue, "Graph never held the fourth email")
	phones := card.PropertiesNamed("TEL")
	require.Len(phones, 3)
	assert.Equal("tel:+15550100", phones[0].RawValue)
	assert.Equal([]string{"work", "voice"}, typeValues(phones[0]))
	assert.Len(phones[0].ParametersNamed("VALUE"), 1)
	assert.Equal("+15550300", phones[2].RawValue, "Graph never held the third home phone")
	addresses := card.PropertiesNamed("ADR")
	require.Len(addresses, 1)
	assert.Equal("PO 7;;1 Main;Springfield;;;", addresses[0].RawValue, "the PO box survives")
	assert.Contains(string(body), "TITLE:Edited in Outlook\r\n")
	assert.NotContains(string(body), "Engineer", "a held line that Outlook changed follows Outlook")
	assert.Contains(string(body), "RELATED;VALUE=text;X-LABEL=friend:Bob\r\n", "an unmapped property stays as saved")
	assert.Contains(string(body), "BDAY:--0315\r\n", "a birthday without a year has no Graph field and stays as saved")
}

// Graph refuses a write with more than three emails. A person with four
// publishes the first three, and the saved vCard keeps the fourth.
func TestPublishBeyondGraphLimitsKeepsTheRest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	for _, address := range []string{"a1@example.test", "a2@example.test", "a3@example.test", "a4@example.test"} {
		_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: address,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	published := f.fake.published()
	require.Len(published, 1)
	assert.Len(published[0].EmailAddresses, 3)
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)

	f.fake.edit(published[0].ID, func(c *contact) { c.JobTitle = "Edited in Outlook" })
	f.sync(t)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
	require.NoError(err)
	assert.Equal(4, strings.Count(string(resource.RemoteBody), "EMAIL"), string(resource.RemoteBody))
}

// overLimit applies Graph's list limits, measured on 2026-10-05: Graph
// refuses the whole write.
func overLimit(c contact) bool {
	return len(c.EmailAddresses) > 3 || len(c.BusinessPhones) > 2 || len(c.HomePhones) > 2
}
