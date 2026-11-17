package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Exercise both readers with the same ledger. The replay reader returns unknown
// errors too; only the statistics reader decides whether the latest error is MIME.
func TestSyncItemSelectionConsumers(t *testing.T) {
	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(t, err)
	run := f.StartSync()
	otherRun, err := f.Store.StartSyncContext(t.Context(), other.ID, "full")
	require.NoError(t, err)
	type outcome struct{ phase, status, kind string }
	mime := outcome{"ingest", "error", "attachment_error"}
	resolved := outcome{"fetch", "resolved", "acquired"}
	for _, tc := range []struct {
		name      string
		outcomes  []outcome
		other     *outcome
		wantKind  string
		wantCount int
	}{
		{"mime", []outcome{mime}, nil, "attachment_error", 4},
		{"images", []outcome{{"ingest", "error", "remote_image_error"}}, nil, "remote_image_error", 1},
		{"mime-and-images", []outcome{{"ingest", "error", "attachment_remote_image_error"}}, nil, "attachment_remote_image_error", 4},
		{"resolved", []outcome{mime, resolved}, nil, "", 1},
		{"reopened", []outcome{mime, resolved, mime}, nil, "attachment_error", 4},
		{"unknown", []outcome{mime, {"ingest", "error", "future_error"}}, nil, "future_error", 1},
		{"skipped", []outcome{mime, {"fetch", "skipped", "gmail_not_found"}}, nil, "", 1},
		{"success", []outcome{mime, {"ingest", "success", "attachment_error"}}, nil, "", 1},
		{"delete-ignored", []outcome{mime, {"delete", "resolved", "acquired"}}, nil, "attachment_error", 4},
		{"fetch-error", []outcome{{"fetch", "error", "fetch_error"}}, nil, "fetch_error", 1},
		{"other-source-resolution", []outcome{mime}, &resolved, "attachment_error", 4},
		{"other-source-error", nil, &mime, "", 1},
		{"no-debt", nil, nil, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			id := f.NewMessage().WithSourceMessageID(tc.name).WithAttachmentCount(3).Create(t, f.Store)
			for attempt, item := range tc.outcomes {
				require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
					SyncRunID: run, SourceMessageID: tc.name, Phase: item.phase, Status: item.status, ErrorKind: item.kind,
				}))
				// Latest means item ID, even when event timestamps go backwards.
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_run_items
					SET created_at = ? WHERE id = (SELECT MAX(id) FROM sync_run_items)`),
					time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(attempt)*time.Hour))
				require.NoError(err)
			}
			if tc.other != nil {
				require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
					SyncRunID: otherRun, SourceMessageID: tc.name, Phase: tc.other.phase, Status: tc.other.status, ErrorKind: tc.other.kind,
				}))
			}
			items, err := f.Store.ListUnresolvedSyncItemErrors(t.Context(), f.Source.ID)
			require.NoError(err)
			var selected []store.SyncRunItem
			for _, item := range items {
				if item.SourceMessageID == tc.name {
					selected = append(selected, item)
				}
			}
			if tc.wantKind == "" {
				assert.Empty(selected)
			} else {
				require.Len(selected, 1)
				assert.Equal(tc.wantKind, selected[0].ErrorKind)
			}
			// A second upsert reuses the occurrence. Neither path may add it twice.
			for range 2 {
				require.NoError(f.Store.UpsertRemoteImageAttachment(t.Context(), id, store.AttachmentWrite{
					SourcePartKey: "remote-image:example", StoragePath: "aa/image", Size: 10,
				}))
				require.NoError(f.Store.RecomputeRemoteImageAttachmentStatsContext(t.Context(), id))
				assertSelectionAttachmentCount(t, f.Store, id, tc.wantCount)
			}
		})
	}
}

func TestSyncItemSelectionCompletedBoundary(t *testing.T) {
	assert, require := assert.New(t), require.New(t)

	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("gmail", "boundary@example.test")
	require.NoError(err)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var runs, messages []int64
	for i, typ := range []string{"full", "incremental", "incremental", "incremental", "full", "incremental"} {
		run, err := f.Store.StartSyncContext(t.Context(), f.Source.ID, typ)
		require.NoError(err)
		name := fmt.Sprintf("boundary-%d", i)
		messages = append(messages, f.NewMessage().WithSourceMessageID(name).WithAttachmentCount(3).Create(t, f.Store))
		require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
			SyncRunID: run, SourceMessageID: name, Phase: "ingest", Status: "error", ErrorKind: "attachment_error",
		}))
		require.NoError(f.Store.CompleteSyncContext(t.Context(), run, "1000"))
		runs = append(runs, run)
	}
	// The lowest incremental ID completed most recently; the highest ID failed.
	for i, hours := range []int{1, 4, 3, 2, 8, 9} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_runs SET completed_at = ? WHERE id = ?`), base.Add(time.Duration(hours)*time.Hour), runs[i])
		require.NoError(err)
	}
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_runs SET status = 'failed' WHERE id = ?`), runs[5])
	require.NoError(err)
	otherRun, err := f.Store.StartSyncContext(t.Context(), other.ID, "incremental")
	require.NoError(err)
	require.NoError(f.Store.CompleteSyncContext(t.Context(), otherRun, "2000"))
	check := func(boundary int) {
		t.Helper()
		items, err := f.Store.ListUnresolvedSyncItemErrors(t.Context(), f.Source.ID)
		require.NoError(err)
		var want []int64
		var got []int64
		for i := boundary; i < len(runs); i++ {
			want = append(want, runs[i])
		}
		for _, item := range items {
			got = append(got, item.SyncRunID)
		}
		assert.Equal(want, got)
		for i, id := range messages {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET attachment_count = 3, has_attachments = TRUE WHERE id = ?`), id)
			require.NoError(err)
			require.NoError(f.Store.RecomputeRemoteImageAttachmentStatsContext(t.Context(), id))
			count := 0
			if i >= boundary {
				count = 3
			}
			assertSelectionAttachmentCount(t, f.Store, id, count)
		}
	}
	check(1)
	// Equal completion times use the greater ID, not insertion order of items.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE sync_runs SET completed_at = ? WHERE id = ?`), base.Add(4*time.Hour), runs[2])
	require.NoError(err)
	check(2)
}

