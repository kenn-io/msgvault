package cmd

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"strconv"
	"testing"
)

func TestFamilyShowUsesDaemonAndListsCopies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "family-cli@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	cmd, out, _ := emlArchiveCommand(t, st, nil)
	require.NoError(runFamilyShow(cmd, []string{strconv.FormatInt(id, 10)}, true, nil))
	assert.Contains(out.String(), "anchor_message_id")
	assert.Contains(out.String(), "seed@example.test")
}
