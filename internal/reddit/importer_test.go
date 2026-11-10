package reddit

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportFictionalExport(t *testing.T) {
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/export"})
	require.NoError(t, err)
	assert.Equal(t, 6, summary.Added)
	assert.Equal(t, 2, summary.Unchanged)
	assert.Contains(t, summary.Unsupported, "votes.csv")
	assert.Equal(t, 4, summary.UnknownDates)
	assert.Equal(t, 2, summary.UnknownSenders)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type IN ('reddit_chat','reddit_pm','reddit_post','reddit_comment')").Scan(&n))
	assert.Equal(t, 6, n)
	var parent string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT p.source_message_id FROM messages m JOIN messages p ON p.id=m.reply_to_message_id WHERE m.source_message_id='chat:c2'").Scan(&parent))
	assert.Equal(t, "chat:c1", parent)
	var date sql.NullTime
	var sender sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT sent_at,sender_id FROM messages WHERE source_message_id='chat:c3'").Scan(&date, &sender))
	assert.False(t, date.Valid)
	assert.False(t, sender.Valid)
	var rawID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='chat:c2'").Scan(&rawID))
	raw, err := st.GetMessageRawContext(t.Context(), rawID)
	require.NoError(t, err)
	var evidence struct {
		Filename string            `json:"filename"`
		Columns  map[string]string `json:"columns"`
	}
	require.NoError(t, json.Unmarshal(raw, &evidence))
	assert.Equal(t, "chat_history.csv", evidence.Filename)
	assert.Equal(t, "lantern chat\nsecond line", evidence.Columns["message"])
	_, present := evidence.Columns["ip"]
	assert.False(t, present)
	again, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/export"})
	require.NoError(t, err)
	assert.Zero(t, again.Added)
	assert.Equal(t, 8, again.Unchanged)
	other, err := Import(t.Context(), st, ImportOptions{RootDir: "testdata/export", Me: "other_owner"})
	require.ErrorContains(t, err, "account")
	assert.Zero(t, other.SourceID)
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
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0600))
			_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: tc.me})
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestImportBOMMalformedAndEmpty(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("\ufefftitle,id,extra\n\"first\nline\",a,original\nmissing\n,no-id,\n"), 0600))
	summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "/u/Fictional_Owner"})
	require.NoError(t, err)
	assert.Equal(t, 2, summary.Added)
	assert.Equal(t, 1, summary.Malformed)
	require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\n"), 0600))
	summary, err = Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(t, err)
	assert.Zero(t, summary.Added)
}

func TestImportReadLimitsAndSymlinks(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "outside.csv"), filepath.Join(root, "posts.csv")))
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(t, err, "recognized")
	require.NoError(t, os.Remove(filepath.Join(root, "posts.csv")))
	f, err := os.Create(filepath.Join(root, "posts.csv"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(maxCSVBytes+1))
	require.NoError(t, f.Close())
	_, err = Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(t, err, "256 MiB")
}

type cancelWriter struct{ cancel context.CancelFunc }

func (w cancelWriter) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestImportInterruptedRepeatAndOriginalRetention(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,thread_parent_message_id\nchild,original,parent\nparent,parent body,\n"), 0600))
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "fictional_owner", Logger: slog.New(slog.NewTextHandler(cancelWriter{cancel}, nil))})
	require.ErrorIs(t, err, context.Canceled)
	assert.Positive(t, summary.Added)
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	require.NoError(t, err)
	var parent string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT p.source_message_id FROM messages m JOIN messages p ON p.id=m.reply_to_message_id WHERE m.source_message_id='chat:child'").Scan(&parent))
	assert.Equal(t, "chat:parent", parent)
	require.NoError(t, os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message\nchild,changed\n"), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(t, err)
	assert.Zero(t, summary.Added)
	var snippet string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT snippet FROM messages WHERE source_message_id='chat:child'").Scan(&snippet))
	assert.Equal(t, "original", snippet)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(t, 2, n)
}

