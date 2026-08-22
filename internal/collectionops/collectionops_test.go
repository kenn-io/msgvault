package collectionops

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestResolveAccountNumericIdentifierRemainsToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	numeric, err := f.Store.GetOrCreateSource("synctech-sms", "15551234567")
	require.NoError(err)

	scope, err := ResolveAccount(f.Store, "15551234567")
	require.NoError(err)
	require.NotNil(scope.Source)
	assert.Equal(numeric.ID, scope.Source.ID)
}

func TestResolveAccountPreservesPrimaryThenCalendarOrdering(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	primary, err := f.Store.GetOrCreateSource("gmail", "family@example.test")
	require.NoError(err)
	calendar, err := f.Store.GetOrCreateSource("gcal", "family@example.test/primary")
	require.NoError(err)
	config, err := json.Marshal(map[string]string{"account_email": primary.Identifier})
	require.NoError(err)
	require.NoError(f.Store.UpdateSourceSyncConfig(calendar.ID, string(config)))

	scope, err := ResolveAccount(f.Store, primary.Identifier)
	require.NoError(err)
	assert.Equal([]int64{primary.ID, calendar.ID}, scope.SourceIDs())
}

func TestResolveCollection(t *testing.T) {
	t.Run("valid collection", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		_, err := f.Store.CreateCollection("Team", "", []int64{f.Source.ID})
		require.NoError(err, "CreateCollection")

		scope, err := ResolveCollection(f.Store, "Team")

		require.NoError(err, "ResolveCollection")
		require.NotNil(scope.Collection, "collection")
		assert.Equal("Team", scope.Collection.Name, "collection name")
		assert.Equal([]int64{f.Source.ID}, scope.SourceIDs(), "source IDs")
	})

	t.Run("empty input", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)

		scope, err := ResolveCollection(f.Store, "")

		require.NoError(err, "ResolveCollection")
		assert.Nil(scope.Collection, "collection")
		assert.Empty(scope.SourceIDs(), "source IDs")
	})

	t.Run("account name is invalid collection scope", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)

		_, err := ResolveCollection(f.Store, f.Source.Identifier)

		require.Error(err, "ResolveCollection")
		assert.Equal(opserr.KindInvalid, opserr.KindOf(err), "error kind")
		require.ErrorContains(err, "is an account, not a collection")
		require.ErrorContains(err, "--account")
	})

	t.Run("missing collection is not found", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)

		_, err := ResolveCollection(f.Store, "missing")

		require.Error(err, "ResolveCollection")
		assert.Equal(opserr.KindNotFound, opserr.KindOf(err), "error kind")
		require.ErrorContains(err, "no collection named")
	})

	t.Run("collection wins when account also exists", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		sharedName := "shared-name"
		_, err := f.Store.GetOrCreateSource("mbox", sharedName)
		require.NoError(err, "GetOrCreateSource")
		_, err = f.Store.CreateCollection(sharedName, "", []int64{f.Source.ID})
		require.NoError(err, "CreateCollection")

		scope, err := ResolveCollection(f.Store, sharedName)

		require.NoError(err, "ResolveCollection")
		require.NotNil(scope.Collection, "collection")
		assert.Equal(sharedName, scope.Collection.Name, "collection name")
	})
}
