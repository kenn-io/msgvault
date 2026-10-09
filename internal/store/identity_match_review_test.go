package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestListIdentityMatchReviewsDoesNotWaitForIdentityWriter(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "listed-review-left", "Listed Review Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "listed-review-right", "Listed Review Right")
	require.NoError(err)
	_, _, err = st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)

	writer, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	_, err = writer.ExecContext(ctx, st.Rebind(
		`UPDATE archive_metadata SET value = value WHERE key = ?`), "identity_revision")
	require.NoError(err)

	listDone := make(chan error, 1)
	go func() {
		_, listErr := st.ListIdentityMatchReviewsContext(ctx,
			[]store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
		listDone <- listErr
	}()
	select {
	case err := <-listDone:
		require.NoError(err)
	case <-time.After(5 * time.Second):
		require.NoError(writer.Rollback())
		require.NoError(<-listDone)
		require.Fail("listing review tokens waited for the identity mutation lock")
	}
	require.NoError(writer.Rollback())
}

func TestListIdentityMatchReviewTokensMatchIndividualReviewSnapshots(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "batched-review-left", "Batched Review Left")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	linkedLeft, err := st.EnsureParticipantByIdentifier("beeper", "batched-review-linked", "Batched Review Linked")
	require.NoError(err)
	_, err = st.LinkParticipants(left, linkedLeft)
	require.NoError(err)
	for _, identifier := range []string{"batched-review-right-a", "batched-review-right-b"} {
		right, err := st.EnsureParticipantByIdentifier("beeper", identifier, "Batched Review Right")
		require.NoError(err)
		candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: linkedLeft,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
			Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
		_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
			EvidenceKind: "email", Detail: new(identifier + "@example.test"),
			Source: store.ProvenanceArchiveObservation,
		})
		require.NoError(err)
	}
	listed, err := st.ListIdentityMatchReviewsContext(ctx,
		[]store.IdentityMatchState{store.IdentityMatchStateCandidate}, 10, 0)
	require.NoError(err)
	require.Len(listed, 2)
	for _, candidate := range listed {
		individual, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
		require.NoError(err)
		assert.Equal(individual.ReviewToken, candidate.ReviewToken)
		require.NotNil(candidate.LeftPerson)
		assert.Equal(person.ID, candidate.LeftPerson.PersonID)
		assert.Equal(individual.LeftPerson, candidate.LeftPerson)
		assert.Nil(candidate.RightPerson)
	}
}

func TestIdentityMatchReviewRejectsChangedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "review-left", "Review Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "review-right", "Review Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	require.NotEmpty(review.ReviewToken)
	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("same@example.test"),
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(
		ctx, candidate.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil,
	)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
	current, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateCandidate, current.State)

	fresh, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.NotEqual(review.ReviewToken, fresh.ReviewToken)
	accepted, firstRevision, err := st.DecideIdentityMatchReviewedContext(
		ctx, candidate.ID, fresh.ReviewToken, store.IdentityMatchStateAccepted, nil,
	)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	retry, retryRevision, err := st.DecideIdentityMatchReviewedContext(
		ctx, candidate.ID, fresh.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	assert.Equal(firstRevision, retryRevision)
	assert.Equal(store.IdentityMatchStateAccepted, retry.State)
}

func TestIdentityMatchReviewRejectsStaleTokenAfterAcceptedEvidenceChanges(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "stale-accepted-left", "Stale Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "stale-accepted-right", "Stale Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Detail: new("stale@example.test"),
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	accepted, _, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	require.Equal(store.IdentityMatchStateAccepted, accepted.State)
	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "display_name", Detail: new("a later observation"),
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
}

func TestIdentityMatchReviewRetryAfterLinkUpdatesPersonBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "bound-left", "Bound Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "unbound-right", "Bound Right")
	require.NoError(err)
	_, _, err = st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	accepted, _, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)

	retried, _, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err, "retry with the same review token must remain idempotent after binding updates")
	assert.Equal(store.IdentityMatchStateAccepted, retried.State)
}

