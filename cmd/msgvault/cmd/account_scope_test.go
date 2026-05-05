package cmd

import (
	"strings"
	"testing"

	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
)

// setupScopeFixture creates a store with one source and one collection for
// resolver tests. Returns the store, source identifier, and collection name.
func setupScopeFixture(t *testing.T) (
	f *storetest.Fixture,
	accountID string,
	collectionName string,
) {
	t.Helper()
	f = storetest.New(t)
	// f.Source is "test@example.com" / gmail, created by storetest.New.
	accountID = f.Source.Identifier // "test@example.com"

	collectionName = "inbox-collection"
	_, err := f.Store.CreateCollection(collectionName, "", []int64{f.Source.ID})
	testutil.MustNoErr(t, err, "CreateCollection")

	return f, accountID, collectionName
}

func TestResolveAccountFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scope.IsEmpty() {
		t.Errorf("expected empty scope, got source=%v collection=%v",
			scope.Source, scope.Collection)
	}
}

func TestResolveCollectionFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !scope.IsEmpty() {
		t.Errorf("expected empty scope, got source=%v collection=%v",
			scope.Source, scope.Collection)
	}
}

func TestResolveAccountFlag_ValidAccount(t *testing.T) {
	f, accountID, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, accountID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.Source == nil {
		t.Fatal("expected Source to be populated")
	}
	if scope.Source.Identifier != accountID {
		t.Errorf("source identifier = %q, want %q", scope.Source.Identifier, accountID)
	}
	if scope.Collection != nil {
		t.Error("expected Collection to be nil")
	}
}

func TestResolveCollectionFlag_ValidCollection(t *testing.T) {
	f, _, collectionName := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, collectionName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.Collection == nil {
		t.Fatal("expected Collection to be populated")
	}
	if scope.Collection.Name != collectionName {
		t.Errorf("collection name = %q, want %q", scope.Collection.Name, collectionName)
	}
	if scope.Source != nil {
		t.Error("expected Source to be nil")
	}
}

func TestResolveAccountFlag_RejectsCollectionName(t *testing.T) {
	f, _, collectionName := setupScopeFixture(t)

	_, err := ResolveAccountFlag(f.Store, collectionName)
	if err == nil {
		t.Fatal("expected error for collection name passed as --account")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is a collection") {
		t.Errorf("error should contain 'is a collection': %q", msg)
	}
	if !strings.Contains(msg, "--collection") {
		t.Errorf("error should contain '--collection': %q", msg)
	}
}

func TestResolveCollectionFlag_RejectsAccountIdentifier(t *testing.T) {
	f, accountID, _ := setupScopeFixture(t)

	_, err := ResolveCollectionFlag(f.Store, accountID)
	if err == nil {
		t.Fatal("expected error for account identifier passed as --collection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "is an account") {
		t.Errorf("error should contain 'is an account': %q", msg)
	}
	if !strings.Contains(msg, "--account") {
		t.Errorf("error should contain '--account': %q", msg)
	}
}

// TestResolveAccountFlag_BothExist verifies the tie-break rule: when a name
// exists as both an account and a collection, --account resolves the account.
func TestResolveAccountFlag_BothExist(t *testing.T) {
	f := storetest.New(t)

	// Create a second source whose identifier matches our collection name.
	sharedName := "shared-name"
	src2, err := f.Store.GetOrCreateSource("mbox", sharedName)
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	_, err = f.Store.CreateCollection(sharedName, "", []int64{f.Source.ID})
	testutil.MustNoErr(t, err, "CreateCollection")

	scope, err := ResolveAccountFlag(f.Store, sharedName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.Source == nil {
		t.Fatal("expected Source to be populated")
	}
	if scope.Source.ID != src2.ID {
		t.Errorf("source ID = %d, want %d", scope.Source.ID, src2.ID)
	}
	if scope.Collection != nil {
		t.Error("expected Collection to be nil when resolving as --account")
	}
}

// TestResolveCollectionFlag_BothExist verifies that when a name exists as both
// an account and a collection, --collection resolves the collection.
func TestResolveCollectionFlag_BothExist(t *testing.T) {
	f := storetest.New(t)

	sharedName := "shared-name"
	_, err := f.Store.GetOrCreateSource("mbox", sharedName)
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	_, err = f.Store.CreateCollection(sharedName, "", []int64{f.Source.ID})
	testutil.MustNoErr(t, err, "CreateCollection")

	scope, err := ResolveCollectionFlag(f.Store, sharedName)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope.Collection == nil {
		t.Fatal("expected Collection to be populated")
	}
	if scope.Collection.Name != sharedName {
		t.Errorf("collection name = %q, want %q", scope.Collection.Name, sharedName)
	}
	if scope.Source != nil {
		t.Error("expected Source to be nil when resolving as --collection")
	}
}
