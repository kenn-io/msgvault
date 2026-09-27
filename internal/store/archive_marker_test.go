package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiveMarkerSetGetDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)

	_, ok, err := st.GetArchiveMarker(ctx, "example.marker")
	require.NoError(err)
	assert.False(ok, "missing marker")

	require.NoError(st.SetArchiveMarker(ctx, "example.marker", "first"))
	require.NoError(st.SetArchiveMarker(ctx, "example.marker", "second"))
	value, ok, err := st.GetArchiveMarker(ctx, "example.marker")
	require.NoError(err)
	assert.True(ok)
	assert.Equal("second", value, "setting again replaces the value")

	require.NoError(st.DeleteArchiveMarker(ctx, "example.marker"))
	require.NoError(st.DeleteArchiveMarker(ctx, "example.marker"), "deleting a missing marker is a no-op")
	_, ok, err = st.GetArchiveMarker(ctx, "example.marker")
	require.NoError(err)
	assert.False(ok)
}
