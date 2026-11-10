package tumblr

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	summary, err = Import(t.Context(), st, "testdata/overlap", opts)
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	assert.Equal(4, summary.MessagesPresent)
	var n int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM messages WHERE message_type = 'tumblr'").Scan(&n))
	assert.Equal(4, n)
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
	assert.Equal(2, s.MediaRemote)
	assert.Equal(1, s.Unsupported)
	assert.Equal(1, s.Malformed)
	assert.Equal(2, s.Undated)
	assert.Equal(1, s.ConflictingDates)
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
	assert.Contains(string(raw), `"Status":"ACTIVE"`)
}
func TestTumblrScopeUnknownSenderAndNoDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	opts := ImportOptions{Blog: "other-blog", AttachmentsDir: t.TempDir()}
	s, err := Import(t.Context(), st, "testdata/observed-shape", opts)
	require.NoError(err)
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
func TestTumblrCancellationThenReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Import(ctx, st, "testdata/observed-shape", ImportOptions{Blog: "orchard-example"})
	require.ErrorIs(err, context.Canceled)
	// A second file fails after the first conversation commits.
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
	bad := filepath.Join(root, "conversations", "z.html")
	require.NoError(os.WriteFile(bad, []byte("<p>not a conversation</p>"), 0600))
	s, err := Import(t.Context(), st, root, ImportOptions{Blog: "orchard-example"})
	require.ErrorContains(err, "Conversation ID")
	assert.Equal(4, s.MessagesAdded)
	require.NoError(os.Remove(bad))
	s, err = Import(t.Context(), st, root, ImportOptions{Blog: "orchard-example"})
	require.NoError(err)
	assert.Equal(4, s.MessagesPresent)
	assert.Zero(s.MessagesAdded)
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
	require.Error(err)
}
func TestTumblrRealQuery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := Import(t.Context(), st, "testdata/observed-shape", ImportOptions{Blog: "orchard-example"})
	require.NoError(err)
	if st.IsPostgreSQL() {
		return
	} // SQLiteEngine is the live SQLite reader.
	engine := query.NewSQLiteEngine(st.DB())
	rows, err := engine.TextSearch(t.Context(), "Repeated", nil, 20, 0)
	require.NoError(err)
	assert.Len(rows, 2)
	conversations, err := engine.ListConversations(t.Context(), query.TextFilter{})
	require.NoError(err)
	assert.Len(conversations, 1)
}

// A real completed conversation triggers cancellation through the progress logger.
type cancelOnProgress struct{ cancel context.CancelFunc }

func (w cancelOnProgress) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }
func TestTumblrInterruptedAfterWritesThenReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	root := t.TempDir()
	require.NoError(os.CopyFS(root, os.DirFS("testdata/observed-shape")))
	data, err := os.ReadFile(filepath.Join(root, "conversations", "convo_700.html"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(root, "conversations", "z.html"), []byte(strings.ReplaceAll(string(data), "Conversation ID: 700", "Conversation ID: 701")), 0600))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opts := ImportOptions{Blog: "orchard-example", Logger: slog.New(slog.NewTextHandler(cancelOnProgress{cancel}, nil))}
	s, err := Import(ctx, st, root, opts)
	require.ErrorIs(err, context.Canceled)
	assert.Equal(4, s.MessagesAdded)
	opts.Logger = nil
	s, err = Import(t.Context(), st, root, opts)
	require.NoError(err)
	assert.Equal(4, s.MessagesPresent)
	assert.Equal(4, s.MessagesAdded)
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
