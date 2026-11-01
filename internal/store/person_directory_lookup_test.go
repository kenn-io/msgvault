package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDirectoryPeopleAddressLookupUsesCurrentSavedPeople(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participant, err := st.EnsureParticipant("primary@example.test", "Observed Alpha", "example.test")
	require.NoError(t, err)
	alpha, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	require.NoError(t, err)
	alpha, err = st.UpdatePersonDisplayNameContext(ctx, alpha.ID, alpha.Revision, new("Curated Alpha"))
	require.NoError(t, err)
	beta := createDirectoryPerson(t, st, "Curated Beta", "beta@example.test", "friend", "inactive", "Example Co")
	_, err = st.EnsureParticipant("unpromoted@example.test", "Observed Only", "example.test")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE participants SET phone_number = ? WHERE id = ?`), "+12025550123", participant)
	require.NoError(t, err)
	for _, value := range []string{"alias@example.test", "ÉCOLE@example.test", `literal%_\value`, "handle  with spaces"} {
		_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO participant_identifiers
			(participant_id, identifier_type, identifier_value, is_primary) VALUES (?, 'matrix', ?, FALSE)`), participant, value)
		require.NoError(t, err)
	}
	for _, value := range []string{"current@example.test", "retired@example.test", "ended@example.test"} {
		envelope := store.ValueEnvelopeInput{Source: store.ProvenanceUser}
		if value == "ended@example.test" {
			envelope.ActiveUntil = new(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		}
		point, err := st.AddPersonContactPointContext(ctx, alpha.ID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressEmail, OriginalValue: value,
			Envelope: envelope,
		})
		require.NoError(t, err)
		if value == "retired@example.test" {
			require.NoError(t, st.SupersedePersonContactPointContext(ctx, alpha.ID, point.Envelope.ID, nil))
		}
	}

	for _, tc := range []struct {
		address string
		query   string
		want    []int64
	}{
		{address: " PRIMARY@EXAMPLE.TEST ", want: []int64{alpha.ID}},
		{address: "+12025550123", want: []int64{alpha.ID}},
		{address: "alias@example.test", want: []int64{alpha.ID}},
		{address: "école@example.test", want: []int64{alpha.ID}},
		{address: `%_\`, want: []int64{alpha.ID}},
		{address: "handle  with", want: []int64{alpha.ID}},
		{address: "handle with", want: []int64{}},
		{address: "current@example.test", want: []int64{alpha.ID}},
		{address: "retired@example.test", want: []int64{}},
		{address: "ended@example.test", want: []int64{}},
		{address: "unpromoted@example.test", want: []int64{}},
		{address: "missing", want: []int64{}},
		{address: "primary", query: "curated", want: []int64{alpha.ID}},
		{address: "primary", query: "beta", want: []int64{}},
		{address: "primary", query: "observed", want: []int64{}},
		{address: "example.test", want: []int64{alpha.ID, beta.ID}},
	} {
		t.Run(tc.address+"/"+tc.query, func(t *testing.T) {
			page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Address: tc.address, Query: tc.query})
			require.NoError(t, err)
			assert.Equal(t, tc.want, directoryPersonIDs(page.People))
		})
	}

	// Matching an archived address must not replace a cleared curated name.
	alpha, err = st.GetPersonContext(ctx, alpha.ID)
	require.NoError(t, err)
	_, err = st.UpdatePersonDisplayNameContext(ctx, alpha.ID, alpha.Revision, nil)
	require.NoError(t, err)
	page, err := st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Address: "primary"})
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	assert.Nil(t, page.People[0].DisplayName)

	// Live membership, rather than a cached copy of addresses, controls lookup.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE person_participants SET person_id = ? WHERE participant_id = ?`), beta.ID, participant)
	require.NoError(t, err)
	page, err = st.DirectoryPeoplePageContext(ctx, store.DirectoryPeopleQuery{Address: "alias@example.test"})
	require.NoError(t, err)
	assert.Equal(t, []int64{beta.ID}, directoryPersonIDs(page.People))
}

func TestDirectoryPeopleAddressLookupBindsCursor(t *testing.T) {
	st := testutil.NewTestStore(t)
	alpha := createDirectoryPerson(t, st, "Alpha Example", "alpha@example.test", "friend", "inactive", "Example Co")
	beta := createDirectoryPerson(t, st, "Beta Example", "beta@example.test", "friend", "inactive", "Example Co")
	first, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: "example.test", Limit: 1})
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor)
	assert.Equal(t, []int64{alpha.ID}, directoryPersonIDs(first.People))
	second, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: " EXAMPLE.TEST ", Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, []int64{beta.ID}, directoryPersonIDs(second.People))
	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: "alpha", Limit: 1, Cursor: first.NextCursor})
	assert.ErrorIs(t, err, store.ErrInvalidDirectoryCursor)
	_, err = st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Limit: 1, Cursor: first.NextCursor})
	assert.ErrorIs(t, err, store.ErrInvalidDirectoryCursor)
}
