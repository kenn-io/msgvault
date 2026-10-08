package carddav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// memoryRemote is a Remote that applies writes to an in-memory book, so the
// service can be tested without a DAV server.
type memoryRemote struct {
	cards   map[string][]byte
	etags   map[string]string
	version int
	puts    int
	// writeErr, when set, fails every write before it is sent.
	writeErr error
	// failed counts the writes that writeErr failed.
	failed int
}

func newMemoryRemote() *memoryRemote {
	return &memoryRemote{cards: map[string][]byte{}, etags: map[string]string{}}
}

func (m *memoryRemote) set(href string, body []byte) {
	m.version++
	m.cards[href] = body
	m.etags[href] = fmt.Sprintf(`"%d"`, m.version)
}

func (m *memoryRemote) Discover(context.Context, string) (Discovery, error) { return Discovery{}, nil }

func (m *memoryRemote) Pull(
	_ context.Context, _ store.CardDAVAddressBook, _ string, _ *Budget,
) (store.CardDAVSyncPlan, error) {
	plan := store.CardDAVSyncPlan{ReplaceAll: true, NextSyncToken: strconv.Itoa(m.version)}
	for href, body := range m.cards {
		resource, err := parseRemoteResource(href, m.etags[href], body)
		if err != nil {
			return store.CardDAVSyncPlan{}, err
		}
		plan.Upserts = append(plan.Upserts, resource)
	}
	return plan, nil
}

func (m *memoryRemote) Get(_ context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	body, ok := m.cards[href]
	if !ok {
		return store.CardDAVRemoteResource{Href: href}, true, nil
	}
	resource, err := parseRemoteResource(href, m.etags[href], body)
	return resource, false, err
}

func (m *memoryRemote) Put(_ context.Context, href string, body []byte, etag string, create bool) error {
	if m.writeErr != nil {
		m.failed++
		return m.writeErr
	}
	_, exists := m.cards[href]
	if create && exists || !create && m.etags[href] != etag {
		return &StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	m.puts++
	m.set(href, body)
	return nil
}

func (m *memoryRemote) Delete(_ context.Context, href, etag string) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	if m.etags[href] != etag {
		return &StatusError{StatusCode: http.StatusPreconditionFailed}
	}
	delete(m.cards, href)
	delete(m.etags, href)
	return nil
}

func (m *memoryRemote) CreateHref(collectionURL, uid string) (string, error) {
	return collectionURL + uid, nil
}

func (m *memoryRemote) Limits() (time.Duration, int64) { return time.Minute, defaultOperationBytes }

func TestServiceSyncsAndPublishesThroughAnyRemote(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(err)
	book := books[0]
	remote := newMemoryRemote()
	remote.set(book.CanonicalURL+"bob", []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:bob\r\nFN:Bob Remote\r\nEMAIL:bob@example.test\r\nEND:VCARD\r\n"))
	service := NewRemoteService(st, remote)

	result, err := service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(1, result.Created)

	var personID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "alice", "Alice Local").Scan(&personID))
	require.NoError(service.PublishPerson(t.Context(), personID))
	assert.Equal(1, remote.puts)
	assert.Contains(string(remote.cards[book.CanonicalURL+"alice"]), "FN:Alice Local")

	result, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	assert.Equal(0, result.Created+result.Updated+result.Removed)
	assert.Equal(1, remote.puts, "a settled publication sends nothing on the next sync")
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.Empty(conflicts)
}

func TestPullStopsFallbackAfterRetryAfter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(server.Close)
	service, st, _ := newPullService(t, server, true)

	_, err := service.Sync(t.Context(), SyncOptions{})
	require.Error(err)
	assert.Equal(int32(1), requests.Load(), "the snapshot fallback must not run after a pause")
	gate, err := st.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	assert.NotNil(gate)
}

func TestCanonicalAbsentReadSavesRetryAfter(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	service, st, book := newPullService(t, server, false)

	_, absent, err := service.fetchCanonical(t.Context(), book.CanonicalURL+"gone.vcf")
	require.NoError(err)
	require.True(absent)
	gate, err := st.GetCardDAVRetryAfterContext(t.Context(), store.DefaultCardDAVAccountID)
	require.NoError(err)
	require.NotNil(gate)
}

// An update and an unpublish that fail on a missing Google sign-in were never
// sent, so they go out after sign-in without a conflict.
func TestWritesBlockedOnSignInResumeWithoutConflict(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(err)
	href := books[0].CanonicalURL + "alice"
	remote := newMemoryRemote()
	service := NewRemoteService(st, remote)
	var personID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
		VALUES (?, ?) RETURNING id`), "alice", "Alice Local").Scan(&personID))
	require.NoError(service.PublishPerson(t.Context(), personID))
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "alice@example.test",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(err)

	remote.writeErr = ErrGoogleAuthorizationRequired
	require.ErrorIs(service.PublishPerson(t.Context(), personID), ErrGoogleAuthorizationRequired)
	remote.writeErr = nil
	_, err = service.Sync(t.Context(), SyncOptions{})
	require.NoError(err)
	require.Contains(string(remote.cards[href]), "alice@example.test")

	remote.writeErr = ErrGoogleAuthorizationRequired
	require.ErrorIs(service.UnpublishPerson(t.Context(), personID), ErrGoogleAuthorizationRequired)
	remote.writeErr = nil
	require.NoError(service.UnpublishPerson(t.Context(), personID))
	require.NotContains(remote.cards, href)
	conflicts, err := st.ListCardDAVConflictsContext(t.Context(), true, store.DefaultCardDAVAccountID)
	require.NoError(err)
	require.Empty(conflicts)
}

// A token failure stops the publication sweep at the first write, instead of
// repeating for every pending person.
func TestTokenFailureStopsThePublicationSweep(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	allowed := true
	_, _, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(err)
	remote := newMemoryRemote()
	service := NewRemoteService(st, remote)
	remote.writeErr = errors.Join(ErrMicrosoftTokenUnavailable, ErrWriteNotSent)
	for _, uid := range []string{"alice", "bob"} {
		var personID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`INSERT INTO persons (vcard_uid, display_name)
			VALUES (?, ?) RETURNING id`), uid, uid).Scan(&personID))
		require.Error(service.PublishPerson(t.Context(), personID))
	}
	remote.failed = 0

	_, err = service.Sync(t.Context(), SyncOptions{})
	require.ErrorIs(err, ErrMicrosoftTokenUnavailable)
	require.Equal(1, remote.failed)
}
