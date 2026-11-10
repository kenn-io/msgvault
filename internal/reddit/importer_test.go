package reddit

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportFictionalExport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/export"})
	require.NoError(err)
	assert.Equal(6, summary.Added)
	assert.Equal(2, summary.Unchanged)
	assert.Contains(summary.Unsupported, "votes.csv")
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type IN ('reddit_chat','reddit_pm','reddit_post','reddit_comment')").Scan(&n))
	assert.Equal(6, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages m JOIN participant_identifiers p ON p.participant_id=m.sender_id WHERE m.message_type IN ('reddit_post','reddit_comment') AND m.source_is_from_me AND p.identifier_type='reddit' AND p.identifier_value='fictional_owner'`).Scan(&n))
	assert.Equal(2, n)
	var date sql.NullTime
	var sender sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sent_at,sender_id FROM messages WHERE source_message_id='chat:c3'").Scan(&date, &sender))
	assert.False(date.Valid)
	assert.False(sender.Valid)
	var rawID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='chat:c2'").Scan(&rawID))
	raw, err := st.GetMessageRawContext(t.Context(), rawID)
	require.NoError(err)
	var evidence csvRow
	require.NoError(json.Unmarshal(raw, &evidence))
	assert.Equal("chat_history.csv", evidence.Filename)
	assert.Equal("lantern chat\nsecond line", evidence.Columns["message"])
	updatedAt, present := evidence.Columns["updated_at"]
	assert.True(present)
	assert.Empty(updatedAt)
	live := query.NewEngine(st.DB(), st.IsPostgreSQL())
	found, err := live.Search(t.Context(), search.Parse("lantern"), 20, 0)
	require.NoError(err)
	assert.Len(found, 6)
	run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.NoError(err)
	assert.Equal("completed", run.Status)
	assert.Equal(int64(8), run.MessagesProcessed)
	assert.Equal(int64(6), run.MessagesAdded)
	chats, err := st.SearchChatsContext(t.Context(), store.ChatDiscoveryQuery{Query: "Invented"})
	require.NoError(err)
	assert.Len(chats.Results, 2, "imported Chat and PM conversations reach discovery")
}

func TestImportSchemas(t *testing.T) {
	for _, tc := range []struct{ name, file, data, want string }{
		{"missing header", "posts.csv", "title,body\na,body\n", "essential"},
		{"duplicate header", "posts.csv", "id,title,title\na,b,c\n", "header"},
		{"empty header", "posts.csv", "id,title,\na,b,c\n", "header"},
		{"no content", "votes.csv", "id\na\n", "recognized"},
		{"invalid utf8", "posts.csv", "id,title\na,\xff\n", "UTF-8"},
		{"invalid utf8 wrong width", "posts.csv", "id,title\na,\xff,extra\n", "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0600))
			_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.ErrorContains(err, tc.want)
		})
	}
}

func TestImportEmptyFiles(t *testing.T) {
	for _, empty := range []string{"statistics.csv", "comments.csv"} {
		t.Run(empty, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, empty), nil, 0600))
			require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\none,lantern\n"), 0600))
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.NoError(err)
			assert.Equal(t, 1, summary.Added)
		})
	}
}

func TestImportQuotedBOMHeaderAndRaggedRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("\ufeff\"title\",id,extra\n\"first\nline\",a,original\nmissing\n,no-id,\n"), 0600))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "/u/Fictional_Owner"})
	require.NoError(err)
	assert.Equal(2, summary.Added)
	assert.Equal(1, summary.Malformed)
}

func TestImportSymlinks(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.Symlink(filepath.Join(t.TempDir(), "outside.csv"), filepath.Join(root, "posts.csv")))
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "recognized")
}

func TestImportInterruptedRepeatAndOriginalRetention(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,thread_parent_message_id\nchild,original,parent\nparent,parent body,\n"), 0600))
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Not parallel: the shared helper replaces the process-wide SQL log handler.
	queryCtx := context.WithoutCancel(t.Context()) // Observe committed rows after cancellation.
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var n int
		require.NoError(st.DB().QueryRowContext(queryCtx, "SELECT count(*) FROM messages").Scan(&n))
		return n > 0
	})
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorIs(err, context.Canceled)
	require.True(canceled())
	assert.Equal(1, summary.Added)
	var count, recorded int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&count))
	assert.Equal(1, count)
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT message_count FROM conversations").Scan(&recorded))
	assert.Equal(count, recorded)
	run, err := st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.NoError(err)
	assert.Equal("failed", run.Status)
	assert.Equal(int64(1), run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	_, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var parent string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT p.source_message_id FROM messages m JOIN messages p ON p.id=m.reply_to_message_id WHERE m.source_message_id='chat:child'").Scan(&parent))
	assert.Equal("chat:parent", parent)
	require.NoError(os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message\nchild,changed\n"), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	assert.Zero(summary.Added)
	var snippet string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT snippet FROM messages WHERE source_message_id='chat:child'").Scan(&snippet))
	assert.Equal("original", snippet)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(2, n)
}

func TestImportAccountIsolation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "comments.csv"), []byte("id,body\nsame,lantern\n,missing key\n"), 0600))
	st := testutil.NewTestStore(t)
	first, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "u/fictional_one"})
	require.NoError(err)
	assert.Equal(1, first.MissingIDs)
	second, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_two"})
	require.NoError(err)
	assert.NotEqual(first.SourceID, second.SourceID)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE source_message_id='comment:same'").Scan(&n))
	assert.Equal(2, n)
}

func TestImportLargeCSV(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "posts.csv"))
	require.NoError(err)
	_, err = f.WriteString("id,title\none,hello\n")
	require.NoError(err)
	blank := []byte(strings.Repeat("\n", 1<<20))
	for range 257 {
		_, err = f.Write(blank)
		require.NoError(err)
	}
	require.NoError(f.Close())
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	assert.Equal(t, 1, summary.Added)
}

func TestImportLargeDirectory(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	for i := range 10001 {
		require.NoError(os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%d", i)), nil, 0600))
	}
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\none,hello\n"), 0600))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	require.Equal(1, summary.Added)
}

func TestImportOwnersAndPlainMarkerNames(t *testing.T) {
	for _, tc := range []struct{ me, account, want string }{
		{"unknown", "", "unknown"},
		{"a", "", "a"}, {"Legacy-Name_1", "", "legacy-name_1"},
		{"", "deleted", "deleted"},
	} {
		t.Run(tc.me+"/"+tc.account, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, "messages.csv"), []byte("id,body,from,to\none,lantern,u/unknown,/u/deleted\n"), 0600))
			require.NoError(os.WriteFile(filepath.Join(root, "statistics.csv"), []byte("statistic,value\naccount name,"+tc.account+"\n"), 0600))
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: tc.me})
			require.NoError(err)
			var account string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT identifier FROM sources WHERE id=?"), summary.SourceID).Scan(&account))
			assert.Equal(tc.want, account)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='reddit' AND identifier_value IN ('unknown','deleted')").Scan(&n))
			assert.Equal(2, n)
			require.Len(summary.Files, 1)
			assert.Zero(summary.Files[0].UnknownSenders)
		})
	}
}

func TestImportInvalidOwnerPreflight(t *testing.T) {
	for _, tc := range []struct{ name, me, statistics, want, split string }{
		{"missing owner", "", "", "owner", ""},
		{"flag URL", "https://example.com/user", "account name,fictional_owner\n", "--me", ""},
		{"flag wrapper", "/u/", "account name,fictional_owner\n", "--me", ""},
		{"statistics URL", "fictional_owner", "account name,https://example.com/user\n", "statistics.csv: record 1", ""},
		{"flag Unicode folds to ASCII", "\u212A", "", "--me", ""},
		{"statistics malformed later record", "fictional_owner", "export time,fictional snapshot\naccount name,other_owner,extra\n", "statistics.csv: record 2", ""},
		{"statistics quoting", "fictional_owner", "account name,\"unterminated\n", "statistics.csv: record 1", ""},
		{"flag statistics conflict", "fictional_two", "account name,fictional_one\n", "account", ""},
		{"statistics split conflict", "", "account name,fictional_one\n", "account", "account name,fictional_two\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\none,lantern\n"), 0600))
			require.NoError(os.WriteFile(filepath.Join(root, "statistics.csv"), []byte("statistic,value\n"+tc.statistics), 0600))
			if tc.split != "" {
				require.NoError(os.WriteFile(filepath.Join(root, "statistics_1.csv"), []byte("statistic,value\n"+tc.split), 0600))
			}
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: tc.me})
			require.ErrorContains(err, tc.want)
			assert.Zero(summary.SourceID)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sources").Scan(&n))
			assert.Zero(n)
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
			assert.Zero(n)
		})
	}
}

func TestImportCoverageCountsExistingRecords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	chat := "message_id,message,username,created_at,channel_url\none,https://example.com/content,[deleted],bad,channel\none,https://example.com/duplicate,[removed],bad,channel\n"
	pm := "id,body,from,date\ntwo,https://example.com/content,[deleted],bad\n"
	for name, data := range map[string]string{"chat_history.csv": chat, "messages.csv": pm, "messages_archive.csv": pm} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	for range 2 {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
		require.NoError(err)
		require.Len(summary.Files, 3)
		for _, file := range summary.Files {
			want := 1
			if file.Filename == "chat_history.csv" {
				want = 2
			}
			assert.Equal(want, file.UnknownDates)
			assert.Equal(want, file.UnknownSenders)
		}
	}
}

func TestImportChatGrouping(t *testing.T) {
	t.Run("channel", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		root := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,channel_url,channel_name,conversation_type,thread_parent_message_id,username\none,lantern,fictional-channel,Fictional garden,group,,u/Fictional_Peer\ntwo,reply,fictional-channel,Fictional garden,group,one,\n"), 0600))
		st := testutil.NewTestStore(t)
		_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
		require.NoError(err)
		var kind, title string
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COALESCE(conversation_type,''),title FROM conversations").Scan(&kind, &title))
		assert.Equal("chat", kind)
		assert.Equal("Fictional garden", title)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&count))
		assert.Equal(1, count)
		var name string
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT p.display_name FROM participants p JOIN participant_identifiers pi ON pi.participant_id = p.id WHERE pi.identifier_value = 'fictional_peer'").Scan(&name))
		assert.Equal("Fictional_Peer", name)
	})

	t.Run("without channel", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		root := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,channel_url\none,hello,channel\ntwo,reply,\nthree,later,channel\n"), 0600))
		require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\npost,later\n"), 0600))
		st := testutil.NewTestStore(t)
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
		require.NoError(err)
		assert.Equal(4, summary.Added)
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
		assert.Equal(4, n)
		require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations WHERE source_conversation_id LIKE 'chat:item:%'").Scan(&n))
		assert.Equal(1, n)
	})
}

func TestImportFailureRetainsCompletedRows(t *testing.T) {
	for _, tc := range []struct {
		name, invalid, fixed              string
		firstAdded, retryAdded, unchanged int
	}{
		{"schema", "renamed\ninvalid\n", "id,body\ntwo,recovered\n", 1, 2, 1},
		{"quoting", "id,body\ntwo,lantern\nfour,\"unterminated\n", "id,body\ntwo,lantern\nfour,recovered\n", 2, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := t.TempDir()
			for name, data := range map[string]string{"chat_history.csv": "message_id,message,channel_url\none,lantern,channel\n", "comments.csv": tc.invalid, "posts.csv": "id,title\nthree,later\n"} {
				require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
			}
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.ErrorContains(err, "comments.csv")
			assert.Equal(tc.firstAdded, summary.Added)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
			assert.Equal(tc.firstAdded, n)
			require.NoError(os.WriteFile(filepath.Join(root, "comments.csv"), []byte(tc.fixed), 0600))
			summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.NoError(err)
			assert.Equal(tc.retryAdded, summary.Added)
			assert.Equal(tc.unchanged, summary.Unchanged)
		})
	}
}

func TestImportPMConversationKind(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	st := testutil.NewTestStore(t)
	data := "id,body,from,to,thread_id\none,first,fictional_owner,fictional_peer,two-people\ntwo,second,fictional_owner,,thread\nthree,recipient,fictional_owner,r/fictional_garden,thread\nfour,sender,/r/fictional_garden,fictional_owner,thread\n"
	require.NoError(os.WriteFile(filepath.Join(root, "messages.csv"), []byte(data), 0600))
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT conversation_type FROM conversations WHERE source_conversation_id='pm:thread:two-people'").Scan(&kind))
	assert.Equal("chat", kind)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='reddit' AND identifier_value IN ('r/fictional_garden','/r/fictional_garden')").Scan(&n))
	assert.Zero(n)
}

func TestImportSubredditTitleCanonical(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	for name, data := range map[string]string{"posts.csv": "id,title,subreddit\none,lantern,Fictional_Garden\nthree,another,Fictional_Garden\n", "comments.csv": "id,body,subreddit\ntwo,comment,r/fictional_garden\n"} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&n))
	assert.Equal(3, n)
	for _, sourceID := range []string{"post:one", "post:three", "comment:two"} {
		var title string
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT c.title FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.source_message_id=?"), sourceID).Scan(&title))
		assert.Equal("r/fictional_garden", title)
	}
}

func TestImportPermalinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	for name, data := range map[string]string{
		"messages.csv":   "id,body,permalink\npm,private text,/message/messages/abc\n",
		"posts.csv":      "id,body,url,permalink\npost,post text,https://example.com/link,https://example.com/post\nsame,,https://example.com/same,https://example.com/same\nprefix,https://example.com/posts/1234,,https://example.com/posts/123\n",
		"comments.csv":   "id,body,media,permalink\ncomment,comment text,https://example.com/media,/r/example/comments/abc/def\n",
		"messages_1.csv": "id,subject\none,hello\n",
		"posts_1.csv":    "id,url\nthree,https://example.com/post\n",
		"comments_1.csv": "id,media\nfour,https://example.com/picture.png\n",
	} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	assert.Equal(8, summary.Added)
	for id, want := range map[string]string{
		"pm:pm":           "private text\nhttps://www.reddit.com/message/messages/abc",
		"post:post":       "post text\nhttps://example.com/link\nhttps://example.com/post",
		"post:same":       "https://example.com/same",
		"post:prefix":     "https://example.com/posts/1234\nhttps://example.com/posts/123",
		"comment:comment": "comment text\nhttps://example.com/media\nhttps://www.reddit.com/r/example/comments/abc/def",
		"post:three":      "https://example.com/post",
		"comment:four":    "https://example.com/picture.png",
	} {
		var messageID int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT id FROM messages WHERE source_message_id=?"), id).Scan(&messageID))
		body, err := st.GetMessageBodyText(t.Context(), messageID)
		require.NoError(err)
		assert.Equal(want, body)
	}
	results, err := query.NewEngine(st.DB(), st.IsPostgreSQL()).Search(t.Context(), search.Parse("example"), 20, 0)
	require.NoError(err)
	assert.Len(results, 6)
}

func TestImportOffsetDateFilter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title,date\noffset,early,2026-05-24T04:19:35+01:00\nutc,later,2026-05-24T03:50:00Z\n"), 0600))
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE sent_at < ?"), time.Date(2026, 5, 24, 3, 30, 0, 0, time.UTC)).Scan(&count))
	assert.Equal(1, count)
}
