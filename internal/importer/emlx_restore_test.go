package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// partialRaw returns an Apple Mail partial message: a text part followed by
// one attachment placeholder per name, at part indexes 2, 3, ...
func partialRaw(headers []string, names ...string) []byte {
	lines := append([]string{
		"From: Bob <bob@example.com>",
		"Subject: Cached attachments",
		"MIME-Version: 1.0",
	}, headers...)
	lines = append(lines,
		`Content-Type: multipart/mixed; boundary="=-b"`,
		"",
		"--=-b",
		"Content-Type: text/plain; charset=utf-8",
		"",
		"see attached",
		"",
	)
	for _, name := range names {
		lines = append(lines,
			"--=-b",
			"Content-Transfer-Encoding: base64",
			fmt.Sprintf("Content-Disposition: attachment; filename=%q", name),
			"Content-Type: application/octet-stream",
			"X-Apple-Content-Length: 12",
			"",
			"",
		)
	}
	lines = append(lines, "--=-b--", "")
	return []byte(strings.Join(lines, "\n"))
}

// cacheAttachment writes content where Apple Mail caches part `part` of
// Messages/<num>.partial.emlx.
func cacheAttachment(t *testing.T, mboxDir, num, part, name string, content []byte) {
	t.Helper()
	dir := filepath.Join(mboxDir, "Attachments", num, part)
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), content, 0600))
}

// countingIngest ingests normally and counts the calls.
func countingIngest(calls *int) rawMessageIngestFunc {
	return func(
		ctx context.Context, st *store.Store,
		sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string,
		raw []byte, fallbackDate time.Time, log *slog.Logger,
	) error {
		*calls++
		return IngestRawMessage(ctx, st, sourceID, identifier, attachmentsDir,
			labelIDs, sourceMsgID, rawHash, raw, fallbackDate, log)
	}
}

// restoredPartialTree creates Mailboxes/Test.mbox with a partial message
// whose two attachments are both cached, and returns the mail root.
func restoredPartialTree(t *testing.T, tmp string, headers ...string) string {
	t.Helper()
	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")
	mkMailboxDir(t, mboxDir, map[string][]byte{
		"5.partial.emlx": partialRaw(headers, "a.bin", "b.bin"),
	})
	cacheAttachment(t, mboxDir, "5", "2", "a.bin", []byte("first cached"))
	cacheAttachment(t, mboxDir, "5", "3", "b.bin", []byte("second cache"))
	return root
}

// A nightly rerun over an unchanged Apple Mail cache must not rewrite
// messages whose archived raw already holds every cached attachment.
func TestImportEmlxDir_RestoredPartialRerunSkipsIngest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, tmp := openTestStore(t)
	root := restoredPartialTree(t, tmp)

	var calls int
	opts := EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
		IngestFunc: countingIngest(&calls),
	}
	summary, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	require.Equal(int64(1), summary.MessagesAdded)
	require.Equal(1, calls)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	storedRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	revision, err := st.DerivedDataRevision()
	require.NoError(err)

	for range 2 {
		calls = 0
		summary, err = ImportEmlxDir(context.Background(), st, root, opts)
		require.NoError(err)
		require.Zero(summary.Errors)
		assert.Zero(calls, "rerun must not ingest")
		assert.Zero(summary.MessagesAdded)
		assert.Zero(summary.MessagesUpdated)
		assert.Equal(int64(1), summary.MessagesSkipped)
		assert.Zero(summary.AttachmentsRestored, "nothing new was archived")
		got, err := st.GetMessageRaw(messageID)
		require.NoError(err)
		assert.Equal(storedRaw, got)
		gotRevision, err := st.DerivedDataRevision()
		require.NoError(err)
		assert.Equal(revision, gotRevision)
	}
}

// Skipping a restored partial still repairs missing header facts, as the
// ordinary skip path does.
func TestImportEmlxDir_RestoredPartialRerunRepairsHeaders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, tmp := openTestStore(t)
	root := restoredPartialTree(t, tmp,
		"Message-ID: <child@example.com>", "In-Reply-To: <parent@example.com>")

	var calls int
	opts := EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
		IngestFunc: countingIngest(&calls),
	}
	_, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	var messageID int64
	var rfcID string
	require.NoError(st.DB().QueryRow(`SELECT id, rfc822_message_id FROM messages`).Scan(&messageID, &rfcID))
	require.NotEmpty(rfcID)
	_, err = st.DB().Exec(`UPDATE messages SET rfc822_message_id = NULL, metadata = NULL WHERE id = ?`, messageID)
	require.NoError(err)

	calls = 0
	summary, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	require.Zero(summary.Errors)
	assert.Zero(calls, "rerun must not ingest")
	assert.Equal(int64(1), summary.MessagesSkipped)
	var gotID, metadata string
	require.NoError(st.DB().QueryRow(`SELECT rfc822_message_id, CAST(metadata AS TEXT) FROM messages WHERE id = ?`, messageID).Scan(&gotID, &metadata))
	assert.Equal(rfcID, gotID)
	assert.Contains(metadata, "email_in_reply_to")
}

