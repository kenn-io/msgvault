package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCollection_CRUD(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("mbox", "backup@example.com")
	require.NoError(err, "GetOrCreateSource")

	// Create
	coll, err := st.CreateCollection("work", "Work emails", []int64{f.Source.ID, src2.ID})
	require.NoError(err, "CreateCollection")
	require.Equal("work", coll.Name)

	// List — includes the auto-created "All" collection plus "work"
	list, err := st.ListCollections()
	require.NoError(err, "ListCollections")
	require.Len(list, 2)
	// Find "work" in the list and verify its sources.
	var workColl *store.CollectionWithSources
	for _, c := range list {
		if c.Name == "work" {
			workColl = c
			break
		}
	}
	require.NotNil(workColl, "expected 'work' collection in list")
	require.Len(workColl.SourceIDs, 2)

	// Get by name
	got, err := st.GetCollectionByName("work")
	require.NoError(err, "GetCollectionByName")
	require.Equal("work", got.Name)

	// Not found
	_, err = st.GetCollectionByName("nonexistent")
	require.ErrorIs(err, store.ErrCollectionNotFound)

	// Duplicate name rejected
	_, err = st.CreateCollection("work", "", []int64{f.Source.ID})
	require.ErrorIs(err, store.ErrCollectionExists, "expected error for duplicate name")

	// Remove source
	err = st.RemoveSourcesFromCollection("work", []int64{src2.ID})
	require.NoError(err, "RemoveSourcesFromCollection")
	got, err = st.GetCollectionByName("work")
	require.NoError(err, "GetCollectionByName after remove")
	require.Len(got.SourceIDs, 1)

	// Add source back
	err = st.AddSourcesToCollection("work", []int64{src2.ID})
	require.NoError(err, "AddSourcesToCollection")
	got, err = st.GetCollectionByName("work")
	require.NoError(err, "GetCollectionByName after add")
	require.Len(got.SourceIDs, 2)

	// Delete
	err = st.DeleteCollection("work")
	require.NoError(err, "DeleteCollection")
	_, err = st.GetCollectionByName("work")
	require.ErrorIs(err, store.ErrCollectionNotFound)
}

func TestCollection_DefaultAll(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	err := st.EnsureDefaultCollection()
	require.NoError(err, "EnsureDefaultCollection")

	coll, err := st.GetCollectionByName("All")
	require.NoError(err, "GetCollectionByName All")
	require.Equal("All", coll.Name)
	// Should include the fixture's source
	require.NotEmpty(coll.SourceIDs, "All collection should have at least 1 source")

	// Idempotent
	err = st.EnsureDefaultCollection()
	require.NoError(err, "EnsureDefaultCollection (2nd call)")
}

func TestCollection_Validation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	t.Run("empty name rejected", func(t *testing.T) {
		_, err := st.CreateCollection("", "", []int64{f.Source.ID})
		require.Error(t, err, "expected error for empty name")
	})

	t.Run("zero sources rejected", func(t *testing.T) {
		_, err := st.CreateCollection("empty", "", nil)
		require.Error(t, err, "expected error for zero sources")
	})

	t.Run("nonexistent source rejected", func(t *testing.T) {
		_, err := st.CreateCollection("bad", "", []int64{99999})
		require.Error(t, err, "expected error for nonexistent source")
	})

	t.Run("delete nonexistent returns error", func(t *testing.T) {
		err := st.DeleteCollection("nonexistent")
		require.ErrorIs(t, err, store.ErrCollectionNotFound)
	})
}

func TestCollection_Idempotent(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	_, err := st.CreateCollection("idem", "", []int64{f.Source.ID})
	require.NoError(t, err, "CreateCollection")

	t.Run("add same source twice is no-op", func(t *testing.T) {
		err := st.AddSourcesToCollection("idem", []int64{f.Source.ID})
		require.NoError(t, err, "AddSourcesToCollection (dupe)")
		coll, err := st.GetCollectionByName("idem")
		require.NoError(t, err, "GetCollectionByName")
		require.Len(t, coll.SourceIDs, 1)
	})

	t.Run("remove absent source is no-op", func(t *testing.T) {
		src2, err := st.GetOrCreateSource("mbox", "other@example.com")
		require.NoError(t, err, "GetOrCreateSource")
		err = st.RemoveSourcesFromCollection("idem", []int64{src2.ID})
		require.NoError(t, err, "RemoveSourcesFromCollection (absent)")
	})
}

// TestCollection_DefaultAllIsImmutable verifies that explicit
// add/remove/delete on the auto-managed "All" collection are rejected
// with ErrCollectionImmutable. Otherwise the next EnsureDefaultCollection
// call would silently revert the change, surprising the user.
func TestCollection_DefaultAllIsImmutable(t *testing.T) {
	require :=
		require.
			New(t)

	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	require.NoError(
		st.EnsureDefaultCollection(), "EnsureDefaultCollection")

	require.ErrorIs(st.AddSourcesToCollection("All", []int64{f.Source.ID}), store.ErrCollectionImmutable,
		"AddSourcesToCollection(All)")
	require.ErrorIs(st.RemoveSourcesFromCollection("All", []int64{f.Source.ID}), store.ErrCollectionImmutable,
		"RemoveSourcesFromCollection(All)")
	assert.ErrorIs(st.DeleteCollection("All"), store.ErrCollectionImmutable,
		"DeleteCollection(All)")
}

func TestCollection_DefaultAllIncremental(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	require.NoError(st.EnsureDefaultCollection(), "EnsureDefaultCollection 1")
	coll, err := st.GetCollectionByName("All")
	require.NoError(err, "GetCollectionByName")
	initialCount := len(coll.SourceIDs)

	_, err = st.GetOrCreateSource("mbox", "new@example.com")
	require.NoError(err, "GetOrCreateSource")

	require.NoError(st.EnsureDefaultCollection(), "EnsureDefaultCollection 2")
	coll, err = st.GetCollectionByName("All")
	require.NoError(err, "GetCollectionByName after add")
	assert.Len(t, coll.SourceIDs, initialCount+1)
}
