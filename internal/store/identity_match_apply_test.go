package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// upsertPairCandidate creates a participant-to-participant candidate with the
// given basis, so each test states only what it is about.
func upsertPairCandidate(
	t *testing.T, st *store.Store, left, right int64, basis store.IdentityMatchBasis,
) *store.IdentityMatchCandidate {
	t.Helper()
	require := require.New(t)

	candidate, _, err := st.UpsertIdentityMatchCandidateContext(
		context.Background(), store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis:           basis,
			NormalizedValue: new("beeper:@alice:beeper.local"),
			State:           store.IdentityMatchStateCandidate,
			Source:          store.ProvenanceArchiveObservation,
		})
	require.NoError(err, "UpsertIdentityMatchCandidateContext")
	return candidate
}

// linkedPair reports whether a and b are in one participant cluster.
func linkedPair(t *testing.T, st *store.Store, a, b int64) bool {
	t.Helper()
	require := require.New(t)

	members, err := st.ClusterMembers(a)
	require.NoError(err, "ClusterMembers")
	return slices.Contains(members, b)
}

func TestAcceptStableProviderIDCandidateLinksParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure second alice")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchStableProviderID)

	accepted, revision, err := st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "AcceptIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	assert.Equal("system", *accepted.DecidedBy)
	assert.NotZero(revision, "the identity revision advances so caches rebuild")
	assert.True(linkedPair(t, st, alice, bob), "an accepted match must be applied, not just recorded")

	// Re-accepting is idempotent: the link already exists.
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "second accept must be idempotent")
}

func TestUserAcceptPromotesExistingSystemAcceptance(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@promote-left:beeper.local", "Test User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@promote-right:beeper.local", "Test User",
	)
	require.NoError(err)
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "system acceptance")

	notes := "confirmed during review"
	promoted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "user", &notes,
	)
	require.NoError(err, "user acceptance")
	require.NotNil(promoted.DecidedBy)
	assert.Equal("user", *promoted.DecidedBy)
	require.NotNil(promoted.Notes)
	assert.Equal(notes, *promoted.Notes)
	assert.Equal(store.IdentityMatchStateAccepted, promoted.State)
	assert.True(linkedPair(t, st, left, right))
}

