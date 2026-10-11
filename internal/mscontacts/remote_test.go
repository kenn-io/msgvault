package mscontacts

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
	"go.kenn.io/msgvault/internal/vcard/registry"
)

// fakeGraph applies contact writes the way Graph does: PATCH checks If-Match,
// DELETE ignores it, and delta rejects $expand.
type fakeGraph struct {
	// patchStatus, when set, answers every PATCH with that status.
	patchStatus int
	mu          sync.Mutex
	parents     map[string]string // folder ID -> parent folder ID
	names       map[string]string
	contacts    map[string]*contact
	changes     []fakeChange
	version     int
	next        int
	posts       int
	patches     int
	deletes     int
	lists       int
	dropPost    bool
	// afterPost, when set, edits a created contact after Graph answers.
	afterPost func(*contact)
	// dropPatch applies the next PATCH and then answers 503.
	dropPatch bool
	// rejectPatch answers the next PATCH with 503 without applying it.
	rejectPatch bool
	// rejectDelete, when set, answers the next DELETE with the status it
	// returns, without applying it, after running on the contact with the
	// lock held.
	rejectDelete func(*contact) int
	// pageSize, when set, pages a folder listing by $skip, as Graph does.
	pageSize int
	// maxTop, when set, answers a listing of several contacts whose $top is
	// missing or exceeds it with a page too large for pageBytes.
	maxTop int
	// thinPages drops the saved vCard from folder listings, as Graph may for
	// a large property; a read by ID still returns it.
	thinPages bool
	// truncatedPages cuts the saved vCard short in folder listings.
	truncatedPages bool
	// thinReads drops the saved vCard from reads by ID too.
	thinReads bool
	// deleteOnRead deletes the next contact read by ID before answering, as
	// if Outlook removed it between the listing and the reread.
	deleteOnRead bool
	// onPage runs, with the lock held, before each listing page after the
	// first.
	onPage func()
	// onLookup runs when a UID lookup arrives.
	onLookup func()
	expire   bool // the next delta with a token answers 410 Gone
	failWith int  // status for every request, when set
	// failListings answers the next folder listings with 503 and a 64 KB
	// body, as a listing that keeps breaking does.
	failListings int
	// lag leaves removals out of delta, as Graph may report a change late. A
	// round's link stops before the first one left out, so a round without lag
	// reports it.
	lag bool
}

// contactFromVCard maps a vCard to the Graph contact fields, without the
// names of dropped lines.
func contactFromVCard(body []byte) (contact, error) {
	c, _, err := mapVCard(body)
	return c, err
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
	c.ID, c.ParentFolderID, c.CreatedDateTime = "c"+strconv.Itoa(f.next), folder, created(f.version+1)
	c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
	f.contacts[c.ID] = &c
	f.record(folder, c.ID, false)
	return c.ID
}

// created returns the creation time of a contact made at version.
func created(version int) string {
	return time.Date(2026, 1, 1, 0, 0, version, 0, time.UTC).Format(time.RFC3339)
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

// view applies $expand as Graph does: only the extended properties that the
// filter names are returned.
func (f *fakeGraph) view(c contact, expand string) contact {
	var kept []singleValueExtendedProperty
	for _, property := range c.Properties {
		if strings.Contains(expand, "id eq '"+property.ID+"'") {
			kept = append(kept, property)
		}
	}
	c.Properties = kept
	return c
}

func (f *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.EscapedPath(), "/v1.0")
	query := r.URL.Query()
	expand := query.Get("$expand")
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
		if query.Has("$expand") {
			fail(http.StatusBadRequest, "BadRequest")
			return
		}
		if f.expire && query.Has("v") {
			f.expire = false
			fail(http.StatusGone, "syncStateNotFound")
			return
		}
		items, until := []map[string]any{}, f.version
		if !query.Has("v") {
			// An initial round names the folder's current contacts, without past removals.
			for id, c := range f.contacts {
				if c.ParentFolderID == segments[2] {
					items = append(items, map[string]any{"id": id})
				}
			}
			reply(http.StatusOK, map[string]any{"value": items, "@odata.deltaLink": "http://" + r.Host + "/v1.0" + path + "?v=" + strconv.Itoa(until)})
			return
		}
		since, _ := strconv.Atoi(query.Get("v"))
		for _, change := range f.changes {
			if change.folder != segments[2] || change.version <= since {
				continue
			}
			item := map[string]any{"id": change.contactID}
			if change.removed && f.lag {
				until = min(until, change.version-1)
				continue
			}
			if change.removed {
				item["@removed"] = map[string]string{"reason": "deleted"}
			}
			items = append(items, item)
		}
		link := "http://" + r.Host + "/v1.0" + path + "?v=" + strconv.Itoa(until)
		reply(http.StatusOK, map[string]any{"value": items, "@odata.deltaLink": link})
	case len(segments) == 4 && segments[3] == "contacts" && r.Method == http.MethodGet:
		f.lists++
		if f.failListings > 0 {
			f.failListings--
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(make([]byte, 64<<10))
			return
		}
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
			view := f.view(*c, expand)
			if f.thinPages {
				view.Properties = slices.DeleteFunc(view.Properties, func(p singleValueExtendedProperty) bool { return p.ID == vcardProperty })
			}
			if f.truncatedPages {
				view.Properties = slices.Clone(view.Properties)
				for i, p := range view.Properties {
					if p.ID == vcardProperty {
						view.Properties[i].Value = p.Value[:len(p.Value)/2]
					}
				}
			}
			values = append(values, view)
		}
		if top, _ := strconv.Atoi(query.Get("$top")); f.maxTop > 0 && len(values) > 1 && (top == 0 || top > f.maxTop) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"value":[],"pad":"` + strings.Repeat("x", pageBytes) + `"}`))
			return
		}
		if f.pageSize == 0 || filter != "" {
			reply(http.StatusOK, map[string]any{"value": values})
			return
		}
		skip, _ := strconv.Atoi(query.Get("$skip"))
		if skip > 0 && f.onPage != nil {
			f.onPage()
			values = slices.DeleteFunc(values, func(c contact) bool { return f.contacts[c.ID] == nil })
		}
		slices.SortFunc(values, func(a, b contact) int { return strings.Compare(a.ID, b.ID) })
		page := map[string]any{"value": values[min(skip, len(values)):min(skip+f.pageSize, len(values))]}
		if skip+f.pageSize < len(values) {
			next := *r.URL
			q := next.Query()
			q.Set("$skip", strconv.Itoa(skip+f.pageSize))
			next.RawQuery = q.Encode()
			page["@odata.nextLink"] = "http://" + r.Host + next.String()
		}
		reply(http.StatusOK, page)
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
		c.ID, c.ParentFolderID, c.CreatedDateTime = "c"+strconv.Itoa(f.next), segments[2], created(f.version+1)
		c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
		f.contacts[c.ID] = &c
		f.record(c.ParentFolderID, c.ID, false)
		if f.dropPost {
			f.dropPost = false
			reply(http.StatusServiceUnavailable, nil)
			return
		}
		reply(http.StatusCreated, c)
		if f.afterPost != nil {
			f.afterPost(f.contacts[c.ID])
			f.afterPost = nil
		}
	case len(segments) == 3 && segments[1] == "contacts":
		c, ok := f.contacts[segments[2]]
		if !ok {
			fail(http.StatusNotFound, "ErrorItemNotFound")
			return
		}
		switch r.Method {
		case http.MethodGet:
			if f.deleteOnRead {
				f.deleteOnRead = false
				f.record(c.ParentFolderID, c.ID, true)
				delete(f.contacts, c.ID)
				fail(http.StatusNotFound, "ErrorItemNotFound")
				return
			}
			view := f.view(*c, expand)
			if f.thinReads {
				view.Properties = slices.DeleteFunc(view.Properties, func(p singleValueExtendedProperty) bool { return p.ID == vcardProperty })
			}
			reply(http.StatusOK, view)
		case http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"emailAddresses"`) {
				// A PATCH without the mapped fields sets only what it sends,
				// after its If-Match, if any.
				if match := r.Header.Get("If-Match"); match != "" && match != c.ETag {
					fail(http.StatusPreconditionFailed, "ErrorIrresolvableConflict")
					return
				}
				var marks contact
				if err := json.Unmarshal(raw, &marks); err != nil {
					fail(http.StatusBadRequest, "BadRequest")
					return
				}
				c.Properties = append(slices.Clone(c.Properties), marks.Properties...)
				c.DisplayName = marks.DisplayName
				c.ETag = `W/"` + strconv.Itoa(f.version+1) + `"`
				f.record(c.ParentFolderID, c.ID, false)
				reply(http.StatusOK, c)
				return
			}
			if f.rejectPatch {
				f.rejectPatch = false
				reply(http.StatusServiceUnavailable, nil)
				return
			}
			if f.patchStatus != 0 {
				fail(f.patchStatus, "ErrorInvalidProperty")
				return
			}
			if r.Header.Get("If-Match") != c.ETag {
				fail(http.StatusPreconditionFailed, "ErrorIrresolvableConflict")
				return
			}
			var next contact
			if err := json.Unmarshal(raw, &next); err != nil {
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
			next.ID, next.ParentFolderID, next.CreatedDateTime, next.Properties = c.ID, c.ParentFolderID, c.CreatedDateTime, properties
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
			if reject := f.rejectDelete; reject != nil {
				f.rejectDelete = nil
				w.Header().Set("Retry-After", "0")
				reply(reject(c), nil)
				return
			}
			f.deletes++
			f.record(c.ParentFolderID, c.ID, true)
			delete(f.contacts, c.ID)
			reply(http.StatusNoContent, nil)
		}
	default:
		reply(http.StatusNotFound, nil)
	}
}