func assertSelectionAttachmentCount(t *testing.T, st *store.Store, id int64, want int) {
	t.Helper()
	var count int
	var has bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT attachment_count, has_attachments FROM messages WHERE id = ?`), id).Scan(&count, &has))
	assert.Equal(t, want, count)
	assert.Equal(t, want > 0, has)
}

func TestSyncItemSelectionCancellation(t *testing.T) {
	require := require.New(t)

	f := storetest.New(t)
	id := f.NewMessage().WithAttachmentCount(3).Create(t, f.Store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.ListUnresolvedSyncItemErrors(ctx, f.Source.ID)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(f.Store.RecomputeRemoteImageAttachmentStatsContext(ctx, id), context.Canceled)
	assertSelectionAttachmentCount(t, f.Store, id, 3)
}

func TestSyncItemSelectionWaitsForMessageLock(t *testing.T) {
	for _, action := range []string{"resolve", "reopen", "cancel"} {
		t.Run(action, func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			if !f.Store.IsPostgreSQL() {
				t.Skip("PostgreSQL row lock; SQLite serializes writes without SELECT FOR UPDATE")
			}
			id := f.NewMessage().WithSourceMessageID("locked").WithAttachmentCount(3).Create(t, f.Store)
			run := f.StartSync()
			status := "error"
			if action == "reopen" {
				status = "resolved"
			}
			require.NoError(f.Store.RecordSyncRunItemContext(t.Context(), store.SyncRunItem{
				SyncRunID: run, SourceMessageID: "locked", Phase: "ingest", Status: status, ErrorKind: "attachment_error",
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
			require.NoError(err)
			defer func() { _ = blocker.Rollback() }()
			var locked, pid int64
			require.NoError(blocker.QueryRowContext(ctx, `SELECT id, pg_backend_pid() FROM messages WHERE id = $1 FOR UPDATE`, id).Scan(&locked, &pid))
			done := make(chan error, 1)
			wait := func() error {
				t.Helper()
				select {
				case err := <-done:
					return err
				case <-time.After(5 * time.Second):
					stacktest.Dump(t)
					require.FailNow("statistics reader did not finish")
					return nil
				}
			}
			go func() { done <- f.Store.RecomputeRemoteImageAttachmentStatsContext(ctx, id) }()
			require.Eventually(func() bool {
				var waiting bool
				err := f.Store.DB().QueryRowContext(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
					AND $1 = ANY(pg_blocking_pids(pid))
					AND query LIKE '%source_message_id FROM messages%FOR UPDATE%')`, pid).Scan(&waiting)
				return err == nil && waiting
			}, 5*time.Second, 10*time.Millisecond, "statistics reader must wait at the message row")
			if action == "cancel" {
				cancel()
				require.ErrorIs(wait(), context.Canceled)
				require.NoError(blocker.Rollback())
				assertSelectionAttachmentCount(t, f.Store, id, 3)
				return
			}
			status = "resolved"
			want := 0
			if action == "reopen" {
				status, want = "error", 3
			}
			// Publish the outcome with the lock held. The waiting reader must
			// base its decision on the committed outcome after acquiring the lock.
			_, err = blocker.ExecContext(ctx, `INSERT INTO sync_run_items
				(sync_run_id, source_message_id, phase, status, error_kind, error_message, created_at)
				VALUES ($1, 'locked', 'ingest', $2, 'attachment_error', '', CURRENT_TIMESTAMP)`, run, status)
			require.NoError(err)
			require.NoError(blocker.Commit())
			require.NoError(wait())
			assertSelectionAttachmentCount(t, f.Store, id, want)
		})
	}
}
