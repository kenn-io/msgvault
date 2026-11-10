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
	for _, renamed := range []string{"none", "owner", "peer"} {
		t.Run(renamed, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			opts := ImportOptions{Blog: "orchard-example", AttachmentsDir: t.TempDir()}
			first, err := Import(t.Context(), st, "testdata/observed-shape", opts)
			require.NoError(err)
			assert.Equal(4, first.MessagesAdded)
			assert.Equal("id:opaque-owner", first.OwnerKey)
			var key string
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_message_id FROM messages WHERE snippet LIKE 'Hello%'").Scan(&key))
			assert.Equal("v1:bea5cff4f42d91da04b28213e31603594695ab1dc1fd511af436f50d77929fa8:1", key)
			var sender int64
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id FROM messages WHERE snippet LIKE 'An example post%'").Scan(&sender))
			if renamed == "owner" {
				opts.Blog = "renamed-owner"
			}
			root := overlapFixture(t, opts.Blog)
			if renamed == "peer" {
				file := filepath.Join(root, "conversations", "renamed.html")
				data, err := os.ReadFile(file)
				require.NoError(err)
				require.NoError(os.WriteFile(file, []byte(strings.ReplaceAll(string(data), "river-example", "renamed-peer")), 0600))
			}
			summary, err := Import(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(first.SourceID, summary.SourceID)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(4, summary.MessagesPresent)
			assert.Equal(1, summary.Files)
			assert.Zero(summary.FilesSkipped)
			var got int64
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sender_id FROM messages WHERE snippet LIKE 'An example post%'").Scan(&got))
			assert.Equal(sender, got)
			var n int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'tumblr'").Scan(&n))
			assert.Equal(4, n)
			var processed, added int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT sum(messages_processed), sum(messages_added) FROM sync_runs").Scan(&processed, &added))
			assert.Equal(8, processed)
			assert.Equal(4, added)
		})
	}
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
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT count(*) FROM messages WHERE source_id = ?"), source).Scan(&n))
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
	m := &Message{Media: []string{"../media/zero.bin", "../media/missing.bin", "../media/large.bin", "../../escape", "https://media.example/image"}}
	s := &ImportSummary{}
	writes, _, err := mediaWrites(t.Context(), st, r, 0, "conversations/c.html", m, ImportOptions{AttachmentsDir: t.TempDir()}, s)
	require.NoError(err)
	assert.Zero(s.MediaStored)
	assert.Equal(1, s.MediaMissing)
	assert.Equal(1, s.MediaOversized)
	assert.Equal(1, s.MediaInvalid)
	assert.Equal(1, s.MediaRemote)
	require.Len(writes, len(m.Media))
	assert.NotEmpty(writes[0].ContentHash)
	assert.Zero(writes[0].Size)
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
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(err)
	assert.Len(conversations, 1)
}

// A real completed conversation triggers cancellation through the progress logger.
type cancelOnProgress struct{ cancel context.CancelFunc }

func (w cancelOnProgress) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestTumblrInterruptedAfterWritesThenReplay(t *testing.T) {
	for _, failure := range []string{"cancel", "parse", "utf8"} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
			data, err := os.ReadFile(filepath.Join(root, "conversations", "convo_700.html"))
			require.NoError(err)
			extra := filepath.Join(root, "conversations", "z.html")
			if failure == "utf8" {
				data = []byte{0xff}
			} else if failure == "parse" {
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
				if failure == "utf8" {
					require.ErrorContains(err, "z.html: tumblr: conversation HTML is not valid UTF-8")
				} else {
					require.ErrorContains(err, "Conversation ID")
				}
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
	var html strings.Builder
	html.WriteString(parts[0])
	for i := 4; i > 0; i-- {
		html.WriteString(`<div class="container ">`)
		html.WriteString(parts[i])
	}
	html.WriteString("</body></html>\n")
	require.NoError(os.WriteFile(filepath.Join(root, "conversations", "renamed.html"), []byte(strings.ReplaceAll(html.String(), "orchard-example", owner)), 0600))
	for _, name := range []string{"._metadata.html", "__MACOSX/cache.html", ".hidden/cache.html"} {
		file := filepath.Join(root, "conversations", name)
		require.NoError(os.MkdirAll(filepath.Dir(file), 0700))
		require.NoError(os.WriteFile(file, []byte("not a conversation"), 0600))
	}
	return root
}

func TestTumblrMediaClassification(t *testing.T) {
	checks := require.New(t)
	verify := assert.New(t)
	for _, tc := range []struct{ ref, kind string }{
		{"data:text/plain,hello", "invalid"}, {"../../escape", "invalid"}, {"/%2Fabsolute", "invalid"}, {"%2e%2e/%2e%2e/escape", "invalid"}, {"../media/%7aero.bin", "local"},
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
		writes, _, err := mediaWrites(t.Context(), st, r, 0, "conversations/c.html", &Message{Media: []string{"../" + tc.name}}, ImportOptions{AttachmentsDir: t.TempDir()}, &ImportSummary{})
		checks.NoError(err)
		checks.Len(writes, 1)
		verify.Equal(tc.kind, writes[0].MediaType)
		verify.Equal(tc.mime, writes[0].MIMEType)
	}
}

func TestTumblrMediaFailureCounters(t *testing.T) {
	for failure, want := range map[string]int{"checksum": 0, "new-db": 0, "replay-db": 1} {
		t.Run(failure, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			if failure != "checksum" && st.IsPostgreSQL() {
				t.Skip("SQLite trigger injects an attachment persistence failure")
			}
			html, err := os.ReadFile("testdata/observed-shape/conversations/convo_700.html")
			require.NoError(err)
			file := testutil.CreateTempZip(t, map[string]string{
				"conversations/c.html": strings.ReplaceAll(string(html), "<p>Hello", `<img src="../good.bin"><img src="../media.bin"><p>Hello`),
				"good.bin": "first payload", "media.bin": "fictional payload",
			})
			if failure == "checksum" {
				data, err := os.ReadFile(file)
				require.NoError(err)
				// Flip the final entry's central-directory CRC, preserving its payload.
				data[bytes.LastIndex(data, []byte("media.bin"))-30] ^= 1
				require.NoError(os.WriteFile(file, data, 0600))
			}
			opts := ImportOptions{Blog: "orchard-example"}
			if failure == "replay-db" {
				_, err = Import(t.Context(), st, file, opts)
				require.NoError(err)
			}
			if failure != "checksum" {
				_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_tumblr_attachment BEFORE INSERT ON attachments WHEN NEW.filename = 'media.bin' BEGIN SELECT RAISE(ABORT, 'forced Tumblr attachment failure'); END`)
				require.NoError(err)
			}
			opts.AttachmentsDir = t.TempDir()
			s, err := Import(t.Context(), st, file, opts)
			if failure == "checksum" {
				require.ErrorIs(err, zip.ErrChecksum)
				require.ErrorContains(err, "conversations/c.html")
				require.ErrorContains(err, "../media.bin")
			} else {
				require.ErrorContains(err, "forced Tumblr attachment failure")
			}
			assert.Equal(want, s.MediaStored)
			var saved int
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE content_hash <> ''").Scan(&saved))
			assert.Equal(want, saved)
		})
	}
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
