package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A canceled invocation cannot register/reset a generation or advance its
// journal checkpoint. A later invocation must still initialize normally.
func TestFilenameDiscoveryBootstrapCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	id := createJournalAttachment(t, f, f.CreateMessage("filename-cancel"), "part:1", "a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.FilenameDiscoveryBatchContext(ctx)
	require.ErrorIs(err, context.Canceled)
	status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Zero(status.Generation)
	_, err = f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
	ready := filenameReady(t, f.Store)
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, ready.Generation, id))
	require.ErrorIs(f.Store.ResetFilenameDiscoveryContext(ctx), context.Canceled)
	unchanged, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(ready, unchanged)
}

func filenameLocation(t *testing.T, s *store.Store) string {
	t.Helper()
	var location string
	if s.IsPostgreSQL() {
		var database, schema string
		require.NoError(t, s.DB().QueryRow(`SELECT current_database(), current_schema()`).Scan(&database, &schema))
		u, err := url.Parse(os.Getenv("MSGVAULT_TEST_DB"))
		require.NoError(t, err)
		u.Path = "/" + database
		params := u.Query()
		params.Set("search_path", schema)
		u.RawQuery = params.Encode()
		location = u.String()
	} else {
		var seq int
		var name string
		require.NoError(t, s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &location))
		require.Equal(t, "main", name)
	}
	return location
}

func filenameReopen(t *testing.T, s *store.Store) *store.Store {
	t.Helper()
	location := filenameLocation(t, s)
	require.NoError(t, s.Close())
	reopened, err := store.OpenForTest(location)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	return reopened
}

func filenameReady(t *testing.T, s *store.Store) store.FilenameDiscoveryStatus {
	t.Helper()
	// This is a fixture drain of bounded calls, never a production drain loop.
	for range 100 {
		batch, err := s.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(t, err)
		assert.LessOrEqual(t, batch.Processed, 500)
		if batch.Status.Phase == "ready" {
			return batch.Status
		}
		require.True(t, batch.More)
	}
	require.FailNow(t, "fixture never became ready within 100 bounded calls")
	return store.FilenameDiscoveryStatus{}
}

func filenameEntries(t *testing.T, s *store.Store, generation int64) int {
	t.Helper()
	var count int
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT count(*)
		FROM filename_discovery_entries WHERE generation=?`), generation).Scan(&count))
	return count
}

func filenameEntryName(t *testing.T, s *store.Store, generation, id int64) string {
	t.Helper()
	var name string
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT normalized_name
		FROM filename_discovery_entries WHERE generation=? AND attachment_id=?`), generation, id).Scan(&name))
	return name
}

type filenameWriteCounter struct {
	enabled       atomic.Bool
	entryInserts  atomic.Int64
	entryDeletes  atomic.Int64
	tokenInserts  atomic.Int64
	deleteInserts atomic.Int64
}

func watchFilenameWrites(t *testing.T, s *store.Store) *filenameWriteCounter {
	t.Helper()
	testutil.SkipIfPostgres(t, "uses the SQLite authorizer to count projection write statements")
	s.DB().SetMaxOpenConns(1)
	s.DB().SetMaxIdleConns(1)

	counter := &filenameWriteCounter{}
	conn, err := s.DB().Conn(t.Context())
	require.NoError(t, err)
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("expected SQLite connection, got %T", driverConn)
		}
		sqliteConn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if counter.enabled.Load() {
				switch {
				case action == sqlite3.SQLITE_INSERT && table == "filename_discovery_entries":
					counter.entryInserts.Add(1)
				case action == sqlite3.SQLITE_DELETE && table == "filename_discovery_entries":
					counter.entryDeletes.Add(1)
				case action == sqlite3.SQLITE_INSERT && table == "filename_discovery_tokens":
					counter.tokenInserts.Add(1)
				case action == sqlite3.SQLITE_INSERT && table == "filename_discovery_deletes":
					counter.deleteInserts.Add(1)
				}
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	t.Cleanup(func() { counter.enabled.Store(false) })
	return counter
}

func (counter *filenameWriteCounter) resetAndEnable() {
	counter.entryInserts.Store(0)
	counter.entryDeletes.Store(0)
	counter.tokenInserts.Store(0)
	counter.deleteInserts.Store(0)
	counter.enabled.Store(true)
}

func (counter *filenameWriteCounter) disable() {
	counter.enabled.Store(false)
}

