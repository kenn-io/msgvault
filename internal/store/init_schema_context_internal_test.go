package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelAtStatement cancels the initialisation the moment a chosen statement is
// about to be issued, by intercepting the store's placeholder-rebind step —
// which every statement passes through on its way to the driver, whichever
// helper issued it.
//
// It is the only seam that can name an INDIVIDUAL statement inside
// InitSchemaContext. The dialect wrappers below can only name the steps the
// dialect is asked about, and most of the method's statements are issued by
// store helpers the dialect never sees. Naming the statement, rather than
// counting how many have gone past, is what keeps these tests from silently
// re-aiming at a different call site when a step is added or reordered: an
// `after` that never matches, or a `stop` that never follows it, leaves the
// context live and the test fails on the missing cancellation rather than
// passing against the wrong statement.
type cancelAtStatement struct {
	// after arms the trigger once a statement containing it has been issued.
	// Empty arms immediately.
	after string
	// stop is the substring of the statement that must run with the context
	// already cancelled.
	stop string

	cancel func()
	armed  bool
	fired  bool
}

// install replaces the store's rebind step with one that cancels at the chosen
// statement. loggedDB captures the rebind func at Open, so swapping s.dialect
// afterwards would not reach it; this writes the field the transaction helpers
// actually call.
func (c *cancelAtStatement) install(db *loggedDB) {
	next := db.rebind
	db.rebind = func(query string) string {
		switch {
		case c.fired:
		case !c.armed:
			if c.after == "" || strings.Contains(query, c.after) {
				c.armed = true
			}
		case strings.Contains(query, c.stop):
			c.fired = true
			c.cancel()
		}
		return next(query)
	}
}

// TestInitSchemaContext_MigrationLedgerStopsWhenTheContextIsCancelled covers the
// ledger reads and writes that gate every one-time migration in the method.
//
// Each gated step asks the applied_migrations ledger whether it has already run
// and, when it finishes, records that it has. Both statements went through the
// contextless wrappers, which substitute context.Background(): on PostgreSQL
// they queue behind any conflicting lock on applied_migrations — the lock an
// interrupted upgrade's own re-run takes — and ignore SIGINT and SIGTERM for as
// long as it is held. The write is the worse of the two: reached with the
// context already cancelled it stamps "this migration is done" for a migration
// that was cut off, and the next open skips it forever.
func TestInitSchemaContext_MigrationLedgerStopsWhenTheContextIsCancelled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, st *Store)
		trigger cancelAtStatement
		wantMsg string
		because string
	}{
		{
			name: "the ledger read",
			trigger: cancelAtStatement{
				// The rfc822 index build is the step immediately before the
				// last_modified gate, and its DDL is unique in the method.
				after: "idx_messages_rfc822_message_id",
				stop:  "FROM applied_migrations",
			},
			wantMsg: "check migration",
			because: "a gate that answers on a cancelled context lets the whole " +
				"migration below it run to completion after the operator asked the " +
				"daemon to stop",
		},
		{
			name: "the ledger write",
			prepare: func(t *testing.T, st *Store) {
				t.Helper()
				// The content_changed_at backfill is the last gated step, so
				// clearing only its sentinel makes its mark the first ledger
				// write of the re-run.
				_, err := st.db.Exec(st.Rebind(
					`DELETE FROM applied_migrations WHERE name = ?`),
					migrationMessagesContentChangedAtBackfill)
				require.NoError(t, err, "clear the content_changed_at sentinel")
			},
			trigger: cancelAtStatement{stop: "INTO applied_migrations"},
			wantMsg: "mark migration",
			because: "a mark that lands on a cancelled context records a cut-off " +
				"migration as done, and the next open skips it forever",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			st, err := Open(filepath.Join(t.TempDir(), "ledger.db"))
			require.NoError(err, "open store")
			t.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema(),
				"the archive has to be fully migrated before the step under test is isolated")
			if tc.prepare != nil {
				tc.prepare(t, st)
			}

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			trigger := tc.trigger
			trigger.cancel = cancel
			trigger.install(st.db)

			err = st.InitSchemaContext(ctx)

			require.True(trigger.fired,
				"the statement this test aims at was never issued, so it proved nothing")
			require.Error(err, "a cancelled initialisation must report failure")
			require.ErrorIs(err, context.Canceled, "and report it as cancellation")
			assert.Containsf(err.Error(), tc.wantMsg,
				"the cancellation has to stop this statement itself: %s. An error raised "+
					"by a LATER step means it ran with the context already cancelled",
				tc.because)
		})
	}
}

