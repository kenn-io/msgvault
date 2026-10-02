package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
)

func TestPocketMeetingSource(t *testing.T) {
	m := Model{accounts: []query.AccountInfo{{ID: 1, SourceType: "pocket", Identifier: "personal"}, {ID: 2, SourceType: "gmail"}}}
	accounts := m.meetingAccounts()
	require.Len(t, accounts, 1)
	assert.Equal(t, int64(1), accounts[0].ID)
	assert.Equal(t, "Pocket", m.meetingSourceLabel(1))
}
