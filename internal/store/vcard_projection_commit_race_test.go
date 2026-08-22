package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vcard"
)

// TestVCardSemanticCommitRejectsProjectionChangedByEmploymentWrite covers the
// half of the read set that no person-scoped row belongs to. An employment
// write touches neither the person record nor any profile component, so before
// the projection revision existed there was nothing for a commit to serialize
// against except the fingerprint itself.
func TestVCardSemanticCommitRejectsProjectionChangedByEmploymentWrite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newVCardProjectionFixture(t)
	ctx := t.Context()
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, snapshot, prepared := renderVCardEnvelope(t, fixture.store, fixture.person.ID,
		raw, "employment-conflict", "Rendered Before The Promotion")
	_, err := fixture.store.UpdateEmploymentContext(
		ctx, fixture.employment.ID, fixture.employment.Revision,
		store.EmploymentInput{
			PersonID: fixture.person.ID, OrganizationID: fixture.organization.ID,
			Title: new("Staff Engineer"), Source: store.ProvenanceUser,
		})
	require.NoError(err)

	_, err = fixture.store.CommitVCardResourceEnvelopeContext(
		ctx, "book", "employment-conflict", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.ErrorIs(err, store.ErrVCardProjectionConflict)

	loaded, err := fixture.store.GetVCardResourceEnvelopeContext(
		ctx, "book", "employment-conflict")
	require.NoError(err)
	assert.Equal(created.Revision, loaded.Revision)
	assert.Equal(raw, loaded.StoredBody)
}

// TestVCardSemanticCommitLeavesProjectionRevisionAlone is what keeps the
// mechanism from chasing its own tail. The commit takes the projection row's
// lock but must not move the row: a commit that bumped it would invalidate the
// snapshot every render was made from, so the next render would conflict
// against the commit before it, forever.
func TestVCardSemanticCommitLeavesProjectionRevisionAlone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, snapshot, prepared := renderVCardEnvelope(t, st, person.ID,
		raw, "idempotent-commit", "Alice Example")
	committed, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "book", "idempotent-commit", created.Revision,
		snapshot.Fingerprint, prepared,
	)
	require.NoError(err)
	assert.Equal(created.Revision+1, committed.Revision)

	afterCommit, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(snapshot.ProjectionRevision, afterCommit.ProjectionRevision)
	assert.Equal(snapshot.Fingerprint, afterCommit.Fingerprint)

	// Re-committing the same render against the same unchanged projection is a
	// no-op, not a conflict and not a new revision.
	repeated, err := st.CommitVCardResourceEnvelopeContext(
		ctx, "book", "idempotent-commit", committed.Revision,
		afterCommit.Fingerprint, prepared,
	)
	require.NoError(err)
	assert.Equal(committed.Revision, repeated.Revision)
	assert.Equal(committed.UpdatedAt, repeated.UpdatedAt)

	settled, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(snapshot.ProjectionRevision, settled.ProjectionRevision)
}