// A 500-item source page and replay page must batch projection writes across
// attachment occurrences. Per-attachment SQL makes ordinary filenames issue
// thousands of sequential statements inside one bounded transaction.
func TestFilenameDiscoveryBatchUsesSetBasedWrites(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "uses the SQLite authorizer to count projection write statements")

	f := storetest.New(t)
	message := f.CreateMessage("filename-batch-writes")
	insert, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	ids := make([]int64, 0, 500)
	for i := range 500 {
		var id int64
		err = insert.QueryRow(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES(?,?, '') RETURNING id`),
			message, fmt.Sprintf("report-%04d.pdf", i)).Scan(&id)
		require.NoError(err)
		ids = append(ids, id)
	}
	require.NoError(insert.Commit())

	counter := watchFilenameWrites(t, f.Store)
	counter.resetAndEnable()
	bootstrap, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	counter.disable()
	require.NoError(err)
	assert.Equal(500, bootstrap.Processed)
	assert.Equal(500, filenameEntries(t, f.Store, bootstrap.Status.Generation))
	assert.Equal("report-0000.pdf", filenameEntryName(t, f.Store, bootstrap.Status.Generation, ids[0]))
	assert.LessOrEqual(counter.entryInserts.Load(), int64(8), "entries use a bounded number of multi-row inserts")
	assert.LessOrEqual(counter.entryDeletes.Load(), int64(1), "a new generation has no prior entries to delete")
	assert.LessOrEqual(counter.tokenInserts.Load(), int64(20), "token writes span the whole page")
	assert.LessOrEqual(counter.deleteInserts.Load(), int64(50), "deletion-key writes span the whole page")

	for range 2 {
		_, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
	}
	ready, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	require.Equal("ready", ready.Phase)

	update, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i, id := range ids {
		_, err = update.Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`),
			fmt.Sprintf("renamed-%04d.pdf", i), id)
		require.NoError(err)
	}
	require.NoError(update.Commit())

	counter.resetAndEnable()
	replay, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	counter.disable()
	require.NoError(err)
	assert.Equal(500, replay.Processed)
	assert.Equal("renamed-0000.pdf", filenameEntryName(t, f.Store, ready.Generation, ids[0]))
	assert.LessOrEqual(counter.entryInserts.Load(), int64(8), "replay entries use a bounded number of multi-row inserts")
	assert.LessOrEqual(counter.entryDeletes.Load(), int64(8), "replay removes entries in bounded batches")
	assert.LessOrEqual(counter.tokenInserts.Load(), int64(20), "replay token writes span the whole page")
	assert.LessOrEqual(counter.deleteInserts.Load(), int64(50), "replay deletion-key writes span the whole page")
}

