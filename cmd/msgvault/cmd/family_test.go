package cmd

import (
	"encoding/json/v2"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"strconv"
	"strings"
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

func TestFamilyDecisionCommandPreviewsAndAppliesViaDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "decision-cli@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	ids := []string{}
	for i := range 2 {
		id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i), MessageType: "email"}, RawMIME: []byte(fmt.Sprintf("Message-ID: <m%d@example.test>\r\n\r\n", i))})
		require.NoError(err)
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	cmd, out, _ := emlArchiveCommand(t, st, nil)
	require.NoError(runFamilyDecision(cmd, ids, "link", false, strings.Repeat("a", 32), nil))
	assert.Contains(out.String(), "before_revision")
	before, err := st.EmailFamilyContext(t.Context(), 1, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(before.Members, 1)
	out.Reset()
	require.NoError(runFamilyDecision(cmd, ids, "link", true, strings.Repeat("a", 32), nil))
	assert.Contains(out.String(), "active")
	after, err := st.EmailFamilyContext(t.Context(), 1, store.EmailFamilyOptions{})
	require.NoError(err)
	assert.Len(after.Members, 2)
	out.Reset()
	require.NoError(runFamilyDecisionWithRevision(cmd, ids, "link", true, strings.Repeat("a", 32), nil, "original-stale-token"))
	assert.Contains(out.String(), "active")
}

func TestFamilyDecisionUndoPreviewAfterDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "undo-cli@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic mail")
	require.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "seed", MessageType: "email"}, RawMIME: []byte("Message-ID: <seed@example.test>\r\n\r\n")})
	require.NoError(err)
	cmd, out, _ := emlArchiveCommand(t, st, nil)
	require.NoError(runFamilyDecision(cmd, []string{strconv.FormatInt(id, 10)}, "handled", true, strings.Repeat("d", 32), nil))
	var applied store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(out.Bytes(), &applied))
	_, err = st.DB().Exec(st.Rebind("DELETE FROM messages WHERE id=?"), id)
	require.NoError(err)
	execute := func(args ...string) {
		out.Reset()
		family := newFamilyCommand()
		family.SetContext(cmd.Context())
		family.SetOut(out)
		family.SetErr(out)
		family.SetArgs(args)
		require.NoError(family.Execute())
	}
	decisionID := strconv.FormatInt(applied.Decision.ID, 10)
	execute("undo", decisionID, "--preview")
	var current store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(out.Bytes(), &current))
	assert.Empty(current.Family.Members)
	assert.Equal("active", current.Decision.State)
	execute("undo", decisionID, "--revision", strconv.FormatInt(current.Decision.Revision, 10), "--family-revision", current.Family.Revision)
	var undone store.EmailFamilyDecisionResult
	require.NoError(json.Unmarshal(out.Bytes(), &undone))
	assert.Equal("undone", undone.Decision.State)
}
