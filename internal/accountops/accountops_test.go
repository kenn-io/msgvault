package accountops_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/accountops"
	"go.kenn.io/msgvault/internal/opserr"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestUpdateDisplayNameRejectsAmbiguousDisplayName(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	first, err := f.Store.GetOrCreateSource("gmail", "first@example.test")
	require.NoError(err)
	second, err := f.Store.GetOrCreateSource("imap", "second@example.test")
	require.NoError(err)
	require.NoError(f.Store.UpdateSourceDisplayName(first.ID, "Work"))
	require.NoError(f.Store.UpdateSourceDisplayName(second.ID, "Work"))

	_, err = accountops.UpdateDisplayName(f.Store, accountops.UpdateRequest{
		Email: "Work", DisplayName: "Renamed",
	})
	require.Error(err)
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err))
	assert.ErrorContains(err, "ambiguous")
}

func TestUpdateDisplayNameTargetsExactSourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	first, err := f.Store.GetOrCreateSource("gmail", "shared@example.test")
	require.NoError(err)
	second, err := f.Store.GetOrCreateSource("imap", "shared@example.test")
	require.NoError(err)

	result, err := accountops.UpdateDisplayName(f.Store, accountops.UpdateRequest{
		SourceID: first.ID, SourceIDSet: true, DisplayName: "Primary",
	})
	require.NoError(err)
	assert.Equal(first.Identifier, result.Email)

	updated, err := f.Store.GetSourceByID(first.ID)
	require.NoError(err)
	assert.Equal("Primary", updated.DisplayName.String)
	untouched, err := f.Store.GetSourceByID(second.ID)
	require.NoError(err)
	assert.False(untouched.DisplayName.Valid)
}

func TestUpdateRequestJSONPreservesExplicitZeroSourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var req accountops.UpdateRequest
	require.NoError(json.Unmarshal([]byte(`{"source_id":0,"display_name":"Work"}`), &req))
	assert.True(req.SourceIDSet)

	_, err := accountops.UpdateDisplayName(storetest.New(t).Store, req)
	require.Error(err)
	assert.Equal(opserr.KindInvalid, opserr.KindOf(err))
	assert.ErrorContains(err, "source ID must be positive")
}
