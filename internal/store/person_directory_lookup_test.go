package store_test

import (
	"fmt"
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

func TestDirectoryPeopleAddressLookupExcludesLanguageMetadata(t *testing.T) {
	st := testutil.NewTestStore(t)
	language := createDirectoryPerson(t, st, "Language Example", "language@example.test", "friend", "inactive", "Example Co")
	address := createDirectoryPerson(t, st, "Address Example", "en-gb@example.test", "friend", "inactive", "Example Co")
	_, err := st.AddPersonContactPointContext(t.Context(), language.ID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressLanguage, OriginalValue: "en-GB",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	require.NoError(t, err)
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: "en-GB"})
	require.NoError(t, err)
	assert.Equal(t, []int64{address.ID}, directoryPersonIDs(page.People))
}

func TestDirectoryPeopleAddressLookupUnicodeParity(t *testing.T) {
	st := testutil.NewTestStore(t)
	person := createDirectoryPerson(t, st, "Unicode Example", "unicode@example.test", "friend", "inactive", "Example Co")
	for _, tc := range []struct {
		name, upper, lower string
	}{
		{name: "U+0130", upper: "İ", lower: "i"},
		{name: "U+1E9E", upper: "ẞ", lower: "ß"},
		{name: "U+1C89", upper: "\u1c89", lower: "\u1c8a"},
		{name: "U+A7CE", upper: "\ua7ce", lower: "\ua7cf"},
		{name: "U+10D50", upper: "\U00010d50", lower: "\U00010d70"},
		{name: "U+16EA0", upper: "\U00016ea0", lower: "\U00016ebb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An unscoped username retains its case, so the normalized column
			// cannot hide a backend-dependent case conversion.
			_, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressUsername, OriginalValue: "lookup-" + tc.upper + "-handle",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(t, err)
			for _, fragment := range []string{tc.lower, tc.upper} {
				page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: "lookup-" + fragment + "-handle"})
				require.NoError(t, err)
				assert.Equal(t, []int64{person.ID}, directoryPersonIDs(page.People))
			}
		})
	}
}

func TestDirectoryPeopleAddressLookupRefillsSparsePagesInEveryOrder(t *testing.T) {
	st := testutil.NewTestStore(t)
	var matches []int64
	for index := range 70 {
		person := createDirectoryPerson(t, st, fmt.Sprintf("Person %03d", index), fmt.Sprintf("person-%03d@example.test", index), "friend", "active", "Example Co")
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_contact_state SET last_contact_at = ? WHERE person_id = ?`),
			time.Date(2026, 1, 1, 0, index, 0, 0, time.UTC), person.ID)
		require.NoError(t, err)
		if index == 34 || index == 69 {
			_, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
				AddressKind: store.ContactAddressUsername, OriginalValue: "shared-handle",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
			require.NoError(t, err)
			matches = append(matches, person.ID)
		}
	}
	for _, order := range []string{store.DirectoryPeopleSortName, store.DirectoryPeopleSortLastContactAsc, store.DirectoryPeopleSortLastContactDesc} {
		t.Run(order, func(t *testing.T) {
			want := []int64{matches[0], matches[1]}
			if order == store.DirectoryPeopleSortLastContactDesc {
				want = []int64{matches[1], matches[0]}
			}
			query := store.DirectoryPeopleQuery{Query: "person", Address: "shared-handle", Sort: order, Limit: 1}
			first, err := st.DirectoryPeoplePageContext(t.Context(), query)
			require.NoError(t, err)
			assert.Equal(t, want[:1], directoryPersonIDs(first.People))
			require.NotEmpty(t, first.NextCursor)
			query.Cursor = first.NextCursor
			second, err := st.DirectoryPeoplePageContext(t.Context(), query)
			require.NoError(t, err)
			assert.Equal(t, want[1:], directoryPersonIDs(second.People))
			assert.Empty(t, second.NextCursor)
		})
	}
	page, err := st.DirectoryPeoplePageContext(t.Context(), store.DirectoryPeopleQuery{Address: "absent-handle", Limit: 1})
	require.NoError(t, err)
	assert.Empty(t, page.People)
	assert.Empty(t, page.NextCursor)
}

func TestDirectoryPeopleAddressLookupRejectsRevokedAnchor(t *testing.T) {
	st := testutil.NewTestStore(t)
	alpha := createDirectoryPerson(t, st, "Alpha Example", "alpha@example.test", "friend", "inactive", "Example Co")
	beta := createDirectoryPerson(t, st, "Beta Example", "beta@example.test", "friend", "inactive", "Example Co")
	var anchorPointID int64
	for _, person := range []*store.Person{alpha, beta} {
		point, err := st.AddPersonContactPointContext(t.Context(), person.ID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressUsername, OriginalValue: "anchor-handle",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(t, err)
		if person.ID == alpha.ID {
			anchorPointID = point.Envelope.ID
		}
	}
	query := store.DirectoryPeopleQuery{Address: "anchor-handle", Limit: 1}
	page, err := st.DirectoryPeoplePageContext(t.Context(), query)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	require.NoError(t, st.SupersedePersonContactPointContext(t.Context(), alpha.ID, anchorPointID, nil))
	query.Cursor = page.NextCursor
	_, err = st.DirectoryPeoplePageContext(t.Context(), query)
	assert.ErrorIs(t, err, store.ErrInvalidDirectoryCursor)
}

func TestDirectoryPeopleAddressLookupRejectsReboundAnchor(t *testing.T) {
	f := newPersonSplitFixture(t)
	createDirectoryPerson(t, f.store, "Zulu Example", "split-absorbed-control@example.test", "friend", "inactive", "Example Co")
	query := store.DirectoryPeopleQuery{Address: "split-absorbed", Limit: 1}
	page, err := f.store.DirectoryPeoplePageContext(t.Context(), query)
	require.NoError(t, err)
	require.NotEmpty(t, page.NextCursor)
	require.Equal(t, []int64{f.survivor.ID}, directoryPersonIDs(page.People))
	current, err := f.store.GetPersonContext(t.Context(), f.survivor.ID)
	require.NoError(t, err)
	split, err := f.store.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{
		SourcePersonID: current.ID, MergeID: f.merge.Merge.ID, ParticipantIDs: f.absorbedParticipants,
		ExpectedSourceRevision: current.Revision, IdempotencyKey: "directory-anchor-split", Actor: "test",
	})
	require.NoError(t, err)
	assert.Equal(t, current.DisplayName, split.SourcePerson.DisplayName)
	query.Cursor = page.NextCursor
	_, err = f.store.DirectoryPeoplePageContext(t.Context(), query)
	assert.ErrorIs(t, err, store.ErrInvalidDirectoryCursor)
}
