package importer

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelOnLabelReadHandler cancels when the store logs its at-th read of a
// message's labels. That read opens AddMessageLabels' transaction, so the
// label insert that follows runs with a cancelled context.
type cancelOnLabelReadHandler struct {
	slog.Handler

	mu     sync.Mutex
	cancel context.CancelFunc
	at     int
	reads  int
}

func (*cancelOnLabelReadHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelOnLabelReadHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "sql" {
		return nil
	}
	labelRead := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key != "stmt" {
			return true
		}
		labelRead = strings.Contains(attr.Value.String(), "SELECT label_id FROM message_labels WHERE message_id")
		return false
	})
	if !labelRead {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	if h.reads == h.at {
		h.cancel()
	}
	return nil
}

// Reimporting an mbox with a new label adds it to messages already archived.
// A cancellation that fails one of those label writes must leave the saved
// offset before that message, so the resumed import applies the label.
//
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportMbox_CancelledLabelWriteDoesNotAdvanceOffset(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	tmp := t.TempDir()
	st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(tmp, "msgvault.db"))

	var mboxData strings.Builder
	for i, subject := range []string{"one", "two", "three"} {
		fmt.Fprintf(&mboxData, "From sender@example.com Mon Jan 1 00:00:0%d 2024\n", i)
		fmt.Fprintf(&mboxData, "From: sender@example.com\nTo: me@example.com\nSubject: %s\n\nBody %s\n\n", subject, subject)
	}
	mboxPath := filepath.Join(tmp, "export.mbox")
	require.NoError(os.WriteFile(mboxPath, []byte(mboxData.String()), 0600))

	opts := MboxImportOptions{
		SourceType:         "mbox",
		Identifier:         "me@example.com",
		CheckpointInterval: 1,
	}
	_, err := ImportMbox(t.Context(), st, mboxPath, opts)
	require.NoError(err, "initial import")

	labeled := func(subject string) bool {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM messages m
			WHERE m.subject = ? AND EXISTS (
				SELECT 1 FROM message_labels ml
				JOIN labels l ON l.id = ml.label_id
				WHERE ml.message_id = m.id AND l.name = 'extra'
			)`, subject).Scan(&n))
		return n > 0
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// A statement over the slow threshold logs "sql slow" in place of the
	// line the handler counts.
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	previous := slog.Default()
	slog.SetDefault(slog.New(&cancelOnLabelReadHandler{Handler: slog.DiscardHandler, cancel: cancel, at: 2}))
	t.Cleanup(func() { slog.SetDefault(previous) })

	opts.Labels = []string{"extra"}
	opts.Logger = slog.New(slog.DiscardHandler)
	_, err = ImportMbox(ctx, st, mboxPath, opts)
	require.NoError(err, "cancelled reimport")
	require.ErrorIs(ctx.Err(), context.Canceled)
	require.True(labeled("one"), "the first label write precedes cancellation")
	require.False(labeled("two"), "cancellation must fail the second label write")

	slog.SetDefault(previous)
	opts.Logger = nil
	summary, err := ImportMbox(t.Context(), st, mboxPath, opts)
	require.NoError(err, "resumed reimport")
	assert.True(summary.WasResumed)
	assert.True(labeled("two"), "the resumed import must apply the label the cancelled run skipped")
	assert.True(labeled("three"))
}
