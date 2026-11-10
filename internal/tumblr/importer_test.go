package tumblr

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTumblrImportReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Blog: "orchard-example", AttachmentsDir: t.TempDir()}
	summary, err := Import(t.Context(), st, "testdata/observed-shape", opts)
	require.NoError(err)
	assert.Equal(4, summary.MessagesAdded)
	assert.Equal("id:opaque-owner", summary.OwnerKey)
	summary, err = Import(t.Context(), st, overlapFixture(t, "orchard-example"), opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(4, summary.MessagesPresent)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'tumblr'").Scan(&n))
	assert.Equal(4, n)
	var processed, added int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sum(messages_processed), sum(messages_added) FROM sync_runs").Scan(&processed, &added))
	assert.Equal(8, processed)
	assert.Equal(4, added)
}

func TestTumblrMediaRefillAndRetention(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS("testdata/synthetic-media")))
	filename := filepath.Join(root, "conversations", "convo_700.html")
	data, err := os.ReadFile(filename)
	require.NoError(err)
	// First export has only a remote link for slot zero.
	remote := strings.ReplaceAll(string(data), "../media/picture.txt", "https://media.example/first")
	require.NoError(os.WriteFile(filename, []byte(remote), 0600))
	opts := ImportOptions{Blog: "orchard-example", AttachmentsDir: t.TempDir()}
	s, err := Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal(3, s.MessagesAdded)
	require.NoError(os.WriteFile(filename, data, 0600))
	s, err = Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Zero(s.MessagesAdded)
	assert.Equal(3, s.MessagesPresent)
	assert.Equal(1, s.MediaStored)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE is_from_me = true").Scan(&id))
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	require.Len(refs, 1)
	hash := refs[0].ContentHash
	assert.NotEmpty(hash)
	require.NoError(os.WriteFile(filename, []byte(remote), 0600))
	s, err = Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Zero(s.MediaStored)
	refs, err = st.MessageMIMEAttachmentsContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(hash, refs[0].ContentHash)
	raw, err := st.GetMessageRawContext(t.Context(), id)
	require.NoError(err)
	assert.Contains(string(raw), "https://media.example/first")
}
func TestTumblrScopeUnknownSenderAndNoDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Blog: "other-blog", AttachmentsDir: t.TempDir()}
	s, err := Import(t.Context(), st, "testdata/observed-shape", opts)
	require.ErrorContains(err, "other-blog")
	assert.Zero(s.SourceID)
	var sources int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM sources").Scan(&sources))
	assert.Zero(sources)
	assert.Equal(1, s.ForeignConversations)
	assert.Zero(s.MessagesAdded)
	opts.Blog = " ORCHARD-EXAMPLE "
	s, err = Import(t.Context(), st, "testdata/observed-shape", opts)
	require.NoError(err)
	source := s.SourceID
	opts.Blog = "river-example"
	s, err = Import(t.Context(), st, "testdata/observed-shape", opts)
	require.NoError(err)
	assert.NotEqual(source, s.SourceID)
	assert.Equal(4, s.MessagesAdded)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	data, err := os.ReadFile("testdata/observed-shape/conversations/convo_700.html")
	require.NoError(err)
	html := string(data)
	start := strings.Index(html, `<div class="container ">`)
	html = html[:start] + `<div class="container"><span class="message-info">unknown-example TEXT @ unknown</span><p>Unknown author</p></div></body></html>`
	require.NoError(os.WriteFile(filepath.Join(root, "conversations", "only.html"), []byte(html), 0600))
	opts.Blog = "orchard-example"
	s, err = Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal(1, s.MessagesAdded)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_id = ? AND sender_id IS NULL AND sent_at IS NULL"), source).Scan(&n))
	assert.Equal(1, n)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_id = ? AND deleted_from_source_at IS NULL"), source).Scan(&n))
	assert.Equal(5, n)
}
func TestTumblrMediaPathsAndBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	require.NoError(os.MkdirAll(filepath.Join(root, "media"), 0700))
	require.NoError(os.WriteFile(filepath.Join(root, "media", "zero.bin"), nil, 0600))
	f, err := os.Create(filepath.Join(root, "media", "large.bin"))
	require.NoError(err)
	require.NoError(f.Truncate(maxMediaBytes + 1))
	require.NoError(f.Close())
	r, err := openExport(t.Context(), root)
	require.NoError(err)
	defer func() { require.NoError(r.close()) }()
	m := &Message{Media: []string{"../media/zero.bin", "../media/missing.bin", "../media/large.bin", "../../escape", "/%2Fabsolute", "%2e%2e/%2e%2e/escape", "https://media.example/image", "../media/%7aero.bin", "data:text/plain,hello"}}
	s := &ImportSummary{}
	writes, err := mediaWrites(t.Context(), st, r, 0, "conversations/c.html", m, ImportOptions{AttachmentsDir: t.TempDir()}, s)
	require.NoError(err)
	assert.Equal(2, s.MediaStored)
	assert.Equal(1, s.MediaMissing)
	assert.Equal(1, s.MediaOversized)
	assert.Equal(4, s.MediaInvalid)
	assert.Equal(1, s.MediaRemote)
	require.Len(writes, len(m.Media))
	assert.NotEmpty(writes[0].ContentHash)
	assert.Zero(writes[0].Size)
	badDir := filepath.Join(t.TempDir(), "file")
	require.NoError(os.WriteFile(badDir, []byte("file"), 0600))
	_, err = mediaWrites(t.Context(), st, r, 0, "conversations/c.html", &Message{Media: []string{"../media/zero.bin"}}, ImportOptions{AttachmentsDir: badDir}, &ImportSummary{})
	require.ErrorContains(err, "store media")
}
func TestTumblrRealQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, "testdata/observed-shape", ImportOptions{Blog: "orchard-example"})
	require.NoError(err)
	if st.IsPostgreSQL() {
		t.Skip("SQLiteEngine reads SQLite only")
	} // SQLiteEngine is the live SQLite reader.
	engine := query.NewSQLiteEngine(st.DB())
	rows, err := engine.TextSearch(t.Context(), "Repeated", nil, 20, 0)
	require.NoError(err)
	assert.Len(rows, 2)
	rows, err = engine.TextSearch(t.Context(), "posts.example", nil, 20, 0)
	require.NoError(err)
	assert.Len(rows, 1)
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT body_text FROM message_bodies WHERE message_id = (SELECT id FROM messages WHERE snippet LIKE 'An example post%')").Scan(&body))
	assert.Contains(body, "https://posts.example/42")
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(err)
	assert.Len(conversations, 1)
}

