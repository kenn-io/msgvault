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
	st := testutil.NewTestStore(t)
	root := fixtureRoot(t)
	opts := ImportOptions{RootDir: root, Me: "100001", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary.MessagesAdded)
	assert.Equal(t, 1, summary.DirectConversations)
	assert.Equal(t, 1, summary.GroupConversations)
	if !st.IsPostgreSQL() {
		engine := query.NewSQLiteEngine(st.DB())
		conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
		require.NoError(t, err)
		assert.Len(t, conversations, 2)
		hits, err := engine.TextSearch(t.Context(), "Synthetic", nil, 10, 0)
		require.NoError(t, err)
		assert.Len(t, hits, 3)

	}

	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id = 'direct:100000-100001:100002'").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"createdAt": "2020-01-02T03:04:05.000Z"`)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE m.source_message_id = 'group:100004:100006' AND r.recipient_type='to'").Scan(&n))
	assert.Zero(t, n, "a group snapshot does not establish message recipients")
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id=cp.conversation_id WHERE c.source_conversation_id='group:100004'").Scan(&n))
	assert.Equal(t, 51, n)
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM conversations WHERE source_conversation_id='group:100004'").Scan(&metadata))
	assert.Contains(t, metadata, "joinConversation")
	assert.Contains(t, metadata, "direct-messages-group.js")
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='x_user'").Scan(&n))
	assert.Positive(t, n)

	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, summary.MessagesAdded)
	assert.EqualValues(t, 3, summary.MessagesSkipped)
	raw2, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, raw2)
}

func TestImportOwnerScopeAndUnknownValues(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"same","messages":[{"messageCreate":{"id":"1","text":"unknown"}},{"messageCreate":{"id":"2","createdAt":"bad"}},{"messageCreate":{"text":"no id"}},{"other":{}}]}}]`)
	synthetic(t, root, "direct-messages-group.js", `[{"dmConversation":{"conversationId":"same","messages":[{"messageCreate":{"id":"1"}}]}}]`)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.ErrorContains(t, err, "--me")
	for _, owner := range []string{"1", "2"} {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: owner})
		require.NoError(t, err)
		assert.EqualValues(t, 3, summary.MessagesAdded)
		assert.EqualValues(t, 1, summary.InvalidRecords)
		assert.EqualValues(t, 1, summary.EventsSkipped)
	}
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE sent_at IS NULL AND sender_id IS NULL").Scan(&n))
	assert.Equal(t, 6, n)
	if !st.IsPostgreSQL() {
		hits, err := query.NewSQLiteEngine(st.DB()).TextSearch(t.Context(), "unknown", nil, 10, 0)
		require.NoError(t, err)
		assert.Len(t, hits, 2)
	}

	testutil.WriteFile(t, root, "account.js", []byte(`window.YTD.account.part0 = [{"account":{"accountId":"1","username":"example","email":"owner@example.org","accountDisplayName":"Example"}}]`))
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "2"})
	require.ErrorContains(t, err, "conflict")
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"new","messages":[{"messageCreate":{"id":"9"}}]}}]`)
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root})
	require.NoError(t, err)
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM messages WHERE source_message_id='direct:new:9'").Scan(&metadata))
	assert.Contains(t, metadata, `"owner_id":"1"`)
	assert.NotContains(t, metadata, "owner@example.org")
	assert.NotContains(t, metadata, "username")
}

func TestImportCancelAfterPartialWorkAndReplay(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	root := fixtureRoot(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryCtx := context.WithoutCancel(t.Context())
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(t, st.DB().QueryRowContext(queryCtx, "SELECT count(*) FROM messages").Scan(&n))
		return n > 0
	})
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "100001"})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, canceled())
	assert.Positive(t, summary.MessagesAdded)
	var status string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal(t, "failed", status)
	firstAdded := summary.MessagesAdded
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "100001"})
	require.NoError(t, err)
	assert.EqualValues(t, 3, summary.MessagesAdded+firstAdded)
}