func TestInitSchemaContext_RelationshipSeedHonoursContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "relationship-seed.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{
		stop:   "FROM relationship_types WHERE universal_id = ?",
		cancel: cancel,
	}
	trigger.install(st.db)

	err = st.InitSchemaContext(ctx)

	require.True(trigger.fired, "the relationship seed query was never issued")
	require.Error(err, "a cancelled relationship seed must stop schema initialisation")
	require.ErrorIs(err, context.Canceled, "and report it as cancellation")
	var count int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM relationship_types`).Scan(&count))
	assert.Zero(count, "a cancelled relationship seed must roll back its transaction")
}

// TestInitSchemaContext_LegacyPhoneMergeStopsWhenTheContextIsCancelled covers
// the link-graph rewrite inside the phone-unique migration's participant merge.
//
// The merge itself was bound to the context, but the link rewrite it calls to
// repoint participant_links was not: it reads every link edge and rewrites the
// affected cluster through the contextless wrappers. It runs inside the same
// maintenance transaction as the rest of the migration, which has the pool-wide
// statement_timeout deliberately disabled, so on PostgreSQL nothing but the
// context can cut short its wait for a conflicting lock.
func TestInitSchemaContext_LegacyPhoneMergeStopsWhenTheContextIsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "phone-merge.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "the archive has to exist before the migration is re-run")

	// A legacy archive: the phone index is not UNIQUE, so two participants can
	// share a phone number — which is what the migration merges. Clearing the
	// sentinel is what an archive that has never run it looks like.
	_, err = st.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`)
	require.NoError(err, "drop the unique phone index a legacy archive never had")
	for _, name := range []string{"winner", "loser"} {
		_, err = st.db.Exec(st.Rebind(
			`INSERT INTO participants (phone_number, display_name) VALUES (?, ?)`),
			"+15550000001", name)
		require.NoError(err, "seed duplicate phone participant %q", name)
	}
	_, err = st.db.Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		migrationPhoneUniqueIndex)
	require.NoError(err, "clear the phone-unique sentinel")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{stop: "FROM participant_links", cancel: cancel}
	trigger.install(st.db)

	err = st.InitSchemaContext(ctx)

	require.True(trigger.fired, "the merge never reached the link-graph rewrite")
	require.Error(err, "a cancelled migration must report failure")
	require.ErrorIs(err, context.Canceled, "and report it as cancellation")
	assert.Contains(err.Error(), "query participant links",
		"the cancellation has to stop the link read itself; an error raised by a LATER "+
			"statement means the rewrite ran with the context already cancelled")

	applied, err := st.IsMigrationApplied(migrationPhoneUniqueIndex)
	require.NoError(err, "read the migration ledger")
	assert.False(applied, "a migration that was cut off must not record itself as done")
}