// testQPS lifts Graph's request limit, which only paces the fake server.
const testQPS = 10_000

type fixture struct {
	fake    *fakeGraph
	service *carddav.Service
	store   *store.Store
	book    store.CardDAVAddressBook
	// tokenErr, when set, is returned in place of an access token.
	tokenErr error
	remote   *budgetRemote
}

// budgetRemote is a Remote whose per-sync byte limit a test can lower.
type budgetRemote struct {
	*Remote

	bytes int64
}

func (r *budgetRemote) Limits() (time.Duration, int64) { return operationTimeout, r.bytes }

// newFixture discovers the fake account and makes the "test" folder the
// subscribed write target. The default Contacts folder gets no role.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	fake := newFakeGraph()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	st := testutil.NewTestStore(t)
	f := &fixture{}
	remote := newRemote(server.URL+"/v1.0", func(context.Context) (string, error) {
		if f.tokenErr != nil {
			return "", f.tokenErr
		}
		return "token", nil
	}, testQPS)
	f.remote = &budgetRemote{Remote: remote, bytes: operationBytes}
	service := carddav.NewRemoteService(st, f.remote)
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
	f.sync(t)
	_, err = f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+bob)
	assert.ErrorIs(err, store.ErrCardDAVResourceNotFound, "deleting the folder's last contact removes it")
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
	assert.Equal("Alice Local", created[0].DisplayName)
	assert.Equal("test", created[0].ParentFolderID)
	assert.Equal([]emailAddress{{Name: "alice@example.test", Address: "alice@example.test"}}, created[0].EmailAddresses)
	publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Equal(f.book.CanonicalURL+"/uid/alice", publication.Href)
	assert.Empty(publication.PendingOperation)

	f.sync(t)
	f.sync(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	assert.Equal(1, f.fake.posts)
	assert.Equal(0, f.fake.patches, "a settled publication, with the URL that Graph drops, sends nothing")
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)

	require.NoError(f.service.UnpublishPerson(t.Context(), personID))
	assert.Empty(f.fake.published())
}