func TestUserAcceptRecordsLegacyAcceptedDecision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@legacy-accept-left:beeper.local", "Test User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@legacy-accept-right:beeper.local", "Test User",
	)
	require.NoError(err)
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "initial system acceptance")
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE identity_match_candidates
		SET decided_by = NULL, decided_at = NULL WHERE id = ?`), candidate.ID)
	require.NoError(err, "simulate legacy accepted decision")

	notes := "confirmed during legacy review"
	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "user", &notes,
	)
	require.NoError(err, "user acceptance")
	require.NotNil(accepted.DecidedBy)
	assert.Equal("user", *accepted.DecidedBy)
	require.NotNil(accepted.Notes)
	assert.Equal(notes, *accepted.Notes)
}

func TestAcceptStableProviderIDCandidateCancellationDoesNotCommitLink(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to cancel during participant linking")
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	st.DB().SetMaxOpenConns(1)

	alice, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure second alice")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchStableProviderID)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	linkStarted := make(chan struct{})
	conn, err := st.DB().Conn(context.Background())
	require.NoError(err, "get SQLite connection")
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok, "driver connection is SQLite")
		return sqliteConn.RegisterFunc("wait_for_identity_match_cancel", func() int {
			close(linkStarted)
			<-ctx.Done()
			return 0
		}, true)
	})
	require.NoError(err, "register cancellation function")
	require.NoError(conn.Close(), "return SQLite connection to pool")
	_, err = st.DB().Exec(`
		CREATE TRIGGER wait_before_identity_match_link
		BEFORE INSERT ON participant_links
		BEGIN
			SELECT wait_for_identity_match_cancel();
		END
	`)
	require.NoError(err, "create cancellation trigger")

	done := make(chan error, 1)
	go func() {
		_, _, err := st.AcceptIdentityMatchCandidateContext(
			ctx, candidate.ID, "system", nil)
		done <- err
	}()

	select {
	case <-linkStarted:
	case <-time.After(time.Second):
		require.FailNow("accepted identity match did not start linking")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(time.Second):
		require.FailNow("accepted identity match did not stop after cancellation")
	}
	require.ErrorIs(err, context.Canceled)
	assert.False(linkedPair(t, st, alice, bob),
		"cancellation during the link statement must roll back the participant link")
}

func TestSQLiteSystemAcceptanceCannotOverwriteConcurrentRejection(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses SQLite writer-lock scheduling")
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	st.DB().SetMaxOpenConns(4)
	st.DB().SetMaxIdleConns(4)
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@race-left:beeper.local", "Test User")
	requirements.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@race-right:beeper.local", "Test User")
	requirements.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)

	// Pause the rejection after it has acquired the identity mutation lock.
	// The system accept starts only after that pause, so its unlocked candidate
	// read sees candidate and then queues on the rejection's lock. Releasing
	// the trigger makes the rejection commit first; the system CAS must then
	// lose instead of overwriting it with accepted.
	rejectionEntered := make(chan struct{})
	releaseRejection := make(chan struct{})
	var rejectionGate sync.Once
	registerConnections := make([]*sql.Conn, 0, 4)
	for range 4 {
		conn, connErr := st.DB().Conn(ctx)
		requirements.NoError(connErr, "reserve SQLite connection for rejection gate")
		registerConnections = append(registerConnections, conn)
		err = conn.Raw(func(driverConn any) error {
			sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
			requirements.True(ok, "driver connection is SQLite")
			return sqliteConn.RegisterFunc("wait_for_identity_match_rejection", func() int {
				rejectionGate.Do(func() { close(rejectionEntered) })
				<-releaseRejection
				return 0
			}, true)
		})
		requirements.NoError(err, "register rejection gate")
	}
	for _, conn := range registerConnections {
		requirements.NoError(conn.Close(), "return SQLite rejection gate connection")
	}
	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		CREATE TRIGGER wait_before_identity_match_rejection
		BEFORE UPDATE OF state ON identity_match_candidates
		WHEN OLD.id = %d AND OLD.state = 'candidate' AND NEW.state = 'rejected'
		BEGIN
			SELECT wait_for_identity_match_rejection();
		END
	`, candidate.ID))
	requirements.NoError(err, "create rejection trigger")

	systemStarted := false
	rejectionStarted := false
	rejectionReleased := false
	systemObserved := false
	rejectionObserved := false
	systemDone := make(chan error, 1)
	rejectionDone := make(chan error, 1)
	t.Cleanup(func() {
		if !rejectionReleased {
			close(releaseRejection)
		}
		if rejectionStarted && !rejectionObserved {
			select {
			case <-rejectionDone:
			case <-time.After(5 * time.Second):
				assertions.Fail("rejection goroutine did not finish during cleanup")
			}
		}
		if systemStarted && !systemObserved {
			select {
			case <-systemDone:
			case <-time.After(5 * time.Second):
				assertions.Fail("system acceptance goroutine did not finish during cleanup")
			}
		}
	})
	rejectionStarted = true
	go func() {
		_, rejectErr := st.DecideIdentityMatchCandidateContext(
			ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
		rejectionDone <- rejectErr
	}()
	requirements.Eventually(func() bool {
		select {
		case <-rejectionEntered:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond,
		"user rejection did not acquire the identity lock before its state update")

	systemStarted = true
	go func() {
		_, _, acceptErr := st.AcceptIdentityMatchCandidateContext(
			ctx, candidate.ID, "system", nil)
		systemDone <- acceptErr
	}()
	requirements.Eventually(func() bool {
		return st.DB().Stats().InUse >= 2
	}, 2*time.Second, 10*time.Millisecond,
		"system acceptance did not queue behind the rejection identity lock")

	close(releaseRejection)
	rejectionReleased = true

	var systemErr error
	select {
	case systemErr = <-systemDone:
		systemObserved = true
	case <-time.After(5 * time.Second):
		requirements.FailNow("system acceptance did not finish")
	}
	assertions.True(systemErr == nil ||
		errors.Is(systemErr, store.ErrIdentityMatchRejected) ||
		errors.Is(systemErr, store.ErrIdentityMatchNotAccepted),
		"system acceptance must treat a concurrent rejection as a stale no-op: %v", systemErr)
	rejectionErr := <-rejectionDone
	rejectionObserved = true
	requirements.NoError(rejectionErr, "user rejection")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err, "reload candidate")
	assertions.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assertions.False(linkedPair(t, st, left, right),
		"a concurrent system acceptance must not leave a rejected identity edge")
}

func TestAcceptUsernameCandidateRequiresAUser(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchServiceScopeUsername)

	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.ErrorIs(err, store.ErrIdentityMatchNotAcceptable,
		"a username-only match must never be auto-accepted")
	assert.False(linkedPair(t, st, alice, bob), "the rejected accept must not have linked anyone")

	accepted, _, err := st.AcceptIdentityMatchCandidateContext(
		ctx, candidate.ID, "user", new("confirmed in review"))
	require.NoError(err, "explicit user confirmation is allowed")
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	assert.True(linkedPair(t, st, alice, bob))
}