// TestInitSchemaContext_LegacyCalendarAttributionStopsWhenTheContextIsCancelled
// covers the chunked UPDATE inside the attribution-provenance migration.
//
// The migration walks legacy calendar events in batches and stamps the ones
// whose organizer is not the account through a chunked IN-list UPDATE. Every
// statement around it carried the context; the chunked exec did not, because
// the chunk helper it used substitutes context.Background(). It is a whole-table
// walk on an archive of calendar events and it runs under the maintenance hatch,
// so it is exactly the shape of statement an operator needs to be able to stop.
func TestInitSchemaContext_LegacyCalendarAttributionStopsWhenTheContextIsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "calendar-attribution.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "the archive has to exist before the migration is re-run")

	// The literals are the migration's own: it selects on source_type 'gcal'
	// and raw_format 'gcal_json'.
	src, err := st.GetOrCreateSource("gcal", "owner@example.com/primary")
	require.NoError(err, "create calendar source")
	convID, err := st.EnsureConversationWithType(
		src.ID, "legacy-event-conversation", "calendar_event", "Legacy calendar event")
	require.NoError(err, "create calendar conversation")
	messageID, err := st.UpsertMessage(&Message{
		SourceID:        src.ID,
		ConversationID:  convID,
		SourceMessageID: "legacy-event",
		MessageType:     "calendar_event",
		IsFromMe:        true,
	})
	require.NoError(err, "persist legacy calendar message")
	require.NoError(st.UpsertMessageRawWithFormat(
		messageID, []byte(`{"organizer":{"email":"owner@example.com"}}`), "gcal_json"),
		"persist legacy calendar raw event")

	// A row written before attribution provenance existed, with the migration
	// sentinel cleared so the walk runs again.
	_, err = st.db.Exec(st.Rebind(`
		UPDATE messages
		SET source_is_from_me = NULL, identity_is_from_me = FALSE, is_from_me = TRUE
		WHERE id = ?`), messageID)
	require.NoError(err, "simulate a row written before attribution provenance")
	_, err = st.db.Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		migrationMessageAttributionProvenance)
	require.NoError(err, "clear the attribution sentinel")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	trigger := cancelAtStatement{
		after:  "AND s.source_type = 'gcal'",
		stop:   "WHERE id IN (",
		cancel: cancel,
	}
	trigger.install(st.db)

	err = st.InitSchemaContext(ctx)

	require.True(trigger.fired, "the migration never reached the chunked stamp")
	require.Error(err, "a cancelled migration must report failure")
	require.ErrorIs(err, context.Canceled, "and report it as cancellation")
	assert.Contains(err.Error(), "backfill calendar attribution provenance",
		"the cancellation has to stop the chunked UPDATE itself; an error raised by a "+
			"LATER statement means it ran with the context already cancelled")

	var provenance sql.NullBool
	require.NoError(st.db.QueryRow(st.Rebind(
		`SELECT source_is_from_me FROM messages WHERE id = ?`), messageID).Scan(&provenance),
		"read the row the cancelled stamp would have written")
	assert.False(provenance.Valid,
		"a cancelled chunk must not have written, or the migration is not resumable")
}

// cancelAtDialectStepDialect cancels the initialisation at the moment the step
// under test asks the dialect what to run, then delegates. It wraps whatever
// dialect the store was built with, so it changes WHEN the cancellation lands
// and nothing else. Each hook fires on the call that immediately precedes the
// statements it is named for.
type cancelAtDialectStepDialect struct {
	Dialect

	cancel        func()
	beforeSchema  bool
	beforeMigrate bool
	beforeFTS     bool
}

// cancelDuringFTSIndexDialect cancels the initialisation the moment the FTS
// index build begins, then issues a statement through the querier it was handed
// and records what that statement did.
//
// The recorded error, not InitSchemaContext's, is the assertion: runMaintenance
// re-checks the context after the callback returns, so the method reports
// cancellation either way. Only the statement's own error says whether the
// querier the call site handed the dialect was bound to the context.
type cancelDuringFTSIndexDialect struct {
	Dialect

	cancel func()
	probe  error
	probed bool
}

func (d *cancelDuringFTSIndexDialect) EnsureFTSIndex(q querier) error {
	d.cancel()
	// Stands in for the real index build: SQLite's EnsureFTSIndex is a no-op,
	// and PostgreSQL's GIN build is the statement whose lock wait an operator
	// has to be able to interrupt.
	d.probed = true
	_, d.probe = q.Exec(
		`CREATE INDEX IF NOT EXISTS idx_messages_fts_index_probe ON messages(id)`)
	if d.probe != nil {
		return d.probe
	}
	return d.Dialect.EnsureFTSIndex(q)
}