func TestImportAccountIsolationAndStatisticsConflict(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "comments.csv"), []byte("id,body\nsame,lantern\n,missing key\n"), 0600))
	st := testutil.NewTestStore(t)
	first, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "u/fictional_one"})
	require.NoError(t, err)
	assert.Equal(t, 1, first.MissingIDs)
	second, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_two"})
	require.NoError(t, err)
	assert.NotEqual(t, first.SourceID, second.SourceID)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE source_message_id='comment:same'").Scan(&n))
	assert.Equal(t, 2, n)
	require.NoError(t, os.WriteFile(filepath.Join(root, "statistics.csv"), []byte("statistic,value\naccount name,fictional_one\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "statistics_1.csv"), []byte("statistic,value\naccount name,fictional_two\n"), 0600))
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root})
	require.ErrorContains(t, err, "account")
	assert.Zero(t, summary.SourceID)
}

func TestImportQuotingFailureRetainsCompletedRows(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\na,lantern\nb,\"unterminated\n"), 0600))
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(t, err, "posts.csv")
	assert.Equal(t, 1, summary.Added)
	require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\na,changed\nb,recovered\n"), 0600))
	summary, err = Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Added)
	assert.Equal(t, 1, summary.Unchanged)
}

func TestImportEntryBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10001; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(root, fmt.Sprintf("entry-%d", i)), nil, 0600))
	}
	_, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(t, err, "directory entries")
}

func TestImportPMSubredditEndpointIsNotAPerson(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "messages.csv"), []byte("id,body,from,to\none,lantern,fictional_owner,r/fictional_garden\ntwo,lantern,/r/fictional_garden,fictional_owner\n"), 0600))
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(t, err)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='reddit' AND identifier_value IN ('r/fictional_garden','/r/fictional_garden')").Scan(&n))
	assert.Zero(t, n)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM conversations WHERE conversation_type='direct_chat'").Scan(&n))
	assert.Zero(t, n)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='pm:one'").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	var row struct {
		Columns map[string]string `json:"columns"`
	}
	require.NoError(t, json.Unmarshal(raw, &row))
	assert.Equal(t, "r/fictional_garden", row.Columns["to"])
}

func TestImportPlainMarkerNamesArePeople(t *testing.T) {
	for _, owner := range []string{"unknown", "deleted", "a", "Legacy-Name_1"} {
		t.Run(owner, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "messages.csv"), []byte("id,body,from,to\none,lantern,u/unknown,/u/deleted\n"), 0600))
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: owner})
			require.NoError(t, err)
			var account string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT identifier FROM sources WHERE id=?", summary.SourceID).Scan(&account))
			assert.Equal(t, strings.ToLower(owner), account)
			var n int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM participant_identifiers WHERE identifier_type='reddit' AND identifier_value IN ('unknown','deleted')").Scan(&n))
			assert.Equal(t, 2, n)
			assert.Zero(t, summary.UnknownSenders)
		})
	}
}

func TestImportInvalidOwnerPreflight(t *testing.T) {
	for _, tc := range []struct{ name, me, statistics, want string }{
		{"flag URL", "https://example.com/user", "account name,fictional_owner\n", "--me"},
		{"flag space", "two words", "", "--me"},
		{"flag subreddit", "r/garden", "account name,fictional_owner\n", "--me"},
		{"flag wrapper", "/u/", "account name,fictional_owner\n", "--me"},
		{"statistics URL", "fictional_owner", "account name,https://example.com/user\n", "statistics.csv: record 1"},
		{"statistics space", "", "account name,two words\n", "statistics.csv: record 1"},
		{"statistics subreddit", "fictional_owner", "account name,/r/garden\n", "statistics.csv: record 1"},
		{"statistics wrapper", "fictional_owner", "account name,u/\n", "statistics.csv: record 1"},
		{"statistics nonASCII", "", "account name,fictional_é\n", "statistics.csv: record 1"},
		{"flag Unicode folds to ASCII", "\u212A", "", "--me"},
		{"statistics wrong width", "fictional_owner", "account name,other_owner,extra\n", "statistics.csv: record 1"},
		{"statistics malformed later record", "fictional_owner", "export time,fictional snapshot\naccount name,other_owner,extra\n", "statistics.csv: record 2"},
		{"statistics invalid UTF8 wrong width", "fictional_owner", "account name,\xff,extra\n", "statistics.csv: record 1"},
		{"statistics quoting", "fictional_owner", "account name,\"unterminated\n", "statistics.csv: record 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\none,lantern\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "statistics.csv"), []byte("statistic,value\n"+tc.statistics), 0600))
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: tc.me})
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, summary.SourceID)
			var n int
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sources").Scan(&n))
			assert.Zero(t, n)
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
			assert.Zero(t, n)
		})
	}
}

