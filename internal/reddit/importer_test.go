package reddit

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
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
	var parent string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT p.source_message_id FROM messages m JOIN messages p ON p.id=m.reply_to_message_id WHERE m.source_message_id='chat:c2'").Scan(&parent))
	assert.Equal("chat:c1", parent)
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
	assert.Equal([]string{"u/Fictional_Owner"}, evidence.AccountEvidence)
	assert.Equal([]string{"2026-06-01 01:02:03 UTC"}, evidence.ExportTimeEvidence)
	canonical, err := json.Marshal(evidence, json.Deterministic(true))
	require.NoError(err)
	assert.Equal(string(canonical), string(raw))
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

func TestImportSchemasAndOwner(t *testing.T) {
	for _, tc := range []struct{ name, file, data, me, want string }{
		{"missing owner", "posts.csv", "id,title\na,title\n", "", "owner"},
		{"missing header", "posts.csv", "id,body\na,body\n", "fictional_owner", "essential"},
		{"duplicate header", "posts.csv", "id,title,title\na,b,c\n", "fictional_owner", "header"},
		{"empty header", "posts.csv", "id,title,\na,b,c\n", "fictional_owner", "header"},
		{"zero byte", "posts.csv", "", "fictional_owner", "schema"},
		{"no content", "votes.csv", "id\na\n", "fictional_owner", "recognized"},
		{"invalid utf8", "posts.csv", "id,title\na,\xff\n", "fictional_owner", "UTF-8"},
		{"invalid utf8 wrong width", "posts.csv", "id,title\na,\xff,extra\n", "fictional_owner", "UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0600))
			_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: tc.me})
			require.ErrorContains(err, tc.want)
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

func TestImportReadLimitsAndSymlinks(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.Symlink(filepath.Join(t.TempDir(), "outside.csv"), filepath.Join(root, "posts.csv")))
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "recognized")
	require.NoError(os.Remove(filepath.Join(root, "posts.csv")))
	f, err := os.Create(filepath.Join(root, "posts.csv"))
	require.NoError(err)
	require.NoError(f.Truncate(maxCSVBytes+1))
	require.NoError(f.Close())
	_, err = Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "256 MiB")
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

func TestImportQuotingFailureRetainsCompletedRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\na,lantern\nb,\"unterminated\n"), 0600))
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "posts.csv")
	assert.Equal(1, summary.Added)
	require.NoError(os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\na,changed\nb,recovered\n"), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	assert.Equal(1, summary.Added)
	assert.Equal(1, summary.Unchanged)
}

func TestImportEntryBound(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	for i := range 10001 {
		require.NoError(os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%d", i)), nil, 0600))
	}
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "directory entries")
}

func TestImportPlainMarkerNamesArePeople(t *testing.T) {
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
			assert.Zero(summary.UnknownSenders)
		})
	}
}

func TestImportInvalidOwnerPreflight(t *testing.T) {
	for _, tc := range []struct{ name, me, statistics, want, split string }{
		{"flag URL", "https://example.com/user", "account name,fictional_owner\n", "--me", ""},
		{"flag wrapper", "/u/", "account name,fictional_owner\n", "--me", ""},
		{"statistics URL", "fictional_owner", "account name,https://example.com/user\n", "statistics.csv: record 1", ""},
		{"statistics wrapper", "fictional_owner", "account name,u/\n", "statistics.csv: record 1", ""},
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
	chat := "message_id,message,username,created_at\none,https://example.com/content,[deleted],bad\none,https://example.com/duplicate,[removed],bad\n"
	pm := "id,body,from,date\ntwo,https://example.com/content,[deleted],bad\n"
	for name, data := range map[string]string{"chat_history.csv": chat, "messages.csv": pm, "messages_archive.csv": pm} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	for run := range 2 {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
		require.NoError(err)
		assert.Equal(4, summary.UnknownDates)
		assert.Equal(4, summary.UnknownSenders)
		assert.Equal(4, summary.LinksNotDownloaded)
		if run == 0 {
			assert.Equal(2, summary.Added)
			assert.Equal(2, summary.Unchanged)
		} else {
			assert.Zero(summary.Added)
			assert.Equal(4, summary.Unchanged)
		}
	}
}

func TestImportContentLinkCoverage(t *testing.T) {
	for _, tc := range []struct {
		name, file, data string
		want             int
	}{
		{"identifying URLs", "chat_history.csv", "message_id,message,permalink,channel_url\none,plain,https://example.com/permalink,https://example.com/channel\n", 0},
		{"body URL", "comments.csv", "id,body\none,https://example.com/content\n", 1},
		{"message URL", "chat_history.csv", "message_id,message\none,https://example.com/content\n", 1},
		{"post URL", "posts.csv", "id,title,url\none,lantern,https://example.com/content\n", 1},
		{"explicit media", "comments.csv", "id,body,media\none,plain,fictional-media-id\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := t.TempDir()
			require.NoError(os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0600))
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.NoError(err)
			assert.Equal(tc.want, summary.LinksNotDownloaded)
		})
	}
}

func TestImportChatGroupingRemainsUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,channel_url,channel_name,conversation_type,thread_parent_message_id\none,lantern,fictional-channel,Fictional garden,group,\ntwo,reply,fictional-channel,Fictional garden,group,one\n"), 0600))
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var kind, title string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COALESCE(conversation_type,''),title FROM conversations").Scan(&kind, &title))
	assert.Empty(kind)
	assert.Equal("Fictional garden", title)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='chat:one'").Scan(&id))
	var reply int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT reply_to_message_id FROM messages WHERE source_message_id='chat:two'").Scan(&reply))
	assert.Equal(id, reply)
}

func TestImportFatalSchemaRetainsEarlierFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	for name, data := range map[string]string{"chat_history.csv": "message_id,message\none,lantern\n", "comments.csv": "id,renamed\ntwo,invalid\n", "posts.csv": "id,title\nthree,later\n"} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(err, "comments.csv")
	assert.Equal(1, summary.Added)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(1, n)
}

func TestImportPMKindDoesNotDependOnRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	st := testutil.NewTestStore(t)
	data := "id,body,from,to,thread_id\none,first,fictional_owner,fictional_peer,thread\ntwo,second,fictional_owner,,thread\nthree,recipient,fictional_owner,r/fictional_garden,thread\nfour,sender,/r/fictional_garden,fictional_owner,thread\n"
	require.NoError(os.WriteFile(filepath.Join(root, "messages.csv"), []byte(data), 0600))
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var kind string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT conversation_type FROM conversations").Scan(&kind))
	assert.Empty(kind)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='reddit' AND identifier_value IN ('r/fictional_garden','/r/fictional_garden')").Scan(&n))
	assert.Zero(n)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='pm:three'").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	var row csvRow
	require.NoError(json.Unmarshal(raw, &row))
	assert.Equal("r/fictional_garden", row.Columns["to"])
}

func TestImportSubredditTitleCanonical(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	for name, data := range map[string]string{"posts.csv": "id,title,subreddit\none,lantern,Fictional_Garden\n", "comments.csv": "id,body,subreddit\ntwo,comment,r/fictional_garden\n"} {
		require.NoError(os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(err)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations").Scan(&n))
	assert.Equal(1, n)
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT title FROM conversations WHERE source_conversation_id='subreddit:fictional_garden'").Scan(&title))
	assert.Equal("r/fictional_garden", title)
}