// cancelDuringFTSProbeDialect cancels the initialisation the moment the FTS
// availability probe begins, then delegates to the real probe.
type cancelDuringFTSProbeDialect struct {
	Dialect

	cancel func()
}

func (d cancelDuringFTSProbeDialect) FTSAvailable(
	ctx context.Context, db *sql.DB,
) (bool, error) {
	d.cancel()
	return d.Dialect.FTSAvailable(ctx, db)
}

// TestInitSchemaContext_FTSIndexBuildRunsOnTheBoundTransaction covers the last
// piece of DDL in the method that was still handed the raw transaction.
//
// EnsureFTSIndex runs under runMaintenance, which disables the pool-wide
// statement_timeout first, and on PostgreSQL it builds a GIN index and a partial
// index over `messages`. Handed the raw transaction — whose Exec substitutes
// context.Background() — a build queued behind a conflicting lock on that table
// has nothing left to cut it off, and ignores SIGINT and SIGTERM for as long as
// the lock is held. The trigger DDL beside it was bound two rounds earlier; this
// one was not.
//
// The wiring under test is dialect-independent — it is which querier the call
// site hands the dialect — so this runs on SQLite.
func TestInitSchemaContext_FTSIndexBuildRunsOnTheBoundTransaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "fts-index.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wrapped := &cancelDuringFTSIndexDialect{Dialect: st.dialect, cancel: cancel}
	st.dialect = wrapped

	err = st.InitSchemaContext(ctx)

	require.True(wrapped.probed, "the FTS index build was never reached")
	require.Error(wrapped.probe,
		"the index build must observe the cancellation itself; a nil error means the "+
			"call site handed the dialect a querier that substitutes context.Background(), "+
			"so on PostgreSQL a build waiting on a table lock is unreachable by a signal")
	require.ErrorIs(wrapped.probe, context.Canceled, "and observe it as cancellation")
	require.Error(err, "a cancelled initialisation must report failure")
	assert.Contains(err.Error(), "ensure FTS index", "reported as the step that stopped")
}

// TestInitSchemaContext_FTSAvailabilityProbeStopsWhenTheContextIsCancelled
// covers the one step whose answer is a bool.
//
// The probe is a query like every other, and it ran on context.Background(). A
// bool has nowhere to put a cancellation, so a cancelled probe was recorded as
// "full-text search is unavailable" on a store the daemon was about to hand to
// callers — a silent, wrong, durable answer produced by an operator's Ctrl-C.
func TestInitSchemaContext_FTSAvailabilityProbeStopsWhenTheContextIsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "fts-probe.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st.dialect = cancelDuringFTSProbeDialect{Dialect: st.dialect, cancel: cancel}

	err = st.InitSchemaContext(ctx)

	require.Error(err,
		"a cancelled probe must fail the initialisation rather than report an answer "+
			"it does not have")
	require.ErrorIs(err, context.Canceled, "and report it as cancellation")
	assert.Contains(err.Error(), "probe FTS availability",
		"reported as the step that stopped")
	assert.False(st.FTS5Available(),
		"and it must not have published an availability answer from a cancelled probe")
}

func (d cancelAtDialectStepDialect) SchemaFiles() []string {
	if d.beforeSchema {
		d.cancel()
	}
	return d.Dialect.SchemaFiles()
}

func (d cancelAtDialectStepDialect) LegacyColumnMigrations() []ColumnMigration {
	if d.beforeMigrate {
		d.cancel()
	}
	return d.Dialect.LegacyColumnMigrations()
}

func (d cancelAtDialectStepDialect) SchemaFTS() string {
	if d.beforeFTS {
		d.cancel()
	}
	return d.Dialect.SchemaFTS()
}

