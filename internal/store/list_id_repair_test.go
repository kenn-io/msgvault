package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestRepairListIDsReplaysArchivedMIME catches a missing historical repair,
// stale-value clear, unchanged-row skip, and accidental application during a
// dry run. It uses the persisted zlib MIME representation, not a parser stub.
func TestRepairListIDsReplaysArchivedMIME(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	missing := f.CreateMessage("list-id-missing")
	stale := f.CreateMessage("list-id-stale")
	unchanged := f.CreateMessage("list-id-unchanged")

	setRaw(t, f, missing,
		[]byte("List-Id: Announcements <announce.example.test>\r\n\r\nbody"), "")
	require.NoError(f.Store.UpsertMessageRaw(stale, []byte("Subject: no list\r\n\r\nbody")))
	require.NoError(f.Store.UpsertMessageRaw(unchanged,
		[]byte("List-Id: <already.example.test>\r\n\r\nbody")))
	setListID(t, f, stale, "<stale.example.test>")
	setListID(t, f, unchanged, "<already.example.test>")

	before, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	dryRun, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 3, Found: 2, Changed: 2}, dryRun)
	assertListID(t, f, missing, sql.NullString{})
	assertListID(t, f, stale, sql.NullString{String: "<stale.example.test>", Valid: true})
	afterDryRun, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(before, afterDryRun)

	applied, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(dryRun, applied)
	assertListID(t, f, missing, sql.NullString{String: "<announce.example.test>", Valid: true})
	assertListID(t, f, stale, sql.NullString{})
	afterApply, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(before+1, afterApply)

	again, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 3, Found: 2}, again)
	afterAgain, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(afterApply, afterAgain)
}

func TestRepairListIDsIncludesLegacyEmailRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-legacy-email")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = '' WHERE id = ?`), messageID)
	require.NoError(err)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <legacy.example.test>\r\n\r\nbody")))

	summary, err := f.Store.RepairListIDs(t.Context(),
		store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1, Changed: 1}, summary)
	assertListID(t, f, messageID,
		sql.NullString{String: "<legacy.example.test>", Valid: true})
}