// TestPostgreSQLVCardResourceCommitRejectsSemanticWriteCommittedAfterSnapshot is the
// regression this whole mechanism exists for, and it only has teeth on
// PostgreSQL. The commit transaction runs at REPEATABLE READ, so its snapshot
// is fixed at its first statement; a semantic write that commits after that
// point is invisible to the fingerprint recheck no matter how carefully the
// recheck reads. The projection row lock is what turns that invisible write
// into a conflict.
//
// The interleaving is forced rather than raced: a trigger stalls the semantic
// writer inside its transaction, after it has taken the person's row lock and
// before it commits, so the commit transaction is guaranteed to open its
// snapshot while the write is still pending and to finish after it lands.
func TestPostgreSQLVCardResourceCommitRejectsSemanticWriteCommittedAfterSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL projection/commit snapshot interleaving regression")
	}
	ctx := t.Context()

	person := createEnvelopePerson(t, st, "alice@example.com")
	_, err := st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
		NameKind: store.PersonNameFormatted, Formatted: new("Alice"),
		OriginalValue: "Alice",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
	})
	requirements.NoError(err)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, snapshot, prepared := renderVCardEnvelope(t, st, person.ID,
		raw, "pg-projection-race", "Rendered From The Old Snapshot")

	// BEFORE UPDATE fires after the row lock is taken and before COMMIT, which
	// is exactly the window the commit transaction has to be caught in.
	gate := openPostgreSQLUpdateGate(ctx, t, st, 725901,
		"persons", person.ID, "wait_for_person_projection_write")

	writerDone := make(chan error, 1)
	gate.run(func() {
		_, writeErr := st.AddPersonCategoryContext(
			context.Background(), person.ID, store.PersonCategoryInput{
				OriginalValue: "Changed After The Snapshot",
				Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
			})
		writerDone <- writeErr
	})
	writerPID := waitForPostgreSQLBlockedPID(ctx, t, st, gate.holderPID,
		"vcard_projection_revision",
		"semantic write did not reach the projection bump")

	commitDone := make(chan error, 1)
	gate.run(func() {
		_, commitErr := st.CommitVCardResourceEnvelopeContext(
			context.Background(), "book", "pg-projection-race", created.Revision,
			snapshot.Fingerprint, prepared,
		)
		commitDone <- commitErr
	})
	// Best effort and deliberately non-fatal: with the projection lock in
	// place the commit parks behind the writer and this observes it, while a
	// build without the lock sails past and finishes inside the window. Either
	// way the assertions below, not this wait, decide the test.
	waitForPostgreSQLBlockedBy(ctx, t, st, writerPID, "FOR UPDATE")

	gate.release()
	requirements.NoError(<-writerDone, "semantic write")
	commitErr := <-commitDone

	requirements.ErrorIs(commitErr, store.ErrVCardProjectionConflict,
		"a semantic write committed inside the commit transaction's snapshot "+
			"window must not be projected over")
	var conflict *store.VCardProjectionConflictError
	requirements.ErrorAs(commitErr, &conflict)
	assertions.Equal(person.ID, conflict.PersonID)
	assertions.Equal(conflict.Expected, snapshot.Fingerprint)

	loaded, err := st.GetVCardResourceEnvelopeContext(
		ctx, "book", "pg-projection-race")
	requirements.NoError(err)
	assertions.Equal(created.Revision, loaded.Revision)
	assertions.Equal(raw, loaded.StoredBody)
	assertions.Equal(vcard.ETagForBody(raw), loaded.ETag)
}

// TestPostgreSQLVCardResourceCommitReportsEnvelopeReplacedAfterSnapshotAsWriteConflict
// covers the other row the commit transaction writes. The projection lock
// serializes commits against semantic writes, but a raw envelope Put takes no
// projection lock: it can replace the envelope row after the commit's
// REPEATABLE READ snapshot and before its revision-qualified UPDATE. PostgreSQL
// then reports that UPDATE as a serialization failure rather than as zero
// affected rows, and the commit has to name it the same stale-revision
// conflict either way.
//
// The interleaving is forced: a trigger stalls the raw writer inside its
// UPDATE, after it holds the envelope row lock and before it commits, so the
// commit is guaranteed to open its snapshot while that write is pending and to
// reach its own UPDATE after the write lands.
func TestPostgreSQLVCardResourceCommitReportsEnvelopeReplacedAfterSnapshotAsWriteConflict(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL envelope/commit snapshot interleaving regression")
	}
	ctx := t.Context()

	person := createEnvelopePerson(t, st, "alice@example.com")
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n")
	created, snapshot, rendered := renderVCardEnvelope(t, st, person.ID,
		raw, "pg-envelope-race", "Rendered From The Old Snapshot")
	rawReplacement := replaceStoreFormattedName(
		t, created.ResourceEnvelope, "Replaced By The Raw Writer",
	)

	gate := openPostgreSQLUpdateGate(ctx, t, st, 725902,
		"vcard_resource_envelopes", created.ID, "wait_for_envelope_write")

	writerDone := make(chan error, 1)
	gate.run(func() {
		expected := created.Revision
		_, writeErr := st.PutVCardResourceEnvelopeContext(
			context.Background(), store.VCardResourceEnvelopeInput{
				PersonID: person.ID, ExpectedRevision: &expected,
				Envelope: rawReplacement,
			})
		writerDone <- writeErr
	})
	writerPID := waitForPostgreSQLBlockedPID(ctx, t, st, gate.holderPID,
		"UPDATE vcard_resource_envelopes",
		"raw writer did not reach the envelope UPDATE")

	commitDone := make(chan error, 1)
	gate.run(func() {
		_, commitErr := st.CommitVCardResourceEnvelopeContext(
			context.Background(), "book", "pg-envelope-race", created.Revision,
			snapshot.Fingerprint, rendered,
		)
		commitDone <- commitErr
	})
	// The commit must park on the raw writer's row lock: that is what proves
	// its snapshot predates the replacement it is about to lose to.
	requirements.True(
		waitForPostgreSQLBlockedBy(ctx, t, st, writerPID, "UPDATE vcard_resource_envelopes"),
		"commit did not reach the envelope UPDATE behind the raw writer",
	)

	gate.release()
	requirements.NoError(<-writerDone, "raw envelope write")
	commitErr := <-commitDone

	requirements.ErrorIs(commitErr, store.ErrVCardResourceWriteConflict,
		"an envelope replaced inside the commit transaction's snapshot window "+
			"must surface as a stale-revision conflict, not a raw backend error")
	var conflict *store.VCardResourceWriteConflictError
	requirements.ErrorAs(commitErr, &conflict)
	assertions.Equal(created.Revision, conflict.ExpectedRevision)

	loaded, err := st.GetVCardResourceEnvelopeContext(
		ctx, "book", "pg-envelope-race")
	requirements.NoError(err)
	assertions.Equal(created.Revision+1, loaded.Revision)
	assertions.Equal(rawReplacement.StoredBody, loaded.StoredBody)
}