func TestIdentityMatchReviewRejectsChangedSourceSupport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "support-left", "Support Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "support-right", "Support Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
		SourceID: &fixture.Source.ID,
	})
	require.NoError(err)
	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "display_name", Source: store.ProvenanceArchiveObservation,
		SourceID: &fixture.Source.ID,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	require.Len(review.Evidence, 2)
	assert.Equal(fixture.Source.ID, review.Evidence[0].SourceSupport[0].SourceID)
	assert.Equal(fixture.Source.ID, review.Evidence[1].SourceSupport[0].SourceID,
		"two evidence rows from one archive source are one provenance source")
	second, err := st.GetOrCreateSource("beeper", "other-account")
	require.NoError(err)
	require.NoError(st.AttachIdentityMatchEvidenceSourceContext(ctx, evidence.ID, second.ID))
	withIndependentSource, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Len(withIndependentSource.Evidence[0].SourceSupport, 2)
	assert.Len(withIndependentSource.Evidence[1].SourceSupport, 1)
	_, _, err = st.DecideIdentityMatchReviewedContext(
		ctx, candidate.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
}

func TestIdentityMatchReviewTracksObservationAndBindingChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "context-left", "Context Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "context-right", "Context Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	before, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(`INSERT INTO participant_contact_observations
		(participant_id, source_id, address_kind, original_value, normalized_value, source)
		VALUES (?, ?, ?, ?, ?, ?)`), left, fixture.Source.ID, "email",
		"context@example.test", "context@example.test", store.ProvenanceArchiveObservation)
	require.NoError(err)
	afterObservation, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.NotEqual(before.ReviewToken, afterObservation.ReviewToken)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		before.ReviewToken, store.IdentityMatchStateRejected, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
	_, _, err = st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	afterBinding, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.NotEqual(afterObservation.ReviewToken, afterBinding.ReviewToken)
}

func TestIdentityMatchReviewFingerprintTracksPublicationOnBoundPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "publication-left", "Publication Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "publication-right", "Publication Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	before, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, st.Rebind(
		`INSERT INTO carddav_publications (person_id, desired) VALUES (?, TRUE)`), person.ID)
	require.NoError(err)
	after, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.NotEqual(before.ReviewToken, after.ReviewToken)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		before.ReviewToken, store.IdentityMatchStateRejected, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
}