// An update of a contact edited or deleted in Outlook records a conflict
// right away and sends nothing.
func TestOutlookEditBeforeUpdateBecomesConflict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outlook func(f *fixture, id string)
		check   func(t *testing.T, f *fixture)
	}{
		{
			name:    "edited",
			outlook: func(f *fixture, id string) { f.fake.edit(id, func(c *contact) { c.JobTitle = "Edited in Outlook" }) },
			check: func(t *testing.T, f *fixture) {
				t.Helper()
				assert.Equal(t, "Edited in Outlook", f.fake.published()[0].JobTitle)
			},
		},
		{name: "deleted", outlook: func(f *fixture, id string) { f.fake.remove(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			personID := publishedAliceWithNewEmail(t, f)
			tc.outlook(f, f.fake.published()[0].ID)

			err := f.service.PublishPerson(t.Context(), personID)
			var conflict *carddav.ConflictError
			require.ErrorAs(t, err, &conflict)
			assert.Equal(t, 0, f.fake.patches)
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
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
	birthday := "1990-05-04T11:59:00Z"
	for _, tc := range []struct {
		name     string
		original contact
	}{
		{
			name: "every field",
			original: contact{
				DisplayName: "Dr. Ada M. Lovelace, Jr.", GivenName: "Ada", MiddleName: "M.", Surname: "Lovelace",
				Title: "Dr.", Generation: "Jr.", NickName: "Ada; the first",
				EmailAddresses: []emailAddress{{Address: "ada@example.test"}, {Address: "ada@work.test"}},
				BusinessPhones: []string{"+1 555 0100"}, HomePhones: []string{"+1 555 0101"}, MobilePhone: "+1 555 0102",
				CompanyName: "Analytical, Ltd", Department: "Engines", JobTitle: "Programmer",
				Birthday: &birthday, PersonalNotes: "line one\nline two", Categories: []string{"Team", "a, b"},
				HomeAddress:     physicalAddress{Street: "1 Home St", City: "London", PostalCode: "N1", CountryOrRegion: "UK"},
				BusinessAddress: physicalAddress{Street: "2 Work Rd", City: "London"},
				OtherAddress:    physicalAddress{City: "Paris"},
			},
		},
		{
			name: "department without company",
			original: contact{
				DisplayName: "Ann", Department: "Sales",
				EmailAddresses: []emailAddress{}, BusinessPhones: []string{}, HomePhones: []string{}, Categories: []string{},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			body, err := tc.original.toVCard("uid-1")
			require.NoError(err)
			assert.Contains(string(body), "UID:uid-1")

			mapped, err := contactFromVCard(body)
			require.NoError(err)
			assert.Equal(tc.original, mapped)
		})
	}
}

// mapVCard fills Graph's fields from a card and names the lines Graph has no
// room for.
func TestMapVCard(t *testing.T) {
	fields := func(c contact) contact {
		c.EmailAddresses = append([]emailAddress{}, c.EmailAddresses...)
		c.BusinessPhones = append([]string{}, c.BusinessPhones...)
		c.HomePhones = append([]string{}, c.HomePhones...)
		c.Categories = append([]string{}, c.Categories...)
		return c
	}
	for _, tc := range []struct {
		name        string
		card        string
		want        contact
		wantDropped []string
	}{
		{
			name: "unmapped properties",
			card: "UID:x\r\nFN:Ann\r\nX-CUSTOM:kept nowhere\r\nRELATED:urn:uuid:y\r\nTEL;TYPE=cell:1\r\nTEL;TYPE=cell:2\r\nBDAY:--0504\r\n",
			// A second mobile number becomes a business phone, and Graph needs a
			// birthday's year.
			want: contact{DisplayName: "Ann", MobilePhone: "1", BusinessPhones: []string{"2"}},
		},
		{
			name:        "first non-phonetic name",
			card:        "FN:Ann Lee\r\nN;PHONETIC=ipa:li;æn;;;\r\nN:Lee;Ann;;;\r\nN:Leigh;Anne;;;\r\nFN:Anne\r\nNICKNAME:A\r\nNICKNAME:B\r\n",
			want:        contact{DisplayName: "Ann Lee", Surname: "Lee", GivenName: "Ann", NickName: "A"},
			wantDropped: []string{"N", "FN", "NICKNAME"},
		},
		{
			name: "schemes in any case",
			card: "FN:Ann\r\nEMAIL:MAILTO:ann@example.test\r\nTEL;TYPE=work:TEL:%2B15550100\r\n",
			want: contact{DisplayName: "Ann", EmailAddresses: []emailAddress{{Address: "ann@example.test"}}, BusinessPhones: []string{"+15550100"}},
		},
		{
			name: "fax",
			card: "FN:Ann\r\nTEL;TYPE=work,fax:+15550009\r\n",
			want: contact{DisplayName: "Ann"},
		},
		{
			name:        "second title",
			card:        "UID:ann\r\nFN:Ann\r\nTITLE:Founder\r\nTITLE:Engineer\r\n",
			want:        contact{DisplayName: "Ann", JobTitle: "Founder"},
			wantDropped: []string{"TITLE"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dropped, err := mapVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" + tc.card + "END:VCARD\r\n"))
			require.NoError(t, err)
			placePhones(&c, nil)
			assert.Equal(t, fields(tc.want), c)
			assert.Equal(t, tc.wantDropped, dropped)
		})
	}
}

// An untyped number matches its Outlook field, by letters too for a vanity
// number, and any other untyped number becomes a business phone.
func TestUntypedNumberKeepsItsField(t *testing.T) {
	for _, tc := range []struct {
		name    string
		card    string
		outlook contact
		want    contact
	}{
		{
			name:    "plain",
			card:    "TEL:tel:+1 555 0100\r\nTEL:+1 555 0199\r\n",
			outlook: contact{HomePhones: []string{"+15550100"}},
			want:    contact{HomePhones: []string{"+1 555 0100"}, BusinessPhones: []string{"+1 555 0199"}},
		},
		{
			name:    "vanity",
			card:    "TEL:1-800-CONTACTS\r\nTEL:1-800-FLOWERS\r\n",
			outlook: contact{MobilePhone: "1-800-FLOWERS"},
			want:    contact{MobilePhone: "1-800-FLOWERS", HomePhones: []string{}, BusinessPhones: []string{"1-800-CONTACTS"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			c, err := contactFromVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Ann\r\n" + tc.card + "END:VCARD\r\n"))
			require.NoError(t, err)
			placePhones(&c, &tc.outlook)
			assert.Equal(tc.want.MobilePhone, c.MobilePhone)
			assert.Equal(tc.want.HomePhones, c.HomePhones)
			assert.Equal(tc.want.BusinessPhones, c.BusinessPhones)
		})
	}
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
	code, message := carddav.SyncFailure(err)
	assert.Equal("microsoft_authorization_required", code, "a refused token asks for sign-in")
	assert.Contains(message, "msgvault carddav authorize-microsoft")

	f.fake.failWith = 0
	f.tokenErr = carddav.ErrMicrosoftAuthorizationRequired
	_, err = f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, message = carddav.SyncFailure(err)
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

// An update whose answer fails is applied once: one Graph rejected before
// applying is sent again, and one Graph applied is recovered without a
// conflict.
func TestFailedUpdateIsAppliedOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fault   func(*fakeGraph)
		wantErr bool
	}{
		{name: "unapplied", fault: func(g *fakeGraph) { g.rejectPatch = true }},
		{name: "applied", fault: func(g *fakeGraph) { g.dropPatch = true }, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFixture(t)
			personID := publishedAliceWithNewEmail(t, f)
			tc.fault(f.fake)

			err := f.service.PublishPerson(t.Context(), personID)
			if tc.wantErr {
				require.Error(err)
				f.sync(t)
			} else {
				require.NoError(err)
			}

			assert.Equal(1, f.fake.patches)
			assert.Contains(f.fake.published()[0].EmailAddresses, emailAddress{Name: "alice@example.test", Address: "alice@example.test"})
			conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
			require.NoError(err)
			assert.Empty(conflicts)
			publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
			require.NoError(err)
			assert.Empty(publication.PendingOperation)
		})
	}
}

// A write that fails before Graph receives it stays planned and goes out
// later without a conflict.
func TestWriteFailingBeforeSendIsSentLater(t *testing.T) {
	signedOut := func(t *testing.T, f *fixture) context.Context {
		t.Helper()
		f.tokenErr = carddav.ErrMicrosoftAuthorizationRequired
		return t.Context()
	}
	failWith := func(status int) func(*testing.T, *fixture) context.Context {
		return func(t *testing.T, f *fixture) context.Context {
			t.Helper()
			f.fake.failWith = status
			return t.Context()
		}
	}
	for _, tc := range []struct {
		name    string
		op      string // create, update or unpublish
		fault   func(*testing.T, *fixture) context.Context
		wantErr error
		heal    func(*testing.T, *fixture)
	}{
		{name: "create while signed out", op: "create", fault: signedOut, wantErr: carddav.ErrMicrosoftAuthorizationRequired},
		{name: "update while signed out", op: "update", fault: signedOut, wantErr: carddav.ErrMicrosoftAuthorizationRequired},
		{name: "unpublish while signed out", op: "unpublish", fault: signedOut, wantErr: carddav.ErrMicrosoftAuthorizationRequired},
		{name: "503 lookup", op: "update", fault: failWith(http.StatusServiceUnavailable)},
		{name: "408 lookup", op: "unpublish", fault: failWith(http.StatusRequestTimeout)},
		{
			name: "cancel during lookup",
			op:   "update",
			fault: func(t *testing.T, f *fixture) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				f.fake.onLookup = func() {
					f.fake.onLookup = nil
					cancel()
				}
				return ctx
			},
		},
		{
			name: "retry gate set during lookup",
			op:   "update",
			fault: func(t *testing.T, f *fixture) context.Context {
				t.Helper()
				f.fake.onLookup = func() {
					f.fake.onLookup = nil
					assert.NoError(t, f.store.SetCardDAVRetryAfterContext(context.Background(), time.Now().Add(time.Hour), store.DefaultCardDAVAccountID))
				}
				return t.Context()
			},
			wantErr: store.ErrCardDAVRetryAfter,
			heal: func(t *testing.T, f *fixture) {
				t.Helper()
				require.Equal(t, 0, f.fake.patches, "the write after the lookup waits for the gate")
				_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM carddav_retry_gate WHERE account_id = ?`), store.DefaultCardDAVAccountID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			f := newFixture(t)
			var personID int64
			switch tc.op {
			case "create":
				personID = f.alice(t)
			case "update":
				personID = publishedAliceWithNewEmail(t, f)
			case "unpublish":
				personID = f.alice(t)
				require.NoError(f.service.PublishPerson(t.Context(), personID))
			}
			write := f.service.PublishPerson
			if tc.op == "unpublish" {
				write = f.service.UnpublishPerson
			}

			err := write(tc.fault(t, f), personID)
			if tc.wantErr != nil {
				require.ErrorIs(err, tc.wantErr)
			} else {
				require.Error(err)
			}
			f.tokenErr, f.fake.failWith = nil, 0
			if tc.heal != nil {
				tc.heal(t, f)
			}

			switch tc.op {
			case "create":
				f.sync(t)
				require.Len(f.fake.published(), 1)
			case "update":
				f.sync(t)
				require.Equal(1, f.fake.patches)
			case "unpublish":
				require.NoError(f.service.UnpublishPerson(t.Context(), personID))
				require.Empty(f.fake.published())
			}
			conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
			require.NoError(err)
			require.Empty(conflicts)
		})
	}
}

// After an Outlook edit, Outlook's fields win. A value still in the field it
// was written to keeps its saved line. A moved or changed value takes
// Outlook's form. Lines Graph never held come back only for URL, RELATED and
// X- properties, and for emails, phones and addresses past Graph's limits.
func TestOutlookEditKeepsSavedLines(t *testing.T) {
	card := func(lines string) string {
		return "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:ann\r\n" + lines + "END:VCARD\r\n"
	}
	for _, tc := range []struct {
		name  string
		saved string // the lines after UID
		edit  func(t *testing.T, c *contact)
		want  string // the whole card's lines after UID, when set
		check func(t *testing.T, c *contact, body string)
	}{
		{
			name: "moved and changed values",
			saved: "FN:Alice\r\nEMAIL;TYPE=work;PREF=1:alice@work.test\r\nEMAIL;TYPE=home:alice@old.test\r\n" +
				"EMAIL:alice@third.test\r\nEMAIL:alice@fourth.test\r\n" +
				"TEL;VALUE=uri;TYPE=voice,home:tel:+15550100\r\nTEL;TYPE=home:+15550200\r\nTEL;TYPE=home:+15550300\r\n" +
				"ADR;TYPE=home:PO 7;;1 Main;Springfield;;;\r\nTITLE:Engineer\r\nBDAY:--0315\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Len(t, c.EmailAddresses, 3, "Graph holds three emails")
				require.Len(t, c.HomePhones, 2, "Graph holds two home phones")
				c.JobTitle = "Edited in Outlook"
				c.EmailAddresses[1].Address = "alice@new.test"
				c.BusinessPhones, c.HomePhones = c.HomePhones[:1], c.HomePhones[1:]
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				document, err := vcard.Decode(strings.NewReader(body))
				require.NoError(t, err)
				card := document.Cards[0]
				emails := card.PropertiesNamed("EMAIL")
				require.Len(t, emails, 4)
				assert.Equal(t, []string{"work"}, typeValues(emails[0]))
				assert.Len(t, emails[0].ParametersNamed("PREF"), 1, "an unmoved line keeps PREF")
				assert.Equal(t, "alice@new.test", emails[1].RawValue)
				assert.Empty(t, emails[1].Parameters, "a changed address drops its saved type")
				assert.Equal(t, "alice@fourth.test", emails[3].RawValue, "Graph never held the fourth email")
				phones := card.PropertiesNamed("TEL")
				require.Len(t, phones, 3)
				assert.Equal(t, "+15550100", phones[0].RawValue, "a moved phone takes Outlook's form")
				assert.Equal(t, []string{"work"}, typeValues(phones[0]))
				assert.Equal(t, "+15550300", phones[2].RawValue, "Graph never held the third home phone")
				addresses := card.PropertiesNamed("ADR")
				require.Len(t, addresses, 1)
				assert.Equal(t, "PO 7;;1 Main;Springfield;;;", addresses[0].RawValue, "the PO box survives")
				assert.Contains(t, body, "TITLE:Edited in Outlook\r\n")
				assert.NotContains(t, body, "Engineer", "a held line that Outlook changed follows Outlook")
				assert.Contains(t, body, "BDAY:--0315\r\n", "a birthday without a year has no Graph field and stays as saved")
			},
		},
		{
			// A saved value that Outlook now holds is not added twice, and a
			// department with several units reads back as one ORG.
			name:  "saved value Outlook now holds",
			saved: "FN:Alice\r\nEMAIL:a1@example.test\r\nEMAIL:a2@example.test\r\nEMAIL:a3@example.test\r\nEMAIL:a4@example.test\r\nORG:Acme;R/D;East\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				assert.Equal(t, "R/D/East", c.Department)
				c.EmailAddresses[2].Address = "a4@example.test"
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 1, strings.Count(body, "a4@example.test"), body)
				document, err := vcard.Decode(strings.NewReader(body))
				require.NoError(t, err)
				card := document.Cards[0]
				assert.Len(t, card.PropertiesNamed("EMAIL"), 3)
				orgs := card.PropertiesNamed("ORG")
				require.Len(t, orgs, 1, body)
				assert.Equal(t, "Acme;R/D;East", orgs[0].RawValue)
			},
		},
		{
			// Graph holds two of three untyped phones. Moving a held phone to
			// another Outlook field must not change which phone the saved vCard
			// alone holds.
			name:  "phone move keeps the overflow phone",
			saved: "FN:Alice\r\nTEL:+15550101\r\nTEL:+15550102\r\nTEL:+15550103\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"+15550101", "+15550102"}, c.BusinessPhones)
				c.HomePhones, c.BusinessPhones = c.BusinessPhones[1:], c.BusinessPhones[:1]
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "+15550103", "the phone that Graph never held survives")
				assert.Equal(t, 3, strings.Count(body, "TEL"), body)
			},
		},
		{
			// A saved fax line is not taken for the work line with the same number.
			name:  "delete of the work number keeps the fax",
			saved: "FN:Ann\r\nTEL;TYPE=fax:+15550009\r\nTEL;TYPE=work:+15550009\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = nil },
			want:  "FN:Ann\r\nTEL;TYPE=fax:+15550009\r\n",
		},
		{
			name:  "delete of one of two equal numbers",
			saved: "FN:Ann\r\nTEL;TYPE=work:+15550001\r\nTEL;TYPE=home:+15550001\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = nil },
			want:  "FN:Ann\r\nTEL;TYPE=home:+15550001\r\n",
		},
		{
			name:  "delete of the work number keeps the overflow home",
			saved: "FN:Ann\r\nTEL;TYPE=home:+15550001\r\nTEL;TYPE=home:+15550002\r\nTEL;TYPE=home:+15550003\r\nTEL;TYPE=work:+15550003\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = nil },
			want:  "FN:Ann\r\nTEL;TYPE=home:+15550001\r\nTEL;TYPE=home:+15550002\r\nTEL;TYPE=home:+15550003\r\n",
		},
		{
			// The second cell line fills the business phone, so the cut work line
			// with the same number is the one that stays.
			name:  "delete of a business number that a cell line filled",
			saved: "FN:Ann\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=cell:+15550002\r\nTEL;TYPE=work:+15550003\r\nTEL;TYPE=work:+15550002\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"+15550002", "+15550003"}, c.BusinessPhones)
				c.BusinessPhones = c.BusinessPhones[1:]
			},
			want: "FN:Ann\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=work:+15550003\r\nTEL;TYPE=work:+15550002\r\n",
		},
		{
			// The labeled work line was never sent, so it must not stand in for
			// the business phone. The cell line would refill the cleared mobile.
			name:  "clear of the mobile keeps the overflow label on its line",
			saved: "FN:Ann\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=cell:+15550002\r\nTEL;TYPE=work:+15550003\r\nTEL;TYPE=work;X-LABEL=Office:+15550002\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"+15550002", "+15550003"}, c.BusinessPhones)
				c.MobilePhone = ""
			},
			want: "FN:Ann\r\nTEL;TYPE=work:+15550002\r\nTEL;TYPE=work:+15550003\r\nTEL;TYPE=work;X-LABEL=Office:+15550002\r\n",
		},
		{
			// The work lines push the untyped line out of the business phones.
			name:  "untyped phone cut behind work phones",
			saved: "FN:Ann\r\nTEL:+15550001\r\nTEL;TYPE=work:+15550002\r\nTEL;TYPE=work:+15550003\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"+15550002", "+15550003"}, c.BusinessPhones)
				c.NickName = "Annie"
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "TEL:+15550001\r\n")
				assert.Equal(t, 3, strings.Count(body, "TEL"), body)
			},
		},
		{
			// The write had no Outlook contact to match, so the untyped line
			// became a business phone rather than a third home.
			name:  "untyped copy the write made a business phone",
			saved: "FN:Ann\r\nTEL;TYPE=home:+15550001\r\nTEL;TYPE=home:+15550002\r\nTEL:+15550002\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = nil },
			want:  "FN:Ann\r\nTEL;TYPE=home:+15550001\r\nTEL;TYPE=home:+15550002\r\n",
		},
		{
			name:  "identical work phones past the limit",
			saved: "FN:Ann\r\nTEL;TYPE=work:+15550001\r\nTEL;TYPE=work:+15550001\r\nTEL;TYPE=work:+15550001\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Len(t, c.BusinessPhones, 2)
				c.BusinessPhones = c.BusinessPhones[:1]
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 2, strings.Count(body, "TEL"), "one held line and one overflow line: "+body)
			},
		},
		{
			name: "identical emails and addresses past their limits",
			saved: "FN:Ann\r\nEMAIL:a@example.test\r\nEMAIL:a@example.test\r\nEMAIL:a@example.test\r\nEMAIL:a@example.test\r\n" +
				"ADR;TYPE=home:;;1 Main;A;;;\r\nADR;TYPE=home:;;1 Main;A;;;\r\nADR;TYPE=home:;;1 Main;A;;;\r\n",
			edit: func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 4, strings.Count(body, "EMAIL"), body)
				assert.Equal(t, 3, strings.Count(body, "ADR"), body)
			},
		},
		{
			// Graph holds one title and one mobile phone, so the card reads back
			// to fill them the same way.
			name:  "one-value fields stay stable",
			saved: "FN:Ann\r\nTITLE:Founder\r\nTITLE:Engineer\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=cell:+15550002\r\n",
			edit:  func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				again, err := contactFromVCard([]byte(body))
				require.NoError(t, err)
				assert.Equal(t, "Founder", again.JobTitle)
				assert.Equal(t, "+15550001", again.MobilePhone)
				assert.Equal(t, []string{"+15550002"}, again.BusinessPhones)
			},
		},
		{
			name:  "clear of the title drops saved titles",
			saved: "FN:Ann\r\nTITLE:Founder\r\nTITLE:Engineer\r\n",
			edit:  func(_ *testing.T, c *contact) { c.JobTitle = "" },
			want:  "FN:Ann\r\n",
		},
		{
			// Clearing Outlook's first phone or address leaves the second one in
			// the field Outlook shows it in.
			name: "clear keeps other fields in place",
			saved: "FN:Ann\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=cell:+15550002\r\n" +
				"ADR;TYPE=home:;;1 Main;Springfield;;;\r\nADR;TYPE=home:;;2 Oak;Shelbyville;;;\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"+15550002"}, c.BusinessPhones)
				require.Equal(t, "2 Oak", c.OtherAddress.Street)
				c.MobilePhone, c.HomeAddress = "", physicalAddress{}
			},
			check: func(t *testing.T, c *contact, body string) {
				t.Helper()
				again, err := contactFromVCard([]byte(body))
				require.NoError(t, err)
				placePhones(&again, c)
				assert.Empty(t, again.MobilePhone, body)
				assert.Equal(t, []string{"+15550002"}, again.BusinessPhones)
				assert.True(t, again.HomeAddress.empty(), body)
				assert.Equal(t, "2 Oak", again.OtherAddress.Street)
			},
		},
		{
			// Saved lines Graph has no field for come back, and a field Outlook
			// cleared stays cleared.
			name: "properties Graph cannot hold",
			saved: "FN:Ann\r\nNOTE:old\r\nANNIVERSARY:20100601\r\nCATEGORIES:friends\r\nIMPP:xmpp:ann@example.test\r\n" +
				"PHOTO:data:image/png;base64,AAAA\r\nRELATED;VALUE=text;X-LABEL=friend:Bob\r\n",
			edit: func(_ *testing.T, c *contact) { c.PersonalNotes = "" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				for _, line := range []string{
					"ANNIVERSARY:20100601\r\n", "CATEGORIES:friends\r\n", "IMPP:xmpp:ann@example.test\r\n",
					"PHOTO:data:image/png;base64,AAAA\r\n", "RELATED;VALUE=text;X-LABEL=friend:Bob\r\n",
				} {
					assert.Contains(t, body, line)
				}
				assert.NotContains(t, body, "NOTE", "a note cleared in Outlook stays cleared")
			},
		},
		{
			// An address past Outlook's three comes back, a phone whose letters
			// Outlook changed follows Outlook, and Outlook's categories win.
			name: "overflow address and Outlook's phone and categories",
			saved: "FN:Ann\r\nADR;TYPE=home:;;1 Main;A;;;\r\nADR;TYPE=work:;;2 Oak;B;;;\r\nADR;TYPE=home:;;3 Elm;C;;;\r\nADR;TYPE=home:;;4 Pine;D;;;\r\n" +
				"TEL;TYPE=work:1-800-FLOWERS\r\nCATEGORIES:Team\r\n",
			edit: func(t *testing.T, c *contact) {
				t.Helper()
				require.Equal(t, []string{"Team"}, c.Categories)
				c.BusinessPhones = []string{"1-800-CONTACTS"}
				c.Categories = []string{"Friends"}
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 4, strings.Count(body, "ADR"), body)
				assert.Contains(t, body, "ADR;TYPE=home:;;4 Pine;D;;;\r\n")
				assert.Contains(t, body, "1-800-CONTACTS")
				assert.NotContains(t, body, "FLOWERS")
				assert.Contains(t, body, "CATEGORIES:Friends\r\n")
				assert.NotContains(t, body, "Team")
			},
		},
		{
			name:  "Outlook adds a birthday, so one BDAY",
			saved: "FN:Ann\r\nBDAY:--0315\r\n",
			edit: func(_ *testing.T, c *contact) {
				birthday := "1990-03-15T11:59:00Z"
				c.NickName, c.Birthday = "Annie", &birthday
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 1, strings.Count(body, "BDAY"), body)
				assert.Contains(t, body, "BDAY:19900315\r\n")
			},
		},
		{
			// A phonetic name saved with the same text as the name is never
			// restored in its place.
			name:  "name beside a phonetic copy",
			saved: "FN:Ann Lee\r\nN;PHONETIC=ipa:Lee;Ann;;;\r\nN:Lee;Ann;;;\r\n",
			edit:  func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				again, err := contactFromVCard([]byte(body))
				require.NoError(t, err)
				assert.Equal(t, "Lee", again.Surname, body)
				assert.Equal(t, "Ann", again.GivenName)
			},
		},
		{
			// A fax that shares its digits with a work number still comes back.
			name: "work fax and overflow home equal to the work number",
			saved: "FN:Ann\r\nN;PHONETIC=ipa:li;æn;;;\r\nN:Lee;Ann;;;\r\n" +
				"TEL;TYPE=work:+15550100\r\nTEL;TYPE=work,fax:+15550100\r\n" +
				"TEL;TYPE=home:+15550201\r\nTEL;TYPE=home:+15550202\r\nTEL;TYPE=home:+15550100\r\n",
			edit: func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "TEL;TYPE=work,fax:+15550100\r\n")
				assert.Contains(t, body, "TEL;TYPE=home:+15550100\r\n", "an overflow home number equal to the work number stays")
				assert.Contains(t, body, "N;PHONETIC=ipa:li;æn;;;\r\n", "a phonetic name stays as saved")
			},
		},
		{
			name:  "fax stays when Outlook adds the same number",
			saved: "FN:Bo\r\nTEL;TYPE=fax:+15550300\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = []string{"+15550300"} },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "TEL;TYPE=fax:+15550300\r\n")
			},
		},
		{
			name:  "overflow home stays when Outlook adds it as a business phone",
			saved: "FN:Cy\r\nTEL;TYPE=home:+15550001\r\nTEL;TYPE=home:+15550002\r\nTEL;TYPE=home:+15550003\r\n",
			edit:  func(_ *testing.T, c *contact) { c.BusinessPhones = []string{"+15550003"} },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "TEL;TYPE=home:+15550003\r\n")
			},
		},
		{
			// A second mobile number and a second home address keep their saved
			// labels while they stay in their Outlook fields.
			name: "labels of values that stayed",
			saved: "FN:Ann\r\nTEL;TYPE=cell:+15550100\r\nTEL;TYPE=cell:+15550200\r\n" +
				"ADR;TYPE=home:;;1 Main;A;;;\r\nADR;TYPE=home:;;2 Oak;B;;;\r\n",
			edit: func(_ *testing.T, c *contact) { c.JobTitle = "Edited in Outlook" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "TEL;TYPE=cell:+15550200\r\n")
				assert.Contains(t, body, "ADR;TYPE=home:;;2 Oak;B;;;\r\n")
			},
		},
		{
			// Each overflow occurrence of a number Outlook now holds once comes
			// back but one.
			name: "overflow occurrences of a number Outlook holds once",
			saved: "FN:Ann\r\nTEL;TYPE=home:+15550301\r\nTEL;TYPE=home:+15550302\r\n" +
				"TEL;TYPE=home;X-LABEL=Cabin:+15550303\r\nTEL;TYPE=home;X-LABEL=Office:+15550303\r\n",
			edit: func(_ *testing.T, c *contact) {
				c.JobTitle = "Edited in Outlook"
				c.HomePhones = []string{"+15550301", "+15550303"}
			},
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 1, strings.Count(body, "X-LABEL="), body)
			},
		},
		{
			// An other address equal to the cleared home one keeps its saved PO
			// box, though the saved home line comes first.
			name:  "clear keeps the equal other address",
			saved: "FN:Ann\r\nADR;TYPE=home:PO 1;;1 Main;A;;;\r\nADR:PO 2;Apt 9;1 Main;A;;;\r\n",
			edit:  func(_ *testing.T, c *contact) { c.HomeAddress = physicalAddress{} },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "ADR:PO 2;Apt 9;1 Main;A;;;\r\n")
			},
		},
		{
			// A repeated value that Outlook takes up from overflow is not added a
			// third time.
			name:  "repeated overflow value",
			saved: "FN:Ann\r\nEMAIL:a@example.test\r\nEMAIL:b@example.test\r\nEMAIL:c@example.test\r\nEMAIL:c@example.test\r\n",
			edit:  func(_ *testing.T, c *contact) { c.EmailAddresses[1].Address = "c@example.test" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Equal(t, 2, strings.Count(body, "c@example.test"), body)
			},
		},
		{
			// Graph has no field for a PO box, so the write sent nothing for it.
			name:  "PO-box-only address",
			saved: "FN:Ann\r\nADR;TYPE=home:PO 7;;;;;;\r\n",
			edit:  func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				assert.Contains(t, body, "ADR;TYPE=home:PO 7;;;;;;\r\n")
			},
		},
		{
			// The second cell line fills a business phone, so the cut work line
			// with the same number is the overflow line, added once.
			name:  "transmitted cell line beside an equal overflow work line",
			saved: "FN:Ann\r\nTEL;TYPE=cell:+15550001\r\nTEL;TYPE=cell:+15550002\r\nTEL;TYPE=work:+15550003\r\nTEL;TYPE=work:+15550002\r\n",
			edit:  func(_ *testing.T, c *contact) { c.NickName = "Annie" },
			check: func(t *testing.T, _ *contact, body string) {
				t.Helper()
				document, err := vcard.Decode(strings.NewReader(body))
				require.NoError(t, err)
				var phones []string
				for _, phone := range document.Cards[0].PropertiesNamed("TEL") {
					phones = append(phones, strings.Join(typeValues(phone), ",")+":"+phone.RawValue)
				}
				assert.ElementsMatch(t, []string{"cell:+15550001", "cell:+15550002", "work:+15550003", "work:+15550002"}, phones, body)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := []byte(card(tc.saved))
			c, err := contactFromVCard(saved)
			require.NoError(t, err)
			placePhones(&c, nil)
			c.Properties = c.saved(saved)
			tc.edit(t, &c)

			body, err := c.body("ann")
			require.NoError(t, err)
			if tc.want != "" {
				assert.Equal(t, card(tc.want), string(body))
			}
			if tc.check != nil {
				tc.check(t, &c, string(body))
			}
		})
	}
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
}

// overLimit applies Graph's list limits, measured on 2026-10-05: Graph
// refuses the whole write.
func overLimit(c contact) bool {
	return len(c.EmailAddresses) > 3 || len(c.BusinessPhones) > 2 || len(c.HomePhones) > 2
}

// Graph rejects an update with 400. The rejection is definitive: the pending
// update is cleared, and no conflict follows.
func TestRejectedUpdateClearsPendingIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	f.fake.patchStatus = http.StatusBadRequest
	err = f.service.PublishPerson(t.Context(), personID)
	status, ok := errors.AsType[*carddav.StatusError](err)
	require.True(ok, "%v", err)
	assert.Equal(http.StatusBadRequest, status.StatusCode)
	publication, err := f.store.GetCardDAVPublicationContext(t.Context(), personID)
	require.NoError(err)
	assert.Empty(publication.PendingOperation)

	f.fake.patchStatus = 0
	f.sync(t)
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
}

// An Outlook edit of another field leaves a plain phone as published, so the
// next publish does not add it a second time.
func TestOutlookEditKeepsPhoneAsPublished(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressPhone, OriginalValue: "+15550001",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)
	id := f.fake.published()[0].ID
	f.fake.edit(id, func(c *contact) { c.JobTitle = "Edited in Outlook" })

	f.sync(t)
	f.sync(t)

	require.Equal(0, f.fake.patches)
	saved := f.fake.contacts[id].property(vcardProperty)
	require.Equal(1, strings.Count(saved, "TEL"), saved)
}

// An Outlook copy of a published contact keeps its UID. The folder still
// syncs, the copy keeps the saved data of its original, such as a website,
// under its own UID, the original keeps the publication, and unpublishing
// leaves the copy.
func TestOutlookCopyOfPublishedContactSyncs(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	personID := publishedAliceWithWebsite(t, f)
	original := *f.fake.published()[0]
	f.fake.next = -1 // the copy's ID sorts before the original's
	copyID := f.fake.put("test", original)
	f.fake.next = 100
	f.fake.put("test", contact{DisplayName: "Carol Outlook"})

	f.sync(t)

	resources, err := f.store.ListCardDAVResourcesContext(t.Context(), f.book.ID)
	require.NoError(err)
	require.Len(resources, 3)
	copied, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+copyID)
	require.NoError(err)
	require.Contains(string(copied.RemoteBody), "UID:"+copyID+"\r\n")
	require.Contains(string(copied.RemoteBody), "https://alice.example.test")
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
	require.NoError(err)
	require.Equal(f.fake.contacts[original.ID].ETag, resource.RemoteETag, "the original keeps the publication")
	require.NoError(f.service.UnpublishPerson(t.Context(), personID))
	require.NotContains(f.fake.contacts, original.ID)
	require.Len(f.fake.published(), 1, "the copy stays")
}

// Deleting the published original in Outlook reads as a delete, and the copy,
// even after msgvault updates it, stays a contact of its own.
func TestOutlookDeleteOfOriginalLeavesTheCopy(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	require.NoError(f.service.PublishPerson(t.Context(), f.alice(t)))
	f.sync(t)
	original := *f.fake.published()[0]
	copyID := f.fake.put("test", original)
	f.sync(t)
	copyHref := f.book.CanonicalURL + "/id/" + copyID
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, copyHref)
	require.NoError(err)
	_, err = f.store.AddPersonContactPointContext(t.Context(), *resource.PersonID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://copy.example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(f.service.PublishPerson(t.Context(), *resource.PersonID))
	f.fake.remove(original.ID)

	f.sync(t)

	_, err = f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, copyHref)
	require.NoError(err, "the copy keeps its ID href")
	require.Equal(2, f.fake.posts, "the person is published again instead of taking over the copy")
}

// An Outlook contact that msgvault updated carries a saved vCard without a
// UID property. Its copy reads as a contact of its own.
func TestOutlookCopyOfUpdatedContactSyncs(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	id := f.fake.put("test", contact{DisplayName: "Bob Outlook"})
	f.sync(t)
	var personID int64
	require.NoError(f.store.DB().QueryRow(`SELECT id FROM persons WHERE display_name = 'Bob Outlook'`).Scan(&personID))
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://bob.example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	copyID := f.fake.put("test", *f.fake.contacts[id])

	f.sync(t)

	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+copyID)
	require.NoError(err)
	require.Contains(string(resource.RemoteBody), "UID:"+copyID+"\r\n")
}

// Graph requires every address field in an update, so a cleared field is sent
// empty.
func TestWriteMapping(t *testing.T) {
	body, err := json.Marshal(contact{HomeAddress: physicalAddress{City: "Paris"}})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"homeAddress":{"street":"","city":"Paris","state":"","postalCode":"","countryOrRegion":""}`)
}

// A write larger than Graph accepts is refused before it is sent.
// A NUL in an Outlook field, for example from imported data, is dropped
// instead of failing the whole folder.
func TestOutlookFieldWithNULSyncs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	f.fake.put("test", contact{DisplayName: "Erin Outlook", PersonalNotes: "before\x00after"})
	f.fake.put("test", contact{DisplayName: "Finn Outlook"})

	assert.Equal(2, f.sync(t).Created)

	resources, err := f.store.ListCardDAVResourcesContext(t.Context(), f.book.ID)
	require.NoError(err)
	erin := slices.IndexFunc(resources, func(resource store.CardDAVResource) bool {
		return strings.Contains(string(resource.RemoteBody), "FN:Erin Outlook")
	})
	require.GreaterOrEqual(erin, 0)
	assert.Contains(string(resources[erin].RemoteBody), "NOTE:beforeafter")
}

func TestOversizedWriteIsRefused(t *testing.T) {
	require := require.New(t)
	err := fitsGraph(contact{PersonalNotes: strings.Repeat("x", writeBytes)})
	status, ok := errors.AsType[*carddav.StatusError](err)
	require.True(ok, "%v", err)
	assert.Equal(t, http.StatusRequestEntityTooLarge, status.StatusCode)
	require.ErrorIs(err, carddav.ErrMicrosoftContactTooLarge)
	require.ErrorIs(statusError(&msgraph.StatusError{StatusCode: http.StatusRequestEntityTooLarge}), carddav.ErrMicrosoftContactTooLarge)
	require.NoError(fitsGraph(contact{PersonalNotes: "short"}))
}

// publishedAliceWithWebsite publishes alice with a website, which only the
// saved vCard holds, and syncs.
func publishedAliceWithWebsite(t *testing.T, f *fixture) int64 {
	t.Helper()
	personID := f.alice(t)
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressURL, OriginalValue: "https://alice.example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	require.NoError(t, f.service.PublishPerson(t.Context(), personID))
	f.sync(t)
	return personID
}

// A publish while listings leave out the saved vCard, including the UID
// lookup's, reads back as published, not as a conflict.
func TestPublishWithThinListingsIsNoConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	f.fake.thinPages = true
	personID := publishedAliceWithWebsite(t, f)
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)

	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
	assert.Equal(1, f.fake.patches)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "https://alice.example.test")
	assert.Contains(string(resource.RemoteBody), "alice@example.test")
}

