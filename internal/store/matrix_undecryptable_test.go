package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMatrixUndecryptableEventsPageInBoundedOrderedBatches(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	sourceID := f.Source.ID
	require.NoError(f.Store.PutMatrixUndecryptableEvents(sourceID, []store.MatrixUndecryptableEvent{
		{EventID: "$c", RoomID: "!room:example.org", RawEvent: []byte(`{"c":1}`)},
		{EventID: "$a", RoomID: "!room:example.org", RawEvent: []byte(`{"a":1}`)},
		{EventID: "$b", RoomID: "!room:example.org"},
	}))

	first, err := f.Store.MatrixUndecryptableEventsPage(sourceID, "", 2)
	require.NoError(err)
	require.Len(first, 2)
	assert.Equal("$a", first[0].EventID)
	assert.Equal("$b", first[1].EventID)
	second, err := f.Store.MatrixUndecryptableEventsPage(sourceID, first[1].EventID, 2)
	require.NoError(err)
	require.Len(second, 1)
	assert.Equal("$c", second[0].EventID)
}

func TestPutMatrixUndecryptableEventsKeepsRecordedCiphertext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	sourceID := f.Source.ID
	put := func(raw []byte) {
		require.NoError(f.Store.PutMatrixUndecryptableEvents(sourceID, []store.MatrixUndecryptableEvent{
			{EventID: "$a", RoomID: "!room:example.org", RawEvent: raw},
		}))
	}
	put([]byte(`{"a":1}`))
	put(nil)
	evt, found, err := f.Store.MatrixUndecryptableEvent(sourceID, "$a")
	require.NoError(err)
	require.True(found)
	assert.JSONEq(`{"a":1}`, string(evt.RawEvent))

	require.NoError(f.Store.DeleteMatrixUndecryptableEvent(sourceID, "$a"))
	_, found, err = f.Store.MatrixUndecryptableEvent(sourceID, "$a")
	require.NoError(err)
	assert.False(found)
}