func TestIdentityMatchReviewMarksOnlyDesiredOrPendingPublicationActive(t *testing.T) {
	for _, scenario := range []struct {
		name             string
		desired          bool
		pendingOperation string
		wantActive       bool
	}{
		{name: "desired publication", desired: true, wantActive: true},
		{name: "settled unpublished row", desired: false, wantActive: false},
		{name: "pending unpublish", desired: false, pendingOperation: "delete", wantActive: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			require := require.New(t)
			st := storetest.New(t).Store
			ctx := t.Context()
			left, err := st.EnsureParticipantByIdentifier("beeper", "publication-state-left", "Publication State Left")
			require.NoError(err)
			right, err := st.EnsureParticipantByIdentifier("beeper", "publication-state-right", "Publication State Right")
			require.NoError(err)
			candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left,
				RightKind: store.IdentityMatchParticipant, RightID: right,
				Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
				Source: store.ProvenanceArchiveObservation,
			})
			require.NoError(err)
			person, _, err := st.CreatePersonFromParticipantContext(ctx, left)
			require.NoError(err)

			var args []any
			query := `INSERT INTO carddav_publications (person_id, desired) VALUES (?, ?)`
			args = []any{person.ID, scenario.desired}
			if scenario.pendingOperation != "" {
				allowed := true
				_, books, discoveryErr := st.ReplaceCardDAVDiscoveryContext(ctx, store.CardDAVDiscoveryInput{
					BaseURL: "https://contacts.example/dav", Username: "alice",
					PrincipalURL: "https://contacts.example/principal/alice/",
					HomeURL:      "https://contacts.example/books/alice/",
					Books: []store.CardDAVDiscoveredBook{{
						CanonicalURL: "https://contacts.example/books/alice/personal/",
						DisplayName:  "Personal", CanDelete: &allowed,
					}},
				})
				require.NoError(discoveryErr)
				require.Len(books, 1)
				query = `INSERT INTO carddav_publications
					(person_id, desired, address_book_id, href, pending_operation, local_hash,
					 connection_generation, book_sync_revision, mapping_revision)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
				args = []any{person.ID, scenario.desired, books[0].ID,
					books[0].CanonicalURL + "person.vcf", scenario.pendingOperation,
					"hash", 1, books[0].SyncRevision, 1}
			}
			_, err = st.DB().ExecContext(ctx, st.Rebind(query), args...)
			require.NoError(err)

			review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
			require.NoError(err)
			require.NotNil(review.LeftPerson)
			assert.Equal(t, scenario.wantActive, review.LeftPerson.ActiveCardDAVPublication)
		})
	}
}

func TestIdentityMatchReviewRecoveryRefusesChangedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "recovery-left", "Recovery Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "recovery-right", "Recovery Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
		SourceID: &fixture.Source.ID,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "recovery-other")
	require.NoError(err)
	reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(func() {
		require.NoError(st.AttachIdentityMatchEvidenceSourceContext(ctx, evidence.ID, second.ID))
	})
	defer reset()
	_, _, err = st.DecideIdentityMatchReviewedContext(
		ctx, candidate.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
	current, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateConflict, current.State)
	members, err := st.ClusterMembers(left)
	require.NoError(err)
	assert.Equal([]int64{left}, members)
}

func TestIdentityMatchReviewRestartRecoveryRechecksEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := storetest.New(t)
	st := fixture.Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "restart-left", "Restart Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "restart-right", "Restart Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	evidence, err := st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "email", Source: store.ProvenanceArchiveObservation,
		SourceID: &fixture.Source.ID,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	decisionContext, cancel := context.WithCancel(ctx)
	reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(cancel)
	_, _, err = st.DecideIdentityMatchReviewedContext(
		decisionContext, candidate.ID, review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	reset()
	require.ErrorIs(err, context.Canceled)
	pending, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.True(pending.ApplicationPending)
	second, err := st.GetOrCreateSource("beeper", "restart-other")
	require.NoError(err)
	require.NoError(st.AttachIdentityMatchEvidenceSourceContext(ctx, evidence.ID, second.ID))
	applied, err := st.ApplyAcceptedIdentityMatchesContext(ctx, 10)
	require.NoError(err)
	assert.Zero(applied)
	current, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateConflict, current.State)
	members, err := st.ClusterMembers(left)
	require.NoError(err)
	assert.Equal([]int64{left}, members)
}

func TestIdentityMatchReviewPendingRetryAppliesOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "retry-left", "Retry Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "retry-right", "Retry Right")
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	decisionContext, cancel := context.WithCancel(ctx)
	reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(cancel)
	_, _, err = st.DecideIdentityMatchReviewedContext(decisionContext, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	reset()
	require.ErrorIs(err, context.Canceled)
	pending, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.True(pending.ApplicationPending)

	applied, revision, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, applied.State)
	linked, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.False(linked.ApplicationPending)
	again, retryRevision, err := st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	assert.Equal(revision, retryRevision)
	assert.Equal(store.IdentityMatchStateAccepted, again.State)
	members, err := st.ClusterMembers(left)
	require.NoError(err)
	assert.Equal([]int64{left, right}, members)
}

func TestIdentityMatchReviewCardDAVResourceRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, account, book := newCardDAVResourceStore(t)
	ctx := t.Context()
	_, err := st.DB().Exec(st.Rebind(`UPDATE carddav_address_books
		SET is_write_target = FALSE WHERE id = ?`), book.ID)
	require.NoError(err)
	for _, uid := range []string{"review-card-a", "review-card-b"} {
		var personID int64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`INSERT INTO persons (vcard_uid) VALUES (?) RETURNING id`), uid).Scan(&personID))
		_, err = st.DB().ExecContext(ctx, st.Rebind(
			`INSERT INTO carddav_publications (person_id, desired) VALUES (?, FALSE)`), personID)
		require.NoError(err)
		_, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
			AddressKind:   store.ContactAddressEmail,
			OriginalValue: "shared@example.test",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	input := remoteResource(book.CanonicalURL+"review-card.vcf", "remote-review-card",
		"Review Card", "shared@example.test", `"one"`)
	_, err = st.ApplyCardDAVSyncPlanContext(ctx, store.CardDAVSyncPlan{
		AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration,
		SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{input},
	})
	require.NoError(err)
	candidates, err := st.ListIdentityMatchReviewsContext(ctx,
		[]store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)
	oldToken := candidates[0].ReviewToken
	individual, err := st.GetIdentityMatchReviewContext(ctx, candidates[0].ID)
	require.NoError(err)
	assert.Equal(individual.ReviewToken, oldToken,
		"batched resource and person fingerprints must match an individual review read")
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidates[1].ID,
		candidates[1].ReviewToken, store.IdentityMatchStateRejected, nil)
	require.NoError(err, "the list token must be accepted by the decision fingerprint check")

	// A remote refresh may also remove/rekey ambiguous suggestions. Isolate
	// the ledger revision here to prove the token binds to that input itself.
	_, err = st.DB().ExecContext(ctx, st.Rebind(`UPDATE carddav_resources
		SET remote_etag = ?, mapping_revision = mapping_revision + 1 WHERE id = ?`),
		`"two"`, candidates[0].LeftID)
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidates[0].ID,
		oldToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)

	fresh, err := st.GetIdentityMatchReviewContext(ctx, candidates[0].ID)
	require.NoError(err)
	assert.NotEqual(oldToken, fresh.ReviewToken)
	accepted, _, err := st.DecideIdentityMatchReviewedContext(ctx, fresh.ID,
		fresh.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, accepted.State)
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_resources
		SET remote_etag = ?, mapping_revision = mapping_revision + 1 WHERE id = ?`),
		`"three"`, candidates[0].LeftID)
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, fresh.ID,
		fresh.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
}

