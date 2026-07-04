package imap

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnumerateMailboxSearchCriteriaConstrainsUIDRange(t *testing.T) {
	criteria := enumerateMailboxSearchCriteria(time.Time{}, time.Time{})

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "1:*", criteria.UID[0].String())
	assert.True(t, criteria.Since.IsZero())
	assert.True(t, criteria.Before.IsZero())
}

func TestEnumerateMailboxSearchCriteriaPreservesDateFilters(t *testing.T) {
	since := time.Date(2026, time.January, 2, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 3, 0, 0, 0, 0, time.UTC)

	criteria := enumerateMailboxSearchCriteria(since, before)

	require.NotNil(t, criteria)
	require.Len(t, criteria.UID, 1)
	assert.Equal(t, "1:*", criteria.UID[0].String())
	assert.Equal(t, since, criteria.Since)
	assert.Equal(t, before, criteria.Before)
}
