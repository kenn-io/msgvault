package whatsapp

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

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
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	a := "13/6/26, 10:00 - Mira: asters\n13/6/26, 10:01 - Ivo: water\n13/6/26, 10:02 - You changed the group description: flowers\n"
	s, err := imp.ImportText(t.Context(), textFile(t, a), opts)
	require.NoError(t, err)
	assert.Equal(t, 3, s.Added)
	assert.Equal(t, 1, s.Notices)
	s, err = imp.ImportText(t.Context(), textFile(t, a), opts)
	require.NoError(t, err)
	assert.Equal(t, 0, s.Added)
	assert.Equal(t, 3, s.Unchanged)
	s, err = imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:03 - Mira: seedlings\n13/6/26, 10:01 - Ivo: water\n"), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Added)
	assert.Equal(t, 4, textCount(t, st, "messages"))
	assert.Equal(t, 2, textCount(t, st, "participants"))
	var sender sql.NullInt64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT sender_id FROM messages WHERE snippet LIKE 'You changed%'").Scan(&sender))
	assert.False(t, sender.Valid)
	var rawFormat, metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT raw_format FROM message_raw LIMIT 1").Scan(&rawFormat))
	assert.Equal(t, "whatsapp_txt", rawFormat)
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM messages LIMIT 1").Scan(&metadata))
	assert.Contains(t, metadata, `"direction":"unknown"`)
	assert.Contains(t, metadata, `"ambiguous_time_policy":"earlier"`)
	if st.FTS5Available() {
		var n int
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'asters'").Scan(&n))
		assert.Equal(t, 1, n)
	}
}

func TestTextImportMultiplicityAndEdits(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	row := "13/6/26, 10:00 - Mira: same\n"
	s, err := imp.ImportText(t.Context(), textFile(t, strings.Repeat(row, 3)), opts)
	require.NoError(t, err)
	assert.Equal(t, 3, s.Added)
	assert.Equal(t, 2, s.DuplicateAmbiguity)
	s, err = imp.ImportText(t.Context(), textFile(t, row), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Unchanged)
	assert.Equal(t, 3, textCount(t, st, "messages"))
	_, err = imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: edited\n"), opts)
	require.NoError(t, err)
	assert.Equal(t, 4, textCount(t, st, "messages"))
}

func TestTextImportScopesAndAssumptions(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	p := textFile(t, "3/6/26, 10:00 - Mira: same\n")
	_, err := imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	for _, change := range []func(*TextOptions){func(o *TextOptions) { o.Timezone = "Europe/Berlin" }, func(o *TextOptions) { o.ChatType = "direct" }} {
		changed := opts
		change(&changed)
		_, err = imp.ImportText(t.Context(), p, changed)
		require.ErrorContains(t, err, "assumptions conflict")
	}
	opts.ChatID = "other"
	_, err = imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	opts.Phone = "+12025550124"
	_, err = imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 3, textCount(t, st, "messages"))
	assert.Equal(t, 3, textCount(t, st, "participants"))
}

func TestTextImportPreflight(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		change     func(*TextOptions)
	}{
		{"parse", "13/6/26, 10:00 - Mira: valid\n31/2/26, 10:01 - Mira: bad", func(*TextOptions) {}},
		{"chat", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.ChatID = "" }},
		{"type", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.ChatType = "unknown" }},
		{"phone", "13/6/26, 10:00 - Mira: hi", func(o *TextOptions) { o.Phone = "Mira" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			opts := textOptions()
			tc.change(&opts)
			_, err := NewImporter(st, nil).ImportText(t.Context(), textFile(t, tc.text), opts)
			require.Error(t, err)
			assert.Zero(t, textCount(t, st, "sources"))
			assert.Zero(t, textCount(t, st, "messages"))
		})
	}
}

func TestTextImportMediaCompletion(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.AttachmentsDir = t.TempDir()
	p := textFile(t, "13/6/26, 10:00 - Mira: <attached: flower.txt>\ncaption\n13/6/26, 10:01 - Ivo: <Media omitted>\n")
	s, err := imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.MediaMissing)
	assert.Equal(t, 1, s.MediaOmitted)
	assert.Zero(t, textCount(t, st, "attachments"))
	opts.MediaDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opts.MediaDir, "flower.txt"), []byte("fictional pressed flower"), 0600))
	s, err = imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.MediaStored)
	assert.Equal(t, 2, s.Unchanged)
	assert.Equal(t, 1, textCount(t, st, "attachments"))
	opts.MediaDir = ""
	_, err = imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT attachment_count FROM messages WHERE snippet LIKE '<attached:%'").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestTextImportMediaContainment(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.AttachmentsDir = t.TempDir()
	opts.MediaDir = t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("fictional"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(opts.MediaDir, "escape.txt")))
	require.NoError(t, os.Mkdir(filepath.Join(opts.MediaDir, "directory"), 0700))
	text := "13/6/26, 10:00 - Mira: <attached: ../outside.txt>\n13/6/26, 10:01 - Mira: <attached: /outside.txt>\n13/6/26, 10:02 - Mira: <attached: escape.txt>\n13/6/26, 10:03 - Mira: <attached: directory>\n"
	s, err := imp.ImportText(t.Context(), textFile(t, text), opts)
	require.NoError(t, err)
	assert.Equal(t, 4, s.MediaSkipped)
	assert.Zero(t, textCount(t, st, "attachments"))
	large, err := os.Create(filepath.Join(opts.MediaDir, "large.txt"))
	require.NoError(t, err)
	require.NoError(t, large.Truncate((100<<20)+1))
	require.NoError(t, large.Close())
	s, err = imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:04 - Mira: large.txt (file attached)"), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.MediaOversized)
}

