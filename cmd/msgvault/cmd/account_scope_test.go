package cmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
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
	require.NoError(t, err, "CreateCollection")

	return f, accountID, collectionName
}

func TestResolveAccountFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, "")
	require.NoError(t, err)
	assert.True(t, scope.IsEmpty(), "expected empty scope, got source=%v collection=%v",
		scope.Source, scope.Collection)
}

func TestResolveCollectionFlag_EmptyInput(t *testing.T) {
	f, _, _ := setupScopeFixture(t)

	scope, err := ResolveCollectionFlag(f.Store, "")
	require.NoError(t, err)
	assert.True(t, scope.IsEmpty(), "expected empty scope, got source=%v collection=%v",
		scope.Source, scope.Collection)
}

func TestResolveAccountFlag_ValidAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)

	scope, err := ResolveAccountFlag(f.Store, accountID)
	require.NoError(err)
	require.NotNil(scope.Source, "expected Source to be populated")
	assert.Equal(accountID, scope.Source.Identifier, "source identifier")
	assert.Nil(scope.Collection, "expected Collection to be nil")
}

func TestResolveAccountFlag_IncludesCalendarSourcesForAccountEmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)

	mkCalendarSource := func(identifier, accountEmail string) *store.Source {
		t.Helper()
		src, err := f.Store.GetOrCreateSource("gcal", identifier)
		require.NoError(err, "GetOrCreateSource(%s)", identifier)
		cfg, err := json.Marshal(map[string]string{
			"account_email": accountEmail,
			"calendar_id":   identifier,
		})
		require.NoError(err, "marshal sync_config")
		require.NoError(f.Store.UpdateSourceSyncConfig(src.ID, string(cfg)), "UpdateSourceSyncConfig")
		return src
	}
	primaryCal := mkCalendarSource("test@example.com/primary", "Test@Example.COM")
	workCal := mkCalendarSource("test@example.com/work", "test@example.com")
	otherCal := mkCalendarSource("other@example.com/primary", "other@example.com")

	scope, err := ResolveAccountFlag(f.Store, accountID)
	require.NoError(err)
	require.NotNil(scope.Source, "direct Gmail source should still be available")
	assert.Equal(f.Source.ID, scope.Source.ID, "source ID")
	assert.ElementsMatch([]int64{f.Source.ID, primaryCal.ID, workCal.ID}, scope.SourceIDs())
	assert.NotContains(scope.SourceIDs(), otherCal.ID, "other account calendar source")
	assert.False(scope.IsCollection(), "calendar expansion should not masquerade as a collection")
	assert.Equal(accountID, scope.DisplayName(), "display name")
}

func TestResolveAccountFlag_IncludesCalendarSourcesForDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f, accountID, _ := setupScopeFixture(t)
	require.NoError(f.Store.UpdateSourceDisplayName(f.Source.ID, "Work"), "UpdateSourceDisplayName")

	cal, err := f.Store.GetOrCreateSource("gcal", accountID+"/primary")
	require.NoError(err, "GetOrCreateSource")
	cfg, err := json.Marshal(map[string]string{
		"account_email": accountID,
		"calendar_id":   "primary",
	})
	require.NoError(err, "marshal sync_config")
	require.NoError(f.Store.UpdateSourceSyncConfig(cal.ID, string(cfg)), "UpdateSourceSyncConfig")

	scope, err := ResolveAccountFlag(f.Store, "Work")
	require.NoError(err)
	require.NotNil(scope.Source, "display-name lookup should resolve the Gmail source")
	assert.Equal(f.Source.ID, scope.Source.ID, "source ID")
	assert.ElementsMatch([]int64{f.Source.ID, cal.ID}, scope.SourceIDs())
}

func TestResolveAccountFlag_CalendarOnlyAccountEmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	cal, err := f.Store.GetOrCreateSource("gcal", "calendar-only@example.com/primary")
	require.NoError(err, "GetOrCreateSource")
	cfg, err := json.Marshal(map[string]string{
		"account_email": "Calendar.Only@Example.COM",
		"calendar_id":   "primary",
	})
	require.NoError(err, "marshal sync_config")
	require.NoError(f.Store.UpdateSourceSyncConfig(cal.ID, string(cfg)), "UpdateSourceSyncConfig")

	scope, err := ResolveAccountFlag(f.Store, "calendar.only@example.com")
	require.NoError(err)
	assert.Nil(scope.Source, "calendar-only account has no single source")
	assert.ElementsMatch([]int64{cal.ID}, scope.SourceIDs())
	assert.False(scope.IsCollection(), "calendar-only account is still an account scope")
	assert.Equal("calendar.only@example.com", scope.DisplayName())
}

func TestResolveCollectionFlag_ValidCollection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require.Error(t, err, "expected error for collection name passed as --account")
	require.ErrorContains(t, err, "is a collection")
	assert.ErrorContains(t, err, "--collection")
}

func TestResolveCollectionFlag_RejectsAccountIdentifier(t *testing.T) {
	f, accountID, _ := setupScopeFixture(t)

	_, err := ResolveCollectionFlag(f.Store, accountID)
	require.Error(t, err, "expected error for account identifier passed as --collection")
	require.ErrorContains(t, err, "is an account")
	assert.ErrorContains(t, err, "--account")
}

// TestResolveAccountFlag_BothExist verifies the tie-break rule: when a name
// exists as both an account and a collection, --account resolves the account.
func TestResolveAccountFlag_BothExist(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
	assert := assert.New(t)
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