// Moving scan_end, rescanning completed pages, or publishing ready before
// the fixed catchup boundary would lose the later filename change/insert.
func TestFilenameDiscoveryBootstrapResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-bootstrap")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	var initialMax, firstID int64
	for i := range 1201 {
		var id int64
		require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES(?,?, '') RETURNING id`),
			message, fmt.Sprintf("report-%04d.pdf", i)).Scan(&id))
		initialMax = max(initialMax, id)
		if i == 0 {
			firstID = id
		}
	}
	require.NoError(tx.Commit())
	first, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(500, first.Processed)
	require.Equal(initialMax, first.Status.ScanEnd)
	assert.Equal("bootstrap", first.Status.Phase)
	assert.True(first.More)
	assert.Equal(500, filenameEntries(t, f.Store, first.Status.Generation))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "changed.pdf", firstID)
	require.NoError(err)
	later := createJournalAttachment(t, f, message, "part:later", "a")
	f.Store = filenameReopen(t, f.Store)
	resumed, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(first.Status, resumed, "reopen resumes the committed cursor/generation")
	for _, want := range []int{500, 201} {
		batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
		assert.Equal(want, batch.Processed)
		require.Equal(initialMax, batch.Status.ScanEnd)
		assert.True(batch.More)
		assert.NotEqual("ready", batch.Status.Phase)
	}
	status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal("catchup", status.Phase)
	assert.Greater(status.CatchupFence, status.Baseline)
	assert.Equal("report-0000.pdf", filenameEntryName(t, f.Store, status.Generation, firstID))
	ready := filenameReady(t, f.Store)
	assert.Equal("changed.pdf", filenameEntryName(t, f.Store, ready.Generation, firstID))
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, ready.Generation, later))
	assert.Equal(1202, filenameEntries(t, f.Store, ready.Generation))
	assert.GreaterOrEqual(ready.LastSequence, ready.CatchupFence)
	empty, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Zero(empty.Processed)
	assert.False(empty.More)
	assert.Equal(ready.Revision, empty.Status.Revision, "empty ready polls cannot invent freshness")
}

// Failure while acknowledging must roll back already-applied projection rows,
// state revision and consumer checkpoint. A real SQL trigger provides the fault.
func TestFilenameDiscoveryApplyAckAtomic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	id := createJournalAttachment(t, f, f.CreateMessage("filename-atomic"), "part:1", "a")
	before := filenameReady(t, f.Store)
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "updated.pdf", id)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION filename_test_fail_ack() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN IF NEW.consumer_key='filename-discovery/v1' AND NEW.last_sequence>OLD.last_sequence
			THEN RAISE EXCEPTION 'synthetic filename ack fault'; END IF; RETURN NEW; END $$;
			CREATE TRIGGER filename_test_fail_ack BEFORE UPDATE ON attachment_change_consumers
			FOR EACH ROW EXECUTE FUNCTION filename_test_fail_ack()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER filename_test_fail_ack BEFORE UPDATE ON attachment_change_consumers
			WHEN NEW.consumer_key='filename-discovery/v1' AND NEW.last_sequence>OLD.last_sequence
			BEGIN SELECT RAISE(ABORT, 'synthetic filename ack fault'); END`)
	}
	require.NoError(err)
	_, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.ErrorContains(err, "synthetic filename ack fault")
	f.Store = filenameReopen(t, f.Store)
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, before.Generation, id))
	failed, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(before, failed)
	still, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.NoError(err)
	assert.Equal(consumer.LastSequence, still.LastSequence)
	statement := `DROP TRIGGER filename_test_fail_ack`
	if f.Store.IsPostgreSQL() {
		statement += ` ON attachment_change_consumers`
	}
	_, err = f.Store.DB().Exec(statement)
	require.NoError(err)
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, batch.Processed)
	assert.Equal("updated.pdf", filenameEntryName(t, f.Store, before.Generation, id))
	advanced, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.NoError(err)
	assert.Greater(advanced.LastSequence, consumer.LastSequence)
	assert.Equal(advanced.LastSequence, batch.Status.LastSequence)
	var sequence sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(`SELECT max(sequence) FROM attachment_change_log`).Scan(&sequence))
	assert.False(sequence.Valid, "sole consumer prunes only after successful apply+ack")
}

