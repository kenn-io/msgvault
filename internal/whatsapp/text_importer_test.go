package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func importTextForTest(imp *Importer, ctx context.Context, path string, opts TextOptions) (TextSummary, error) {
	input, err := LoadText(ctx, path, opts)
	if err != nil {
		return TextSummary{}, err
	}
	return imp.ImportText(ctx, input)
}

func textFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "chat.txt")
	require.NoError(t, os.WriteFile(p, []byte(text), 0600))
	return p
}
func textOptions() TextOptions {
	return TextOptions{Phone: "+12025550123", ChatID: "garden", ChatType: "group", DateOrder: "dmy", Timezone: "UTC"}
}
func textCount(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n))
	return n
}

func TestTextImportRepeatOverlap(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	a := "13/6/26, 10:00 - Mira: asters\n13/6/26, 10:01 - Ivo: water\n13/6/26, 10:02 - You changed the group description: flowers\n"
	s, err := importTextForTest(imp, t.Context(), textFile(t, a), opts)
	require.NoError(err)
	assert.Equal(3, s.Added)
	assert.Equal(1, s.Notices)
	var sourceID, labeledID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT source_id,id FROM messages WHERE snippet = 'asters'").Scan(&sourceID, &labeledID))
	labelID, err := st.EnsureLabel(t.Context(), sourceID, "saved", "Saved", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(t.Context(), labeledID, []int64{labelID}))
	s, err = importTextForTest(imp, t.Context(), textFile(t, a), opts)
	require.NoError(err)
	assert.Equal(0, s.Added)
	assert.Equal(3, s.Unchanged)
	var labels int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?"), labeledID, labelID).Scan(&labels))
	assert.Equal(1, labels, "re-import must not rewrite existing rows")
	s, err = importTextForTest(imp, t.Context(), textFile(t, "13/6/26, 10:03 - Mira: seedlings\n13/6/26, 10:01 - Ivo: water\n"), opts)
	require.NoError(err)
	assert.Equal(1, s.Added)
	assert.Equal(4, textCount(t, st, "messages"))
	assert.Equal(2, textCount(t, st, "participants"))
}

func TestTextImportRefreshesOnlyTouchedChat(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	path := textFile(t, "13/6/26, 10:00 - Mira: one\n")
	_, err := importTextForTest(imp, t.Context(), path, opts)
	require.NoError(t, err)
	opts.ChatID = "other"
	_, err = importTextForTest(imp, t.Context(), path, opts)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), "UPDATE conversations SET message_count = 77 WHERE source_conversation_id = ?", textChatKey("other"))
	require.NoError(t, err)
	opts.ChatID = "garden"
	_, err = importTextForTest(imp, t.Context(), textFile(t, "13/6/26, 10:01 - Mira: two\n"), opts)
	require.NoError(t, err)
	var touched, other int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT message_count FROM conversations WHERE source_conversation_id = ?", textChatKey("garden")).Scan(&touched))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT message_count FROM conversations WHERE source_conversation_id = ?", textChatKey("other")).Scan(&other))
	assert.Equal(t, 2, touched)
	assert.Equal(t, 77, other)
}

func TestTextImportMultiplicityAndEdits(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	row := "13/6/26, 10:00 - Mira: same\n"
	s, err := importTextForTest(imp, t.Context(), textFile(t, strings.Repeat(row, 3)), opts)
	require.NoError(err)
	assert.Equal(3, s.Added)
	assert.Equal(2, s.DuplicateAmbiguity)
	s, err = importTextForTest(imp, t.Context(), textFile(t, row), opts)
	require.NoError(err)
	assert.Equal(1, s.Unchanged)
	assert.Equal(3, textCount(t, st, "messages"))
	_, err = importTextForTest(imp, t.Context(), textFile(t, "13/6/26, 10:00 - Mira: edited\n"), opts)
	require.NoError(err)
	assert.Equal(4, textCount(t, st, "messages"))
}

func TestTextImportScopes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	p := textFile(t, "3/6/26, 10:00 - Mira: same\n")
	_, err := importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	opts.ChatID = "other"
	_, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	opts.Phone = "+12025550124"
	_, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(3, textCount(t, st, "messages"))
	assert.Equal(3, textCount(t, st, "participants"))
}

func TestTextImportPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		change     func(*TextOptions)
	}{
		{"parse", "13/6/26, 10:00 - Mira: valid\n31/2/26, 10:01 - Mira: bad", func(*TextOptions) {}},
		{"named month", "13/6/26, 10:00 - Mira: valid\n13 Jun 2026, 10:01 - Ivo: bad", func(*TextOptions) {}},
		{"missing space", "13/6/26, 10:00 - Mira: valid\n13/6/26,10:01 - Ivo: bad", func(*TextOptions) {}},
		{"dot clock", "13/6/26, 10:00 - Mira: valid\n13/6/26 10.01 - Ivo: bad", func(*TextOptions) {}},
		{"missing comma", "13/6/26, 10:00 - Mira: valid\n13/6/26 10:01 - Ivo: bad", func(*TextOptions) {}},
		{"spacing", "13/6/26, 10:00 - Mira: valid\n13/6/26,  10:01 - Ivo: bad", func(*TextOptions) {}},
		{"chat", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.ChatID = "" }},
		{"type", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.ChatType = "unknown" }},
		{"phone", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.Phone = "Mira" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			opts := textOptions()
			tc.change(&opts)
			_, err := importTextForTest(NewImporter(st, nil), t.Context(), textFile(t, tc.text), opts)
			require.Error(t, err)
			if tc.name == "named month" {
				require.ErrorContains(t, err, "line 2: unsupported or malformed TXT header")
			}
			assert.Zero(t, textCount(t, st, "sources"))
			assert.Zero(t, textCount(t, st, "messages"))
		})
	}
}

