package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type acceptedApplicationResult struct {
	candidate *store.IdentityMatchCandidate
	revision  int64
	applied   int
	err       error
}

func TestPostgreSQLSystemAcceptanceCannotOverwriteConcurrentRejection(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL identity decision/rejection interleaving regression")
	}
	ctx := t.Context()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@pg-race-left:beeper.local", "Test User")
	requirements.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@pg-race-right:beeper.local", "Test User")
	requirements.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)

	const advisoryKey int64 = 725034
	holder, err := st.DB().Conn(ctx)
	requirements.NoError(err, "reserve advisory-lock connection")
	locked := true
	rejectionStarted := false
	systemStarted := false
	rejectionFinished := make(chan struct{})
	systemFinished := make(chan struct{})
	rejectionDone := make(chan error, 1)
	systemDone := make(chan error, 1)
	t.Cleanup(func() {
		if locked {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
		if rejectionStarted {
			waitForPostgreSQLTestGoroutine(t, rejectionFinished)
		}
		if systemStarted {
			waitForPostgreSQLTestGoroutine(t, systemFinished)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey)
	requirements.NoError(err, "hold rejection trigger")
	var holderPID int
	requirements.NoError(holder.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&holderPID), "read rejection trigger holder PID")

	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION wait_for_identity_match_rejection() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%d);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER wait_for_identity_match_rejection
		BEFORE UPDATE OF state ON identity_match_candidates
		FOR EACH ROW
		WHEN (OLD.id = %d AND OLD.state = 'candidate' AND NEW.state = 'rejected')
		EXECUTE FUNCTION wait_for_identity_match_rejection()
	`, advisoryKey, candidate.ID))
	requirements.NoError(err, "install controlled rejection trigger")

	rejectionStarted = true
	go func() {
		defer close(rejectionFinished)
		_, rejectErr := st.DecideIdentityMatchCandidateContext(
			ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
		rejectionDone <- rejectErr
	}()
	rejectionPID := waitForPostgreSQLBlockedPID(ctx, t, st, holderPID,
		"UPDATE identity_match_candidates SET",
		"user rejection must hold the identity lock while its state update is paused")

	// Accept reads the candidate before it acquires the mutation lock. Its
	// unlocked read therefore sees candidate while the rejection is paused;
	// the state CAS must lose after the rejection commits.
	systemStarted = true
	go func() {
		defer close(systemFinished)
		_, _, acceptErr := st.AcceptIdentityMatchCandidateContext(
			ctx, candidate.ID, "system", nil)
		systemDone <- acceptErr
	}()
	waitForPostgreSQLBlockedPID(ctx, t, st, rejectionPID, "archive_metadata",
		"stale system acceptance must wait for the rejection identity lock")

	var unlocked bool
	err = holder.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, advisoryKey).Scan(&unlocked)
	requirements.NoError(err, "release rejection trigger")
	requirements.True(unlocked, "the test connection held the advisory lock")
	locked = false

	var systemErr error
	select {
	case systemErr = <-systemDone:
	case <-time.After(5 * time.Second):
		requirements.FailNow("system acceptance did not finish")
	}
	requirements.NoError(<-rejectionDone, "user rejection")
	requirements.ErrorIs(systemErr, store.ErrIdentityMatchRejected,
		"the stale system CAS must lose to the committed rejection")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err, "reload candidate")
	assertions.Equal(store.IdentityMatchStateRejected, reloaded.State)
	assertions.False(linkedPair(t, st, left, right),
		"a concurrent system acceptance must not leave a rejected identity edge")
}

func TestPostgreSQLAcceptedMatchApplicationRechecksStateAfterLock(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL identity decision/application interleaving regression")
	}
	ctx := t.Context()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@stale-left:beeper.local", "Test User")
	requirements.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@stale-right:beeper.local", "Test User")
	requirements.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	requirements.NoError(err, "accept candidate without applying link")

	const advisoryKey int64 = 725031
	holder, err := st.DB().Conn(ctx)
	requirements.NoError(err, "reserve advisory-lock connection")
	locked := true
	decisionFinished := make(chan struct{})
	applicationFinished := make(chan struct{})
	decisionStarted := false
	applicationStarted := false
	t.Cleanup(func() {
		if locked {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
		if decisionStarted {
			waitForPostgreSQLTestGoroutine(t, decisionFinished)
		}
		if applicationStarted {
			waitForPostgreSQLTestGoroutine(t, applicationFinished)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey)
	requirements.NoError(err, "hold decision trigger")
	var holderPID int
	requirements.NoError(holder.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&holderPID), "read decision trigger holder PID")

	_, err = st.DB().ExecContext(ctx, `
		CREATE FUNCTION wait_for_identity_match_conflict() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(725031);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER wait_for_identity_match_conflict
		BEFORE UPDATE OF state ON identity_match_candidates
		FOR EACH ROW
		WHEN (OLD.state = 'accepted' AND NEW.state = 'conflict')
		EXECUTE FUNCTION wait_for_identity_match_conflict()
	`)
	requirements.NoError(err, "install controlled decision trigger")

	decisionDone := make(chan error, 1)
	decisionStarted = true
	go func() {
		defer close(decisionFinished)
		_, decideErr := st.DecideIdentityMatchCandidateContext(
			ctx, candidate.ID, store.IdentityMatchStateConflict, "system",
			new("concurrent conflict"))
		decisionDone <- decideErr
	}()

	decisionPID := waitForPostgreSQLBlockedPID(ctx, t, st, holderPID,
		"UPDATE identity_match_candidates SET",
		"the decision must hold the identity lock while its update is paused")

	applicationDone := make(chan acceptedApplicationResult, 1)
	applicationStarted = true
	go func() {
		defer close(applicationFinished)
		applied, applyErr := st.ApplyAcceptedIdentityMatchesContext(ctx, 1)
		applicationDone <- acceptedApplicationResult{applied: applied, err: applyErr}
	}()

	waitForPostgreSQLBlockedPID(ctx, t, st, decisionPID, "archive_metadata",
		"application must read accepted state and then wait for the identity lock")

	var unlocked bool
	err = holder.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, advisoryKey).Scan(&unlocked)
	requirements.NoError(err, "release decision trigger")
	requirements.True(unlocked, "the test connection held the advisory lock")
	locked = false

	select {
	case err = <-decisionDone:
		requirements.NoError(err, "commit conflict decision")
	case <-time.After(5 * time.Second):
		requirements.FailNow("conflict decision did not finish")
	}
	select {
	case result := <-applicationDone:
		requirements.NoError(result.err, "stale accepted-match application is a no-op")
		assertions.Zero(result.applied)
	case <-time.After(5 * time.Second):
		requirements.FailNow("accepted-match application did not finish")
	}

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err, "reload candidate")
	assertions.Equal(store.IdentityMatchStateConflict, reloaded.State)
	assertions.False(linkedPair(t, st, left, right),
		"a stale accepted snapshot must not create a link after conflict commits")
}

func TestPostgreSQLAppliedMatchBlocksConcurrentConflictDecision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL identity application/decision interleaving regression")
	}
	ctx := t.Context()

	left, err := st.EnsureParticipantByIdentifier(
		"beeper", "@applied-first-left:beeper.local", "Test User")
	requirements.NoError(err, "ensure left")
	right, err := st.EnsureParticipantByIdentifier(
		"beeper", "@applied-first-right:beeper.local", "Test User")
	requirements.NoError(err, "ensure right")
	candidate := upsertPairCandidate(t, st, left, right, store.IdentityMatchStableProviderID)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	requirements.NoError(err, "accept candidate without applying link")

	const advisoryKey int64 = 725033
	holder, err := st.DB().Conn(ctx)
	requirements.NoError(err, "reserve advisory-lock connection")
	locked := true
	applicationFinished := make(chan struct{})
	decisionFinished := make(chan struct{})
	applicationStarted := false
	decisionStarted := false
	t.Cleanup(func() {
		if locked {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
		if applicationStarted {
			waitForPostgreSQLTestGoroutine(t, applicationFinished)
		}
		if decisionStarted {
			waitForPostgreSQLTestGoroutine(t, decisionFinished)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey)
	requirements.NoError(err, "hold participant-link trigger")
	var holderPID int
	requirements.NoError(holder.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&holderPID), "read participant-link trigger holder PID")

	_, err = st.DB().ExecContext(ctx, `
		CREATE FUNCTION wait_for_identity_match_link() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(725033);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER wait_for_identity_match_link
		BEFORE INSERT ON participant_links
		FOR EACH ROW
		EXECUTE FUNCTION wait_for_identity_match_link()
	`)
	requirements.NoError(err, "install controlled participant-link trigger")

	applicationDone := make(chan acceptedApplicationResult, 1)
	applicationStarted = true
	go func() {
		defer close(applicationFinished)
		applied, applyErr := st.ApplyAcceptedIdentityMatchesContext(ctx, 1)
		applicationDone <- acceptedApplicationResult{applied: applied, err: applyErr}
	}()
	applicationPID := waitForPostgreSQLBlockedPID(ctx, t, st, holderPID,
		"INSERT INTO participant_links",
		"application must hold the identity lock while its link insert is paused")

	decisionDone := make(chan error, 1)
	decisionStarted = true
	go func() {
		defer close(decisionFinished)
		_, decideErr := st.DecideIdentityMatchCandidateContext(
			ctx, candidate.ID, store.IdentityMatchStateConflict, "system",
			new("concurrent conflict"))
		decisionDone <- decideErr
	}()
	waitForPostgreSQLBlockedPID(ctx, t, st, applicationPID, "archive_metadata",
		"conflict decision must wait for the application identity lock")

	var unlocked bool
	err = holder.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, advisoryKey).Scan(&unlocked)
	requirements.NoError(err, "release participant-link trigger")
	requirements.True(unlocked, "the test connection held the advisory lock")
	locked = false

	select {
	case result := <-applicationDone:
		requirements.NoError(result.err, "commit accepted-match application")
		assertions.Equal(1, result.applied)
	case <-time.After(5 * time.Second):
		requirements.FailNow("accepted-match application did not finish")
	}
	select {
	case err = <-decisionDone:
		requirements.ErrorIs(err, store.ErrIdentityMatchAlreadyApplied,
			"a conflict decision must not contradict the committed participant link")
	case <-time.After(5 * time.Second):
		requirements.FailNow("conflict decision did not finish")
	}

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err, "reload candidate")
	assertions.Equal(store.IdentityMatchStateAccepted, reloaded.State)
	assertions.True(linkedPair(t, st, left, right))
}

func TestPostgreSQLAcceptedMatchApplicationSurvivesParticipantMerge(t *testing.T) {
	for _, test := range []struct {
		name        string
		collapse    bool
		directApply string
	}{
		{name: "rewritten endpoints", collapse: false},
		{name: "deleted self link", collapse: true},
		{name: "direct resume of deleted self link", collapse: true, directApply: "resume"},
		{name: "direct accept of deleted self link", collapse: true, directApply: "accept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			testPostgreSQLAcceptedApplicationDuringParticipantMerge(
				t, test.collapse, test.directApply,
			)
		})
	}
}

func testPostgreSQLAcceptedApplicationDuringParticipantMerge(
	t *testing.T, collapse bool, directApply string,
) {
	t.Helper()
	requirements := require.New(t)
	assertions := assert.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL participant-merge/application interleaving regression")
	}
	ctx := t.Context()

	absorbed, err := st.EnsureParticipantByIdentifier(
		"beeper", "@merge-absorbed:beeper.local", "Test User")
	requirements.NoError(err, "ensure absorbed participant")
	survivor, err := st.EnsureParticipantByIdentifier(
		"beeper", "@merge-survivor:beeper.local", "Test User")
	requirements.NoError(err, "ensure survivor participant")
	other := survivor
	if !collapse {
		other, err = st.EnsureParticipantByIdentifier(
			"beeper", "@merge-other:beeper.local", "Test User")
		requirements.NoError(err, "ensure other participant")
	}
	candidate := upsertPairCandidate(
		t, st, absorbed, other, store.IdentityMatchStableProviderID)
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	requirements.NoError(err, "accept candidate without applying link")

	const advisoryKey int64 = 725032
	holder, err := st.DB().Conn(ctx)
	requirements.NoError(err, "reserve advisory-lock connection")
	locked := true
	mergeFinished := make(chan struct{})
	applicationFinished := make(chan struct{})
	mergeStarted := false
	applicationStarted := false
	t.Cleanup(func() {
		if locked {
			_, _ = holder.ExecContext(context.Background(),
				`SELECT pg_advisory_unlock($1)`, advisoryKey)
		}
		if mergeStarted {
			waitForPostgreSQLTestGoroutine(t, mergeFinished)
		}
		if applicationStarted {
			waitForPostgreSQLTestGoroutine(t, applicationFinished)
		}
		_ = holder.Close()
	})
	_, err = holder.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, advisoryKey)
	requirements.NoError(err, "hold participant merge trigger")
	var holderPID int
	requirements.NoError(holder.QueryRowContext(ctx,
		`SELECT pg_backend_pid()`).Scan(&holderPID), "read participant merge trigger holder PID")

	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION wait_for_identity_match_merge() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_advisory_xact_lock(%d);
			IF TG_OP = 'DELETE' THEN
				RETURN OLD;
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER wait_for_identity_match_merge
		BEFORE UPDATE OR DELETE ON identity_match_candidates
		FOR EACH ROW
		WHEN (OLD.id = %d)
		EXECUTE FUNCTION wait_for_identity_match_merge()
	`, advisoryKey, candidate.ID))
	requirements.NoError(err, "install controlled participant merge trigger")

	mergeDone := make(chan error, 1)
	mergeStarted = true
	go func() {
		defer close(mergeFinished)
		mergeDone <- st.MergeParticipants(absorbed, survivor)
	}()

	mergePID := waitForPostgreSQLBlockedPID(ctx, t, st, holderPID,
		"identity_match_candidates",
		"participant merge must hold the identity lock while rewriting the candidate")

	applicationDone := make(chan acceptedApplicationResult, 1)
	applicationStarted = true
	go func() {
		defer close(applicationFinished)
		var appliedCandidate *store.IdentityMatchCandidate
		var revision int64
		var applied int
		var applyErr error
		switch directApply {
		case "resume":
			var linked bool
			appliedCandidate, revision, linked, applyErr =
				st.ResumeAcceptedIdentityMatchCandidateContext(
					ctx, candidate.ID,
				)
			if linked {
				applied = 1
			}
		case "accept":
			appliedCandidate, revision, applyErr = st.AcceptIdentityMatchCandidateContext(
				ctx, candidate.ID, "system", nil,
			)
		default:
			applied, applyErr = st.ApplyAcceptedIdentityMatchesContext(ctx, 1)
		}
		applicationDone <- acceptedApplicationResult{
			candidate: appliedCandidate,
			revision:  revision,
			applied:   applied,
			err:       applyErr,
		}
	}()

	waitForPostgreSQLBlockedPID(ctx, t, st, mergePID, "archive_metadata",
		"application must read the candidate and then wait for the merge identity lock")

	var unlocked bool
	err = holder.QueryRowContext(ctx,
		`SELECT pg_advisory_unlock($1)`, advisoryKey).Scan(&unlocked)
	requirements.NoError(err, "release participant merge trigger")
	requirements.True(unlocked, "the test connection held the advisory lock")
	locked = false

	select {
	case err = <-mergeDone:
		requirements.NoError(err, "commit participant merge")
	case <-time.After(5 * time.Second):
		requirements.FailNow("participant merge did not finish")
	}
	var result acceptedApplicationResult
	select {
	case result = <-applicationDone:
		requirements.NoError(result.err,
			"accepted-match recovery must tolerate a concurrent participant merge")
	case <-time.After(5 * time.Second):
		requirements.FailNow("accepted-match application did not finish")
	}

	if collapse {
		assertions.Zero(result.applied)
		if directApply != "" {
			requirements.NotNil(result.candidate,
				"a merge-completed direct application must return its accepted snapshot")
			assertions.Equal(store.IdentityMatchStateAccepted, result.candidate.State)
			assertions.Positive(result.revision)
		}
		_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
		assertions.ErrorIs(err, store.ErrIdentityMatchNotFound)
		return
	}

	assertions.Equal(1, result.applied)
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	requirements.NoError(err, "reload rewritten candidate")
	assertions.Equal(survivor, reloaded.LeftID)
	assertions.Equal(other, reloaded.RightID)
	assertions.True(linkedPair(t, st, survivor, other),
		"recovery must apply the rewritten accepted candidate endpoints")
}

func waitForPostgreSQLBlockedPID(
	ctx context.Context,
	t *testing.T,
	st *store.Store,
	blockerPID int,
	queryFragment string,
	message string,
) int {
	t.Helper()
	requirements := require.New(t)
	var blockedPID int
	var waitErr error
	require.Eventually(t, func() bool {
		waitErr = st.DB().QueryRowContext(ctx, `SELECT COALESCE(MIN(pid), 0)
			FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))
			  AND POSITION($2 IN query) > 0`,
			blockerPID, queryFragment).Scan(&blockedPID)
		return waitErr == nil && blockedPID > 0
	}, 5*time.Second, 10*time.Millisecond, message)
	requirements.NoError(waitErr)
	return blockedPID
}

func waitForPostgreSQLTestGoroutine(t *testing.T, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		assert.Fail(t, "PostgreSQL test goroutine did not finish during cleanup")
	}
}