// A listing that leaves out or cuts short a contact's saved vCard sends no
// update back and is not a conflict. The contact is read again. One that stays
// thin fails the sync instead of reading as an Outlook edit that drops the
// saved lines, and one deleted before the reread is published again.
func TestThinListingIsReread(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fault   func(*fakeGraph)
		wantErr error
		deleted bool
	}{
		{name: "thin", fault: func(g *fakeGraph) { g.thinPages = true }},
		{name: "truncated", fault: func(g *fakeGraph) { g.truncatedPages = true }},
		{name: "still thin on reread", fault: func(g *fakeGraph) { g.thinPages, g.thinReads = true, true }, wantErr: carddav.ErrIncompleteMultiget},
		{name: "deleted before reread", fault: func(g *fakeGraph) { g.thinPages, g.deleteOnRead = true, true }, deleted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newFixture(t)
			personID := publishedAliceWithWebsite(t, f)
			before, err := f.store.ListPersonContactPointsContext(t.Context(), personID, true)
			require.NoError(err)
			tc.fault(f.fake)

			_, err = f.service.Sync(t.Context(), carddav.SyncOptions{Full: true})

			if tc.wantErr != nil {
				require.ErrorIs(err, tc.wantErr)
				code, _ := carddav.SyncFailure(err)
				assert.Equal("sync_failed", code)
				after, err := f.store.ListPersonContactPointsContext(t.Context(), personID, true)
				require.NoError(err)
				assert.Equal(before, after)
			} else {
				require.NoError(err)
			}
			conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
			require.NoError(err)
			assert.Empty(conflicts)
			assert.Equal(0, f.fake.patches)
			if tc.deleted {
				assert.False(f.fake.deleteOnRead, "the sync reread the thin contact")
				restored := f.fake.published()
				require.Len(restored, 1)
				assert.Contains(restored[0].property(vcardProperty), "https://alice.example.test")
				return
			}
			resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
			require.NoError(err)
			assert.Contains(string(resource.RemoteBody), "https://alice.example.test")
		})
	}
}

