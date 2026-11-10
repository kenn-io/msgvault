package xarchive

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"direct-messages.js", "direct-messages-group.js"} {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		require.NoError(t, err)
		testutil.WriteFile(t, root, name, b)
	}
	return root
}

func synthetic(t *testing.T, root, name, body string) {
	t.Helper()
	wrapper := "direct_messages"
	if strings.Contains(name, "group") {
		wrapper += "_group"
	}
	testutil.WriteFile(t, root, name, []byte("window.YTD."+wrapper+".part0 = "+body))
}

func TestImportRedactedEvidenceReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := fixtureRoot(t)
	opts := ImportOptions{RootDir: root, Me: "100001", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded)
	assert.Equal(1, summary.DirectConversations)
	assert.Equal(1, summary.GroupConversations)
	if !st.IsPostgreSQL() {
		engine := query.NewSQLiteEngine(st.DB())
		conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
		require.NoError(err)
		assert.Len(conversations, 2)
		hits, err := engine.TextSearch(t.Context(), "Synthetic", nil, 10, 0)
		require.NoError(err)
		assert.Len(hits, 3)
	}

	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id = 'direct:100000-100001:100002'").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Contains(string(raw), `"createdAt": "2020-01-02T03:04:05.000Z"`)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE m.source_message_id = 'group:100004:100006' AND r.recipient_type='to'").Scan(&n))
	assert.Zero(n, "a group snapshot does not establish message recipients")
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id=cp.conversation_id WHERE c.source_conversation_id='group:100004'").Scan(&n))
	assert.Equal(51, n)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM conversations WHERE source_conversation_id='group:100004'").Scan(&metadata))
	assert.Contains(metadata, "joinConversation")
	assert.Contains(metadata, "direct-messages-group.js")
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='x_user'").Scan(&n))
	assert.Positive(n)

	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.EqualValues(3, summary.MessagesSkipped)
	raw2, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(raw, raw2)
}