func TestTextImportCanceledAndRetry(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	p := textFile(t, "13/6/26, 10:00 - Mira: retry")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := imp.ImportText(ctx, p, opts)
	require.ErrorIs(t, err, context.Canceled)
	_, err = imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, textCount(t, st, "messages"))
}

func TestTextImportFixtures(t *testing.T) {
	for _, tc := range []struct{ file, order string }{{"android-dmy.txt", "dmy"}, {"ios-mdy.txt", "mdy"}} {
		t.Run(tc.file, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			opts := textOptions()
			opts.DateOrder = tc.order
			opts.MediaDir = "testdata/text"
			opts.AttachmentsDir = t.TempDir()
			summary, err := NewImporter(st, nil).ImportText(t.Context(), filepath.Join("testdata/text", tc.file), opts)
			require.NoError(t, err)
			assert.Equal(t, 4, summary.Records)
			assert.Equal(t, 1, summary.Notices)
			assert.Equal(t, 1, summary.MediaStored)
			assert.Equal(t, 1, summary.MediaOmitted)
		})
	}
}

func TestTextImportPartialCancelAndRetry(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	p := textFile(t, "13/6/26, 10:00 - Mira: first\n13/6/26, 10:01 - Ivo: second\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryCtx := context.WithoutCancel(t.Context())
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(t, st.DB().QueryRowContext(queryCtx, "SELECT COUNT(*) FROM messages").Scan(&count))
		return count == 1
	})
	_, err := imp.ImportText(ctx, p, opts)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, canceled())
	assert.Equal(t, 1, textCount(t, st, "messages"))
	var status string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal(t, "failed", status)
	var counted int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT SUM(message_count) FROM conversations").Scan(&counted))
	assert.Equal(t, 1, counted)
	s, err := imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Added)
	assert.Equal(t, 1, s.Unchanged)
	assert.Equal(t, 2, textCount(t, st, "messages"))
	assert.Equal(t, 2, textCount(t, st, "conversation_participants"))
}

func TestTextImportPartialAttachmentFailureAndRetry(t *testing.T) {
	st := testutil.NewSQLiteTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	opts.MediaDir = t.TempDir()
	opts.AttachmentsDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(opts.MediaDir, "note.txt"), []byte("fictional"), 0600))
	_, err := st.DB().ExecContext(t.Context(), "CREATE TRIGGER reject_text_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(FAIL, 'synthetic attachment failure'); END")
	require.NoError(t, err)
	p := textFile(t, "13/6/26, 10:00 - Mira: <attached: note.txt>")
	_, err = imp.ImportText(t.Context(), p, opts)
	require.ErrorContains(t, err, "synthetic attachment failure")
	assert.Equal(t, 1, textCount(t, st, "messages"))
	assert.Zero(t, textCount(t, st, "attachments"))
	_, err = st.DB().ExecContext(t.Context(), "DROP TRIGGER reject_text_attachment")
	require.NoError(t, err)
	s, err := imp.ImportText(t.Context(), p, opts)
	require.NoError(t, err)
	assert.Equal(t, 1, s.Unchanged)
	assert.Equal(t, 1, s.MediaStored)
	assert.Equal(t, 1, textCount(t, st, "attachments"))
}

func TestTextImportRetainsLabelsAndAbsentRecords(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	_, err := imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: first\n13/6/26, 10:01 - Ivo: second\n"), opts)
	require.NoError(t, err)
	var sourceID, messageID int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT source_id,id FROM messages ORDER BY id LIMIT 1").Scan(&sourceID, &messageID))
	labelID, err := st.EnsureLabel(t.Context(), sourceID, "saved", "Saved", "user")
	require.NoError(t, err)
	require.NoError(t, st.AddMessageLabels(t.Context(), messageID, []int64{labelID}))
	_, err = imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: first\n"), opts)
	require.NoError(t, err)
	assert.Equal(t, 2, textCount(t, st, "messages"))
	assert.Equal(t, 1, textCount(t, st, "message_labels"))
	assert.Equal(t, 2, textCount(t, st, "conversation_participants"))
}