// Events after the captured catchup fence are freshness lag. They cannot keep
// a finite generation unavailable or make one invocation drain two batches.
func TestFilenameDiscoveryBootstrapFiniteCatchup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-finite-catchup")
	createJournalAttachment(t, f, message, "part:initial", "a")
	first, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, first.Processed)
	require.Equal("catchup", first.Status.Phase)
	assert.True(first.More, "even an empty fixed catchup needs its own invocation")
	assert.Equal(first.Status.Baseline, first.Status.CatchupFence)
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete, "journal replay eligibility is separate from API readiness")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i := range 501 {
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES (?, ?, '')`), message, fmt.Sprintf("later-%04d.pdf", i))
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	ready, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Zero(ready.Processed)
	assert.Equal("ready", ready.Status.Phase, "later events must not move the catchup fence")
	assert.True(ready.More, "post-fence events need a separate bounded scheduler pass")
	assert.Equal(first.Status.CatchupFence, ready.Status.CatchupFence)
	assert.Equal(first.Status.CatchupFence, ready.Status.LastSequence)
	assert.Equal(1, filenameEntries(t, f.Store, ready.Status.Generation))
	next, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(500, next.Processed)
	assert.True(next.More)
	assert.Equal(501, filenameEntries(t, f.Store, ready.Status.Generation))
	last, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, last.Processed)
	assert.Equal(502, filenameEntries(t, f.Store, ready.Status.Generation))
}

func TestFilenameDiscoveryAttachmentLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-lifecycle")
	id := createJournalAttachment(t, f, message, "part:stable", "a")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages
		SET deleted_at=CURRENT_TIMESTAMP, deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), message)
	require.NoError(err)
	other, _, err := f.Store.RegisterAttachmentChangeConsumer(t.Context(), "synthetic-other/v1")
	require.NoError(err)
	require.NoError(f.Store.CompleteAttachmentChangeReconciliation(t.Context(), other.ConsumerKey, other.BaselineSequence))
	ready := filenameReady(t, f.Store)
	assert.Equal("synthetic.pdf", filenameEntryName(t, f.Store, ready.Generation, id), "bootstrap includes hidden/source-deleted occurrences")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at=NULL WHERE id=?`), message)
	require.NoError(err)
	_, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(1, filenameEntries(t, f.Store, ready.Generation), "unhide while source-deleted retains occurrence")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), message, store.AttachmentWrite{
		Filename: "replacement.pdf", MIMEType: "application/pdf", Size: 64,
		ContentHash: strings.Repeat("b", 64), StoragePath: "bb/" + strings.Repeat("b", 64),
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: "part:stable",
	}))
	_, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal("replacement.pdf", filenameEntryName(t, f.Store, ready.Generation, id), "stable source part retains identity across byte replacement")
	assert.Equal(1, filenameEntries(t, f.Store, ready.Generation))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), strings.Repeat("a", 65), id)
	require.NoError(err)
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(int64(1), batch.Status.Skipped)
	var postings int
	require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM filename_discovery_tokens`).Scan(&postings))
	assert.Zero(postings, "oversized name is excluded as a whole")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "short.pdf", id)
	require.NoError(err)
	batch, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Zero(batch.Status.Skipped)
	assert.Equal("short.pdf", filenameEntryName(t, f.Store, ready.Generation, id))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM attachments WHERE id=?`), id)
	require.NoError(err)
	batch, err = f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Zero(filenameEntries(t, f.Store, ready.Generation))
	assert.Zero(batch.Status.Skipped)
	changes, err := f.Store.ListAttachmentChanges(t.Context(), other.ConsumerKey, 500)
	require.NoError(err)
	require.NotEmpty(changes, "filename acknowledgements cannot prune another consumer's events")
	require.NoError(f.Store.AdvanceAttachmentChangeConsumer(t.Context(), other.ConsumerKey, changes[len(changes)-1].Sequence))
	assert.Zero(attachmentChangeCount(t, f))
	// Reset modifies only this consumer and starts an invisible new generation.
	require.NoError(f.Store.ResetFilenameDiscoveryContext(t.Context()))
	reset, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(ready.Generation+1, reset.Generation)
	assert.Equal("bootstrap", reset.Phase)
	retained, err := f.Store.GetAttachmentChangeConsumer(t.Context(), other.ConsumerKey)
	require.NoError(err)
	assert.Equal(other.BaselineSequence, retained.BaselineSequence)
	assert.True(retained.ReconciliationComplete)
}

