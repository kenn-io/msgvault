package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityGrantSelectionReadsPersonWithoutMutation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, a, _ := newUnlinkGuardStore(t)
	person, _, err := st.CreatePersonFromParticipant(a)
	requirements.NoError(err)
	revision, err := st.IdentityRevision()
	requirements.NoError(err)
	var beforeProjection int64
	requirements.NoError(st.db.QueryRow(`SELECT vcard_projection_revision FROM persons WHERE id=?`, person.ID).Scan(&beforeProjection))
	selection, err := st.IdentityGrantSelectionContext(t.Context(), []int64{person.ID}, nil)
	requirements.NoError(err)
	requirements.Len(selection.Persons, 1)
	assertions.Equal(person.ID, selection.Persons[0].ID)
	assertions.Equal(person.VCardUID, selection.Persons[0].UID)
	assertions.Equal(person.Revision, selection.Persons[0].Revision)
	assertions.Equal(beforeProjection, selection.Persons[0].ProjectionRevision)
	current, err := st.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(revision, current)
	var actualRevision, actualProjection int64
	requirements.NoError(st.db.QueryRow(`SELECT revision,vcard_projection_revision FROM persons WHERE id=?`, person.ID).Scan(&actualRevision, &actualProjection))
	assertions.Equal(person.Revision, actualRevision)
	assertions.Equal(beforeProjection, actualProjection)
}

func TestIdentityGrantSelectionTracksAddressBookOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, _, _ := newUnlinkGuardStore(t)
	input := CardDAVDiscoveryInput{BaseURL: "https://contacts.example.test/dav", Username: "synthetic-first", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/synthetic/", DisplayName: "Synthetic Book"}}}
	account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	requirements.NoError(err)
	requirements.Len(books, 1)
	first, err := st.IdentityGrantSelectionContext(t.Context(), nil, []int64{books[0].ID})
	requirements.NoError(err)
	requirements.Len(first.AddressBooks, 1)
	assertions.Equal(account.ID, first.AddressBooks[0].AccountID)
	assertions.Equal(books[0].ID, first.AddressBooks[0].BookID)
	assertions.Equal(books[0].CanonicalURL, first.AddressBooks[0].CanonicalURL)
	assertions.Len(first.AddressBooks[0].OwnershipFingerprint, 64)
	input.Username = "synthetic-changed-owner"
	_, books, err = st.ReplaceCardDAVDiscoveryContext(t.Context(), input)
	requirements.NoError(err)
	requirements.Len(books, 1)
	changed, err := st.IdentityGrantSelectionContext(t.Context(), nil, []int64{books[0].ID})
	requirements.NoError(err)
	requirements.Len(changed.AddressBooks, 1)
	assertions.NotEqual(first.AddressBooks[0].OwnershipFingerprint, changed.AddressBooks[0].OwnershipFingerprint)
}

func TestIdentityGrantSelectionRejectsInvalidMissingAndCancelledScope(t *testing.T) {
	st, _, _ := newUnlinkGuardStore(t)
	for _, ids := range [][]int64{nil, {0}, {9_007_199_254_740_992}, {1, 1}, make([]int64, 101), {8_000_000}} {
		_, err := st.IdentityGrantSelectionContext(t.Context(), ids, nil)
		require.Error(t, err)
	}
	_, err := st.IdentityGrantSelectionContext(t.Context(), nil, []int64{8_000_000})
	require.ErrorIs(t, err, ErrCardDAVAddressBookNotFound)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = st.IdentityGrantSelectionContext(ctx, []int64{1}, nil)
	require.ErrorIs(t, err, context.Canceled)
}
