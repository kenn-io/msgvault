package importer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/email"
)

func TestImportEmlxDiscoveryFailureEndsSync(t *testing.T) {
	for _, fileRoot := range []bool{false, true} {
		name := "missing root"
		if fileRoot {
			name = "file root"
		}
		t.Run(name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Mail")
			if fileRoot {
				r.NoError(os.WriteFile(root, []byte("not a directory"), 0600))
			}
			source, err := st.GetOrCreateSource("apple-mail", "owner@example.test")
			r.NoError(err)
			_, err = ImportEmlxDir(t.Context(), st, root, EmlxImportOptions{Identifier: source.Identifier})
			r.ErrorContains(err, "discover mailboxes")
			run, err := st.GetLatestSync(source.ID)
			r.NoError(err)
			a.Equal(store.SyncStatusFailed, run.Status)
			a.True(run.CompletedAt.Valid)
			a.Contains(run.ErrorMessage.String, "discover mailboxes")
		})
	}
}

func TestImportEmlxReconcileInvalidationFailureEndsSync(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	st, tmp := openTestStore(t)
	root := filepath.Join(tmp, "Inbox.mbox")
	mkMailboxDir(t, root, map[string][]byte{
		"1.emlx": email.NewMessage().From("sender@example.test").Body("Synthetic sync fixture.").Bytes(),
	})
	opts := EmlxImportOptions{Identifier: "owner@example.test"}
	first, err := ImportEmlxDir(t.Context(), st, root, opts)
	r.NoError(err)
	r.False(first.HardErrors)
	_, err = st.DB().Exec(`CREATE TRIGGER reject_emlx_invalidation
 BEFORE UPDATE ON source_import_items
 WHEN OLD.provider = 'emlx-occurrence' AND NEW.status = 'pending'
 BEGIN SELECT RAISE(FAIL, 'invalidation unavailable'); END`)
	r.NoError(err)
	opts.FullReconcile = true
	_, err = ImportEmlxDir(t.Context(), st, root, opts)
	r.ErrorContains(err, "invalidate EMLX root")
	run, err := st.GetLatestSync(first.SourceID)
	r.NoError(err)
	a.Equal(store.SyncStatusFailed, run.Status)
	a.True(run.CompletedAt.Valid)
	a.Contains(run.ErrorMessage.String, "invalidation unavailable")
}

func TestImportEmlxCancellationRetainsResumableSync(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancelled"
		if deadline {
			name = "deadline exceeded"
		}
		t.Run(name, func(t *testing.T) {
			r, a := require.New(t), assert.New(t)
			st, tmp := openTestStore(t)
			root := filepath.Join(tmp, "Inbox.mbox")
			mkMailboxDir(t, root, map[string][]byte{
				"1.emlx": email.NewMessage().From("sender@example.test").Body("Synthetic resume fixture.").Bytes(),
			})
			ctx, cancel := context.WithCancel(t.Context())
			wantErr := context.Canceled
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(t.Context(), time.Unix(1, 0))
				wantErr = context.DeadlineExceeded
			}
			cancel()
			opts := EmlxImportOptions{Identifier: "owner@example.test"}
			interrupted, err := ImportEmlxDir(ctx, st, root, opts)
			r.ErrorIs(err, wantErr)
			r.NotNil(interrupted)
			run, err := st.GetLatestSync(interrupted.SourceID)
			r.NoError(err)
			a.Equal(store.SyncStatusRunning, run.Status)
			a.False(run.CompletedAt.Valid)
			a.NotEmpty(run.CursorBefore.String)

			resumed, err := ImportEmlxDir(t.Context(), st, root, opts)
			r.NoError(err)
			a.True(resumed.WasResumed)
			a.False(resumed.HardErrors)
			a.Equal(int64(1), resumed.MessagesAdded)
			run, err = st.GetLatestSync(resumed.SourceID)
			r.NoError(err)
			a.Equal(store.SyncStatusCompleted, run.Status)
		})
	}
}