func TestTextImportMediaCompletion(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.AttachmentsDir = t.TempDir()
	p := textFile(t, "13/6/26, 10:00 - Mira: <attached: flower.txt>\ncaption\n")
	s, err := importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.MediaMissing)
	assert.Zero(textCount(t, st, "attachments"))
	opts.MediaDir = t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(opts.MediaDir, "flower.txt"), []byte("fictional pressed flower"), 0600))
	s, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.MediaStored)
	assert.Equal(1, s.Unchanged)
	assert.Equal(1, textCount(t, st, "attachments"))
	before, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	s, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.MediaStored, "repeat verification still counts the reference")
	after, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before, after, "an unchanged attachment must not invalidate the archive cache")
	require.NoError(os.WriteFile(filepath.Join(opts.MediaDir, "flower.txt"), []byte("fictional updated flower"), 0600))
	s, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.MediaStored)
	changed, err := st.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Greater(changed, after, "changed bytes still invalidate atomically")
	assert.Equal(1, textCount(t, st, "attachments"))
	opts.MediaDir = ""
	_, err = importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT attachment_count FROM messages WHERE snippet LIKE '<attached:%'").Scan(&count))
	assert.Equal(1, count)
}

func TestTextImportMediaContainment(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.AttachmentsDir = t.TempDir()
	opts.MediaDir = t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(os.WriteFile(outside, []byte("fictional"), 0600))
	require.NoError(os.Symlink(outside, filepath.Join(opts.MediaDir, "escape.txt")))
	require.NoError(os.Mkdir(filepath.Join(opts.MediaDir, "directory"), 0700))
	text := "13/6/26, 10:00 - Mira: <attached: ../outside.txt>\n13/6/26, 10:01 - Mira: <attached: dir\\outside.txt>\n13/6/26, 10:02 - Mira: <attached: escape.txt>\n13/6/26, 10:03 - Mira: <attached: directory>\n"
	s, err := importTextForTest(imp, t.Context(), textFile(t, text), opts)
	require.NoError(err)
	assert.Equal(4, s.MediaSkipped)
	assert.Zero(textCount(t, st, "attachments"))
	large, err := os.Create(filepath.Join(opts.MediaDir, "large.txt"))
	require.NoError(err)
	require.NoError(large.Truncate((100 << 20) + 1))
	require.NoError(large.Close())
	s, err = importTextForTest(imp, t.Context(), textFile(t, "13/6/26, 10:04 - Mira: large.txt (file attached)"), opts)
	require.NoError(err)
	assert.Equal(1, s.MediaOversized)
}

func TestTextImportFixtures(t *testing.T) {
	for _, tc := range []struct{ file, order string }{{"android-dmy.txt", "dmy"}, {"ios-mdy.txt", "mdy"}} {
		t.Run(tc.file, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			opts := textOptions()
			opts.DateOrder = tc.order
			opts.MediaDir = "testdata/text"
			opts.AttachmentsDir = t.TempDir()
			summary, err := importTextForTest(NewImporter(st, nil), t.Context(), filepath.Join("testdata/text", tc.file), opts)
			require.NoError(t, err)
			assert.Equal(t, 4, summary.Records)
			assert.Equal(t, 1, summary.MediaStored)
		})
	}
}

func TestTextImportPartialCancelAndRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	p := textFile(t, "13/6/26, 10:00 - Mira: first\n13/6/26, 10:01 - Ivo: second\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryCtx := context.WithoutCancel(t.Context())
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(st.DB().QueryRowContext(queryCtx, "SELECT COUNT(*) FROM messages").Scan(&count))
		return count == 1
	})
	summary, err := importTextForTest(imp, ctx, p, opts)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled())
	assert.Equal(1, summary.Records)
	assert.Equal(1, summary.Added)
	assert.Equal(1, textCount(t, st, "messages"))
	var status string
	var processed, added int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status,messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status, &processed, &added))
	assert.Equal("failed", status)
	assert.Equal(1, processed)
	assert.Equal(1, added)
	var counted int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT SUM(message_count) FROM conversations").Scan(&counted))
	assert.Equal(1, counted)
	s, err := importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.Added)
	assert.Equal(1, s.Unchanged)
	assert.Equal(2, textCount(t, st, "messages"))
	assert.Equal(2, textCount(t, st, "conversation_participants"))
}

func TestTextImportPartialAttachmentFailureAndRetry(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.MediaDir = t.TempDir()
	opts.AttachmentsDir = t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(opts.MediaDir, "note.txt"), []byte("fictional"), 0600))
	_, err := st.DB().ExecContext(t.Context(), "CREATE TRIGGER reject_text_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(FAIL, 'synthetic attachment failure'); END")
	require.NoError(err)
	p := textFile(t, "13/6/26, 10:00 - Mira: <attached: note.txt>")
	_, err = importTextForTest(imp, t.Context(), p, opts)
	require.ErrorContains(err, "synthetic attachment failure")
	var processed, added int
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&processed, &added))
	assert.Equal(1, processed)
	assert.Equal(1, added)
	assert.Equal(1, textCount(t, st, "messages"))
	assert.Zero(textCount(t, st, "attachments"))
	_, err = st.DB().ExecContext(t.Context(), "DROP TRIGGER reject_text_attachment")
	require.NoError(err)
	s, err := importTextForTest(imp, t.Context(), p, opts)
	require.NoError(err)
	assert.Equal(1, s.Unchanged)
	assert.Equal(1, s.MediaStored)
	assert.Equal(1, textCount(t, st, "attachments"))
}
