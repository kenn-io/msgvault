package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/personenrichment"
)

// ParseDBTime is exported for testing unexported timestamp parsing behavior.
var ParseDBTime = parseDBTime

// MessagesTableColumns returns the live column names of the messages table on
// whichever backend the store uses. Test-only: it exists so
// TestMessagesColumnClassificationIsExhaustive can compare the real table
// against MessagesContentColumns + MessagesNonContentColumns. No production
// caller reads the schema this way, so it stays out of the package's API.
func MessagesTableColumns(s *Store) ([]string, error) {
	q := `SELECT name FROM pragma_table_info('messages')`
	if s.IsPostgreSQL() {
		q = `SELECT column_name FROM information_schema.columns
		     WHERE table_name = 'messages' AND table_schema = current_schema()
		     ORDER BY ordinal_position`
	}
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("read messages columns: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan messages column: %w", err)
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// SetFTS5AvailableForTest flips the cached availability flag. Tests use this
// to exercise the guarantee that RebuildFTS works even when FTS5 looks
// unavailable — the symptom that motivates a rebuild in the first place.
func SetFTS5AvailableForTest(s *Store, v bool) {
	s.fts5Available = v
}

// SetSenderRepairMessageLockHookForTest pauses sender repair immediately after
// it has acquired the message-level recipient-write lock.
func (s *Store) SetSenderRepairMessageLockHookForTest(fn func()) {
	s.senderRepairMessageLockHook = fn
}

// SetCardDAVConflictResolutionSnapshotHookForTest pauses keep-remote after its
// unlocked routing snapshot and before the ordered resolution transaction.
func (s *Store) SetCardDAVConflictResolutionSnapshotHookForTest(fn func()) {
	s.cardDAVConflictResolveSnapshotHook = fn
}

// SetCardDAVConflictTombstonePreparationSnapshotHookForTest pauses keep-local
// tombstone preparation after its unlocked routing snapshot and before the
// ordered mutation transaction.
func (s *Store) SetCardDAVConflictTombstonePreparationSnapshotHookForTest(fn func()) {
	s.cardDAVTombstonePrepareSnapshotHook = fn
}

// SetBackfillFTSBatchErrHookForTest installs (or, with nil, clears) the
// test-only hook that forces backfillFTSBatch to fail for a chosen id range.
// Tests use it to deterministically trigger backfillFTSRowByRow's
// skip-the-bad-row-and-continue fallback. Returns a restore func that clears
// the hook, so callers can defer it.
//
// Scoped to this Store: other Stores migrating concurrently in the same test
// binary — test fixtures build their schemas in the background — must not see
// this test's injected failure.
func (s *Store) SetBackfillFTSBatchErrHookForTest(fn func(fromID, toID int64) error) func() {
	s.backfillFTSBatchErrHook = fn
	return func() { s.backfillFTSBatchErrHook = nil }
}

// SetContentChangedBackfillBatchHookForTest installs (or, with nil, clears) the
// test-only hook consulted before each content_changed_at backfill batch, with
// that batch's first and last id (inclusive). A non-nil return from it aborts
// the backfill at that batch boundary, which is how a test interrupts an upgrade
// between two committed batches; counting the calls is how a test measures the
// transactions an archive's shape costs. Returns a restore func that clears the
// hook, so callers can defer it.
//
// Scoped to this Store; see SetBackfillFTSBatchErrHookForTest.
func (s *Store) SetContentChangedBackfillBatchHookForTest(fn func(fromID, toID int64) error) func() {
	s.contentChangedBackfillBatchHook = fn
	return func() { s.contentChangedBackfillBatchHook = nil }
}

// SetContentChangedBackfillBatchSizeForTest shrinks how many rows one backfill
// batch stamps, so a test can span several batches over a handful of rows.
// Returns a restore func that puts the production value back, so callers can
// defer it.
//
// Scoped to this Store; see SetBackfillFTSBatchErrHookForTest.
func (s *Store) SetContentChangedBackfillBatchSizeForTest(n int64) func() {
	prev := s.contentChangedBackfillBatchSizeOverride
	s.contentChangedBackfillBatchSizeOverride = n
	return func() { s.contentChangedBackfillBatchSizeOverride = prev }
}

// SetInitSchemaWindowHookForTest installs (or, with nil, clears) the test-only
// hook that fires inside InitSchema after the content_changed_at backfill has
// recorded itself and while the remaining index builds are still pending. Tests
// use it to perform a real INSERT exactly where a concurrent writer used to lose
// its watermark. Returns a restore func, so callers can defer it.
//
// Scoped to this Store; see SetBackfillFTSBatchErrHookForTest. This one is the
// reason the seams are per-Store at all: it calls back into the installing
// test's own Store, so a concurrent fixture build firing it would write through
// a Store the test had already closed.
func (s *Store) SetInitSchemaWindowHookForTest(fn func()) func() {
	s.initSchemaWindowHook = fn
	return func() { s.initSchemaWindowHook = nil }
}

// SetAttributeSeedReadHookForTest installs a hook after both identities for a
// seeded definition have been read but before reconciliation starts. Tests use
// it to make two real callers enter the same missing-seed race. Returns a
// restore func that clears the hook.
func (s *Store) SetAttributeSeedReadHookForTest(fn func(slug string)) func() {
	s.attributeSeedReadHook = fn
	return func() { s.attributeSeedReadHook = nil }
}

// SetAttachmentRoleRepairPreparedHookForTest installs a hook after historical
// MIME evidence has been prepared but before the repair transaction begins.
// Tests use it to reproduce a concurrent resync that changes attachment bytes.
func (s *Store) SetAttachmentRoleRepairPreparedHookForTest(fn func()) func() {
	s.attachmentRoleRepairPreparedHook = fn
	return func() { s.attachmentRoleRepairPreparedHook = nil }
}

// SetIdentityMatchAcceptBeforeDecisionHookForTest pauses a user acceptance
// after its initial read and before its locked decision transaction.
func (s *Store) SetIdentityMatchAcceptBeforeDecisionHookForTest(fn func()) func() {
	s.identityMatchAcceptBeforeDecisionHook = fn
	return func() { s.identityMatchAcceptBeforeDecisionHook = nil }
}

// SetPersonOperationBeforeIdentityLockHookForTest installs a per-Store barrier
// immediately before merge and split transactions acquire the identity lock.
// Concurrency tests use it to prove every competing transaction is open and at
// the lock boundary before either is released.
func (s *Store) SetPersonOperationBeforeIdentityLockHookForTest(fn func()) func() {
	s.personOperationBeforeIdentityLockHook = fn
	return func() { s.personOperationBeforeIdentityLockHook = nil }
}

// SetPersonMergeAfterSnapshotHookForTest installs a barrier after a merge has
// captured its reversal snapshot but before it mutates referenced rows.
func (s *Store) SetPersonMergeAfterSnapshotHookForTest(fn func()) func() {
	s.personMergeAfterSnapshotHook = fn
	return func() { s.personMergeAfterSnapshotHook = nil }
}

// SetPersonEnrichmentClockForTest installs a deterministic clock on one Store.
func SetPersonEnrichmentClockForTest(s *Store, clock func() time.Time) {
	s.personEnrichmentClock = clock
}

// SetPersonEnrichmentBudgetBarrierForTest coordinates real concurrent budget
// reservations after counter creation and before their fixed-order locks.
func SetPersonEnrichmentBudgetBarrierForTest(s *Store, barrier func()) {
	s.personEnrichmentBudgetBarrier = barrier
}

// SetPersonEnrichmentRunBarrierForTest coordinates run-lock interleavings.
func SetPersonEnrichmentRunBarrierForTest(s *Store, barrier func(phase string)) {
	s.personEnrichmentRunBarrier = barrier
}

// SetPersonEnrichmentTxBarrierForTest coordinates person/work/attempt lock order.
func SetPersonEnrichmentTxBarrierForTest(s *Store, barrier func(phase string)) {
	s.personEnrichmentTxBarrier = barrier
}

// setPersonEnrichmentProviderIdentityBarrierForTest pauses one Store after its
// durable provider-identity key lock and before the ownership read.
func setPersonEnrichmentProviderIdentityBarrierForTest(
	s *Store, barrier func(phase string, tx *loggedTx),
) {
	s.personEnrichmentOwnershipBarrier = barrier
}

// ReservePersonEnrichmentBudgetForTest exercises the exact transaction-local
// reservation helper used by BeginAttempt without bypassing its counter logic.
func ReservePersonEnrichmentBudgetForTest(
	ctx context.Context, s *Store, start personenrichment.AttemptStart,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		policy, err := loadPersonEnrichmentBudgetPolicyTx(ctx, tx, start.ProfileFingerprint)
		if err != nil {
			return err
		}
		return s.reservePersonEnrichmentBudgetTx(ctx, tx, policy, start)
	})
}

func EnsurePersonEnrichmentBudgetCountersForTest(
	ctx context.Context, s *Store, start personenrichment.AttemptStart,
) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		return ensurePersonEnrichmentBudgetCountersTx(
			ctx, tx, start, s.personEnrichmentTime().Format("2006-01-02"),
		)
	})
}

type PersonEnrichmentAttemptCompletion = personEnrichmentAttemptCompletion

func CompletePersonEnrichmentAttemptForTest(
	ctx context.Context,
	s *Store,
	token personenrichment.LeaseToken,
	completion PersonEnrichmentAttemptCompletion,
) error {
	return s.completePersonEnrichmentAttemptContext(ctx, token, completion)
}

func RollbackPersonEnrichmentAttemptCompletionForTest(
	ctx context.Context,
	s *Store,
	token personenrichment.LeaseToken,
	completion PersonEnrichmentAttemptCompletion,
) error {
	errRollback := errors.New("rollback successful person enrichment completion")
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if _, completeErr := s.completePersonEnrichmentAttemptTx(ctx, tx, token, completion); completeErr != nil {
			return completeErr
		}
		return errRollback
	})
	if errors.Is(err, errRollback) {
		return nil
	}
	return err
}