// TestRepairListIDsIdempotentApplyPreservesMessageWatermarks catches a SQLite
// writer-lock statement that updates an arbitrary message column and therefore
// advances its optimistic-CAS watermark even though every List-Id is current.
func TestRepairListIDsIdempotentApplyPreservesMessageWatermarks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite writer-lock watermark regression")
	}
	messageID := f.CreateMessage("list-id-idempotent-watermarks")
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <idempotent.example.test>\r\n\r\nbody")))
	_, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)

	const lastModified = "2001-02-03 04:05:06"
	const contentChangedAt = "2002-03-04 05:06:07.008"
	_, err = f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE messages
		SET last_modified = ?, content_changed_at = ?
		WHERE id = ?`), lastModified, contentChangedAt, messageID)
	require.NoError(err)
	beforeLastModified, beforeContentChangedAt := messageWatermarks(t, f, messageID)
	beforeRevision, err := f.Store.DerivedDataRevision()
	require.NoError(err)

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	afterLastModified, afterContentChangedAt := messageWatermarks(t, f, messageID)
	afterRevision, err := f.Store.DerivedDataRevision()
	require.NoError(err)

	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1}, summary)
	assert.Equal(beforeLastModified, afterLastModified)
	assert.Equal(beforeContentChangedAt, afterContentChangedAt)
	assert.Equal(beforeRevision, afterRevision)
}

// TestRepairListIDsSkipsNonMIMEAndUndecodableRows catches repair attempts that
// accidentally treat chat payloads or damaged/oversized streams as email MIME.
func TestRepairListIDsSkipsNonMIMEAndUndecodableRows(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	nonEmail := f.NewMessage().WithSourceMessageID("list-id-chat").Build()
	nonEmail.MessageType = "chat"
	nonEmailID, err := f.Store.UpsertMessage(nonEmail)
	require.NoError(err)
	unsupported := f.CreateMessage("list-id-unsupported")
	corrupt := f.CreateMessage("list-id-corrupt")
	oversized := f.CreateMessage("list-id-oversized")

	require.NoError(f.Store.UpsertMessageRaw(nonEmailID,
		[]byte("List-Id: <chat.example.test>\r\n\r\nbody")))
	require.NoError(f.Store.UpsertMessageRawWithFormat(unsupported,
		[]byte("List-Id: <other.example.test>\r\n\r\nbody"), "gcal_json"))
	setRaw(t, f, corrupt, []byte("not-zlib"), "zlib")
	require.NoError(f.Store.UpsertMessageRaw(oversized,
		[]byte("List-Id: <"+strings.Repeat("a", 256)+">\r\n\r\nbody")))
	setListID(t, f, nonEmailID, "<keep-chat.example.test>")

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{
		Apply:          true,
		MaxHeaderBytes: 64,
	}, nil)
	require.NoError(err)
	assert.Equal(t, store.ListIDRepairSummary{Scanned: 3, Undecodable: 3}, summary)
	assertListID(t, f, nonEmailID, sql.NullString{String: "<keep-chat.example.test>", Valid: true})
	assertListID(t, f, unsupported, sql.NullString{})
	assertListID(t, f, corrupt, sql.NullString{})
	assertListID(t, f, oversized, sql.NullString{})
}

// TestRepairListIDsCancellation catches a repair that ignores an already
// cancelled caller context and makes archive changes after cancellation.
func TestRepairListIDsCancellation(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-cancelled")
	require.NoError(t, f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <cancel.example.test>\r\n\r\nbody")))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	summary, err := f.Store.RepairListIDs(ctx, store.ListIDRepairOptions{Apply: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, store.ListIDRepairSummary{}, summary)
	assertListID(t, f, messageID, sql.NullString{})
}

// TestRepairListIDsCancellationRollsBackAllBatches catches an apply repair
// that commits a first keyset batch before a later cancellation. The cache
// revision and every list ID must remain unchanged until the whole pass commits.
func TestRepairListIDsCancellationRollsBackAllBatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.CreateMessage("list-id-cancel-after-first")
	second := f.CreateMessage("list-id-cancel-after-second")
	require.NoError(f.Store.UpsertMessageRaw(first,
		[]byte("List-Id: <first.example.test>\r\n\r\nbody")))
	require.NoError(f.Store.UpsertMessageRaw(second,
		[]byte("List-Id: <second.example.test>\r\n\r\nbody")))
	before, err := f.Store.DerivedDataRevision()
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	summary, err := f.Store.RepairListIDs(ctx, store.ListIDRepairOptions{
		Apply:     true,
		BatchSize: 1,
	}, func(store.ListIDRepairSummary) {
		cancel()
	})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(store.ListIDRepairSummary{}, summary)
	assertListID(t, f, first, sql.NullString{})
	assertListID(t, f, second, sql.NullString{})
	after, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(before, after)
}

// TestRepairListIDsRollsBackEarlierBatchesOnLaterWriteFailure catches a repair
// that exposes a first keyset batch when a later batch fails. The SQLite
// trigger forces a real second message update error after the first batch has
// been prepared, proving the pass is one cache-visible transaction.
func TestRepairListIDsRollsBackEarlierBatchesOnLaterWriteFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite trigger injects a later-batch write failure")
	}
	first := f.CreateMessage("list-id-rollback-first")
	second := f.CreateMessage("list-id-rollback-second")
	require.NoError(f.Store.UpsertMessageRaw(first,
		[]byte("List-Id: <rollback-first.example.test>\r\n\r\nbody")))
	require.NoError(f.Store.UpsertMessageRaw(second,
		[]byte("List-Id: <rollback-second.example.test>\r\n\r\nbody")))
	before, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	_, err = f.Store.DB().Exec(`
		CREATE TRIGGER fail_later_list_id_repair
		BEFORE UPDATE OF list_id ON messages
		WHEN NEW.source_message_id = 'list-id-rollback-second'
		BEGIN
			SELECT RAISE(ABORT, 'forced later List-Id repair failure');
		END`)
	require.NoError(err)

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{
		Apply:     true,
		BatchSize: 1,
	}, nil)
	require.Error(err)
	assert.Equal(store.ListIDRepairSummary{}, summary)
	assertListID(t, f, first, sql.NullString{})
	assertListID(t, f, second, sql.NullString{})
	after, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(before, after)
}

// TestRepairListIDsBoundsRawDataRead catches a repair query that asks the
// driver to materialize a full archived body before applying its MIME-header
// cap. The allocation measurement covers a real SQLite BLOB scan, after the
// large fixture has already been persisted and collected.
func TestRepairListIDsBoundsRawDataRead(t *testing.T) {
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-large-raw")
	raw := append([]byte("List-Id: <large.example.test>\r\n\r\n"), bytes.Repeat([]byte("x"), 8<<20)...)
	setRaw(t, f, messageID, raw, "")
	runtime.GC()

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{
		Apply:          true,
		MaxHeaderBytes: 64,
		MaxRawBytes:    256,
	}, nil)
	require.NoError(t, err)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	assert.Equal(t, store.ListIDRepairSummary{Scanned: 1, Found: 1, Changed: 1}, summary)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20),
		"repair must not materialize the eight-megabyte archived body")
	assertListID(t, f, messageID, sql.NullString{String: "<large.example.test>", Valid: true})
}

// TestRepairListIDsIncludesNonpositiveMessageIDs catches a keyset cursor that
// assumes identifiers begin at one, although the archive permits zero and
// negative primary keys.
func TestRepairListIDsIncludesNonpositiveMessageIDs(t *testing.T) {
	f := storetest.New(t)
	zero := insertMessageAtID(t, f, 0, "list-id-zero")
	negative := insertMessageAtID(t, f, -1, "list-id-negative")
	setRaw(t, f, zero, []byte("List-Id: <zero.example.test>\r\n\r\n"), "")
	setRaw(t, f, negative, []byte("List-Id: <negative.example.test>\r\n\r\n"), "")

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, store.ListIDRepairSummary{Scanned: 2, Found: 2, Changed: 2}, summary)
	assertListID(t, f, zero, sql.NullString{String: "<zero.example.test>", Valid: true})
	assertListID(t, f, negative, sql.NullString{String: "<negative.example.test>", Valid: true})
}

// TestRepairListIDsHonorsExactHeaderLimit catches a delimiter search that
// accepts a header one byte past the configured decompression limit.
func TestRepairListIDsHonorsExactHeaderLimit(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	const maxHeaderBytes = 64
	within := f.CreateMessage("list-id-limit-within")
	over := f.CreateMessage("list-id-limit-over")
	withinRaw := listIDHeaderAtLength(maxHeaderBytes)
	overRaw := listIDHeaderAtLength(maxHeaderBytes + 1)
	require.Len(withinRaw, maxHeaderBytes)
	require.Len(overRaw, maxHeaderBytes+1)
	setRaw(t, f, within, withinRaw, "")
	setRaw(t, f, over, overRaw, "")

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{
		Apply:          true,
		MaxHeaderBytes: maxHeaderBytes,
	}, nil)
	require.NoError(err)
	assert.Equal(t, store.ListIDRepairSummary{Scanned: 2, Found: 1, Changed: 1, Undecodable: 1}, summary)
	assertListID(t, f, within, sql.NullString{String: "<" + strings.Repeat("x", 49) + ">", Valid: true})
	assertListID(t, f, over, sql.NullString{})
}

// TestRepairListIDsLeavesConcurrentRawResyncUntouched catches a repair that
// overwrites a real sync which replaces both raw MIME and list_id before the
// repair transaction reads its first batch.
func TestRepairListIDsLeavesConcurrentRawResyncUntouched(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-concurrent-resync")
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <old.example.test>\r\n\r\nbody")))
	restore := f.Store.SetListIDRepairBeforeApplyHookForTest(func() {
		require.NoError(f.Store.UpsertMessageRaw(messageID,
			[]byte("List-Id: <new.example.test>\r\n\r\nbody")))
		setListID(t, f, messageID, "<new.example.test>")
	})
	defer restore()

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1}, summary)
	assertListID(t, f, messageID, sql.NullString{String: "<new.example.test>", Valid: true})
	revision, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Zero(revision)
}

// TestRepairListIDsSkipsPostScanRawResync catches a repair that writes a
// List-Id derived from stale MIME after the same transaction has replaced the
// archived payload and its authoritative value. Removing the fingerprint
// predicates would overwrite the resync with <old.example.test>.
func TestRepairListIDsSkipsPostScanRawResync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-post-scan-resync")
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <old.example.test>\r\n\r\nbody")))
	restore := f.Store.SetListIDRepairAfterScanMutationForTest(
		func(id int64, replaceRawAndListID func([]byte, string) error) error {
			require.Equal(messageID, id)
			return replaceRawAndListID(
				[]byte("List-Id: <new.example.test>\r\n\r\nbody"),
				"<new.example.test>")
		})
	defer restore()

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1}, summary)
	assertListID(t, f, messageID, sql.NullString{String: "<new.example.test>", Valid: true})
	revision, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Zero(revision)
	decoded, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1}, decoded)
}

// TestRepairListIDsSerializesSQLiteWriterBeforeSnapshot catches an apply pass
// that takes its first read snapshot before acquiring SQLite's writer slot. A
// second file-backed WAL connection commits after the repair scan; a deferred
// repair then cannot upgrade to write and returns SQLITE_BUSY_SNAPSHOT.
func TestRepairListIDsSerializesSQLiteWriterBeforeSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	st.DB().SetMaxOpenConns(2)
	st.DB().SetMaxIdleConns(2)
	source, err := st.GetOrCreateSource("gmail", "list-id-wal@example.test")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "list-id-wal", "List-Id WAL")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "list-id-wal-message",
		MessageType:     "email",
		SizeEstimate:    1,
	})
	require.NoError(err)
	require.NoError(st.UpsertMessageRaw(messageID,
		[]byte("List-Id: <wal.example.test>\r\n\r\nbody")))

	writerCtx, cancelWriter := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWriter()
	writer, err := st.DB().Conn(writerCtx)
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })
	writerStarted := make(chan struct{})
	writerLocked := make(chan struct{})
	writerDone := make(chan error, 1)
	writerFinished := false
	var writerErr error
	startWriter := func() {
		go func() {
			close(writerStarted)
			begun := false
			defer func() {
				if begun {
					cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(writerCtx), time.Second)
					defer cleanupCancel()
					_, _ = writer.ExecContext(cleanupCtx, "ROLLBACK")
				}
			}()
			if _, err := writer.ExecContext(writerCtx, "BEGIN IMMEDIATE"); err != nil {
				writerDone <- err
				return
			}
			begun = true
			close(writerLocked)
			if _, err := writer.ExecContext(writerCtx,
				`UPDATE messages SET snippet = ? WHERE id = ?`, "concurrent writer", messageID); err != nil {
				writerDone <- err
				return
			}
			if _, err := writer.ExecContext(writerCtx, "COMMIT"); err != nil {
				writerDone <- err
				return
			}
			begun = false
			writerDone <- nil
		}()
	}

	restore := st.SetListIDRepairAfterScanMutationForTest(
		func(_ int64, _ func([]byte, string) error) error {
			startWriter()
			select {
			case <-writerStarted:
			case <-time.After(time.Second):
				return errors.New("concurrent SQLite writer did not start")
			}
			select {
			case <-writerLocked:
				select {
				case writerErr = <-writerDone:
					writerFinished = true
					return writerErr
				case <-time.After(time.Second):
					return errors.New("concurrent SQLite writer did not commit")
				}
			case <-time.After(200 * time.Millisecond):
				return nil
			}
		})
	defer restore()

	summary, err := st.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	if !writerFinished {
		writerErr = <-writerDone
	}
	require.NoError(writerErr)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1, Changed: 1}, summary)
	var snippet sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT snippet FROM messages WHERE id = ?`), messageID).Scan(&snippet))
	assert.Equal(sql.NullString{String: "concurrent writer", Valid: true}, snippet)
	var listID sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT list_id FROM messages WHERE id = ?`), messageID).Scan(&listID))
	assert.Equal(sql.NullString{String: "<wal.example.test>", Valid: true}, listID)
}

// TestPostgreSQLRepairListIDsLocksFingerprintBeforeUpdate catches removing
// SELECT FOR UPDATE from the fingerprint guard. The second real connection
// must block until repair commits its old-MIME update, then publish its newer
// raw MIME and List-Id.
func TestPostgreSQLRepairListIDsLocksFingerprintBeforeUpdate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL fingerprint-lock regression")
	}
	messageID := f.CreateMessage("list-id-postgres-fingerprint-lock")
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <old.example.test>\r\n\r\nbody")))

	locked := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRepair := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRepair()
	restore := f.Store.SetListIDRepairAfterFingerprintLockHookForTest(func() {
		locked <- struct{}{}
		<-release
	})
	defer restore()

	type repairResult struct {
		summary store.ListIDRepairSummary
		err     error
	}
	repairDone := make(chan repairResult, 1)
	go func() {
		summary, err := f.Store.RepairListIDs(
			t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
		repairDone <- repairResult{summary: summary, err: err}
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		require.FailNow("repair did not lock its fingerprint")
	}

	writer, err := f.Store.DB().Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = writer.Close() })
	writerDone := make(chan error, 1)
	go func() {
		tx, err := writer.BeginTx(t.Context(), nil)
		if err != nil {
			writerDone <- err
			return
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(),
			`UPDATE message_raw SET raw_data = $1, compression = NULL WHERE message_id = $2`,
			[]byte("List-Id: <new.example.test>\r\n\r\nbody"), messageID)
		if err != nil {
			writerDone <- err
			return
		}
		_, err = tx.ExecContext(t.Context(),
			`UPDATE messages SET list_id = $1 WHERE id = $2`, "<new.example.test>", messageID)
		if err != nil {
			writerDone <- err
			return
		}
		writerDone <- tx.Commit()
	}()

	require.Eventually(func() bool {
		var waiting int
		err := f.Store.DB().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		require.NoError(err)
		return waiting > 0
	}, 5*time.Second, 10*time.Millisecond, "concurrent raw-MIME writer did not block on fingerprint lock")

	releaseRepair()
	repair := <-repairDone
	require.NoError(repair.err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1, Changed: 1}, repair.summary)
	require.NoError(<-writerDone)
	assertListID(t, f, messageID, sql.NullString{String: "<new.example.test>", Valid: true})
	revision, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(int64(1), revision)
	decoded, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1}, decoded)
}

// TestRepairListIDsCommitsRevisionWithRepair catches an apply repair that
// commits changed list IDs without the matching derived-data revision.
func TestRepairListIDsCommitsRevisionWithRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("list-id-atomic-revision")
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("List-Id: <atomic.example.test>\r\n\r\nbody")))

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{Apply: true}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 1, Found: 1, Changed: 1}, summary)
	assertListID(t, f, messageID, sql.NullString{String: "<atomic.example.test>", Valid: true})
	revision, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(int64(1), revision)
}

// TestRepairListIDsBumpsRevisionOnceAcrossBatches catches a per-batch revision
// bump. A complete two-batch repair must publish one derived-data revision at
// its atomic whole-pass commit.
func TestRepairListIDsBumpsRevisionOnceAcrossBatches(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first := f.CreateMessage("list-id-first-batch")
	second := f.CreateMessage("list-id-second-batch")
	require.NoError(f.Store.UpsertMessageRaw(first,
		[]byte("List-Id: <first-batch.example.test>\r\n\r\nbody")))
	require.NoError(f.Store.UpsertMessageRaw(second,
		[]byte("List-Id: <second-batch.example.test>\r\n\r\nbody")))
	before, err := f.Store.DerivedDataRevision()
	require.NoError(err)

	summary, err := f.Store.RepairListIDs(t.Context(), store.ListIDRepairOptions{
		Apply:     true,
		BatchSize: 1,
	}, nil)
	require.NoError(err)
	assert.Equal(store.ListIDRepairSummary{Scanned: 2, Found: 2, Changed: 2}, summary)
	after, err := f.Store.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(before+1, after)
	assertListID(t, f, first, sql.NullString{String: "<first-batch.example.test>", Valid: true})
	assertListID(t, f, second, sql.NullString{String: "<second-batch.example.test>", Valid: true})
}

func setListID(t *testing.T, f *storetest.Fixture, messageID int64, value string) {
	t.Helper()
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET list_id = ? WHERE id = ?`), value, messageID)
	require.NoError(t, err)
}