// A contact over Outlook's 4 MB limit fails with a reason the user can act
// on, in the publish and in the next sync's history.
func TestOversizedPublishNamesTheLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := publishedAliceWithWebsite(t, f)
	_, err := f.store.AddPersonMediaContext(t.Context(), personID, store.PersonMediaInput{
		MediaKind: store.PersonMediaPhoto, MediaType: new("image/png"),
		Data: make([]byte, 3_100_000), OriginalValue: "data:image/png;base64,<elided>",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	_, err = f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, message := carddav.SyncFailure(err)
	assert.Equal("microsoft_contact_too_large", code)
	assert.Contains(message, "4 MB")

	err = f.service.PublishPerson(t.Context(), personID)
	require.ErrorIs(err, carddav.ErrMicrosoftContactTooLarge)
	tooLarge, ok := errors.AsType[*carddav.ContactTooLargeError](err)
	require.True(ok)
	assert.Equal(personID, tooLarge.PersonID)
	assert.Equal(0, f.fake.patches)

	f.fake.thinPages = true
	_, err = f.service.Sync(t.Context(), carddav.SyncOptions{Full: true})
	code, _ = carddav.SyncFailure(err)
	assert.Equal("microsoft_contact_too_large", code)
	conflicts, err := f.store.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/uid/alice")
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "https://alice.example.test")
}