func TestAcceptAcrossDifferentPersonsBecomesAConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, alice)
	require.NoError(err, "promote alice")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, bob)
	require.NoError(err, "promote bob")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchStableProviderID)

	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.ErrorIs(err, store.ErrPersonBindingConflict,
		"two curated people must never be merged by an automatic match")
	assert.False(linkedPair(t, st, alice, bob))

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateConflict, reloaded.State,
		"the failed accept is recorded as a conflict for review")
}

func TestFailedUserAcceptPreservesObservationConflictCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "rollback-left", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "rollback-right", "Right")
	require.NoError(err)
	normalized := "rollback-shared@example.org"
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
		},
	)
	require.NoError(err)
	require.True(created)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: normalized,
		ProviderUserID: new("provider-left"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	leftObservation, err := st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	conflicting, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(conflicting.Conflicting)
	assert.Equal(candidate.ID, *conflicting.CandidateID)
	_, _, err = st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	_, _, err = st.CreatePersonFromParticipantContext(ctx, right)
	require.NoError(err)

	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "user", nil)
	require.ErrorIs(err, store.ErrPersonBindingConflict)
	restored, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateConflict, restored.State)

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, left, leftObservation.Observation.Envelope.ID, nil,
	))
	demoted, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateCandidate, demoted.State)
}

func TestAcceptRejectsUnsupportedEndpointKinds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	person, _, err := st.CreatePersonFromParticipantContext(ctx, alice)
	require.NoError(err, "promote alice")

	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: alice,
		RightKind: store.IdentityMatchPerson, RightID: person.ID,
		Basis: store.IdentityMatchStableProviderID, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err, "UpsertIdentityMatchCandidateContext")

	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "user", nil)
	require.ErrorIs(err, store.ErrIdentityMatchEndpointUnsupported)

	unchanged, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateCandidate, unchanged.State,
		"an unsupported endpoint must leave the candidate untouched")
}

func TestApplyAcceptedIdentityMatchesIsResumableAndIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure second alice")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchStableProviderID)

	// Simulate a crash between the accept transaction and the link
	// transaction: the candidate is accepted but no link exists.
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	require.NoError(err, "DecideIdentityMatchCandidateContext")
	require.False(linkedPair(t, st, alice, bob), "precondition: not linked yet")

	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 0)
	require.NoError(err, "ApplyAcceptedIdentityMatchesContext")
	assert.Equal(1, applied)
	assert.True(linkedPair(t, st, alice, bob))

	again, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 0)
	require.NoError(err, "second ApplyAcceptedIdentityMatchesContext")
	assert.Equal(0, again, "an already-linked accepted match is not re-applied")
}