func pendingReviewedIdentityMatch(t *testing.T, st *store.Store, left, right int64) *store.IdentityMatchCandidate {
	t.Helper()
	require := require.New(t)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	review, err := st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	interrupted, cancel := context.WithCancel(t.Context())
	defer cancel()
	reset := st.SetIdentityMatchReviewAfterDecisionHookForTest(cancel)
	defer reset()
	_, _, err = st.DecideIdentityMatchReviewedContext(interrupted, candidate.ID,
		review.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, context.Canceled)
	pending, err := st.GetIdentityMatchReviewContext(t.Context(), candidate.ID)
	require.NoError(err)
	require.Equal(store.IdentityMatchStateAccepted, pending.State)
	require.True(pending.ApplicationPending)
	return review
}

func TestIdentityMatchReviewFreshPendingAcceptanceRefreshesProofAndReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := storetest.New(t).Store
	left, err := st.EnsureParticipantByIdentifier("beeper", "fresh-pending-left", "Synthetic Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "fresh-pending-right", "Synthetic Right")
	require.NoError(err)
	original := pendingReviewedIdentityMatch(t, st, left, right)
	_, err = st.AddIdentityMatchEvidenceContext(t.Context(), original.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "display_name", Detail: new("Synthetic corroboration"), Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(t.Context(), original.ID,
		original.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)
	fresh, err := st.GetIdentityMatchReviewContext(t.Context(), original.ID)
	require.NoError(err)
	_, revision, err := st.DecideIdentityMatchReviewedContext(t.Context(), original.ID,
		fresh.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err, "a fresh review must authorize the newly reviewed evidence")
	current, err := st.GetIdentityMatchReviewContext(t.Context(), original.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateAccepted, current.State)
	assert.False(current.ApplicationPending)
	members, err := st.ClusterMembers(left)
	require.NoError(err)
	assert.Equal([]int64{left, right}, members)
	_, retryRevision, err := st.DecideIdentityMatchReviewedContext(t.Context(), original.ID,
		fresh.ReviewToken, store.IdentityMatchStateAccepted, nil)
	require.NoError(err, "the new review receipt must support a repeated request")
	assert.Equal(revision, retryRevision)
}

func TestIdentityMatchReviewPendingRecoverySettlesExistingConnection(t *testing.T) {
	for _, indirect := range []bool{false, true} {
		name := "direct"
		if indirect {
			name = "indirect"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := storetest.New(t).Store
			left, err := st.EnsureParticipantByIdentifier("beeper", "connected-pending-left", "Synthetic Left")
			require.NoError(err)
			right, err := st.EnsureParticipantByIdentifier("beeper", "connected-pending-right", "Synthetic Right")
			require.NoError(err)
			_, _, err = st.CreatePersonFromParticipantContext(t.Context(), left)
			require.NoError(err)
			original := pendingReviewedIdentityMatch(t, st, left, right)
			from := left
			if indirect {
				from, err = st.EnsureParticipantByIdentifier("beeper", "connected-pending-bridge", "Synthetic Bridge")
				require.NoError(err)
				_, err = st.LinkParticipants(left, from)
				require.NoError(err)
			}
			linkedRevision, err := st.LinkParticipants(from, right)
			require.NoError(err)
			before, err := st.GetIdentityMatchReviewContext(t.Context(), original.ID)
			require.NoError(err)
			var edgeCount int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_links`).Scan(&edgeCount))
			for range 2 {
				_, revision, linked, err := st.ResumeAcceptedIdentityMatchCandidateContext(t.Context(), original.ID)
				require.NoError(err)
				assert.False(linked)
				assert.Equal(linkedRevision, revision)
				current, err := st.GetIdentityMatchReviewContext(t.Context(), original.ID)
				require.NoError(err)
				assert.Equal(store.IdentityMatchStateAccepted, current.State)
				assert.False(current.ApplicationPending)
				assert.Equal(before.DecidedBy, current.DecidedBy)
				assert.Equal(before.DecidedAt, current.DecidedAt)
			}
			var afterEdgeCount int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participant_links`).Scan(&afterEdgeCount))
			assert.Equal(edgeCount, afterEdgeCount, "settling must not add another edge")
			applied, err := st.ApplyAcceptedIdentityMatchesContext(t.Context(), 1)
			require.NoError(err)
			assert.Zero(applied)
		})
	}
}