func TestImportMediaRepairAndPreserve(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	dest := t.TempDir()
	// The offset timestamp and mismatched URL/message IDs are synthetic contract cases.
	synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"7","text":"first","createdAt":"2020-01-02T03:04:05+03:00","mediaUrls":["https://ton.twitter.com/dm/42/x/image.jpg","https://video.twimg.com/dm_video/99/movie.mp4"]}}]}}]`)
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: dest}
	s, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, s.AttachmentsMissing)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "+03:00")
	testutil.WriteFile(t, root, "direct_messages_media/7-image.jpg", []byte("original"))
	testutil.WriteFile(t, root, "direct_messages_media/7-movie.mp4", []byte("synthetic video bytes"))
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Equal(t, 2, s.AttachmentsStored)
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	hash := refs[0].ContentHash
	assert.NotEmpty(t, hash)
	testutil.WriteFile(t, root, "direct_messages_media/7-image.jpg", []byte("changed"))
	s, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.Zero(t, s.AttachmentsStored)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, hash, refs[0].ContentHash)
	raw2, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, raw, raw2)
}

func TestImportUnsafeMediaAndFatalDestination(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversize", "destination"} {
		t.Run(kind, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			dest := t.TempDir()
			media := "direct_messages_media/1-test.jpg"
			switch kind {
			case "symlink":
				require.NoError(t, os.MkdirAll(filepath.Join(root, "direct_messages_media"), 0700))
				outside := filepath.Join(t.TempDir(), "outside")
				require.NoError(t, os.WriteFile(outside, []byte("synthetic outside bytes"), 0600))
				require.NoError(t, os.Symlink(outside, filepath.Join(root, media)))
			case "directory":
				require.NoError(t, os.MkdirAll(filepath.Join(root, media), 0700))
			case "oversize":
				testutil.WriteFile(t, root, media, nil)
				f, err := os.OpenFile(filepath.Join(root, media), os.O_WRONLY, 0600)
				require.NoError(t, err)
				require.NoError(t, f.Truncate(1<<30))
				require.NoError(t, f.Close())
			case "destination":
				testutil.WriteFile(t, root, media, []byte("image"))
				dest = filepath.Join(dest, "file")
				require.NoError(t, os.WriteFile(dest, []byte("blocked"), 0600))
			}
			encoded, err := json.Marshal("https://ton.twitter.com/dm/99/token/test.jpg")
			require.NoError(t, err)
			synthetic(t, root, "direct-messages.js", `[{"dmConversation":{"conversationId":"1-2","messages":[{"messageCreate":{"id":"1","mediaUrls":[`+string(encoded)+`]}}]}}]`)
			s, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "1", AttachmentsDir: dest})
			if kind == "destination" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, 1, s.AttachmentsMissing)
			}
		})
	}
}

// The message record is redacted real export structure; the accompanying
// bytes are synthetic. See testdata/README.md for scope and provenance.
func TestImportRedactedMediaReferenceWithSyntheticBytes(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	b, err := os.ReadFile("testdata/media-reference.js")
	require.NoError(t, err)
	testutil.WriteFile(t, root, "data/direct-messages.js", b)
	testutil.WriteFile(t, root, "data/account.js", []byte(`window.YTD.account.part0 = [{"account":{"accountId":"100001"}}];`))
	testutil.WriteFile(t, root, "data/direct_messages_media/100002-asset.png", []byte("synthetic image bytes"))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, AttachmentsDir: t.TempDir()})
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, 1, summary.AttachmentsStored)
}

func TestImportGroupMedia(t *testing.T) {
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	synthetic(t, root, "direct-messages-group.js", `[{"dmConversation":{"conversationId":"9","messages":[{"messageCreate":{"id":"1","senderId":"2","mediaUrls":["https://ton.twitter.com/dm/42/token/image.jpg"]}}]}}]`)
	opts := ImportOptions{RootDir: root, Me: "1", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesAdded)
	assert.Equal(t, 1, summary.AttachmentsMissing)
	testutil.WriteFile(t, root, "direct_messages_group_media/1-image.jpg", []byte("synthetic group media"))
	summary, err = Import(t.Context(), st, opts)
	require.NoError(t, err)
	assert.EqualValues(t, 1, summary.MessagesSkipped)
	assert.Equal(t, 1, summary.AttachmentsStored)
}
