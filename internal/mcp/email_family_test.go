package mcp

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestEmailFamilyCatalogAndReads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-mcp@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, EmailFamilyBackend: st}
	listed := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(listed, "get_email_family")
	assert.Equal(true, toolReadOnlyHint(t, listed["get_email_family"]))
	response := rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": "get_email_family", "arguments": map[string]any{"message_id": id}})
	content := toolStructuredContent(t, response.Result)
	assert.EqualValues(id, content["anchor_message_id"])
	opts.DelegatedOnly = true
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), "get_email_family")
}
