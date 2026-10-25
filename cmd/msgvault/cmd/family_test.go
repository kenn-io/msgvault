package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func persistRelatedMail(t *testing.T, st *store.Store, sourceID, conv int64, key, messageID, headers string) int64 {
	t.Helper()
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{
			SourceID: sourceID, ConversationID: conv, SourceMessageID: key, MessageType: "email",
			RFC822MessageID: sql.NullString{String: messageID, Valid: true}, Subject: sql.NullString{String: "Quote", Valid: true},
		},
		RawMIME: []byte("Message-ID: <" + messageID + ">\r\n" + headers + "\r\n"),
	})
	require.NoError(t, err)
	return id
}

func TestFamilyShowListsCopiesAndReplies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	work, err := st.GetOrCreateSource("imap", "work@example.test")
	require.NoError(err)
	home, err := st.GetOrCreateSource("imap", "home@example.test")
	require.NoError(err)
	workConv, err := st.EnsureConversation(work.ID, "thread", "Quote")
	require.NoError(err)
	homeConv, err := st.EnsureConversation(home.ID, "thread", "Quote")
	require.NoError(err)
	id := persistRelatedMail(t, st, work.ID, workConv, "m1", "quote@example.test", "")
	copyID := persistRelatedMail(t, st, home.ID, homeConv, "m2", "quote@example.test", "")
	replyID := persistRelatedMail(t, st, home.ID, homeConv, "m3", "reply@example.test", "In-Reply-To: <quote@example.test>\r\n")

	run := func(args ...string) (string, string) {
		cmd, out, _ := emlArchiveCommand(t, st, nil)
		related := newFamilyCmd()
		related.SetContext(cmd.Context())
		stderr := &bytes.Buffer{}
		related.SetOut(out)
		related.SetErr(stderr)
		related.SetArgs(append([]string{"show"}, args...))
		require.NoError(related.Execute())
		return out.String(), stderr.String()
	}
	text, stderr := run(strconv.FormatInt(id, 10))
	assert.Contains(text, "Message "+strconv.FormatInt(id, 10)+": 2 related copies")
	assert.Contains(text, strconv.FormatInt(copyID, 10)+"\thome@example.test\t")
	assert.Contains(text, "\tsame message\tQuote")
	assert.Contains(text, strconv.FormatInt(replyID, 10)+"\thome@example.test\t")
	assert.Contains(text, "\treply\tQuote")
	assert.Empty(stderr)

	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL WHERE id = ?`), replyID)
	require.NoError(err)
	_, stderr = run(strconv.FormatInt(id, 10))
	assert.Contains(stderr, "haven't been indexed yet")

	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET email_linkage_outcome = 'size_limit' WHERE id = ?`), replyID)
	require.NoError(err)
	text, stderr = run(strconv.FormatInt(id, 10))
	assert.Contains(text, "(headers not fully read)")
	assert.Contains(stderr, "couldn't be fully read")
	assert.NotContains(stderr, "repair-derived")

	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET email_linkage_outcome = NULL WHERE id = ?`), replyID)
	require.NoError(err)
	asJSON, _ := run(strconv.FormatInt(id, 10), "--json")
	var family store.EmailFamily
	require.NoError(json.Unmarshal([]byte(asJSON), &family))
	assert.Equal(id, family.AnchorMessageID)
}

func TestFamilyShowRejectsBadAndMissingIDs(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	for _, arg := range []string{"0", "abc", "999999"} {
		cmd, out, _ := emlArchiveCommand(t, st, nil)
		related := newFamilyCmd()
		related.SetContext(cmd.Context())
		related.SetOut(out)
		related.SetErr(out)
		related.SetArgs([]string{"show", arg})
		assert.Error(t, related.Execute(), arg)
	}
}
