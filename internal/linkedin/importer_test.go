package linkedin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const ownerURL = "https://www.linkedin.com/in/alex-example/"

func exportZIP(t *testing.T) string {
	t.Helper()
	require := require.New(t)
	file := filepath.Join(t.TempDir(), "export.zip")
	f, err := os.Create(file)
	require.NoError(err)
	z := zip.NewWriter(f)
	require.NoError(fs.WalkDir(os.DirFS("testdata/export"), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk fixture export: %w", err)
		}
		if entry.IsDir() {
			return nil
		}
		w, err := z.Create("LinkedIn-export/" + path)
		if err != nil {
			return fmt.Errorf("create ZIP fixture entry: %w", err)
		}
		data, err := os.ReadFile(filepath.Join("testdata/export", path))
		if err != nil {
			return fmt.Errorf("read fixture CSV: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write ZIP fixture entry: %w", err)
		}
		return nil
	}))
	require.NoError(z.Close())
	require.NoError(f.Close())
	return file
}

func count(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	require := require.New(t)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), query).Scan(&n))
	return n
}

func TestImportExport(t *testing.T) {
	for _, zipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "zip"}[zipped], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			path := "testdata/export"
			if zipped {
				path = exportZIP(t)
			}
			opts := Options{Identifier: "example-account", Me: ownerURL, DisplayName: "Alex Example"}
			summary, err := Import(t.Context(), st, path, opts)
			require.NoError(err)
			identities, err := st.ListAccountIdentities(summary.SourceID)
			require.NoError(err)
			require.Len(identities, 1)
			assert.Equal(strings.TrimSuffix(ownerURL, "/"), identities[0].Address)
			assert.Equal(int64(4), summary.MessagesAdded)
			assert.Equal(int64(1), summary.DraftsSkipped)
			assert.Equal(4, count(t, st, "SELECT COUNT(*) FROM messages"))
			assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM conversations"))
			assert.Equal(3, count(t, st, "SELECT COUNT(*) FROM messages WHERE is_from_me = TRUE"))
			assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages WHERE subject = 'Planning'"))
			assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM conversations WHERE title = 'Project chat'"))
			assert.Equal(3, count(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = 'dm:2-example'`))
			assert.Equal(5, count(t, st, "SELECT COUNT(*) FROM attachments"))
			assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
			assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM labels WHERE name = 'INBOX'"))
			assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM participant_contact_observations WHERE original_value = 'morgan@example.com'"))
			var body string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies WHERE message_id = (SELECT id FROM messages WHERE source_message_id LIKE 'comment:%')`).Scan(&body))
			assert.Equal("A \"quoted\" reply\nNext line", body)
			again, err := Import(t.Context(), st, path, opts)
			require.NoError(err)
			assert.Zero(again.MessagesAdded)
			assert.Equal(int64(4), again.MessagesUpdated)
			assert.Equal(4, count(t, st, "SELECT COUNT(*) FROM messages"))
			opts.IncludeDrafts = true
			withDrafts, err := Import(t.Context(), st, path, opts)
			require.NoError(err)
			assert.Equal(int64(1), withDrafts.MessagesAdded)
			assert.Equal(5, count(t, st, "SELECT COUNT(*) FROM messages"))
			opts.Identifier = "second-source"
			_, err = Import(t.Context(), st, path, opts)
			require.NoError(err)
			assert.Equal(10, count(t, st, "SELECT COUNT(*) FROM messages"))
		})
	}
}

func writeCSV(t *testing.T, dir, name, content string) {
	t.Helper()
	require := require.New(t)
	require.NoError(os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700))
	require.NoError(os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
}

func TestImportNewerExport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	header := "\ufeffCONTENT,DATE,SENDER PROFILE URL,CONVERSATION ID,FROM,NEW COLUMN\n"
	first := "first,2026-10-01 12:00:00 UTC," + ownerURL + ",thread,Alex Example,ignored\n"
	writeCSV(t, dir, "messages.csv", header+first)
	opts := Options{Identifier: "example", Me: ownerURL}
	sum, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesAdded)
	writeCSV(t, dir, "messages.csv", header+"second,2026-10-02 12:00:00 UTC,"+ownerURL+",thread,Alex Example,ignored\n"+first)
	sum, err = Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(int64(1), sum.MessagesAdded)
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM messages"))
}