func TestImportUnnamedParticipantsRemainVisible(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"3","senderId":"2","recipientId":"1","text":"fictional needle"}}]}}]`)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1"})
	require.NoError(err)
	var id, convID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id, conversation_id FROM messages").Scan(&id, &convID))
	msg, err := st.GetMessageContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("2", msg.From)
	assert.Equal([]string{"1"}, msg.To)
	var names int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participants WHERE COALESCE(display_name, '') <> ''").Scan(&names))
	assert.Zero(names, "an identifier label must not become a person's name")
	if !st.IsPostgreSQL() {
		engine := query.NewSQLiteEngine(st.DB())
		timeline, err := engine.ListConversationMessages(t.Context(), convID, query.TextFilter{})
		require.NoError(err)
		require.Len(timeline, 1)
		assert.Equal("2", timeline[0].FromName)
		hits, err := engine.TextSearch(t.Context(), "fictional", nil, 10, 0)
		require.NoError(err)
		require.Len(hits, 1)
		assert.Equal("2", hits[0].FromName)
		groups, err := engine.TextAggregate(t.Context(), query.TextViewContactNames, query.TextAggregateOptions{})
		require.NoError(err)
		require.Len(groups, 1)
		assert.Equal("2", groups[0].Key)
		conversations, err := engine.ListConversations(t.Context(), query.TextFilter{ContactName: groups[0].Key})
		require.NoError(err)
		require.Len(conversations, 1)
		assert.Equal(convID, conversations[0].ConversationID)
	}
}

func TestImportOwnerScopeAndUnknownValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"same","messages":[{"messageCreate":{"id":"1","text":"unknown"}},{"messageCreate":{"id":"2","createdAt":"bad"}},{"messageCreate":{"text":"no id"}},{"other":{}}]}}]`)
	synthetic(t, root, "direct-messages-group.js", `[{"dmConversation":{"conversationId":"same","messages":[{"messageCreate":{"id":"1"}}]}}]`)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.ErrorContains(err, "--me")
	for _, owner := range []string{"1", "2"} {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: owner})
		require.NoError(err)
		assert.EqualValues(3, summary.MessagesAdded)
		assert.EqualValues(1, summary.InvalidRecords)
		assert.EqualValues(1, summary.EventsSkipped)
	}
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE sent_at IS NULL AND sender_id IS NULL").Scan(&n))
	assert.Equal(6, n)
	if !st.IsPostgreSQL() {
		hits, err := query.NewSQLiteEngine(st.DB()).TextSearch(t.Context(), "unknown", nil, 10, 0)
		require.NoError(err)
		assert.Len(hits, 2)
	}

	testutil.WriteFile(t, root, "account.js", []byte(`window.YTD.account.part0 = [{"account":{"accountId":"1","username":"example","email":"owner@example.org","accountDisplayName":"Example"}}]`))
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "2"})
	require.ErrorContains(err, "conflict")
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"new","messages":[{"messageCreate":{"id":"9"}}]}}]`)
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(err)
	var metadata string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM messages WHERE source_message_id='direct:new:9'").Scan(&metadata))
	assert.JSONEq(`{"owner_id":"1","source_file":"direct-messages.js","conversation_id":"direct:new"}`, metadata)
}

func TestImportCancelAfterPartialWorkAndReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	root := fixtureRoot(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryCtx := context.WithoutCancel(t.Context())
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(st.DB().QueryRowContext(queryCtx, "SELECT count(*) FROM messages").Scan(&n))
		return n > 0
	})
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "100001"})
	require.ErrorIs(err, context.Canceled)
	require.True(canceled())
	assert.Positive(summary.MessagesAdded)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal("failed", status)
	firstAdded := summary.MessagesAdded
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "100001"})
	require.NoError(err)
	assert.EqualValues(3, summary.MessagesAdded+firstAdded)
}

func TestImportMediaRepairAndPreserve(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	dest := t.TempDir()
	// Offset time, URL/message-ID mismatches, escaped filenames and changed
	// media lists are synthetic contract cases.
	first := `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"7","text":"first","createdAt":"2020-01-02T03:04:05+03:00","mediaUrls":["https://ton.twitter.com/dm/42/x/image.jpg","https://video.twimg.com/dm_video/99/movie.mp4","https://ton.twitter.com/dm/42/x/asset%20image.png"]}}]}}]`
	synthetic(t, root, "direct-messages.js", first)
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: dest}
	s, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(3, s.AttachmentsMissing)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Contains(string(raw), "+03:00")
	testutil.WriteFile(t, root, "direct_messages_media/7-movie.mp4", []byte("synthetic video bytes"))
	// A later export cannot redefine the occurrences in the retained first event.
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"7","text":"later","mediaUrls":["https://video.twimg.com/dm_video/99/movie.mp4","https://ton.twitter.com/dm/42/x/image.jpg"]}}]}}]`)
	s, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(1, s.AttachmentsStored)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 3)
	hashes := make(map[string]string, len(refs))
	for _, ref := range refs {
		hashes[ref.SourcePartKey] = ref.ContentHash
	}
	require.Contains(hashes, "media:0")
	assert.Empty(hashes["media:0"], "the missing image must not receive the video bytes")
	assert.NotEmpty(hashes["media:1"])
	testutil.WriteFile(t, root, "direct_messages_media/7-image.jpg", []byte("original"))
	// Escaped filenames follow the producer's literal URL-segment contract.
	testutil.WriteFile(t, root, "direct_messages_media/7-asset%20image.png", []byte("synthetic escaped filename bytes"))
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"7","mediaUrls":[]}}]}}]`)
	s, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Equal(2, s.AttachmentsStored)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 3)
	var hash string
	for _, ref := range refs {
		assert.NotEmpty(ref.ContentHash)
		if ref.SourcePartKey == "media:0" {
			hash = ref.ContentHash
		}
	}
	require.NotEmpty(hash)
	testutil.WriteFile(t, root, "direct_messages_media/7-image.jpg", []byte("changed"))
	s, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.Zero(s.AttachmentsStored)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	var replayHash string
	for _, ref := range refs {
		if ref.SourcePartKey == "media:0" {
			replayHash = ref.ContentHash
		}
	}
	assert.Equal(hash, replayHash)
	raw2, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(raw, raw2)
}

func TestImportUnsafeMediaAndFatalDestination(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize", "destination"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			dest := t.TempDir()
			media := "direct_messages_media/1-test.jpg"
			switch kind {
			case "symlink":
				require.NoError(os.MkdirAll(filepath.Join(root, "direct_messages_media"), 0700))
				outside := filepath.Join(t.TempDir(), "outside")
				require.NoError(os.WriteFile(outside, []byte("synthetic outside bytes"), 0600))
				require.NoError(os.Symlink(outside, filepath.Join(root, media)))
			case "directory":
				require.NoError(os.MkdirAll(filepath.Join(root, media), 0700))
			case "oversize":
				testutil.WriteFile(t, root, media, nil)
				f, err := os.OpenFile(filepath.Join(root, media), os.O_WRONLY, 0600)
				require.NoError(err)
				require.NoError(f.Truncate(1 << 30))
				require.NoError(f.Close())
			case "destination":
				testutil.WriteFile(t, root, media, []byte("image"))
				dest = filepath.Join(dest, "file")
				require.NoError(os.WriteFile(dest, []byte("blocked"), 0600))
			}
			encoded, err := json.Marshal("https://ton.twitter.com/dm/99/token/test.jpg")
			require.NoError(err)
			synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"1","mediaUrls":[`+string(encoded)+`]}}]}}]`)
			s, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1", AttachmentsDir: dest})
			if kind == "destination" {
				require.Error(err)
			} else {
				require.NoError(err)
				assert.Equal(1, s.AttachmentsMissing)
			}
		})
	}
}

// The message record is redacted real export structure; the accompanying
// bytes are synthetic. See testdata/README.md for scope and provenance.
func TestImportRedactedMediaReferenceWithSyntheticBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	b, err := os.ReadFile("testdata/media-reference.js")
	require.NoError(err)
	testutil.WriteFile(t, root, "data/direct-messages.js", b)
	testutil.WriteFile(t, root, "data/account.js", []byte(`window.YTD.account.part0 = [{"account":{"accountId":"100001"}}];`))
	testutil.WriteFile(t, root, "data/direct_messages_media/100002-asset.png", []byte("synthetic image bytes"))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.Equal(1, summary.AttachmentsStored)
}

func TestImportGroupMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	synthetic(t, root, "direct-messages-group.js", `[{"dmConversation":{"conversationId":"9","messages":[{"messageCreate":{"id":"1","senderId":"2","mediaUrls":["https://ton.twitter.com/dm/42/token/image.jpg"]}}]}}]`)
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesAdded)
	assert.Equal(1, summary.AttachmentsMissing)
	testutil.WriteFile(t, root, "direct_messages_group_media/1-image.jpg", []byte("synthetic group media"))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(err)
	assert.EqualValues(1, summary.MessagesSkipped)
	assert.Equal(1, summary.AttachmentsStored)
}
