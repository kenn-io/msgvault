package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSourceMessageIDsPageContext(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	first := f.CreateMessage("call-1")
	second := f.CreateMessage("call-2")
	third := f.CreateMessage("call-3")
	other, err := f.Store.GetOrCreateSource("bland", "other-source")
	requirements.NoError(err)
	conversation, err := f.Store.EnsureConversation(other.ID, "other-call", "Other call")
	requirements.NoError(err)
	_, err = f.Store.UpsertMessage(&store.Message{SourceID: other.ID, ConversationID: conversation, SourceMessageID: "other-call"})
	requirements.NoError(err)
	page, err := f.Store.ListSourceMessageIDsPageContext(t.Context(), f.Source.ID, 0, 2)
	requirements.NoError(err)
	assertions.Equal([]store.SourceMessageIdentity{{ID: first, SourceMessageID: "call-1"}, {ID: second, SourceMessageID: "call-2"}}, page)
	page, err = f.Store.ListSourceMessageIDsPageContext(t.Context(), f.Source.ID, second, 2)
	requirements.NoError(err)
	assertions.Equal([]store.SourceMessageIdentity{{ID: third, SourceMessageID: "call-3"}}, page)
	page, err = f.Store.ListSourceMessageIDsPageContext(t.Context(), f.Source.ID, third, 2)
	requirements.NoError(err)
	assertions.Empty(page)
	_, err = f.Store.ListSourceMessageIDsPageContext(t.Context(), f.Source.ID, 0, 0)
	requirements.Error(err)
	_, err = f.Store.ListSourceMessageIDsPageContext(t.Context(), f.Source.ID, 0, 1001)
	requirements.Error(err)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.ListSourceMessageIDsPageContext(cancelled, f.Source.ID, 0, 2)
	requirements.ErrorIs(err, context.Canceled)
}