// A real completed conversation triggers cancellation through the progress logger.
type cancelOnProgress struct{ cancel context.CancelFunc }

func (w cancelOnProgress) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestTumblrInterruptedAfterWritesThenReplay(t *testing.T) {
	for _, failure := range []string{"cancel", "parse"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
			data, err := os.ReadFile(filepath.Join(root, "conversations", "convo_700.html"))
			require.NoError(err)
			extra := filepath.Join(root, "conversations", "z.html")
			if failure == "parse" {
				data = []byte("<p>not a conversation</p>")
			} else {
				data = []byte(strings.ReplaceAll(string(data), "Conversation ID: 700", "Conversation ID: 701"))
			}
			require.NoError(os.WriteFile(extra, data, 0600))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := ImportOptions{Blog: "orchard-example"}
			if failure == "cancel" {
				opts.Logger = slog.New(slog.NewTextHandler(cancelOnProgress{cancel}, nil))
			}
			s, err := Import(ctx, st, root, opts)
			if failure == "cancel" {
				require.ErrorIs(err, context.Canceled)
			} else {
				require.ErrorContains(err, "Conversation ID")
				require.NoError(os.Remove(extra))
			}
			assert.Equal(4, s.MessagesAdded)
			var processed, added int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT messages_processed, messages_added FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&processed, &added))
			assert.Equal(4, processed)
			assert.Equal(4, added)
			opts.Logger = nil
			s, err = Import(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(4, s.MessagesPresent)
			if failure == "cancel" {
				assert.Equal(4, s.MessagesAdded)
			} else {
				assert.Zero(s.MessagesAdded)
			}
		})
	}
}
func TestTumblrNameFallbackAndNewObservations(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
	file := filepath.Join(root, "conversations", "convo_700.html")
	data, err := os.ReadFile(file)
	require.NoError(err)
	html := strings.ReplaceAll(strings.ReplaceAll(string(data), "opaque-owner", ""), "opaque-peer", "")
	require.NoError(os.WriteFile(file, []byte(html), 0600))
	opts := ImportOptions{Blog: "orchard-example"}
	s, err := Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal("blog:orchard-example:name:orchard-example", s.OwnerKey)
	html = strings.ReplaceAll(html, "Hello 🌳", "Changed observation 🌳")
	require.NoError(os.WriteFile(file, []byte(html), 0600))
	s, err = Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal(1, s.MessagesAdded)
	assert.Equal(3, s.MessagesPresent)
}

func TestTumblrOpaqueSenderIdentitySurvivesRename(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Blog: "orchard-example"}
	_, err := Import(t.Context(), st, "testdata/observed-shape", opts)
	require.NoError(err)
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
	file := filepath.Join(root, "conversations", "convo_700.html")
	data, err := os.ReadFile(file)
	require.NoError(err)
	require.NoError(os.WriteFile(file, []byte(strings.ReplaceAll(string(data), "river-example", "renamed-example")), 0600))
	s, err := Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Zero(s.MessagesAdded)
	assert.Equal(4, s.MessagesPresent)
}

