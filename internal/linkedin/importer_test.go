package linkedin

import (
	"archive/zip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const ownerURL = "https://www.linkedin.com/in/alex-example/"

func exportZIP(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "export.zip")
	f, err := os.Create(file)
	require.NoError(t, err)
	z := zip.NewWriter(f)
	require.NoError(t, fs.WalkDir(os.DirFS("testdata/export"), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		w, err := z.Create("LinkedIn-export/" + path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join("testdata/export", path))
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}))
	require.NoError(t, z.Close())
	require.NoError(t, f.Close())
	return file
}

func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRow(query, args...).Scan(&n))
	return n
}

func TestImportExport(t *testing.T) {
	for _, zipped := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "zip"}[zipped], func(t *testing.T) {
			st := testutil.NewTestStore(t)
			path := "testdata/export"
			if zipped {
				path = exportZIP(t)
			}
			opts := Options{Identifier: "example-account", Me: ownerURL, DisplayName: "Alex Example"}
			summary, err := Import(t.Context(), st, path, opts)
			require.NoError(t, err)
			identities, err := st.ListAccountIdentities(summary.SourceID)
			require.NoError(t, err)
			require.Len(t, identities, 1)
			assert.Equal(t, strings.TrimSuffix(ownerURL, "/"), identities[0].Address)
			assert.Equal(t, int64(4), summary.MessagesAdded)
			assert.Equal(t, int64(1), summary.DraftsSkipped)
			assert.Equal(t, 4, count(t, st, "SELECT COUNT(*) FROM messages"))
			assert.Equal(t, 2, count(t, st, "SELECT COUNT(*) FROM conversations"))
			assert.Equal(t, 3, count(t, st, "SELECT COUNT(*) FROM messages WHERE is_from_me = TRUE"))
			assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM messages WHERE subject = 'Planning'"))
			assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM conversations WHERE title = 'Project chat'"))
			assert.Equal(t, 3, count(t, st, `SELECT COUNT(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = 'dm:2-example'`))
			assert.Equal(t, 5, count(t, st, "SELECT COUNT(*) FROM attachments"))
			assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM messages WHERE reply_to_message_id IS NOT NULL"))
			assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM labels WHERE name = 'INBOX'"))
			assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM participant_contact_observations WHERE original_value = 'morgan@example.com'"))
			var body string
			require.NoError(t, st.DB().QueryRow(`SELECT body_text FROM message_bodies WHERE message_id = (SELECT id FROM messages WHERE source_message_id LIKE 'comment:%')`).Scan(&body))
			assert.Equal(t, "A \"quoted\" reply\nNext line", body)
			again, err := Import(t.Context(), st, path, opts)
			require.NoError(t, err)
			assert.Zero(t, again.MessagesAdded)
			assert.Equal(t, int64(4), again.MessagesUpdated)
			assert.Equal(t, 4, count(t, st, "SELECT COUNT(*) FROM messages"))
			opts.IncludeDrafts = true
			withDrafts, err := Import(t.Context(), st, path, opts)
			require.NoError(t, err)
			assert.Equal(t, int64(1), withDrafts.MessagesAdded)
			assert.Equal(t, 5, count(t, st, "SELECT COUNT(*) FROM messages"))
			opts.Identifier = "second-source"
			_, err = Import(t.Context(), st, path, opts)
			require.NoError(t, err)
			assert.Equal(t, 10, count(t, st, "SELECT COUNT(*) FROM messages"))
		})
	}
}

func writeCSV(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
}

func TestImportNewerExport(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	header := "\ufeffCONTENT,DATE,SENDER PROFILE URL,CONVERSATION ID,FROM,NEW COLUMN\n"
	first := "first,2026-10-01 12:00:00 UTC," + ownerURL + ",thread,Alex Example,ignored\n"
	writeCSV(t, dir, "messages.csv", header+first)
	opts := Options{Identifier: "example", Me: ownerURL}
	sum, err := Import(t.Context(), st, dir, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MessagesAdded)
	writeCSV(t, dir, "messages.csv", header+"second,2026-10-02 12:00:00 UTC,"+ownerURL+",thread,Alex Example,ignored\n"+first)
	sum, err = Import(t.Context(), st, dir, opts)
	require.NoError(t, err)
	assert.Equal(t, int64(1), sum.MessagesAdded)
	assert.Equal(t, 2, count(t, st, "SELECT COUNT(*) FROM messages"))
}

func TestImportInvalidAndCancelled(t *testing.T) {
	for _, content := range []string{"Date,Message\ninvalid,text\n", "Date,Message\n2026-10-02 12:00:00 UTC,\"unterminated"} {
		st := testutil.NewTestStore(t)
		dir := t.TempDir()
		writeCSV(t, dir, "Comments.csv", content)
		_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "unterminated")
	}
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Import(ctx, st, "testdata/export", Options{Identifier: "example", Me: ownerURL})
	require.ErrorIs(t, err, context.Canceled)
	_, err = Import(t.Context(), st, t.TempDir(), Options{Identifier: "example", Me: ownerURL})
	require.ErrorContains(t, err, "no supported CSV")
	assert.Zero(t, count(t, st, "SELECT COUNT(*) FROM sources"))
}

func TestImportBindsOwnerAndResolvesPeopleByURL(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "Connections.csv", "First Name,Last Name,URL\nSame,Example,https://www.linkedin.com/in/one-example/\nSame,Example,https://www.linkedin.com/in/two-example/\n")
	opts := Options{Identifier: "example", Me: ownerURL}
	_, err := Import(t.Context(), st, dir, opts)
	require.NoError(t, err)
	assert.Zero(t, count(t, st, "SELECT COUNT(*) FROM messages"))
	assert.Equal(t, 2, count(t, st, "SELECT COUNT(*) FROM persons WHERE display_name = 'Same Example'"))
	opts.Me = "https://www.linkedin.com/in/another-example/"
	_, err = Import(t.Context(), st, dir, opts)
	require.ErrorContains(t, err, "different owner")
}

func TestImportMessageShardsAndLinkCommas(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	writeCSV(t, dir, "messages/part-1.csv", "CONVERSATION ID,DATE,CONTENT,SENDER PROFILE URL,ATTACHMENTS\nthread,2026-10-01 12:00:00 UTC,first,"+ownerURL+",\"https://files.example.com/asset?ids=1,2\"\n")
	_, err := Import(t.Context(), st, dir, Options{Identifier: "example", Me: ownerURL})
	require.NoError(t, err)
	assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM messages"))
	assert.Equal(t, 1, count(t, st, "SELECT COUNT(*) FROM attachments WHERE storage_path = 'https://files.example.com/asset?ids=1,2'"))
}

func TestRecipientsURLLists(t *testing.T) {
	for _, separator := range []string{"; ", ", ", ",", "\n"} {
		people := recipients(row{"recipient profile urls": "https://www.linkedin.com/in/one-example/" + separator + "https://www.linkedin.com/in/two-example/", "to": "One Example; Two Example"})
		require.Len(t, people, 2)
		for _, person := range people {
			_, err := ProfileURL(person.URL)
			require.NoError(t, err)
		}
	}
}