func assertListID(t *testing.T, f *storetest.Fixture, messageID int64, want sql.NullString) {
	t.Helper()
	var got sql.NullString
	err := f.Store.DB().QueryRow(f.Store.Rebind(`SELECT list_id FROM messages WHERE id = ?`), messageID).Scan(&got)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func messageWatermarks(t *testing.T, f *storetest.Fixture, messageID int64) (string, string) {
	t.Helper()
	var lastModified, contentChangedAt string
	err := f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT CAST(last_modified AS TEXT), CAST(content_changed_at AS TEXT)
		FROM messages
		WHERE id = ?`), messageID).Scan(&lastModified, &contentChangedAt)
	require.NoError(t, err)
	return lastModified, contentChangedAt
}

func setRaw(t *testing.T, f *storetest.Fixture, messageID int64, raw []byte, compression string) {
	t.Helper()
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		VALUES (?, ?, ?, ?)`), messageID, raw, "mime", compression)
	require.NoError(t, err)
}

func insertMessageAtID(t *testing.T, f *storetest.Fixture, id int64, sourceMessageID string) int64 {
	t.Helper()
	insert := `
		INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, size_estimate)
		VALUES (?, ?, ?, ?, 'email', 1)`
	if f.Store.IsPostgreSQL() {
		insert = `
			INSERT INTO messages (id, conversation_id, source_id, source_message_id, message_type, size_estimate)
			OVERRIDING SYSTEM VALUE
			VALUES (?, ?, ?, ?, 'email', 1)`
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(insert), id, f.ConvID, f.Source.ID, sourceMessageID)
	require.NoError(t, err)
	return id
}

func listIDHeaderAtLength(length int) []byte {
	const fixed = "List-Id: <>\r\n\r\n"
	return []byte("List-Id: <" + strings.Repeat("x", length-len(fixed)) + ">\r\n\r\n")
}
