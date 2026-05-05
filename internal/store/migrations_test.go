package store_test

import (
	"testing"

	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

func TestIsMigrationApplied_NotApplied(t *testing.T) {
	f := storetest.New(t)

	applied, err := f.Store.IsMigrationApplied("test_migration")
	testutil.MustNoErr(t, err, "IsMigrationApplied")
	if applied {
		t.Error("migration should not be applied yet")
	}
}

func TestMarkAndCheckMigrationApplied(t *testing.T) {
	f := storetest.New(t)

	testutil.MustNoErr(t, f.Store.MarkMigrationApplied("test_migration"), "MarkMigrationApplied")

	applied, err := f.Store.IsMigrationApplied("test_migration")
	testutil.MustNoErr(t, err, "IsMigrationApplied")
	if !applied {
		t.Error("migration should be marked as applied")
	}
}

func TestMarkMigrationApplied_Idempotent(t *testing.T) {
	f := storetest.New(t)

	for range 2 {
		if err := f.Store.MarkMigrationApplied("test_migration"); err != nil {
			t.Fatalf("MarkMigrationApplied: %v", err)
		}
	}

	applied, err := f.Store.IsMigrationApplied("test_migration")
	testutil.MustNoErr(t, err, "IsMigrationApplied")
	if !applied {
		t.Error("migration should be marked as applied after two calls")
	}
}
