package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExploreProviderMailboxFilters(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	parsed, err := exploreContext([]ExploreFilter{{Dimension: "inbox_state", Values: []string{"inbox"}}, {Dimension: "read_state", Values: []string{"unread", "unknown"}}})
	requirements.NoError(err)
	assertions.Equal([]string{"inbox"}, parsed.InboxStates)
	assertions.Equal([]string{"unread", "unknown"}, parsed.ReadStates)
	for _, filters := range [][]ExploreFilter{
		{{Dimension: "inbox_state", Values: []string{"archived"}}},
		{{Dimension: "read_state", Values: []string{"local_read"}}},
		{{Dimension: "read_state", Values: []string{"read", "read"}}},
		{{Dimension: "inbox_state", Values: []string{""}}},
		{{Dimension: "read_state", Values: []string{"read"}}, {Dimension: "read_state", Values: []string{"unread"}}},
	} {
		_, err := exploreContext(filters)
		assertions.Error(err)
	}
}