// Graph pages a folder listing by offset. A contact deleted in Outlook during
// the listing shifts the next one out of that page, which must not read as a
// delete of the contact that is still there, even while delta reports the
// delete late. A contact that delta still names after it left the folder is
// absent.
// An Outlook insert during a listing shifts a contact already read onto the
// next offset page. The repeat must not reach the plan as a second upsert of
// the same href.
func TestListingThatRepeatsAContactSyncsItOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	f.fake.pageSize = 1
	ann := f.fake.put("test", contact{DisplayName: "Ann Outlook"})
	f.fake.put("test", contact{DisplayName: "Bob Outlook"})
	f.fake.put("test", contact{DisplayName: "Cy Outlook"})
	f.sync(t)
	f.fake.edit(ann, func(c *contact) { c.JobTitle = "Edited" })
	f.fake.onPage = func() {
		f.fake.onPage = nil
		// "a0" sorts before every other ID, so the third page repeats Bob.
		f.fake.contacts["a0"] = &contact{ID: "a0", ParentFolderID: "test", DisplayName: "Ada Outlook",
			CreatedDateTime: created(f.fake.version + 1), ETag: `W/"` + strconv.Itoa(f.fake.version+1) + `"`}
		f.fake.record("test", "a0", false)
	}

	result, err := f.service.Sync(t.Context(), carddav.SyncOptions{})

	require.NoError(err)
	assert.Equal(1, result.Updated)
	resource, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+ann)
	require.NoError(err)
	assert.Contains(string(resource.RemoteBody), "TITLE:Edited")
}

