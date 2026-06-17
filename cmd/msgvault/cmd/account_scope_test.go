package cmd

import (
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/storetest"
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
	requirepkg.NoError(t, err, "CreateCollection")

	return f, accountID, collectionName
}

func TestResolveAccountFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, "")
	requirepkg.NoError(t, err)
	assertpkg.True(t, scope.IsEmpty(), "expected empty scope, got source=%v collection=%v",
		scope.Source, scope.Collection)
}

func TestResolveCollectionFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, "")
	requirepkg.NoError(t, err)
	assertpkg.True(t, scope.IsEmpty(), "expected empty scope, got source=%v collection=%v",
		scope.Source, scope.Collection)
}

func TestResolveAccountFlag_ValidAccount(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f, accountID, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, accountID)
	require.NoError(err)
	require.NotNil(scope.Source, "expected Source to be populated")
	assert.Equal(accountID, scope.Source.Identifier, "source identifier")
	assert.Nil(scope.Collection, "expected Collection to be nil")
}

func TestResolveCollectionFlag_ValidCollection(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f, _, collectionName := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, collectionName)
	require.NoError(err)
	require.NotNil(scope.Collection, "expected Collection to be populated")
	assert.Equal(collectionName, scope.Collection.Name, "collection name")
	assert.Nil(scope.Source, "expected Source to be nil")
}

func TestResolveAccountFlag_RejectsCollectionName(t *testing.T) {
	f, _, collectionName := setupScopeFixture(t)

	_, err := ResolveAccountFlag(f.Store, collectionName)
	requirepkg.Error(t, err, "expected error for collection name passed as --account")
	requirepkg.ErrorContains(t, err, "is a collection")
	assertpkg.ErrorContains(t, err, "--collection")
}

func TestResolveCollectionFlag_RejectsAccountIdentifier(t *testing.T) {
	f, accountID, _ := setupScopeFixture(t)

	_, err := ResolveCollectionFlag(f.Store, accountID)
	requirepkg.Error(t, err, "expected error for account identifier passed as --collection")
	requirepkg.ErrorContains(t, err, "is an account")
	assertpkg.ErrorContains(t, err, "--account")
}

// TestResolveAccountFlag_BothExist verifies the tie-break rule: when a name
// exists as both an account and a collection, --account resolves the account.
func TestResolveAccountFlag_BothExist(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Create a second source whose identifier matches our collection name.
	sharedName := "shared-name"
	src2, err := f.Store.GetOrCreateSource("mbox", sharedName)
	require.NoError(err, "GetOrCreateSource")

	_, err = f.Store.CreateCollection(sharedName, "", []int64{f.Source.ID})
	require.NoError(err, "CreateCollection")

	scope, err := ResolveAccountFlag(f.Store, sharedName)
	require.NoError(err)
	require.NotNil(scope.Source, "expected Source to be populated")
	assert.Equal(src2.ID, scope.Source.ID, "source ID")
	assert.Nil(scope.Collection, "expected Collection to be nil when resolving as --account")
}

// TestResolveCollectionFlag_BothExist verifies that when a name exists as both
// an account and a collection, --collection resolves the collection.
func TestResolveCollectionFlag_BothExist(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	sharedName := "shared-name"
	_, err := f.Store.GetOrCreateSource("mbox", sharedName)
	require.NoError(err, "GetOrCreateSource")

	_, err = f.Store.CreateCollection(sharedName, "", []int64{f.Source.ID})
	require.NoError(err, "CreateCollection")

	scope, err := ResolveCollectionFlag(f.Store, sharedName)
	require.NoError(err)
	require.NotNil(scope.Collection, "expected Collection to be populated")
	assert.Equal(sharedName, scope.Collection.Name, "collection name")
	assert.Nil(scope.Source, "expected Source to be nil when resolving as --collection")
}
