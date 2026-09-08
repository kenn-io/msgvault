package importer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Groups exports may have thread IDs without References or In-Reply-To.
// Losing those IDs splits replies; losing group labels makes a multi-group
// archive impossible to filter by group.
func TestImportMbox_GoogleGroups(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	var data strings.Builder
	for i, group := range []string{"test-group", "test-group", "other-group", "test-group"} {
		threadID := "123456"
		if i == 3 {
			threadID = "654321"
		}
		fmt.Fprintf(&data, "From synthetic@example.invalid Mon Jan 1 12:00:00 +0200 2024\r\nFrom: Alice <alice@example.com>\r\nTo: %s@googlegroups.com\r\nMessage-ID: <message-%d@example.com>\r\nX-Google-Groups: %s\r\nX-GM-THRID: %s\r\nX-Gmail-Labels: Starred, =?UTF-8?Q?R=C3=A9solu?=\r\nSubject: Topic %d\r\n\r\nSynthetic message %d.\r\n\r\n", group, i, group, threadID, i, i)
	}
	path := filepath.Join(tmp, "topics.mbox")
	require.NoError(os.WriteFile(path, []byte(data.String()), 0600))
	opts := MboxImportOptions{SourceType: "google-groups", Identifier: "groups-export", Labels: []string{"archive"}, NoResume: true}
	summary, err := ImportMbox(context.Background(), st, path, opts)
	require.NoError(err)
	assert.Equal(int64(4), summary.MessagesAdded)
	assert.Zero(summary.Errors)
	var conversations int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM conversations").Scan(&conversations))
	assert.Equal(3, conversations, "same group and thread should join; different groups should stay separate")
	rows, err := st.DB().Query("SELECT name FROM labels ORDER BY name")
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var labels []string
	for rows.Next() {
		var name string
		require.NoError(rows.Scan(&name))
		labels = append(labels, name)
	}
	require.NoError(rows.Err())
	assert.ElementsMatch([]string{"archive", "test-group", "other-group", "Starred", "Résolu"}, labels)
	var sent sql.NullTime
	require.NoError(st.DB().QueryRow("SELECT sent_at FROM messages LIMIT 1").Scan(&sent))
	require.True(sent.Valid)
	assert.True(sent.Time.Equal(time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)))
	// Reimport restores metadata labels even for messages whose raw MIME exists.
	_, err = st.DB().Exec("DELETE FROM message_labels")
	require.NoError(err)
	summary, err = ImportMbox(context.Background(), st, path, opts)
	require.NoError(err)
	assert.Equal(int64(4), summary.MessagesSkipped)
	var links int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM message_labels").Scan(&links))
	assert.Equal(16, links)
}

func TestImportMbox_GoogleGroupsHeadersInOrdinaryMail(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	var data strings.Builder
	for i := range 2 {
		fmt.Fprintf(&data, "From synthetic@example.invalid Mon Jan 1 12:00:00 +0200 2024\r\nFrom: Alice <alice@example.com>\r\nTo: user@example.com\r\nMessage-ID: <message-%d@example.com>\r\nReferences: <root@example.com>\r\nX-GM-THRID: 123456\r\nX-Gmail-Labels: Inbox,Important\r\n", i)
		if i == 0 {
			data.WriteString("X-BeenThere: test-group@googlegroups.com\r\n")
		}
		data.WriteString("Subject: Re: Topic\r\n\r\nSynthetic reply.\r\n\r\n")
	}
	path := filepath.Join(tmp, "mail.mbox")
	require.NoError(os.WriteFile(path, []byte(data.String()), 0600))
	summary, err := ImportMbox(context.Background(), st, path, MboxImportOptions{Identifier: "user@example.com"})
	require.NoError(err)
	require.Equal(int64(2), summary.MessagesAdded)
	var conversations, labels int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM conversations").Scan(&conversations))
	assert.Equal(t, 1, conversations, "on-list and off-list replies must retain ordinary email threading")
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM labels").Scan(&labels))
	assert.Zero(t, labels, "Groups metadata must require an explicit Groups import")
}

func TestImportMbox_GoogleGroupsHeaderFallbackKeepsThread(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	var data strings.Builder
	for i, header := range []string{
		"X-Google-Groups: TeSt-GrOuP\r\n",
		"X-BeenThere: Test-Group@googlegroups.com\r\n",
		"",
	} {
		fmt.Fprintf(&data, "From synthetic@example.invalid Mon Jan 1 12:00:00 +0000 2024\r\nFrom: Alice <alice@example.com>\r\nMessage-ID: <message-%d@example.com>\r\nX-GM-THRID: 123\r\n%sSubject: Topic\r\n\r\nSynthetic message.\r\n\r\n", i, header)
	}
	path := filepath.Join(t.TempDir(), "topics.mbox")
	require.NoError(os.WriteFile(path, []byte(data.String()), 0600))
	summary, err := ImportMbox(t.Context(), st, path, MboxImportOptions{
		SourceType: "google-groups", Identifier: "TEST-GROUP@GoogleGroups.com",
	})
	require.NoError(err)
	require.Equal(int64(3), summary.MessagesAdded)
	var conversations int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM conversations").Scan(&conversations))
	assert.Equal(1, conversations, "header and fallback identities must share a thread")
	var groupLabels int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM message_labels ml JOIN labels l ON l.id = ml.label_id WHERE l.name = 'test-group'").Scan(&groupLabels))
	assert.Equal(3, groupLabels, "every message must retain the same group label")
}
