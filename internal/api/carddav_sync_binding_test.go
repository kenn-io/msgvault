package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

type bindingSyncRemote struct {
	cards    map[string][]byte
	versions map[string]string
	n        int
}

func (r *bindingSyncRemote) set(href string, body []byte) {
	r.n++
	r.cards[href] = body
	r.versions[href] = fmt.Sprintf("\"%d\"", r.n)
}
func (r *bindingSyncRemote) Discover(context.Context, string) (carddav.Discovery, error) {
	return carddav.Discovery{}, nil
}
func (r *bindingSyncRemote) resource(href string) (store.CardDAVRemoteResource, error) {
	body := r.cards[href]
	envelope, err := vcard.ParseResourceEnvelope(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	hash, err := carddav.SemanticHash(body)
	if err != nil {
		return store.CardDAVRemoteResource{}, err
	}
	resource := store.CardDAVRemoteResource{Href: href, RemoteBody: body, RemoteETag: r.versions[href], SemanticHash: hash}
	for _, occurrence := range envelope.PropertyTree {
		p := occurrence.Property
		switch strings.ToUpper(p.Name) {
		case "UID":
			resource.RemoteUID = p.RawValue
		case "FN":
			resource.DisplayName, err = vcard.PropertyValue(envelope.RenderMetadata.StoredVersion, p)
		case "EMAIL":
			var value string
			value, _, err = vcard.ContactPointValue(envelope.RenderMetadata.StoredVersion, p)
			resource.Emails = append(resource.Emails, value)
		}
		if err != nil {
			return store.CardDAVRemoteResource{}, err
		}
	}
	return resource, nil
}
func (r *bindingSyncRemote) Pull(_ context.Context, _ store.CardDAVAddressBook, _ string, _ *carddav.Budget) (store.CardDAVSyncPlan, error) {
	plan := store.CardDAVSyncPlan{ReplaceAll: true, NextSyncToken: fmt.Sprint(r.n)}
	for href := range r.cards {
		resource, err := r.resource(href)
		if err != nil {
			return plan, err
		}
		plan.Upserts = append(plan.Upserts, resource)
	}
	return plan, nil
}
func (r *bindingSyncRemote) Get(_ context.Context, href string) (store.CardDAVRemoteResource, bool, error) {
	if _, ok := r.cards[href]; !ok {
		return store.CardDAVRemoteResource{Href: href}, true, nil
	}
	resource, err := r.resource(href)
	return resource, false, err
}
func (r *bindingSyncRemote) Put(_ context.Context, href string, body []byte, _ string, _ bool) error {
	r.set(href, body)
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
	participant, err := st.EnsureParticipant("shared@example.test", "Shared", "example.test")
	require.NoError(t, err)
	remote := &bindingSyncRemote{cards: map[string][]byte{}, versions: map[string]string{}}
	remote.set(books[0].CanonicalURL+"first", []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:first\r\nFN:First\r\nEMAIL:shared@example.test\r\nEND:VCARD\r\n"))
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
	revision, err := st.IdentityRevision()
	require.NoError(t, err)
	boundBefore, err := st.PersonForParticipantsContext(t.Context(), []int64{participant})
	require.NoError(t, err)
	preview, err := st.ReconcileCardDAVParticipantBindingsContext(t.Context(), store.CardDAVParticipantBindingOptions{DryRun: true})
	require.NoError(t, err)
	require.Equal(t, 1, preview.Conflicts)
	bound, err := st.PersonForParticipantsContext(t.Context(), []int64{participant})
	require.NoError(t, err)
	assert.Nil(t, bound)
	assert.Equal(t, boundBefore, bound)
	afterRevision, err := st.IdentityRevision()
	require.NoError(t, err)
	assert.Equal(t, revision, afterRevision)
	require.NotNil(t, result.BindingConflicts)
	assert.Equal(t, 1, *result.BindingConflicts)
}
