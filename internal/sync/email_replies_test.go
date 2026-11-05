package sync

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// threadMIME builds a synthetic email. inReplyTo and references are written
// verbatim when set, so tests can use msg-id lists and trailing comments.
func threadMIME(messageID, inReplyTo, references string) []byte {
	raw := "From: Sender <sender@example.com>\r\nTo: test@example.com\r\nSubject: Thread\r\n" +
		"Date: Mon, 01 Jan 2024 12:00:00 +0000\r\nMessage-ID: <" + messageID + ">\r\n"
	if inReplyTo != "" {
		raw += "In-Reply-To: " + inReplyTo + "\r\n"
	}
	if references != "" {
		raw += "References: " + references + "\r\n"
	}
	return []byte(raw + "\r\nBody\r\n")
}

func addThreadMessage(env *TestEnv, id, thread string, raw []byte) {
	env.Mock.AddMessage(id, raw, []string{"INBOX"})
	env.Mock.Messages[id].ThreadID = thread
}

// replyLinks maps each source message ID to the source message ID its
// archived reply link points at, or "" when the message has no link.
func replyLinks(t *testing.T, st *store.Store, sourceID int64) map[string]string {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), `SELECT c.source_message_id, COALESCE(p.source_message_id, '')
		FROM messages c LEFT JOIN messages p ON p.id = c.reply_to_message_id
		WHERE c.source_id = ?`, sourceID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	links := make(map[string]string)
	for rows.Next() {
		var child, parent string
		require.NoError(t, rows.Scan(&child, &parent))
		links[child] = parent
	}
	require.NoError(t, rows.Err())
	return links
}

func messageMetadata(t *testing.T, st *store.Store, sourceID int64, sourceMessageID string) map[string]string {
	t.Helper()
	var metadata sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT metadata FROM messages WHERE source_id = ? AND source_message_id = ?`,
		sourceID, sourceMessageID).Scan(&metadata))
	fields := map[string]string{}
	if metadata.Valid {
		require.NoError(t, json.Unmarshal([]byte(metadata.String), &fields))
	}
	return fields
}

func TestGmailSyncLinksReplyToArchivedParent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.UseRawThreadID = true
	env.Mock.Profile.MessagesTotal = 5
	addThreadMessage(env, "parent", "thread-a", threadMIME("parent@example.test", "", ""))
	addThreadMessage(env, "reply", "thread-a", threadMIME("reply@example.test",
		"<parent@example.test> (Sender's message of Mon)", "<parent@example.test>"))
	// Two archived copies share one Message-ID, so their reply stays unlinked.
	addThreadMessage(env, "twin-1", "thread-b", threadMIME("twin@example.test", "", ""))
	addThreadMessage(env, "twin-2", "thread-b", threadMIME("twin@example.test", "", ""))
	addThreadMessage(env, "twin-reply", "thread-b", threadMIME("twin-reply@example.test", "<twin@example.test>", ""))

	runFullSync(t, env)

	source := env.CreateSource(t)
	assert.Equal(map[string]string{
		"parent": "", "reply": "parent", "twin-1": "", "twin-2": "", "twin-reply": "",
	}, replyLinks(t, env.Store, source.ID))
	assert.Equal(map[string]string{"email_in_reply_to": "parent@example.test"},
		messageMetadata(t, env.Store, source.ID, "reply"))
	assert.Empty(messageMetadata(t, env.Store, source.ID, "parent"))
}

func TestGmailIncrementalSyncLinksParentArchivedAfterReply(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	env.Mock.UseRawThreadID = true
	env.Mock.Profile.MessagesTotal = 1
	addThreadMessage(env, "reply", "thread-a", threadMIME("reply@example.test",
		"<parent@example.test>", "<parent@example.test>"))
	runFullSync(t, env)
	source := env.CreateSource(t)
	require.Equal(map[string]string{"reply": ""}, replyLinks(t, env.Store, source.ID))

	addThreadMessage(env, "parent", "thread-a", threadMIME("parent@example.test", "", ""))
	added := historyAdded("parent")
	added.MessagesAdded[0].Message.ThreadID = "thread-a"
	env.SetHistory(13000, added)
	runIncrementalSync(t, env)

	assert.Equal(map[string]string{"reply": "parent", "parent": ""}, replyLinks(t, env.Store, source.ID))
}

func TestIMAPSyncLinksRepliesInEitherOrder(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	opts := DefaultOptions()
	opts.SourceType = sourceTypeIMAP
	addr, user := testutil.StartIMAPMemServerWithSpecialUse(t,
		map[string]int{"INBOX": 0, "Envoyes": 0},
		map[string][]imapv2.MailboxAttr{"Envoyes": {imapv2.MailboxAttrSent}},
	)
	// The inbound reply arrives before the sent message it answers.
	testutil.AppendIMAPRawMessage(t, user, "INBOX", threadMIME("reply@example.test",
		"<root@example.test>", "<root@example.test>"))
	first := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(first, env.Store, opts)
	runFullSync(t, env)
	require.NoError(first.Close())

	testutil.AppendIMAPRawMessage(t, user, "Envoyes", threadMIME("root@example.test", "", ""))
	testutil.AppendIMAPRawMessage(t, user, "Envoyes", threadMIME("followup@example.test",
		"<reply@example.test>", "<root@example.test> <reply@example.test>"))
	second := newSyncTestIMAPClient(t, addr)
	env.Syncer = New(second, env.Store, opts)
	runFullSync(t, env)
	require.NoError(second.Close())

	source, err := env.Store.GetSourceByIdentifier(t.Context(), testEmail)
	require.NoError(err)
	assert.Equal(map[string]string{
		"INBOX|1": "Envoyes|1", "Envoyes|1": "", "Envoyes|2": "INBOX|1",
	}, replyLinks(t, env.Store, source.ID))
	followup := messageMetadata(t, env.Store, source.ID, "Envoyes|2")
	assert.Equal("reply@example.test", followup["email_in_reply_to"])
}
