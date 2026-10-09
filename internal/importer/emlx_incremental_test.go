//go:build linux || darwin

package importer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

// countIngests wraps the production ingestion and counts its calls.
func countIngests(opts EmlxImportOptions, calls *int) emlxImportIO {
	io := defaultEmlxImportIO(opts)
	ingest := io.ingest
	io.ingest = func(
		ctx context.Context, s *store.Store, sid int64, identifier, dest string, labels []int64,
		target, hash string, raw []byte, date time.Time, log *slog.Logger,
	) error {
		*calls++
		return ingest(ctx, s, sid, identifier, dest, labels, target, hash, raw, date, log)
	}
	return io
}

func TestImportEmlxUnchangedFileRestoresRemovedLabel(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Header("Message-ID", "<label@example.test>").
		Body("synthetic label").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	_, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM message_labels`)
	r.NoError(err)

	warm, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	a.Equal(int64(1), warm.FilesUnchanged)
	var labels int
	r.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_labels`).Scan(&labels))
	a.Equal(1, labels, "the mailbox label returns without reading the file")
}

func TestImportEmlxFullReconcileIngestsSharedMessageOnce(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Mail")
	raw := email.NewMessage().From("sender@example.test").Header("Message-ID", "<shared@example.test>").
		Body("synthetic shared").Bytes()
	mkMailboxDir(t, filepath.Join(root, "A.mbox"), map[string][]byte{"1.emlx": raw})
	mkMailboxDir(t, filepath.Join(root, "B.mbox"), map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.Equal(int64(1), first.MessagesAdded)

	opts.FullReconcile = true
	var ingests int
	reconciled, err := importEmlxDir(t.Context(), st, root, opts, countIngests(opts, &ingests))
	r.NoError(err)
	a.Equal(1, ingests, "one run completes a shared message once")
	a.Equal(int64(1), reconciled.MessagesUpdated)
	a.Equal(int64(1), reconciled.MessagesSkipped)
	a.Equal(2, countEmlxLedgerEntries(t, st, first.SourceID, "emlx-occurrence", "imported"))

	// A later reconciliation is a new run, so it completes the message again.
	ingests = 0
	_, err = importEmlxDir(t.Context(), st, root, opts, countIngests(opts, &ingests))
	r.NoError(err)
	a.Equal(1, ingests)
}

func TestImportEmlxCancellationIsNotAnError(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	raw := email.NewMessage().From("sender@example.test").Body("synthetic cancel").Bytes()
	mkMailboxDir(t, root, map[string][]byte{"1.emlx": raw})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	ctx, cancel := context.WithCancel(t.Context())
	io := defaultEmlxImportIO(opts)
	io.ingest = func(
		context.Context, *store.Store, int64, string, string, []int64, string, string, []byte, time.Time,
		*slog.Logger,
	) error {
		cancel()
		return context.Canceled
	}
	summary, err := importEmlxDir(ctx, st, root, opts, io)
	r.ErrorIs(err, context.Canceled)
	a.Zero(summary.Errors)
	a.False(summary.HardErrors)
}

func TestIsEmlxRetryable(t *testing.T) {
	database := errors.New("database is locked")
	file := emlxRetryable(errors.New("permission denied"))
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"plain error", database, false},
		{"retryable", file, true},
		{"wrapped retryable", fmt.Errorf("occurrence: %w", file), true},
		{"joined retryable", errors.Join(file, emlxRetryable(errors.New("index"))), true},
		{"retryable joined with database failure", errors.Join(file, database), false},
		{"wrapped mixed join", fmt.Errorf("occurrence: %w", errors.Join(database, file)), false},
		{"empty join", errors.Join(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isEmlxRetryable(tc.err))
		})
	}
}