func TestFilenameDiscoveryBootstrapConcurrentAndResetGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-concurrent-reset")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i := range 1201 {
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO attachments
			(message_id, filename, storage_path) VALUES(?,?, '')`), message, fmt.Sprintf("report-%04d.pdf", i))
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	second, err := store.OpenForTest(filenameLocation(t, f.Store))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(second.Close()) })
	start := make(chan struct{})
	type result struct {
		worker *store.Store
		batch  store.FilenameDiscoveryBatch
		err    error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, worker := range []*store.Store{f.Store, second} {
		workers.Go(func() {
			<-start
			batch, err := worker.FilenameDiscoveryBatchContext(t.Context())
			results <- result{worker: worker, batch: batch, err: err}
		})
	}
	close(start)
	workers.Wait()
	var completed int
	var retry []*store.Store
	for range 2 {
		got := <-results
		if got.err != nil {
			if f.Store.IsPostgreSQL() {
				require.ErrorContains(got.err, "lock timeout")
			} else {
				require.ErrorContains(got.err, "database is locked")
			}
			retry = append(retry, got.worker)
			continue
		}
		assert.Equal(500, got.batch.Processed)
		completed++
	}
	require.Positive(completed, "one independent worker must commit a bounded page")
	status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(completed*500, filenameEntries(t, f.Store, status.Generation), "a timed-out worker commits no partial page")
	// The real lock timeout is an allowed bounded outcome, not a throughput
	// assertion. Once contention ends, retry each failed invocation only once.
	for _, worker := range retry {
		batch, err := worker.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
		assert.Equal(500, batch.Processed)
	}
	status, err = f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(1000, filenameEntries(t, f.Store, status.Generation), "serialized invocations process separate pages")
	ready := filenameReady(t, f.Store)
	require.NoError(f.Store.ResetFilenameDiscoveryContext(t.Context()))
	reset, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(ready.Generation+1, reset.Generation)
	assert.Equal(1201, filenameEntries(t, f.Store, ready.Generation), "reset never clears a whole generation")
	newReady := filenameReady(t, f.Store)
	assert.Equal(1201, filenameEntries(t, f.Store, newReady.Generation))
	// Interleave a ready event with old-generation GC and require both to make
	// progress through bounded invocations, without wall-clock thresholds.
	var id int64
	require.NoError(f.Store.DB().QueryRow(`SELECT min(id) FROM attachments`).Scan(&id))
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "current.pdf", id)
	require.NoError(err)
	previous := 1201
	for range 8 {
		batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
		assert.LessOrEqual(batch.Processed, 500)
		remaining := filenameEntries(t, f.Store, ready.Generation)
		assert.LessOrEqual(previous-remaining, 500, "one invocation cannot clear more than 500 old occurrences")
		previous = remaining
	}
	assert.Zero(previous)
	assert.Equal("current.pdf", filenameEntryName(t, f.Store, newReady.Generation, id), "GC cannot starve replay")
}

// A surviving state cannot replay across an absent durable consumer boundary.
// Maintenance starts a new finite generation without clearing old rows first.
func TestFilenameDiscoveryMissingConsumerRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	id := createJournalAttachment(t, f, f.CreateMessage("filename-consumer-recovery"), "part:1", "a")
	ready := filenameReady(t, f.Store)
	_, err := f.Store.DB().Exec(`DELETE FROM attachment_change_consumers WHERE consumer_key='filename-discovery/v1'`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "recovered.pdf", id)
	require.NoError(err)
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Equal(ready.Generation+1, batch.Status.Generation)
	assert.NotEqual("ready", batch.Status.Phase, "a missing checkpoint requires finite bootstrap and catchup")
	assert.True(batch.More)
	assert.Equal(1, filenameEntries(t, f.Store, ready.Generation), "recovery leaves bounded old-generation cleanup to maintenance")
	recovered := filenameReady(t, f.Store)
	assert.Equal("recovered.pdf", filenameEntryName(t, f.Store, recovered.Generation, id))
	consumer, err := f.Store.GetAttachmentChangeConsumer(t.Context(), "filename-discovery/v1")
	require.NoError(err)
	assert.True(consumer.ReconciliationComplete)
	assert.Equal(recovered.LastSequence, consumer.LastSequence)
}

// A surviving consumer with empty state must also choose a new safe boundary,
// never acknowledge old retained events on the assumption that bootstrap ran.
func TestFilenameDiscoverySurvivingConsumerEmptyState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	id := createJournalAttachment(t, f, f.CreateMessage("filename-state-recovery"), "part:1", "a")
	ready := filenameReady(t, f.Store)
	_, err := f.Store.DB().Exec(`DELETE FROM filename_discovery_state`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename=? WHERE id=?`), "current.pdf", id)
	require.NoError(err)
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Greater(batch.Status.Generation, ready.Generation, "recovery must not reuse a populated generation")
	assert.NotEqual("ready", batch.Status.Phase)
	assert.True(batch.More)
	recovered := filenameReady(t, f.Store)
	assert.Equal("current.pdf", filenameEntryName(t, f.Store, recovered.Generation, id))
	assert.Equal(1, filenameEntries(t, f.Store, recovered.Generation))
}

// The projection is over all extant occurrences. Role, hidden-message state,
// source deletion and content availability are request-only concerns.
func TestFilenameDiscoveryAttachmentLifecycleAllRoles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	message := f.CreateMessage("filename-all-roles")
	roles := []store.AttachmentRole{store.AttachmentRoleStandalone, store.AttachmentRoleInline,
		store.AttachmentRoleAvatar, store.AttachmentRoleThumbnail, store.AttachmentRolePreview,
		store.AttachmentRoleSticker, store.AttachmentRoleUIAsset, store.AttachmentRoleUnknown}
	var ids []int64
	for i, role := range roles {
		var id int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments
   (message_id, filename, storage_path, attachment_role) VALUES (?, ?, '', ?) RETURNING id`),
			message, fmt.Sprintf("role-%d.pdf", i), string(role)).Scan(&id))
		ids = append(ids, id)
	}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP,
  deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), message)
	require.NoError(err)
	ready := filenameReady(t, f.Store)
	assert.Equal(len(ids), filenameEntries(t, f.Store, ready.Generation))
	for i, id := range ids {
		assert.Equal(fmt.Sprintf("role-%d.pdf", i), filenameEntryName(t, f.Store, ready.Generation, id))
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), message)
	require.NoError(err)
	batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
	require.NoError(err)
	assert.Zero(filenameEntries(t, f.Store, ready.Generation), "true cascade deletion removes all occurrences")
	assert.Zero(batch.Status.Skipped)
}