// The same partial message found in another mailbox gains that mailbox's
// label without being ingested again.
func TestImportEmlxDir_RestoredPartialSecondMailboxAddsLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, tmp := openTestStore(t)
	root := restoredPartialTree(t, tmp)

	var calls int
	opts := EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
		IngestFunc: countingIngest(&calls),
	}
	_, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)

	otherDir := filepath.Join(root, "Mailboxes", "Other.mbox")
	mkMailboxDir(t, otherDir, map[string][]byte{
		"7.partial.emlx": partialRaw(nil, "a.bin", "b.bin"),
	})
	cacheAttachment(t, otherDir, "7", "2", "a.bin", []byte("first cached"))
	cacheAttachment(t, otherDir, "7", "3", "b.bin", []byte("second cache"))

	calls = 0
	summary, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	require.Zero(summary.Errors)
	assert.Zero(calls, "rerun must not ingest")
	assert.Zero(summary.MessagesUpdated)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&count))
	assert.Equal(2, count)
}

// A cached attachment too large for the message size limit stays a
// placeholder on every run; that alone must not trigger re-ingestion.
func TestImportEmlxDir_RestoredPartialOverBudgetRerunSkips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Mail")
	mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")
	mkMailboxDir(t, mboxDir, map[string][]byte{
		"5.partial.emlx": partialRaw(nil, "small.bin", "large.bin"),
	})
	cacheAttachment(t, mboxDir, "5", "2", "small.bin", []byte("small cached"))
	cacheAttachment(t, mboxDir, "5", "3", "large.bin", bytes.Repeat([]byte("x"), 4096))

	var calls int
	opts := EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
		MaxMessageBytes: 2048, IngestFunc: countingIngest(&calls),
	}
	summary, err := ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	require.Equal(int64(1), summary.MessagesAdded)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
	storedRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	require.Equal(1, bytes.Count(storedRaw, []byte("X-Apple-Content-Length:")))

	calls = 0
	summary, err = ImportEmlxDir(context.Background(), st, root, opts)
	require.NoError(err)
	require.Zero(summary.Errors)
	assert.Zero(calls, "rerun must not ingest")
	assert.Equal(int64(1), summary.MessagesSkipped)
}

// Before the skip existed, a restored partial always reached ingestion with
// the mailbox label, which retried a label that failed to attach. Keep that
// retry rather than reporting the message as skipped.
func TestImportEmlxDir_RestoredPartialLabelFailureReingests(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, tmp := openTestStore(t)
	root := restoredPartialTree(t, tmp)
	_, err := ImportEmlxDir(context.Background(), st, root, EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
	})
	require.NoError(err)

	otherDir := filepath.Join(root, "Mailboxes", "Other.mbox")
	mkMailboxDir(t, otherDir, map[string][]byte{
		"7.partial.emlx": partialRaw(nil, "a.bin", "b.bin"),
	})
	cacheAttachment(t, otherDir, "7", "2", "a.bin", []byte("first cached"))
	_, err = st.DB().Exec(`CREATE TRIGGER block_labels BEFORE INSERT ON message_labels
		BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
	require.NoError(err)

	var labelIDs [][]int64
	summary, err := ImportEmlxDir(context.Background(), st, root, EmlxImportOptions{
		Identifier: "alice@example.com", NoResume: true,
		IngestFunc: func(
			_ context.Context, _ *store.Store, _ int64, _, _ string,
			ids []int64, _, _ string, _ []byte, _ time.Time, _ *slog.Logger,
		) error {
			labelIDs = append(labelIDs, ids)
			return nil
		},
	})
	require.NoError(err)
	require.Len(labelIDs, 1, "a failed label add must fall back to ingestion")
	assert.Len(labelIDs[0], 2, "ingestion carries both mailbox labels")
	assert.Equal(int64(1), summary.MessagesUpdated)
}

// AttachmentsRestored counts attachment parts added to the archive, once
// each, however many duplicate partial files supplied them.
func TestImportEmlxDir_PartialDuplicateRestoreCount(t *testing.T) {
	failIngest := func(
		context.Context, *store.Store, int64, string, string,
		[]int64, string, string, []byte, time.Time, *slog.Logger,
	) error {
		return errors.New("injected failure")
	}
	for _, tc := range []struct {
		name     string
		caches   [][2]string // {num, part}
		ingest   rawMessageIngestFunc
		restored int64
	}{
		{"complementary caches", [][2]string{{"2", "2"}, {"3", "3"}}, nil, 2},
		{"overlapping caches", [][2]string{{"2", "2"}, {"3", "2"}}, nil, 1},
		{"ingest failure", [][2]string{{"2", "2"}, {"3", "3"}}, failIngest, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Mail")
			mboxDir := filepath.Join(root, "Mailboxes", "Test.mbox")
			raw := partialRaw(nil, "a.bin", "b.bin")
			mkMailboxDir(t, mboxDir, map[string][]byte{
				"2.partial.emlx": raw, "3.partial.emlx": raw,
			})
			names := map[string]string{"2": "a.bin", "3": "b.bin"}
			for _, c := range tc.caches {
				cacheAttachment(t, mboxDir, c[0], c[1], names[c[1]], []byte("cached bytes"))
			}
			summary, err := ImportEmlxDir(context.Background(), st, root, EmlxImportOptions{
				Identifier: "alice@example.com", NoResume: true, IngestFunc: tc.ingest,
			})
			require.NoError(err)
			assert.Equal(t, int64(2), summary.PartialFiles)
			assert.Equal(t, tc.restored, summary.AttachmentsRestored)
		})
	}
}