func TestListingThatShiftsDoesNotDropAContact(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	f.fake.pageSize = 1
	first := f.fake.put("test", contact{DisplayName: "Ann Outlook"})
	bob := f.fake.put("test", contact{DisplayName: "Bob Outlook"})
	cy := f.fake.put("test", contact{DisplayName: "Cy Outlook"})
	f.sync(t)
	f.fake.lag = true
	f.fake.edit(first, func(c *contact) { c.JobTitle = "Edited" })
	f.fake.onPage = func() {
		f.fake.onPage = nil
		f.fake.record("test", first, true)
		delete(f.fake.contacts, first)
	}

	f.sync(t)

	_, err := f.store.GetCardDAVResourceContext(t.Context(), f.book.ID, f.book.CanonicalURL+"/id/"+bob)
	require.NoError(err, "Bob is still in Outlook")

	f.fake.mu.Lock()
	f.fake.contacts[bob].ParentFolderID = "root"
	f.fake.record("test", bob, true)
	f.fake.record("root", bob, false)
	f.fake.mu.Unlock()
	f.fake.edit(cy, func(c *contact) { c.JobTitle = "Edited" })

	require.Equal(2, f.sync(t).Removed, "Ann is deleted and Bob moved out")
}

// A listing page over the size cap, from large saved vCards, is read again in
// smaller pages.
func TestOversizedListingPageIsReadInSmallerPages(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"Ann Outlook", "Bob Outlook", "Cy Outlook"} {
		f.fake.put("test", contact{DisplayName: name})
	}
	f.fake.maxTop = 12

	f.sync(t)

	resources, err := f.store.ListCardDAVResourcesContext(t.Context(), f.book.ID)
	require.NoError(t, err)
	require.Len(t, resources, 3)
	var bodies []string
	for _, resource := range resources {
		bodies = append(bodies, string(resource.RemoteBody))
	}
	for _, name := range []string{"Ann Outlook", "Bob Outlook", "Cy Outlook"} {
		assert.True(t, slices.ContainsFunc(bodies, func(body string) bool { return strings.Contains(body, "FN:"+name) }), name)
	}
}

