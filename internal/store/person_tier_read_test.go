package store_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTiersEvaluatesOnlyRequestedPeople(t *testing.T) {
	p := newTierPeople(t)

	got, err := p.f.Store.PersonTiersContext(t.Context(),
		[]int64{p.frequent, p.regular, p.oneWay, p.old, p.latest, 999999}, tierOptions())

	require.NoError(t, err)
	want := map[int64]store.PersonTier{
		p.frequent: {PersonID: p.frequent, Tier: 1, Source: "observed"},
		p.regular:  {PersonID: p.regular, Tier: 2, Source: "observed"},
		p.oneWay:   {PersonID: p.oneWay, Tier: 3, Source: "observed"},
		p.old:      {PersonID: p.old, Tier: 3, Source: "observed"},
		p.latest:   {PersonID: p.latest, Tier: 1, Source: "observed"},
	}
	assert.Equal(t, want, got)
}

func TestPersonTiersEmptyInput(t *testing.T) {
	p := newTierPeople(t)
	for _, ids := range [][]int64{nil, {}} {
		got, err := p.f.Store.PersonTiersContext(t.Context(), ids, tierOptions())
		require.NoError(t, err)
		assert.NotNil(t, got)
		assert.Empty(t, got)
	}
}

func TestPersonTiersChunksLargeInput(t *testing.T) {
	p := newTierPeople(t)
	ids := makeTierReadPeople(t, p.f.Store, 1200)
	want := make(map[int64]store.PersonTier, len(ids))
	for _, id := range ids {
		want[id] = store.PersonTier{PersonID: id, Tier: 3, Source: "observed"}
	}

	got, err := p.f.Store.PersonTiersContext(t.Context(), ids, tierOptions())

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func makeTierReadPeople(t *testing.T, st *store.Store, count int) []int64 {
	t.Helper()
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	ids := make([]int64, 0, count)
	for i := range count {
		var id int64
		err := tx.QueryRowContext(t.Context(), st.Rebind(`INSERT INTO persons(vcard_uid) VALUES (?) RETURNING id`), fmt.Sprintf("tier-read-%d", i)).Scan(&id)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, tx.Commit())
	return ids
}
