package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestTwentyMeetingSourceSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	model := NewBuilder().WithAccounts(query.AccountInfo{ID: 1, SourceType: "twenty", Identifier: "work"}, query.AccountInfo{ID: 2, SourceType: "gmail", Identifier: "you@example.com"}).Build()
	require.Len(model.meetingAccounts(), 1)
	assert.Equal("Twenty", model.meetingSourceLabel(1))
}
