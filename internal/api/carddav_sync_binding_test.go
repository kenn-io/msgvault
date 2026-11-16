package api

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type bindingSyncFixture struct {
	cardDAVListFixture
	err error
}

func (f bindingSyncFixture) Sync(context.Context, carddav.SyncOptions) (carddav.SyncResult, error) {
	return carddav.SyncResult{}, f.err
}

func TestCardDAVSyncReportsBindingConflict(t *testing.T) {
	st := testutil.NewTestStore(t)
	allowed := true
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(t, err)
	_, err = st.EnsureParticipant("shared@example.test", "Shared", "example.test")
	require.NoError(t, err)
	var cards []store.CardDAVRemoteResource
	for _, uid := range []string{"first", "second"} {
		cards = append(cards, store.CardDAVRemoteResource{
			Href: books[0].CanonicalURL + uid, RemoteUID: uid, RemoteETag: `"one"`, SemanticHash: uid,
			RemoteBody:  []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:" + uid + "\r\nFN:Person\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n"),
			DisplayName: "Person", Emails: []string{"shared@example.test"},
		})
	}
	_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{
		AddressBookID: books[0].ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: books[0].SyncRevision, Upserts: cards,
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"completed", nil},
		{"partial", errors.New("later book failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			controller := &CardDAVController{cfg: &config.Config{CardDAV: config.CardDAVConfig{Enabled: true}}, store: st, service: bindingSyncFixture{err: tc.err}}
			result, err := controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
			assert.ErrorIs(t, err, tc.err)
			require.NotNil(t, result.BindingConflicts)
			assert.Equal(t, 1, *result.BindingConflicts)
		})
	}
}

type bindingSyncRemote struct {
	cards map[string]store.CardDAVRemoteResource
}

func (r *bindingSyncRemote) Discover(context.Context, string) (carddav.Discovery, error) {
	return carddav.Discovery{}, nil
}
func (r *bindingSyncRemote) Pull(_ context.Context, _ store.CardDAVAddressBook, _ string, _ *carddav.Budget) (store.CardDAVSyncPlan, error) {
	plan := store.CardDAVSyncPlan{ReplaceAll: true}
	for _, resource := range r.cards {
		plan.Upserts = append(plan.Upserts, resource)
	}
	return plan, nil
}
func (r *bindingSyncRemote) Get(_ context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	resource, ok := r.cards[href]
	return resource, !ok, nil
}
func (r *bindingSyncRemote) Put(_ context.Context, href string, body []byte, _ string, _ bool) error {
	hash, err := carddav.SemanticHash(body)
	if err != nil {
		return err
	}
	resource := store.CardDAVRemoteResource{Href: href, RemoteBody: body, RemoteETag: `"published"`, SemanticHash: hash, RemoteUID: "second", DisplayName: "Second"}
	if bytes.Contains(body, []byte("shared@example.test")) {
		resource.Emails = []string{"shared@example.test"}
	}
	r.cards[href] = resource
	return nil
}
func (r *bindingSyncRemote) Delete(_ context.Context, href, _ string) error {
	delete(r.cards, href)
	return nil
}
func (r *bindingSyncRemote) CreateHref(collectionURL, uid string) (string, error) {
	return collectionURL + uid, nil
}
func (r *bindingSyncRemote) Limits() (time.Duration, int64) { return time.Minute, 10 << 20 }

func TestCardDAVSyncReportsPublicationBindingConflict(t *testing.T) {
	st := testutil.NewTestStore(t)
	allowed := true
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
		BaseURL: "https://memory.test", Username: "alice", PrincipalURL: "https://memory.test/principal/", HomeURL: "https://memory.test/books/",
		Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://memory.test/books/personal/", DisplayName: "Personal", CanCreate: &allowed}},
	})
	require.NoError(t, err)
	_, err = st.EnsureParticipant("shared@example.test", "Shared", "example.test")
	require.NoError(t, err)
	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:first\r\nFN:First\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n")
	hash, err := carddav.SemanticHash(body)
	require.NoError(t, err)
	href := books[0].CanonicalURL + "first"
	remote := &bindingSyncRemote{cards: map[string]store.CardDAVRemoteResource{
		href: {Href: href, RemoteBody: body, RemoteETag: `"first"`, SemanticHash: hash, RemoteUID: "first", DisplayName: "First", Emails: []string{"shared@example.test"}},
	}}
	service := carddav.NewRemoteService(st, remote)
	controller := &CardDAVController{cfg: &config.Config{CardDAV: config.CardDAVConfig{Enabled: true}}, store: st, service: service}
	_, err = controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
	require.NoError(t, err)
	var personID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?) RETURNING id`), "second", "Second").Scan(&personID))
	require.NoError(t, service.PublishPerson(t.Context(), personID))
	_, err = st.AddPersonContactPointContext(t.Context(), personID, store.PersonContactPointInput{AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.test", Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	before, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(t, err)
	require.Zero(t, before.Conflicts)
	result, err := controller.Sync(t.Context(), CardDAVSyncRequest{Connection: "default"})
	require.NoError(t, err)
	require.NotNil(t, result.BindingConflicts)
	assert.Equal(t, 1, *result.BindingConflicts)
}