// TestInitSchemaContext_DDLStopsWhenTheContextIsCancelled covers the rest of the
// migration the earlier cancellation tests do not reach.
//
// InitSchemaContext advertises the whole upgrade as interruptible, and an
// operator's SIGINT or SIGTERM has to be able to reach it: on PostgreSQL every
// one of these statements queues behind any conflicting lock on the table it
// touches — an import's, say — and a statement issued on context.Background()
// ignores the signal for as long as that lock is held, leaving SIGKILL on a
// process in the middle of writing as the only remaining move. The schema
// scripts, the legacy ADD COLUMN loop (which is where content_changed_at itself
// arrives on an upgraded archive) and the FTS schema all ran that way.
//
// The existing tests cancel at a backfill batch boundary and during trigger
// replacement, which are later steps and different queriers.
//
// The wiring under test is dialect-independent — it is which exec each call
// site uses — so this runs on SQLite, where a store can be opened without the
// package's external test helpers.
func TestInitSchemaContext_DDLStopsWhenTheContextIsCancelled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		step    func(*cancelAtDialectStepDialect)
		wantMsg string
		because string
	}{
		{
			name:    "the schema scripts",
			step:    func(d *cancelAtDialectStepDialect) { d.beforeSchema = true },
			wantMsg: "execute",
			because: "the schema scripts are the first statements of the upgrade, and on " +
				"PostgreSQL their CREATE TABLE/INDEX statements wait on whatever holds " +
				"the table",
		},
		{
			name:    "the legacy ADD COLUMN migrations",
			step:    func(d *cancelAtDialectStepDialect) { d.beforeMigrate = true },
			wantMsg: "migrate schema",
			because: "this loop is where content_changed_at is added to an existing " +
				"archive, and an ALTER TABLE takes an exclusive lock",
		},
		{
			name:    "the FTS schema",
			step:    func(d *cancelAtDialectStepDialect) { d.beforeFTS = true },
			wantMsg: "init FTS schema",
			because: "a cancelled upgrade must not run the FTS schema to completion and " +
				"then report success, which is what a contextless exec here did",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			st, err := Open(filepath.Join(t.TempDir(), "schema.db"))
			require.NoError(err, "open store")
			t.Cleanup(func() { _ = st.Close() })

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			wrapped := cancelAtDialectStepDialect{Dialect: st.dialect, cancel: cancel}
			tc.step(&wrapped)
			st.dialect = wrapped

			err = st.InitSchemaContext(ctx)

			require.Error(err,
				"a cancelled initialisation must report failure, not a silent partial upgrade")
			require.ErrorIs(err, context.Canceled,
				"and report it as cancellation, so the daemon exits on the signal rather "+
					"than treating an operator's Ctrl-C as a corrupt archive")
			assert.Containsf(err.Error(), tc.wantMsg,
				"the cancellation has to stop this step itself: %s. An error raised by a "+
					"LATER step means these statements ran to completion with the context "+
					"already cancelled", tc.because)
		})
	}
}

// TestEnsureParticipantsPhoneUniqueIndex_HonoursItsContext covers the other
// long step InitSchemaContext calls out to.
//
// The phone-index migration dedupes participants and rebuilds a UNIQUE index
// over the whole table inside one maintenance transaction — which has the
// pool-wide statement_timeout deliberately disabled, so on PostgreSQL nothing
// but the context can cut short its wait for a conflicting lock. It ran on
// context.Background(), so InitSchemaContext's promise to stop on a signal did
// not cover it.
func TestEnsureParticipantsPhoneUniqueIndex_HonoursItsContext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "phone.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "the archive has to exist before the migration is re-run")

	// Re-run it from scratch: clearing the ledger entry is what an archive that
	// has never had this migration looks like.
	_, err = st.db.Exec(st.Rebind(`DELETE FROM applied_migrations WHERE name = ?`),
		migrationPhoneUniqueIndex)
	require.NoError(err, "clear the migration ledger entry")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = st.ensureParticipantsPhoneUniqueIndex(ctx)
	require.Error(err, "a cancelled migration must report failure rather than run to completion")
	require.ErrorIs(err, context.Canceled, "and report it as cancellation")

	applied, err := st.IsMigrationApplied(migrationPhoneUniqueIndex)
	require.NoError(err, "read the migration ledger")
	assert.False(applied,
		"a migration that was cut off must not record itself as done, or the next "+
			"open skips it and the unique index is never built")
}