// waitForPostgreSQLBlockedBy waits for any backend to be blocked by blockerPID
// on a statement containing queryFragment. Unlike waitForPostgreSQLBlockedPID
// it reports rather than asserts, for interleavings where the wait is the
// expected behavior of a fixed build but its absence is the symptom under test
// rather than a broken fixture.
func waitForPostgreSQLBlockedBy(
	ctx context.Context,
	t *testing.T,
	st *store.Store,
	blockerPID int,
	queryFragment string,
) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var blockedPID int
		if err := st.DB().QueryRowContext(ctx, `SELECT COALESCE(MIN(pid), 0)
			FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))
			  AND POSITION($2 IN query) > 0`,
			blockerPID, queryFragment).Scan(&blockedPID); err == nil &&
			blockedPID > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// renderVCardEnvelope stores raw as person's first envelope under sourceUID,
// snapshots the projection, and prepares a render of that envelope with its
// FN replaced — the state every commit-path test starts from.
func renderVCardEnvelope(
	t *testing.T, st *store.Store, personID int64, raw []byte,
	sourceUID, formattedName string,
) (*store.VCardResourceEnvelopeRecord, *store.PersonVCardSnapshot, vcard.ResourceEnvelope) {
	t.Helper()
	created, err := st.PutVCardResourceEnvelopeContext(
		t.Context(), store.VCardResourceEnvelopeInput{
			PersonID: personID,
			Envelope: parseStoreEnvelope(t, raw, "book", sourceUID),
		})
	require.NoError(t, err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(t.Context(), personID)
	require.NoError(t, err)
	return created, snapshot, replaceStoreFormattedName(t, created.ResourceEnvelope, formattedName)
}

// postgreSQLUpdateGate stalls every UPDATE of one row inside its transaction,
// after the row lock is taken and before COMMIT, until release is called. It
// is built from a BEFORE UPDATE trigger that waits on a transaction advisory
// lock the gate's own connection holds; goroutines started through run are
// waited for during cleanup so a stalled one cannot outlive the test.
type postgreSQLUpdateGate struct {
	t         *testing.T
	holder    *sql.Conn
	key       int64
	holderPID int
	locked    bool
	finished  []<-chan struct{}
}

func openPostgreSQLUpdateGate(
	ctx context.Context, t *testing.T, st *store.Store,
	key int64, table string, rowID int64, name string,
) *postgreSQLUpdateGate {
	t.Helper()
	requirements := require.New(t)
	holder, err := st.DB().Conn(ctx)
	requirements.NoError(err, "reserve advisory-lock connection")
	gate := &postgreSQLUpdateGate{t: t, holder: holder, key: key, locked: true}
	t.Cleanup(func() {
		gate.release()
		for _, finished := range gate.finished {
			waitForPostgreSQLTestGoroutine(t, finished)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, key)
	requirements.NoError(err, "hold update gate")
	requirements.NoError(holder.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&gate.holderPID), "read gate holder PID")
	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%[2]d);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %[1]s
		BEFORE UPDATE ON %[3]s
		FOR EACH ROW
		WHEN (OLD.id = %[4]d)
		EXECUTE FUNCTION %[1]s()
	`, name, key, table, rowID))
	requirements.NoError(err, "install update gate trigger")
	return gate
}

// run starts fn on a goroutine the gate waits for at cleanup.
func (g *postgreSQLUpdateGate) run(fn func()) {
	finished := make(chan struct{})
	g.finished = append(g.finished, finished)
	go func() {
		defer close(finished)
		fn()
	}()
}

// release opens the gate; it is idempotent so cleanup can call it again.
func (g *postgreSQLUpdateGate) release() {
	if !g.locked {
		return
	}
	g.locked = false
	_, err := g.holder.ExecContext(context.Background(),
		`SELECT pg_advisory_unlock($1)`, g.key)
	assert.NoError(g.t, err, "release update gate")
}
