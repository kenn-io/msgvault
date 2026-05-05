package store_test

import (
	"testing"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

func TestCollection_CRUD(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	src2, err := st.GetOrCreateSource("mbox", "backup@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	// Create
	coll, err := st.CreateCollection("work", "Work emails", []int64{f.Source.ID, src2.ID})
	testutil.MustNoErr(t, err, "CreateCollection")
	if coll.Name != "work" {
		t.Fatalf("name = %q, want work", coll.Name)
	}

	// List — includes the auto-created "All" collection plus "work"
	list, err := st.ListCollections()
	testutil.MustNoErr(t, err, "ListCollections")
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
	}
	// Find "work" in the list and verify its sources.
	var workColl *store.CollectionWithSources
	for _, c := range list {
		if c.Name == "work" {
			workColl = c
			break
		}
	}
	if workColl == nil {
		t.Fatal("expected 'work' collection in list")
	}
	if len(workColl.SourceIDs) != 2 {
		t.Fatalf("sourceIDs = %d, want 2", len(workColl.SourceIDs))
	}

	// Get by name
	got, err := st.GetCollectionByName("work")
	testutil.MustNoErr(t, err, "GetCollectionByName")
	if got.Name != "work" {
		t.Fatalf("got name = %q", got.Name)
	}

	// Not found
	_, err = st.GetCollectionByName("nonexistent")
	if err != store.ErrCollectionNotFound {
		t.Fatalf("expected ErrCollectionNotFound, got %v", err)
	}

	// Duplicate name rejected
	_, err = st.CreateCollection("work", "", []int64{f.Source.ID})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}

	// Remove source
	err = st.RemoveSourcesFromCollection("work", []int64{src2.ID})
	testutil.MustNoErr(t, err, "RemoveSourcesFromCollection")
	got, err = st.GetCollectionByName("work")
	testutil.MustNoErr(t, err, "GetCollectionByName after remove")
	if len(got.SourceIDs) != 1 {
		t.Fatalf("sourceIDs after remove = %d, want 1", len(got.SourceIDs))
	}

	// Add source back
	err = st.AddSourcesToCollection("work", []int64{src2.ID})
	testutil.MustNoErr(t, err, "AddSourcesToCollection")
	got, err = st.GetCollectionByName("work")
	testutil.MustNoErr(t, err, "GetCollectionByName after add")
	if len(got.SourceIDs) != 2 {
		t.Fatalf("sourceIDs after add = %d, want 2", len(got.SourceIDs))
	}

	// Delete
	err = st.DeleteCollection("work")
	testutil.MustNoErr(t, err, "DeleteCollection")
	_, err = st.GetCollectionByName("work")
	if err != store.ErrCollectionNotFound {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}

func TestCollection_DefaultAll(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	err := st.EnsureDefaultCollection()
	testutil.MustNoErr(t, err, "EnsureDefaultCollection")

	coll, err := st.GetCollectionByName("All")
	testutil.MustNoErr(t, err, "GetCollectionByName All")
	if coll.Name != "All" {
		t.Fatalf("name = %q, want All", coll.Name)
	}
	// Should include the fixture's source
	if len(coll.SourceIDs) < 1 {
		t.Fatalf("All collection should have at least 1 source")
	}

	// Idempotent
	err = st.EnsureDefaultCollection()
	testutil.MustNoErr(t, err, "EnsureDefaultCollection (2nd call)")
}

func TestCollection_Validation(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	t.Run("empty name rejected", func(t *testing.T) {
		_, err := st.CreateCollection("", "", []int64{f.Source.ID})
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("zero sources rejected", func(t *testing.T) {
		_, err := st.CreateCollection("empty", "", nil)
		if err == nil {
			t.Fatal("expected error for zero sources")
		}
	})

	t.Run("nonexistent source rejected", func(t *testing.T) {
		_, err := st.CreateCollection("bad", "", []int64{99999})
		if err == nil {
			t.Fatal("expected error for nonexistent source")
		}
	})

	t.Run("delete nonexistent returns error", func(t *testing.T) {
		err := st.DeleteCollection("nonexistent")
		if err != store.ErrCollectionNotFound {
			t.Fatalf("expected ErrCollectionNotFound, got %v", err)
		}
	})
}

func TestCollection_Idempotent(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	_, err := st.CreateCollection("idem", "", []int64{f.Source.ID})
	testutil.MustNoErr(t, err, "CreateCollection")

	t.Run("add same source twice is no-op", func(t *testing.T) {
		err := st.AddSourcesToCollection("idem", []int64{f.Source.ID})
		testutil.MustNoErr(t, err, "AddSourcesToCollection (dupe)")
		coll, err := st.GetCollectionByName("idem")
		testutil.MustNoErr(t, err, "GetCollectionByName")
		if len(coll.SourceIDs) != 1 {
			t.Fatalf("sourceIDs = %d, want 1", len(coll.SourceIDs))
		}
	})

	t.Run("remove absent source is no-op", func(t *testing.T) {
		src2, err := st.GetOrCreateSource("mbox", "other@example.com")
		testutil.MustNoErr(t, err, "GetOrCreateSource")
		err = st.RemoveSourcesFromCollection("idem", []int64{src2.ID})
		testutil.MustNoErr(t, err, "RemoveSourcesFromCollection (absent)")
	})
}

// TestCollection_DefaultAllIsImmutable verifies that explicit
// add/remove/delete on the auto-managed "All" collection are rejected
// with ErrCollectionImmutable. Otherwise the next EnsureDefaultCollection
// call would silently revert the change, surprising the user.
func TestCollection_DefaultAllIsImmutable(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t, st.EnsureDefaultCollection(), "EnsureDefaultCollection")

	if err := st.AddSourcesToCollection("All", []int64{f.Source.ID}); err != store.ErrCollectionImmutable {
		t.Errorf("AddSourcesToCollection(All) = %v, want ErrCollectionImmutable", err)
	}
	if err := st.RemoveSourcesFromCollection("All", []int64{f.Source.ID}); err != store.ErrCollectionImmutable {
		t.Errorf("RemoveSourcesFromCollection(All) = %v, want ErrCollectionImmutable", err)
	}
	if err := st.DeleteCollection("All"); err != store.ErrCollectionImmutable {
		t.Errorf("DeleteCollection(All) = %v, want ErrCollectionImmutable", err)
	}
}

func TestCollection_DefaultAllIncremental(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	testutil.MustNoErr(t, st.EnsureDefaultCollection(), "EnsureDefaultCollection 1")
	coll, err := st.GetCollectionByName("All")
	testutil.MustNoErr(t, err, "GetCollectionByName")
	initialCount := len(coll.SourceIDs)

	_, err = st.GetOrCreateSource("mbox", "new@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	testutil.MustNoErr(t, st.EnsureDefaultCollection(), "EnsureDefaultCollection 2")
	coll, err = st.GetCollectionByName("All")
	testutil.MustNoErr(t, err, "GetCollectionByName after add")
	if len(coll.SourceIDs) != initialCount+1 {
		t.Errorf("sourceIDs = %d, want %d", len(coll.SourceIDs), initialCount+1)
	}
}