func TestImportInvalidAndCancelled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, content := range []string{"Date,Message\ninvalid,text\n", "Date,Message\n2026-10-02 12:00:00 UTC,\"unterminated"} {
		st := testutil.NewTestStore(t)
		dir := t.TempDir()
		writeCSV(t, dir, "Comments.csv", content)
		_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
		require.Error(err)
		assert.NotContains(err.Error(), "unterminated")
	}
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Import(ctx, st, "testdata/export", Options{Identifier: "example", Me: ownerURL})
	require.ErrorIs(err, context.Canceled)
	_, err = Import(t.Context(), st, t.TempDir(), Options{Identifier: "example", Me: ownerURL})
	require.ErrorContains(err, "no supported CSV")
	assert.Zero(count(t, st, "SELECT COUNT(*) FROM sources"))
}

func TestImportBindsOwnerAndResolvesPeopleByURL(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "Connections.csv", "First Name,Last Name,URL\nSame,Example,https://www.linkedin.com/in/one-example/\nSame,Example,https://www.linkedin.com/in/two-example/\n")
	opts := Options{Identifier: "example", Me: ownerURL}
	_, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(count(t, st, "SELECT COUNT(*) FROM messages"))
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM persons WHERE display_name = 'Same Example'"))
	opts.Me = "https://www.linkedin.com/in/another-example/"
	_, err = Import(t.Context(), st, dir, opts)
	require.ErrorContains(err, "different owner")
}

func TestImportMessageShardsAndLinkCommas(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "messages/part-1.csv", "CONVERSATION ID,DATE,CONTENT,SENDER PROFILE URL,ATTACHMENTS\nthread,2026-10-01 12:00:00 UTC,first,"+ownerURL+",\"https://files.example.com/asset?ids=1,2\"\n")
	_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
	require.NoError(err)
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages"))
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM attachments WHERE storage_path = 'https://files.example.com/asset?ids=1,2'"))
}

func TestRecipientsURLLists(t *testing.T) {
	require := require.New(t)
	for _, separator := range []string{"; ", ", ", ",", "\n"} {
		people, err := recipients(row{"recipient profile urls": "https://www.linkedin.com/in/one-example/" + separator + "https://www.linkedin.com/in/two-example/", "to": "One Example; Two Example"})
		require.NoError(err)
		require.Len(people, 2)
		for _, person := range people {
			_, err := ProfileURL(person.URL)
			require.NoError(err)
		}
	}
}

func writeMessageRows(t *testing.T, dir string, rows [][]string) {
	t.Helper()
	require := require.New(t)
	var data bytes.Buffer
	writer := csv.NewWriter(&data)
	require.NoError(writer.WriteAll(append([][]string{{"CONVERSATION ID", "DATE", "CONTENT", "SENDER PROFILE URL", "TO", "RECIPIENT PROFILE URLS", "ATTACHMENTS"}}, rows...)))
	writeCSV(t, dir, "messages.csv", data.String())
}

func TestImportDistinctAttachmentOnlyMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	rows := [][]string{
		{"thread", "2026-10-01 12:00:00 UTC", "", ownerURL, "Morgan Example", "https://www.linkedin.com/in/morgan-example/", "https://files.example.com/one.pdf"},
		{"thread", "2026-10-01 12:00:00 UTC", "", ownerURL, "Morgan Example", "https://www.linkedin.com/in/morgan-example/", "https://files.example.com/two.pdf"},
	}
	writeMessageRows(t, dir, rows)
	opts := Options{Identifier: "example", Me: ownerURL}
	sum, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesAdded)
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM messages WHERE has_attachments = TRUE AND attachment_count = 1"))
	results, total, err := st.SearchMessagesQuery(search.Parse("has:attachment"), 0, 100)
	require.NoError(err)
	assert.EqualValues(2, total)
	assert.Len(results, 2)
	writeMessageRows(t, dir, [][]string{rows[1], rows[0]})
	sum, err = Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(sum.MessagesAdded)
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM messages"))
}

func TestImportFallbackKeysIncludeRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	rows := [][]string{
		{"", "2026-10-01 12:00:00 UTC", "Same text", ownerURL, "One Example", "https://www.linkedin.com/in/one-example/", ""},
		{"", "2026-10-01 12:00:00 UTC", "Same text", ownerURL, "Two Example", "https://www.linkedin.com/in/two-example/", ""},
		{"", "2026-10-01 12:00:00 UTC", "Same text", ownerURL, "One Example; Two Example", "https://www.linkedin.com/in/one-example/; https://www.linkedin.com/in/two-example/", ""},
	}
	writeMessageRows(t, dir, rows)
	opts := Options{Identifier: "example", Me: ownerURL}
	sum, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(int64(3), sum.MessagesAdded)
	assert.Equal(3, count(t, st, "SELECT COUNT(*) FROM conversations"))
	rows[2][4] = "Two Example; One Example"
	rows[2][5] = "https://www.linkedin.com/in/two-example/; https://www.linkedin.com/in/one-example/"
	writeMessageRows(t, dir, [][]string{rows[2], rows[0], rows[1]})
	sum, err = Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(sum.MessagesAdded)
	assert.Equal(3, count(t, st, "SELECT COUNT(*) FROM messages"))
}

func TestImportPreservesPartialRecipientURLs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeMessageRows(t, dir, [][]string{{"thread", "2026-10-01 12:00:00 UTC", "Hello", ownerURL, "One Example; Two Example; Three Example", "https://www.linkedin.com/in/one-example/;;https://www.linkedin.com/in/three-example/", ""}})
	_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
	require.NoError(err)
	assert.Equal(4, count(t, st, "SELECT COUNT(*) FROM conversation_participants"))
	assert.Equal(3, count(t, st, "SELECT COUNT(*) FROM message_recipients WHERE recipient_type = 'to' AND display_name != ''"))
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM conversations WHERE conversation_type = 'group_chat'"))
	writeMessageRows(t, dir, [][]string{{"thread", "2026-10-02 12:00:00 UTC", "Ambiguous", ownerURL, "One Example; Two Example; Three Example", "https://www.linkedin.com/in/one-example/;https://www.linkedin.com/in/three-example/", ""}})
	_, err = Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
	require.ErrorContains(err, "recipient")
}

func TestImportLinksPreviouslyArchivedCommentToPost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	link := "https://www.linkedin.com/feed/update/urn:li:activity:123456/"
	writeCSV(t, dir, "Comments.csv", "Date,Link,Message\n2026-10-02 13:00:00 UTC,"+link+",A reply\n")
	opts := Options{Identifier: "example", Me: ownerURL}
	_, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
	writeCSV(t, dir, "Shares.csv", "Date,ShareLink,ShareCommentary\n2026-10-02 12:00:00 UTC,"+link+",A post\n")
	_, err = Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM messages WHERE has_attachments = TRUE AND attachment_count = 1"))
}

func TestImportPostsWithoutPermalinksKeepDistinctMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "Shares.csv", "Date,ShareCommentary,MediaUrl\n2026-10-02 12:00:00 UTC,An update,https://files.example.com/one.jpg\n2026-10-02 12:00:00 UTC,An update,https://files.example.com/two.jpg\n")
	opts := Options{Identifier: "example", Me: ownerURL}
	sum, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Equal(int64(2), sum.MessagesAdded)
	sum, err = Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(sum.MessagesAdded)
	assert.Equal(2, count(t, st, "SELECT COUNT(*) FROM messages"))
}

func TestImportLinklessCommentsNeverReplyToThemselves(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "Comments.csv", "Date,Message\n2026-10-02 12:00:00 UTC,A linkless comment\n")
	opts := Options{Identifier: "example", Me: ownerURL}
	for range 2 {
		_, err := Import(t.Context(), st, dir, opts)
		require.NoError(err)
	}
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages"))
	assert.Zero(count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages").Scan(&id))
	require.NoError(st.SetMessageReplyContext(t.Context(), id, id))
	_, err := Import(t.Context(), st, dir, opts)
	require.NoError(err)
	assert.Zero(count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
}

func TestImportMalformedCommentsPreservesEarlierStandardRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	value := `Comma, literal C:\new\thing`
	var data bytes.Buffer
	writer := csv.NewWriter(&data)
	require.NoError(writer.WriteAll([][]string{{"Date", "Link", "Message"}, {"2026-10-02 12:00:00 UTC", "https://example.com/post", value}}))
	data.WriteString("2026-10-02 13:00:00 UTC,https://example.com/post,\"unterminated")
	writeCSV(t, dir, "Comments.csv", data.String())
	_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
	require.ErrorContains(err, "invalid CSV at record 3")
	assert.Equal(1, count(t, st, "SELECT COUNT(*) FROM messages"))
	var id int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&id))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), id).Scan(&body))
	assert.Equal(value, body)
}