func TestAcceptedIdentityMatchApplicationStateTracksInterruptedWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()

	applicationPending := func(candidateID int64) bool {
		var pending bool
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT application_pending FROM identity_match_candidates WHERE id = ?`),
			candidateID,
		).Scan(&pending), "read candidate application state")
		return pending
	}

	firstLeft, err := st.EnsureParticipantByIdentifier(
		"beeper", "@application-state-first-left:beeper.local", "Test User")
	require.NoError(err)
	firstRight, err := st.EnsureParticipantByIdentifier(
		"beeper", "@application-state-first-right:beeper.local", "Test User")
	require.NoError(err)
	first := upsertPairCandidate(t, st, firstLeft, firstRight, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, first.ID, "system", nil)
	require.NoError(err)
	assert.False(applicationPending(first.ID),
		"a successfully linked candidate must leave no recovery work")

	secondLeft, err := st.EnsureParticipantByIdentifier(
		"beeper", "@application-state-second-left:beeper.local", "Test User")
	require.NoError(err)
	secondRight, err := st.EnsureParticipantByIdentifier(
		"beeper", "@application-state-second-right:beeper.local", "Test User")
	require.NoError(err)
	second := upsertPairCandidate(t, st, secondLeft, secondRight, store.IdentityMatchStableProviderID)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, second.ID, store.IdentityMatchStateAccepted, "system", nil)
	require.NoError(err)
	assert.True(applicationPending(second.ID),
		"an acceptance whose link transaction has not run must remain pending")

	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 1)
	require.NoError(err)
	assert.Equal(1, applied)
	assert.False(applicationPending(second.ID),
		"bounded recovery must clear the work item it applied")
}

func TestUnlinkUserAcceptedIdentityMatchesSuppressReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@unlink-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@unlink-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	first := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, first.ID, "user", nil)
	require.NoError(err, "accept first candidate")

	second, created, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, NormalizedValue: new("shared@example.com"),
			State:  store.IdentityMatchStateCandidate,
			Source: store.ProvenanceArchiveObservation,
		})
	require.NoError(err, "create second candidate")
	require.True(created, "second candidate must be distinct")
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, second.ID, "user", nil)
	require.NoError(err, "accept second candidate")
	require.True(linkedPair(t, st, left, right), "accepted candidates must link the pair")

	_, err = st.UnlinkParticipants(left, right)
	require.NoError(err, "unlink accepted identity edge")

	for _, candidateID := range []int64{first.ID, second.ID} {
		rejected, err := st.GetIdentityMatchCandidateContext(ctx, candidateID)
		require.NoError(err, "reload candidate %d", candidateID)
		assert.Equal(store.IdentityMatchStateRejected, rejected.State,
			"unlink must reject candidate %d", candidateID)
		require.NotNil(rejected.DecidedBy, "candidate %d decided_by", candidateID)
		assert.Equal("user", *rejected.DecidedBy,
			"unlink must record user suppression for candidate %d", candidateID)
	}

	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 0)
	require.NoError(err, "replay accepted matches")
	assert.Equal(0, applied, "unlink suppression must prevent accepted-match replay")
	assert.False(linkedPair(t, st, left, right),
		"replay must not recreate an explicitly unlinked identity edge")
}

func TestUnlinkSystemAcceptedIdentityMatchSuppressesReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@system-unlink-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@system-unlink-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "accept system candidate")
	lo, hi := normalizeEdgeForTest(left, right)
	var owner int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT identity_match_candidate_id FROM participant_links
		WHERE participant_a = ? AND participant_b = ?`), lo, hi).Scan(&owner),
		"read system link ownership")
	assert.Equal(candidate.ID, owner, "system acceptance must own its direct edge")

	_, err = st.UnlinkParticipants(left, right)
	require.NoError(err, "unlink system-owned identity edge")
	rejected, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload system candidate")
	assert.Equal(store.IdentityMatchStateRejected, rejected.State)
	require.NotNil(rejected.DecidedBy)
	assert.Equal("user", *rejected.DecidedBy,
		"an explicit unlink is a user suppression even for a system-owned edge")

	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 0)
	require.NoError(err, "replay accepted matches")
	assert.Equal(0, applied)
	assert.False(linkedPair(t, st, left, right))
}

func TestUnlinkManualLinkLeavesUnrelatedCandidateAccepted(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	manualLeft, err := st.EnsureParticipantByIdentifier(
		"beeper", "@manual-unlink-left:beeper.local", "Test User")
	require.NoError(err, "ensure manual left")
	manualRight, err := st.EnsureParticipantByIdentifier(
		"beeper", "@manual-unlink-right:beeper.local", "Test User")
	require.NoError(err, "ensure manual right")
	_, err = st.LinkParticipants(manualLeft, manualRight)
	require.NoError(err, "create manual link")
	manualCandidate := upsertPairCandidate(t, st, manualLeft, manualRight,
		store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, manualCandidate.ID, "system", nil)
	require.NoError(err, "accept candidate for pre-existing manual link")

	unrelatedLeft, err := st.EnsureParticipantByIdentifier(
		"beeper", "@unrelated-unlink-left:beeper.local", "Test User")
	require.NoError(err, "ensure unrelated left")
	unrelatedRight, err := st.EnsureParticipantByIdentifier(
		"beeper", "@unrelated-unlink-right:beeper.local", "Test User")
	require.NoError(err, "ensure unrelated right")
	unrelated := upsertPairCandidate(t, st, unrelatedLeft, unrelatedRight,
		store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, unrelated.ID, "system", nil)
	require.NoError(err, "accept unrelated candidate")

	var owner sql.NullInt64
	lo, hi := normalizeEdgeForTest(manualLeft, manualRight)
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT identity_match_candidate_id FROM participant_links
		WHERE participant_a = ? AND participant_b = ?`), lo, hi).Scan(&owner),
		"read manual link ownership")
	assert.False(owner.Valid, "manual links must remain unowned")

	_, err = st.UnlinkParticipants(manualLeft, manualRight)
	require.NoError(err, "unlink manual link")
	assert.False(linkedPair(t, st, manualLeft, manualRight))
	reloadedManual, err := st.GetIdentityMatchCandidateContext(ctx, manualCandidate.ID)
	require.NoError(err, "reload candidate for manual link")
	assert.Equal(store.IdentityMatchStateRejected, reloadedManual.State,
		"an accepted candidate for a manual edge must be suppressed on unlink")
	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 0)
	require.NoError(err, "replay after manual unlink")
	assert.Equal(0, applied, "manual unlink suppression must prevent replay")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, unrelated.ID)
	require.NoError(err, "reload unrelated candidate")
	assert.Equal(store.IdentityMatchStateAccepted, reloaded.State,
		"unlinking a manual edge must not reject an unrelated candidate")
	assert.True(linkedPair(t, st, unrelatedLeft, unrelatedRight),
		"unlinking a manual edge must not remove an unrelated accepted edge")
}

func TestUnlinkOwnedIdentityMatchRollsBackSuppressionAndEdge(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to force unlink rollback")
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@rollback-unlink-left:beeper.local", "Test User")
	require.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@rollback-unlink-right:beeper.local", "Test User")
	require.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.NoError(err, "accept system candidate")

	_, err = st.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_identity_unlink_suppression
		BEFORE UPDATE OF state ON identity_match_candidates
		WHEN OLD.id = %d AND OLD.state = 'accepted' AND NEW.state = 'rejected'
		BEGIN
			SELECT RAISE(ABORT, 'unlink suppression failure');
		END
	`, candidate.ID))
	require.NoError(err, "create rollback trigger")
	t.Cleanup(func() {
		_, _ = st.DB().Exec(`DROP TRIGGER IF EXISTS fail_identity_unlink_suppression`)
	})

	_, err = st.UnlinkParticipants(left, right)
	require.Error(err, "unlink suppression failure must abort the transaction")
	assert.True(linkedPair(t, st, left, right),
		"failed suppression must roll back edge deletion")
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload candidate after rollback")
	assert.Equal(store.IdentityMatchStateAccepted, reloaded.State,
		"failed suppression must roll back candidate rejection")
}

