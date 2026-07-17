package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMessagesQuery_NormalizesOffsetDateBounds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	minusFive := time.FixedZone("UTC-5", -5*60*60)

	earlierID := f.NewMessage().
		WithSourceMessageID("offset-bound-earlier").
		WithSubject("earlier").
		WithSentAt(time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC)).
		Create(t, f.Store)
	laterID := f.NewMessage().
		WithSourceMessageID("offset-bound-later").
		WithSubject("later").
		WithSentAt(time.Date(2024, 1, 15, 11, 0, 0, 0, minusFive)).
		Create(t, f.Store)

	bound := time.Date(2024, 1, 15, 10, 30, 0, 0, minusFive) // 15:30 UTC

	after, total, err := f.Store.SearchMessagesQuery(&search.Query{AfterDate: &bound}, 0, 50)
	require.NoError(err, "after offset bound")
	require.Equal(int64(1), total, "after total")
	require.Len(after, 1, "after results")
	assert.Equal(laterID, after[0].ID, "after result")

	before, total, err := f.Store.SearchMessagesQuery(&search.Query{BeforeDate: &bound}, 0, 50)
	require.NoError(err, "before offset bound")
	require.Equal(int64(1), total, "before total")
	require.Len(before, 1, "before results")
	assert.Equal(earlierID, before[0].ID, "before result")
}