func TestImportCoverageCountsExistingRecords(t *testing.T) {
	root := t.TempDir()
	chat := "message_id,message,username,created_at\none,https://example.com/content,[deleted],bad\none,https://example.com/duplicate,[removed],bad\n"
	pm := "id,body,from,date\ntwo,https://example.com/content,[deleted],bad\n"
	for name, data := range map[string]string{"chat_history.csv": chat, "messages.csv": pm, "messages_archive.csv": pm} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	for run := range 2 {
		summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
		require.NoError(t, err)
		assert.Equal(t, 4, summary.UnknownDates)
		assert.Equal(t, 4, summary.UnknownSenders)
		assert.Equal(t, 4, summary.LinksNotDownloaded)
		if run == 0 {
			assert.Equal(t, 2, summary.Added)
			assert.Equal(t, 2, summary.Unchanged)
		} else {
			assert.Zero(t, summary.Added)
			assert.Equal(t, 4, summary.Unchanged)
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
		{"explicit media", "messages.csv", "id,body,media\none,plain,fictional-media-id\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, tc.file), []byte(tc.data), 0600))
			summary, err := Import(t.Context(), testutil.NewTestStore(t), ImportOptions{RootDir: root, Me: "fictional_owner"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, summary.LinksNotDownloaded)
		})
	}
}

func TestImportChatGroupingRemainsUnknown(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "chat_history.csv"), []byte("message_id,message,channel_url,channel_name,conversation_type,thread_parent_message_id\none,lantern,fictional-channel,Fictional garden,group,\ntwo,reply,fictional-channel,Fictional garden,group,one\n"), 0600))
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.NoError(t, err)
	var kind, title string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COALESCE(conversation_type,''),title FROM conversations").Scan(&kind, &title))
	assert.Empty(t, kind)
	assert.Equal(t, "Fictional garden", title)
	var metadata sql.NullString
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM messages WHERE source_message_id='chat:one'").Scan(&metadata))
	assert.False(t, metadata.Valid)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM messages WHERE source_message_id='chat:two'").Scan(&metadata))
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadata.String), &fields))
	assert.Equal(t, map[string]any{"referenced_message_id": "chat:one"}, fields)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE source_message_id='chat:one'").Scan(&id))
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(t, err)
	var row csvRow
	require.NoError(t, json.Unmarshal(raw, &row))
	assert.Equal(t, "group", row.Columns["conversation_type"])
	var reply int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT reply_to_message_id FROM messages WHERE source_message_id='chat:two'").Scan(&reply))
	assert.Equal(t, id, reply)
}

func TestImportFatalSchemaRetainsEarlierFiles(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{"chat_history.csv": "message_id,message\none,lantern\n", "comments.csv": "id,renamed\ntwo,invalid\n", "posts.csv": "id,title\nthree,later\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(data), 0600))
	}
	st := testutil.NewTestStore(t)
	summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: "fictional_owner"})
	require.ErrorContains(t, err, "comments.csv")
	assert.Equal(t, 1, summary.Added)
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages").Scan(&n))
	assert.Equal(t, 1, n)
}

func TestImportStatisticsMarkerNamesAndEmptyFallback(t *testing.T) {
	for _, owner := range []string{"unknown", "deleted", ""} {
		t.Run("account="+owner, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "statistics.csv"), []byte("statistic,value\naccount name,"+owner+"\n"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "posts.csv"), []byte("id,title\none,lantern\n"), 0600))
			me := ""
			want := owner
			if owner == "" {
				me = "fictional_owner"
				want = me
			}
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{RootDir: root, Me: me})
			require.NoError(t, err)
			var account string
			require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT identifier FROM sources WHERE id=?", summary.SourceID).Scan(&account))
			assert.Equal(t, want, account)
		})
	}
}