func TestIdentityMatchReviewIgnoresPublicationTimestampRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, book := newCardDAVResourceStore(t)
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("beeper", "refresh-left", "Synthetic Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "refresh-right", "Synthetic Right")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, left)
	require.NoError(err)
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchEmail, State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	snapshot, err := st.LoadPersonVCardSnapshotContext(ctx, person.ID)
	require.NoError(err)
	publication, err := st.PrepareCardDAVPublicationContext(ctx, store.CardDAVPublicationPlan{
		PersonID: person.ID, Desired: true, AddressBookID: book.ID, Href: book.CanonicalURL + "example.vcf",
		OutgoingBody:         []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Synthetic Example\r\nEND:VCARD\r\n"),
		OutgoingSemanticHash: "example-semantic", LocalHash: snapshot.Fingerprint,
	})
	require.NoError(err)
	// Represent a pending publication persisted before this process started.
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_publications SET updated_at = ? WHERE person_id = ?`),
		"2026-01-01 00:00:00", person.ID)
	require.NoError(err)
	before, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	refreshed, err := st.RefreshCardDAVPublicationFenceContext(ctx, person.ID)
	require.NoError(err)
	assert.Equal(publication.BookSyncRevision, refreshed.BookSyncRevision)
	assert.Equal(publication.MappingRevision, refreshed.MappingRevision)
	assert.Equal(publication.MutationRevision, refreshed.MutationRevision)
	assert.Equal(publication.LocalHash, refreshed.LocalHash)
	after, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(before.ReviewToken, after.ReviewToken)
	listed, err := st.ListIdentityMatchReviewsContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(listed, 1)
	assert.Equal(before.ReviewToken, listed[0].ReviewToken)

	// A real fence change must still invalidate the review.
	_, err = st.DB().Exec(st.Rebind(`UPDATE carddav_address_books SET sync_revision = sync_revision + 1 WHERE id = ?`), book.ID)
	require.NoError(err)
	_, err = st.RefreshCardDAVPublicationFenceContext(ctx, person.ID)
	require.NoError(err)
	_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
		before.ReviewToken, store.IdentityMatchStateRejected, nil)
	require.ErrorIs(err, store.ErrIdentityMatchReviewStale)

	for _, change := range []struct {
		name, query string
	}{
		{"publication intent", `UPDATE carddav_publications SET desired = FALSE WHERE person_id = ?`},
		{"content", `UPDATE carddav_publications SET outgoing_body = NULL WHERE person_id = ?`},
		{"revision", `UPDATE carddav_publications SET mutation_revision = mutation_revision + 1 WHERE person_id = ?`},
	} {
		fresh, err := st.GetIdentityMatchReviewContext(ctx, candidate.ID)
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(change.query), person.ID)
		require.NoError(err)
		_, _, err = st.DecideIdentityMatchReviewedContext(ctx, candidate.ID,
			fresh.ReviewToken, store.IdentityMatchStateRejected, nil)
		require.ErrorIs(err, store.ErrIdentityMatchReviewStale, change.name)
	}
}