// An Outlook edit before msgvault marks a new contact stands and goes to
// conflict review, and the contact still counts as the original.
func TestOutlookEditBeforeMarkStands(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	f.fake.afterPost = func(c *contact) { c.DisplayName = "Alice Outlook"; c.ETag = `W/"edited"` }
	_, conflict := errors.AsType[*carddav.ConflictError](f.service.PublishPerson(t.Context(), personID))
	require.True(conflict)
	published := f.fake.published()
	require.Len(published, 1)
	require.Equal("Alice Outlook", published[0].DisplayName)
	require.Empty(published[0].property(idProperty))
}

// A DELETE whose answer is lost is sent again only while the contact is
// unchanged, and a throttled DELETE is not repeated in place, so an Outlook
// edit made meanwhile is not deleted.
func TestFailedDeleteRepeatsOnlyWhileUnchanged(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)
	f.fake.rejectDelete = func(*contact) int { return http.StatusServiceUnavailable }
	require.NoError(f.service.UnpublishPerson(t.Context(), personID))
	assert.Empty(f.fake.published(), "an unchanged contact is deleted on the repeat")

	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		f := newFixture(t)
		personID := f.alice(t)
		require.NoError(f.service.PublishPerson(t.Context(), personID))
		f.sync(t)
		f.fake.rejectDelete = func(c *contact) int {
			c.JobTitle = "Edited in Outlook"
			c.ETag = `W/"edited"`
			return status
		}
		require.Error(f.service.UnpublishPerson(t.Context(), personID))
		published := f.fake.published()
		require.Len(published, 1, "the edited contact stays after a %d", status)
		assert.Equal("Edited in Outlook", published[0].JobTitle)
	}
}

// Every property that mapVCard maps, and every line card writes, is in
// graphProperties, so an Outlook edit decides it instead of the saved vCard.
func TestGraphPropertiesCoverTheMapping(t *testing.T) {
	require := require.New(t)
	snapshot, err := registry.Load()
	require.NoError(err)
	empty, _, err := mapVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nEND:VCARD\r\n"))
	require.NoError(err)
	values := []string{"x", "Lee;Ann;;;", "19900101", ";;1 Main;Springfield;;;", "+15550100", "ann@example.test"}
	for _, element := range snapshot.Properties {
		for _, value := range values {
			c, _, err := mapVCard([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" + element.Name + ":" + value + "\r\nEND:VCARD\r\n"))
			if err == nil && !reflect.DeepEqual(c, empty) {
				require.Contains(graphProperties, element.Name, "mapVCard maps %s", element.Name)
			}
		}
	}
	birthday := "1990-01-01T00:00:00Z"
	full := contact{
		DisplayName: "Ann", GivenName: "Ann", NickName: "A", JobTitle: "CTO", CompanyName: "Acme", PersonalNotes: "n",
		MobilePhone: "+15550001", BusinessPhones: []string{"+15550002"}, HomePhones: []string{"+15550003"},
		EmailAddresses: []emailAddress{{Address: "ann@example.test"}}, Birthday: &birthday,
		HomeAddress: physicalAddress{Street: "1 Main"}, BusinessAddress: physicalAddress{Street: "2 Oak"}, OtherAddress: physicalAddress{Street: "3 Elm"},
	}
	card, err := full.card("ann")
	require.NoError(err)
	for _, property := range card.Properties {
		require.Contains(graphProperties, strings.ToUpper(property.Name))
	}
}

// Outlook text fields may hold a line break that vCard can't carry raw.
func TestOutlookLineBreaksSurviveTheVCard(t *testing.T) {
	c := contact{DisplayName: "A\r\nB", JobTitle: "x\ry", HomeAddress: physicalAddress{City: "x\r\ny"}}
	body, err := c.toVCard("ann")
	require.NoError(t, err)
	assert.Contains(t, string(body), `TITLE:x\ny`)
}

// Outlook copies keep the original's UID and saved vCard, so a UID lookup
// that finds many large copies still fits under the page cap.
func TestUpdateFindsTheOriginalAmongLargeCopies(t *testing.T) {
	require := require.New(t)
	f := newFixture(t)
	personID := f.alice(t)
	require.NoError(f.service.PublishPerson(t.Context(), personID))
	f.sync(t)
	original := f.fake.published()[0]
	f.fake.put("test", *original)
	f.fake.maxTop = 1
	_, err := f.store.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	require.NoError(f.service.PublishPerson(t.Context(), personID))
	require.Contains(f.fake.contacts[original.ID].property(vcardProperty), "alice@example.test")
}

// The warning about lines Outlook can't hold is logged once for a contact,
// not again on each republish of the same lines.
func TestDroppedLinesAreLoggedOncePerContact(t *testing.T) {
	require := require.New(t)
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	fake := newFakeGraph()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	remote := newRemote(server.URL+"/v1.0", func(context.Context) (string, error) { return "token", nil }, testQPS)
	href, err := remote.CreateHref(server.URL+"/v1.0/me/contactFolders/test/contacts", "alice")
	require.NoError(err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:ann\r\nFN:Ann\r\nTITLE:Founder\r\nTITLE:Engineer\r\nEND:VCARD\r\n")

	require.NoError(remote.Put(t.Context(), href, body, "", true))
	for range 2 {
		published := fake.published()
		require.Len(published, 1)
		require.NoError(remote.Put(t.Context(), href, body, published[0].ETag, false))
	}

	require.Equal(1, strings.Count(logs.String(), "extra lines are not published"), logs.String())
}

// A listing that keeps failing counts toward the sync limit, so retries stop
// once Graph has sent more than the limit.
func TestRetriedListingCountsTowardTheSyncLimit(t *testing.T) {
	f := newFixture(t)
	f.remote.bytes = 128 << 10
	f.fake.failListings = 3

	_, err := f.service.Sync(t.Context(), carddav.SyncOptions{})
	code, _ := carddav.SyncFailure(err)
	assert.Equal(t, "safety_limit", code)
}