func overlapFixture(t *testing.T, owner string) string {
	t.Helper()
	require := require.New(t)
	root := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	data, err := os.ReadFile("testdata/observed-shape/conversations/convo_700.html")
	require.NoError(err)
	parts := strings.Split(string(data), `<div class="container ">`)
	require.Len(parts, 5)
	parts[4] = strings.TrimSuffix(parts[4], "</body></html>\n")
	html := parts[0]
	for i := 4; i > 0; i-- {
		html += `<div class="container ">` + parts[i]
	}
	html += "</body></html>\n"
	require.NoError(os.WriteFile(filepath.Join(root, "conversations", "renamed.html"), []byte(strings.ReplaceAll(html, "orchard-example", owner)), 0600))
	return root
}

func TestTumblrOwnerRenameReusesSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	first, err := Import(t.Context(), st, "testdata/observed-shape", ImportOptions{Blog: "orchard-example"})
	require.NoError(err)
	renamed, err := Import(t.Context(), st, overlapFixture(t, "renamed-owner"), ImportOptions{Blog: "renamed-owner"})
	require.NoError(err)
	assert.Equal(first.SourceID, renamed.SourceID)
	assert.Equal(4, renamed.MessagesPresent)
	assert.Zero(renamed.MessagesAdded)
}

func TestTumblrMediaClassification(t *testing.T) {
	checks := require.New(t)
	verify := assert.New(t)
	for _, tc := range []struct{ ref, kind string }{
		{"https:broken", "invalid"}, {"https:///photo.jpg", "invalid"}, {"file://localhost/photo.jpg", "invalid"}, {"//", "invalid"},
		{"https://media.example/image", "remote"}, {"//media.example/image", "remote"}, {"../media/photo.JPG", "local"},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			_, kind := mediaPath("conversations/c.html", tc.ref)
			assert.Equal(t, tc.kind, kind)
		})
	}
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	checks.NoError(os.MkdirAll(filepath.Join(root, "conversations"), 0700))
	r, err := openExport(t.Context(), root)
	checks.NoError(err)
	defer func() { checks.NoError(r.close()) }()
	for _, tc := range []struct{ name, kind, mime string }{
		{"x.JPG", "image", "image/jpeg"}, {"x.mp4", "video", "video/mp4"}, {"x.mp3", "audio", "audio/mpeg"}, {"x.bin", "document", "application/octet-stream"},
	} {
		checks.NoError(os.WriteFile(filepath.Join(root, tc.name), nil, 0600))
		writes, err := mediaWrites(t.Context(), st, r, 0, "conversations/c.html", &Message{Media: []string{"../" + tc.name}}, ImportOptions{AttachmentsDir: t.TempDir()}, &ImportSummary{})
		checks.NoError(err)
		checks.Len(writes, 1)
		verify.Equal(tc.kind, writes[0].MediaType)
		verify.Equal(tc.mime, writes[0].MIMEType)
	}
	// No media means no attachment lookup, even when a message ID is supplied.
	_, err = mediaWrites(t.Context(), nil, r, 1, "conversations/c.html", &Message{}, ImportOptions{}, &ImportSummary{})
	checks.NoError(err)
}

func TestTumblrMediaChecksumDiagnostic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	_, err := z.Create("conversations/c.html")
	require.NoError(err)
	w, err := z.CreateHeader(&zip.FileHeader{Name: "media.bin", Method: zip.Store})
	require.NoError(err)
	_, err = w.Write([]byte("fictional payload"))
	require.NoError(err)
	require.NoError(z.Close())
	data := b.Bytes()
	data[bytes.Index(data, []byte("fictional payload"))] ^= 1
	file := filepath.Join(t.TempDir(), "corrupt.zip")
	require.NoError(os.WriteFile(file, data, 0600))
	r, err := openExport(t.Context(), file)
	require.NoError(err)
	defer func() { require.NoError(r.close()) }()
	_, err = mediaWrites(t.Context(), testutil.NewTestStore(t), r, 0, "conversations/c.html", &Message{Media: []string{"../media.bin"}}, ImportOptions{AttachmentsDir: t.TempDir()}, &ImportSummary{})
	require.ErrorIs(err, zip.ErrChecksum)
	assert.ErrorContains(err, "conversations/c.html")
	assert.ErrorContains(err, "../media.bin")
}

func TestTumblrLargeZIP(t *testing.T) {
	require := require.New(t)
	file := filepath.Join(t.TempDir(), "large.zip")
	entries := make([]testutil.ArchiveEntry, 100001)
	for i := range entries {
		entries[i].Name = fmt.Sprintf("conversations/%06d.txt", i)
	}
	testutil.CreateZip(t, file, entries)
	r, err := openExport(t.Context(), file)
	require.NoError(err)
	assert.Equal(t, 100001, r.skipped)
	require.NoError(r.close())
}