func TestApplyAcceptedIdentityMatchesBoundsPendingWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	// Accepted candidates whose links already landed are not recovery work and
	// must not consume the per-import bound.
	for i := range store.DefaultIdentityObservationLookupLimit + 1 {
		left, err := st.EnsureParticipantByIdentifier(
			"beeper", fmt.Sprintf("@linked-left-%d:beeper.local", i), "Test User")
		require.NoError(err, "ensure linked left %d", i)
		right, err := st.EnsureParticipantByIdentifier(
			"beeper", fmt.Sprintf("@linked-right-%d:beeper.local", i), "Test User")
		require.NoError(err, "ensure linked right %d", i)
		candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
		_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
		require.NoError(err, "accept and link candidate %d", i)
	}

	var pending [2]struct {
		left, right int64
	}
	for i := range pending {
		left, err := st.EnsureParticipantByIdentifier(
			"beeper", fmt.Sprintf("@pending-left-%d:beeper.local", i), "Test User")
		require.NoError(err, "ensure pending left %d", i)
		right, err := st.EnsureParticipantByIdentifier(
			"beeper", fmt.Sprintf("@pending-right-%d:beeper.local", i), "Test User")
		require.NoError(err, "ensure pending right %d", i)
		candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
		_, err = st.DecideIdentityMatchCandidateContext(
			ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
		require.NoError(err, "accept pending candidate %d without linking", i)
		pending[i] = struct{ left, right int64 }{left: left, right: right}
	}

	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 1)
	require.NoError(err, "ApplyAcceptedIdentityMatchesContext")
	assert.Equal(1, applied, "the caller limit bounds pending candidates processed")
	assert.True(linkedPair(t, st, pending[0].left, pending[0].right),
		"recovery must page past already-linked accepted candidates")
	assert.False(linkedPair(t, st, pending[1].left, pending[1].right),
		"the next missing link waits because one successful application reached the limit")
}

func TestGetIdentityMatchCandidateLoadsEvidenceAndReportsMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchServiceScopeUsername)

	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "phone", Detail: new("+12025550123"),
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err, "AddIdentityMatchEvidenceContext")

	loaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "GetIdentityMatchCandidateContext")
	require.Len(loaded.Evidence, 1)
	assert.Equal("phone", loaded.Evidence[0].EvidenceKind)

	_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID+10_000)
	assert.ErrorIs(err, store.ErrIdentityMatchNotFound)
}

func TestGetIdentityMatchCandidateApplicationHonorsCancellation(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	alice, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure second alice")
	candidate := upsertPairCandidate(t, st, alice, bob, store.IdentityMatchStableProviderID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.ErrorIs(err, context.Canceled, "single-candidate read")
	_, _, err = st.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	require.ErrorIs(err, context.Canceled, "accept and apply")
	_, err = st.ApplyAcceptedIdentityMatchesContext(ctx, 10)
	require.ErrorIs(err, context.Canceled, "accepted-match resume")
}
