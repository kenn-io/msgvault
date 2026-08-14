package beeper

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// captureAndMatch runs the production capture-then-match pair for one user and
// returns the aggregated outcome, so tests read like the importer's own path.
func captureAndMatch(
	t *testing.T,
	recorder *observationRecorder,
	matcher *identityMatcher,
	participantID int64,
	u *User,
	cc captureContext,
) matchOutcome {
	t.Helper()
	require := require.New(t)

	results, err := recorder.capture(context.Background(), participantID, u, cc)
	require.NoError(err, "capture")
	var combined matchOutcome
	for _, result := range results {
		outcome, err := matcher.match(context.Background(), participantID, result)
		require.NoError(err, "match")
		combined.AutoResolved = append(combined.AutoResolved, outcome.AutoResolved...)
		combined.Suggested = append(combined.Suggested, outcome.Suggested...)
		combined.Conflicts = append(combined.Conflicts, outcome.Conflicts...)
	}
	return combined
}

// linked reports whether two participants share an identity cluster.
func linked(t *testing.T, st *store.Store, a, b int64) bool {
	t.Helper()
	require := require.New(t)

	members, err := st.ClusterMembers(a)
	require.NoError(err, "ClusterMembers")
	return slices.Contains(members, b)
}

func TestRepeatedStableProviderIDResolvesToTheSameParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)

	// The same remote person reached through two Beeper logins of one network:
	// two distinct Beeper user IDs, one provider-native ID.
	firstAccount := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}
	secondAccount := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}
	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram2_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure the second sighting")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice",
		Raw: []byte(`{"id":"@telegram_alice:beeper.local","providerID":"tg-alice-1"}`),
	}, firstAccount)
	outcome := captureAndMatch(t, recorder, matcher, alsoAlice, &User{
		ID: "@telegram2_alice:beeper.local", Username: "@alice",
		Raw: []byte(`{"id":"@telegram2_alice:beeper.local","providerID":"tg-alice-1"}`),
	}, secondAccount)

	require.Len(outcome.AutoResolved, 1,
		"a repeated stable provider ID is the one basis that may resolve automatically")
	assert.Empty(outcome.Conflicts)
	assert.True(linked(t, st, alice, alsoAlice), "the accepted match must be applied")

	candidate, err := st.GetIdentityMatchCandidateContext(ctx, outcome.AutoResolved[0])
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStableProviderID, candidate.Basis)
	assert.Equal(store.IdentityMatchStateAccepted, candidate.State)
	assert.Equal("system", *candidate.DecidedBy)
	require.NotNil(candidate.NormalizedValue)
	assert.Equal("provider:8:telegram:7:account:8:telegram:10:tg-alice-1", *candidate.NormalizedValue,
		"the provider ID is namespaced by service so it cannot collide across networks")
	require.Len(candidate.Evidence, 1, "the resolution records why it happened")
	assert.Equal(evidenceProviderID, candidate.Evidence[0].EvidenceKind)
}

func TestStableMatchRetainsEverySupportingObservationSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	firstSource := newBeeperTestSource(t, st, "account-a-first-import")
	secondSource := newBeeperTestSource(t, st, "account-a-second-import")
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@source-left:beeper.local", "Left User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@source-right:beeper.local", "Right User",
	)
	require.NoError(err)
	leftResults, err := recorder.capture(context.Background(), left, &User{
		ID:  "@source-left:beeper.local",
		Raw: []byte(`{"id":"@source-left:beeper.local","providerID":"shared-source-id"}`),
	}, captureContext{SourceID: firstSource, AccountID: "account-a", Network: "Telegram"})
	require.NoError(err)
	require.Len(leftResults, 1)
	require.NotNil(leftResults[0].Observation.ProviderUserID)
	_, err = matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err)
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID:  "@source-right:beeper.local",
		Raw: []byte(`{"id":"@source-right:beeper.local","providerID":"shared-source-id"}`),
	}, captureContext{SourceID: secondSource, AccountID: "account-a", Network: "Telegram"})
	require.NoError(err)
	require.Len(rightResults, 1)
	require.NotNil(rightResults[0].Observation.ProviderUserID)
	assert.Equal(*leftResults[0].Observation.ProviderUserID,
		*rightResults[0].Observation.ProviderUserID,
		"the two synthetic imports must share one scoped stable provider ID")
	outcome, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err)
	require.Len(outcome.AutoResolved, 1,
		"unexpected match outcome: suggested=%v conflicts=%v",
		outcome.Suggested, outcome.Conflicts)

	var candidateSupport, evidenceSupport int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_candidate_sources
		WHERE candidate_id = ? AND source_id IN (?, ?)
	`), outcome.AutoResolved[0], firstSource, secondSource).Scan(&candidateSupport))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM identity_match_evidence_sources
		WHERE evidence_id IN (
			SELECT id FROM identity_match_evidence WHERE candidate_id = ?
		) AND source_id IN (?, ?)
	`), outcome.AutoResolved[0], firstSource, secondSource).Scan(&evidenceSupport))
	assert.Equal(2, candidateSupport,
		"candidate support must include both matching observations")
	assert.Equal(2, evidenceSupport,
		"evidence support must include both matching observations")

	require.NoError(st.RemoveSource(secondSource))
	_, err = st.GetIdentityMatchCandidateContext(context.Background(), outcome.AutoResolved[0])
	require.ErrorIs(err, store.ErrIdentityMatchNotFound,
		"removing either endpoint observation must invalidate conjunctive support")
	assert.False(linked(t, st, left, right),
		"the automatic link must not survive without both matching observations")

	require.NoError(st.RemoveSource(firstSource))
}

func TestStableMatchRecordsIndependentAssertionsForTheSamePair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	matcher := newIdentityMatcher(st)
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@independent-left:beeper.local", "Left User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@independent-right:beeper.local", "Right User",
	)
	require.NoError(err)

	recordAssertion := func(accountID, providerID string) int64 {
		sourceID := newBeeperTestSource(t, st, accountID)
		recorder := newObservationRecorder(st)
		capture := func(participantID int64, userID string) *store.RecordContactObservationResult {
			results, captureErr := recorder.capture(t.Context(), participantID, &User{
				ID:  userID,
				Raw: []byte(`{"id":"` + userID + `","providerID":"` + providerID + `"}`),
			}, captureContext{
				SourceID: sourceID, AccountID: accountID,
				Network: "Telegram", BridgePrefix: "telegram",
			})
			require.NoError(captureErr)
			require.Len(results, 1)
			return results[0]
		}
		leftResult := capture(left, "@independent-left:beeper.local")
		rightResult := capture(right, "@independent-right:beeper.local")
		_, matchErr := matcher.match(t.Context(), left, leftResult)
		require.NoError(matchErr)
		_, matchErr = matcher.match(t.Context(), right, rightResult)
		require.NoError(matchErr)
		return sourceID
	}

	firstSource := recordAssertion("independent-a", "stable-a")
	secondSource := recordAssertion("independent-b", "stable-b")
	assert.True(linked(t, st, left, right))

	require.NoError(st.RemoveSource(firstSource))
	assert.True(linked(t, st, left, right),
		"the second stable assertion must remain accepted and keep the pair linked")

	require.NoError(st.RemoveSource(secondSource))
	assert.False(linked(t, st, left, right))
}

func TestStableMatchRetainsAlternateCompleteObservationPair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	leftSources := []int64{
		newBeeperTestSource(t, st, "alternate-left-a"),
		newBeeperTestSource(t, st, "alternate-left-b"),
	}
	rightSource := newBeeperTestSource(t, st, "alternate-right")
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@alternate-left:beeper.local", "Left User",
	)
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@alternate-right:beeper.local", "Right User",
	)
	require.NoError(err)

	for _, sourceID := range leftSources {
		results, captureErr := recorder.capture(t.Context(), left, &User{
			ID:  "@alternate-left:beeper.local",
			Raw: []byte(`{"id":"@alternate-left:beeper.local","providerID":"alternate-shared-id"}`),
		}, captureContext{SourceID: sourceID, AccountID: "account-a", Network: "Telegram"})
		require.NoError(captureErr)
		require.Len(results, 1)
		_, matchErr := matcher.match(t.Context(), left, results[0])
		require.NoError(matchErr)
	}
	rightResults, err := recorder.capture(t.Context(), right, &User{
		ID:  "@alternate-right:beeper.local",
		Raw: []byte(`{"id":"@alternate-right:beeper.local","providerID":"alternate-shared-id"}`),
	}, captureContext{SourceID: rightSource, AccountID: "account-a", Network: "Telegram"})
	require.NoError(err)
	require.Len(rightResults, 1)
	outcome, err := matcher.match(t.Context(), right, rightResults[0])
	require.NoError(err)
	require.Len(outcome.AutoResolved, 1)

	require.NoError(st.RemoveSource(leftSources[0]))
	remaining, err := st.GetIdentityMatchCandidateContext(t.Context(), outcome.AutoResolved[0])
	require.NoError(err, "the second left observation still forms a complete pair")
	assert.Equal(store.IdentityMatchStateAccepted, remaining.State)
	assert.True(linked(t, st, left, right))

	require.NoError(st.RemoveSource(leftSources[1]))
	_, err = st.GetIdentityMatchCandidateContext(t.Context(), outcome.AutoResolved[0])
	require.ErrorIs(err, store.ErrIdentityMatchNotFound)
}

func TestIDOnlyUsersProduceNonExportableStableEvidenceAndMatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	idOnlyUser := &User{
		ID:  "@telegram_id_only_left:beeper.local",
		Raw: []byte(`{"id":"@telegram_id_only_left:beeper.local","providerID":"native-id-only"}`),
	}
	assert.Equal("provider:8:telegram:7:account:8:telegram:14:native-id-only",
		providerUserIDScoped("telegram", "telegram", nil, nil, idOnlyUser))
	assert.Len(observedAddresses(idOnlyUser,
		"provider:8:telegram:7:account:8:telegram:14:native-id-only"), 1)
	cc := captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram"),
		AccountID: "telegram", Network: "Telegram",
	}
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_id_only_left:beeper.local", "Test User")
	require.NoError(err, "ensure left participant")
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_id_only_right:beeper.local", "Test User")
	require.NoError(err, "ensure right participant")

	leftResults, err := recorder.capture(context.Background(), left, idOnlyUser, cc)
	require.NoError(err, "capture left")
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID:  "@telegram_id_only_right:beeper.local",
		Raw: []byte(`{"id":"@telegram_id_only_right:beeper.local","providerID":"native-id-only"}`),
	}, cc)
	require.NoError(err, "capture right")
	require.Len(leftResults, 1, "an ID-only user still needs one stable observation")
	require.Len(rightResults, 1, "an ID-only user still needs one stable observation")
	require.NotNil(leftResults[0].Observation.ProviderUserID)
	require.NotNil(rightResults[0].Observation.ProviderUserID)
	assert.Equal("provider:8:telegram:7:account:8:telegram:14:native-id-only",
		*leftResults[0].Observation.ProviderUserID)
	assert.Equal("provider:8:telegram:7:account:8:telegram:14:native-id-only",
		*rightResults[0].Observation.ProviderUserID)
	assert.Equal(store.ContactAddressProviderIdentity,
		leftResults[0].Observation.AddressKind)
	assert.Equal(store.ContactAddressProviderIdentity,
		rightResults[0].Observation.AddressKind)

	first, err := matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err, "match left")
	second, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err, "match right")
	assert.Len(first.AutoResolved, 1,
		"provider-only observations must enter the stable identity matcher")
	assert.Empty(second.AutoResolved)
	assert.Empty(first.Suggested)
	assert.True(linked(t, st, left, right))

	observations, err := st.ListParticipantObservationsContext(
		context.Background(), left, true)
	require.NoError(err, "list left observations")
	require.Len(observations, 1)
	assert.Equal(store.ContactAddressProviderIdentity, observations[0].AddressKind)
}

func TestProviderIdentityIsScopedByAccountForScopedServices(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	leftContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "slack-a"),
		AccountID: "slack-a", Network: "Slack", BridgePrefix: "slack",
	}
	rightContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "slack-b"),
		AccountID: "slack-b", Network: "Slack", BridgePrefix: "slack",
	}
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_scope_left:beeper.local", "Test User")
	require.NoError(err, "ensure left participant")
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_scope_right:beeper.local", "Test User")
	require.NoError(err, "ensure right participant")

	leftResults, err := recorder.capture(context.Background(), left, &User{
		ID:  "@slack_scope_left:beeper.local",
		Raw: []byte(`{"id":"@slack_scope_left:beeper.local","providerID":"same-native-id"}`),
	}, leftContext)
	require.NoError(err, "capture left")
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID:  "@slack_scope_right:beeper.local",
		Raw: []byte(`{"id":"@slack_scope_right:beeper.local","providerID":"same-native-id"}`),
	}, rightContext)
	require.NoError(err, "capture right")
	require.Len(leftResults, 1)
	require.Len(rightResults, 1)
	assert.NotEqual(*leftResults[0].Observation.ProviderUserID,
		*rightResults[0].Observation.ProviderUserID,
		"the same native ID must not cross observed account scopes")

	_, err = matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err, "match left")
	outcome, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err, "match right")
	assert.Empty(outcome.AutoResolved)
	assert.False(linked(t, st, left, right),
		"same provider ID in different account scopes must not auto-link")
}

func TestProviderIdentityIsScopedByAccountForUnscopedServices(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	leftContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram-account-a"),
		AccountID: "telegram-account-a", Network: "Telegram", BridgePrefix: "telegram",
	}
	rightContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram-account-b"),
		AccountID: "telegram-account-b", Network: "Telegram", BridgePrefix: "telegram",
	}
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_unscoped_left:beeper.local", "Test User")
	require.NoError(err, "ensure left participant")
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_unscoped_right:beeper.local", "Test User")
	require.NoError(err, "ensure right participant")
	leftResults, err := recorder.capture(context.Background(), left, &User{
		ID:  "@telegram_unscoped_left:beeper.local",
		Raw: []byte(`{"id":"@telegram_unscoped_left:beeper.local","providerID":"same-native-id"}`),
	}, leftContext)
	require.NoError(err, "capture left")
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID:  "@telegram_unscoped_right:beeper.local",
		Raw: []byte(`{"id":"@telegram_unscoped_right:beeper.local","providerID":"same-native-id"}`),
	}, rightContext)
	require.NoError(err, "capture right")
	require.Len(leftResults, 1)
	require.Len(rightResults, 1)
	assert.NotEqual(*leftResults[0].Observation.ProviderUserID,
		*rightResults[0].Observation.ProviderUserID,
		"an unscoped service still needs the observed account in its native key")
	_, err = matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err, "match left")
	outcome, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err, "match right")
	assert.Empty(outcome.AutoResolved)
	assert.False(linked(t, st, left, right),
		"the same native ID on two unscoped-service accounts must not auto-link")
}

func TestUndocumentedProviderFieldsCannotCreateAutomaticLink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	cc := captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram"),
		AccountID: "telegram", Network: "Telegram", BridgePrefix: "telegram",
	}
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_undocumented_left:beeper.local", "Test User")
	require.NoError(err, "ensure left participant")
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_undocumented_right:beeper.local", "Test User")
	require.NoError(err, "ensure right participant")
	leftResults, err := recorder.capture(context.Background(), left, &User{
		ID:  "@telegram_undocumented_left:beeper.local",
		Raw: []byte(`{"id":"@telegram_undocumented_left:beeper.local","remoteUserID":"same-remote-id","providerUserID":"same-provider-id"}`),
	}, cc)
	require.NoError(err, "capture left")
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID:  "@telegram_undocumented_right:beeper.local",
		Raw: []byte(`{"id":"@telegram_undocumented_right:beeper.local","remoteUserID":"same-remote-id","providerUserID":"same-provider-id"}`),
	}, cc)
	require.NoError(err, "capture right")
	require.Len(leftResults, 1)
	require.Len(rightResults, 1)
	assert.NotEqual(*leftResults[0].Observation.ProviderUserID,
		*rightResults[0].Observation.ProviderUserID,
		"undocumented raw fields must fall back to the account-scoped Beeper ID")
	_, err = matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err, "match left")
	outcome, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err, "match right")
	assert.Empty(outcome.AutoResolved)
	assert.False(linked(t, st, left, right))
}

func TestBeeperFallbackIdentityIsScopedByAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	leftContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "account-a"),
		AccountID: "account-a", Network: "Telegram", BridgePrefix: "telegram",
	}
	rightContext := captureContext{
		SourceID:  newBeeperTestSource(t, st, "account-b"),
		AccountID: "account-b", Network: "Telegram", BridgePrefix: "telegram",
	}
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@same-id-a:beeper.local", "Test User")
	require.NoError(err, "ensure left participant")
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@same-id-b:beeper.local", "Test User")
	require.NoError(err, "ensure right participant")
	leftResults, err := recorder.capture(context.Background(), left, &User{
		ID: "@same-id:beeper.local",
	}, leftContext)
	require.NoError(err, "capture left")
	rightResults, err := recorder.capture(context.Background(), right, &User{
		ID: "@same-id:beeper.local",
	}, rightContext)
	require.NoError(err, "capture right")
	require.Len(leftResults, 1)
	require.Len(rightResults, 1)
	assert.NotEqual(*leftResults[0].Observation.ProviderUserID,
		*rightResults[0].Observation.ProviderUserID)
	_, err = matcher.match(context.Background(), left, leftResults[0])
	require.NoError(err, "match left")
	outcome, err := matcher.match(context.Background(), right, rightResults[0])
	require.NoError(err, "match right")
	assert.Empty(outcome.AutoResolved)
	assert.False(linked(t, st, left, right),
		"a Beeper fallback ID must not cross account boundaries")
}

func TestManyObservationsSharingOneAnchorProduceOneCandidatePerPair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	cc := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure the second sighting")

	// Three addresses on one user all carry the same anchor, so the anchor
	// lookup returns several rows per participant. The matcher must fan out
	// per participant, not per row.
	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice",
		PhoneNumber: "+12025550123", Email: "alice@example.com",
		Raw: []byte(`{"id":"@telegram_alice:beeper.local","providerID":"tg-alice-1"}`),
	}, cc)
	outcome := captureAndMatch(t, recorder, matcher, alsoAlice, &User{
		ID: "@telegram_alice2:beeper.local", Username: "@alice2",
		PhoneNumber: "+12025550124",
		Raw:         []byte(`{"id":"@telegram_alice2:beeper.local","providerID":"tg-alice-1"}`),
	}, cc)
	assert.Len(outcome.AutoResolved, 1, "one pair, one candidate, however many rows share the anchor")

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err, "ListIdentityMatchCandidatesContext")
	assert.Len(candidates, 1)
}

func TestStableProviderMatchRetriesMissingLinkAfterSameRunFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	cc := captureContext{
		SourceID:  newBeeperTestSource(t, st, "telegram"),
		AccountID: "telegram",
		Network:   "Telegram",
	}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_retry_a:beeper.local", "Test User")
	require.NoError(err, "ensure first participant")
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_retry_b:beeper.local", "Test User")
	require.NoError(err, "ensure second participant")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID:          "@telegram_retry_a:beeper.local",
		Username:    "@retry-a",
		PhoneNumber: "+12025550121",
		Raw:         []byte(`{"providerID":"tg-retry-shared"}`),
	}, cc)
	results, err := recorder.capture(ctx, alsoAlice, &User{
		ID:          "@telegram_retry_b:beeper.local",
		Username:    "@retry-b",
		PhoneNumber: "+12025550122",
		Raw:         []byte(`{"providerID":"tg-retry-shared"}`),
	}, cc)
	require.NoError(err, "capture second participant")
	require.GreaterOrEqual(len(results), 2,
		"multiple address observations give the same import run a retry opportunity")

	releaseFailure := installParticipantLinkFailure(t, st)
	_, err = matcher.match(ctx, alsoAlice, results[0])
	require.ErrorContains(err, "forced participant link failure")

	candidates, err := st.ListIdentityMatchCandidatesContext(
		ctx, []store.IdentityMatchState{store.IdentityMatchStateAccepted}, 10, 0)
	require.NoError(err, "read accepted candidate after partial failure")
	require.Len(candidates, 1)
	firstDecision := candidates[0]
	require.NotNil(firstDecision.DecidedAt)
	assert.False(linked(t, st, alice, alsoAlice),
		"the failed link leaves the accepted candidate for retry")

	releaseFailure()
	retried, err := matcher.match(ctx, alsoAlice, results[1])
	require.NoError(err, "same-run stable-ID retry")
	require.Equal([]int64{firstDecision.ID}, retried.AutoResolved)
	assert.True(linked(t, st, alice, alsoAlice),
		"the retry must finish the accepted candidate's missing link")

	after, err := st.GetIdentityMatchCandidateContext(ctx, firstDecision.ID)
	require.NoError(err, "reload candidate after retry")
	assert.Equal(firstDecision.DecidedBy, after.DecidedBy)
	assert.Equal(firstDecision.Notes, after.Notes)
	require.NotNil(after.DecidedAt)
	assert.True(firstDecision.DecidedAt.Equal(*after.DecidedAt),
		"retrying an accepted candidate must preserve its original decision time")
}

func TestStaleAcceptedMatchUsesCurrentReviewOutcome(t *testing.T) {
	for _, test := range []struct {
		name          string
		deleteMatch   bool
		wantConflicts int
	}{
		{name: "conflict", wantConflicts: 1},
		{name: "deleted by participant merge", deleteMatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			matcher := newIdentityMatcher(st)
			left, err := st.EnsureParticipantByIdentifier(
				participantIdentifierType, "@stale-review-left:beeper.local", "Test User")
			requirements.NoError(err, "ensure left participant")
			right, err := st.EnsureParticipantByIdentifier(
				participantIdentifierType, "@stale-review-right:beeper.local", "Test User")
			requirements.NoError(err, "ensure right participant")
			candidate, _, err := st.UpsertIdentityMatchCandidateContext(
				ctx, store.IdentityMatchCandidateInput{
					LeftKind: store.IdentityMatchParticipant, LeftID: left,
					RightKind: store.IdentityMatchParticipant, RightID: right,
					Basis:           store.IdentityMatchStableProviderID,
					NormalizedValue: new("beeper:stale-review"),
					State:           store.IdentityMatchStateCandidate,
					Source:          store.ProvenanceArchiveObservation,
				})
			requirements.NoError(err, "create candidate")
			_, err = st.DecideIdentityMatchCandidateContext(
				ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
			requirements.NoError(err, "accept candidate without linking")

			if test.deleteMatch {
				requirements.NoError(st.MergeParticipants(left, right),
					"merge candidate endpoints")
			} else {
				_, err = st.DecideIdentityMatchCandidateContext(
					ctx, candidate.ID, store.IdentityMatchStateConflict, "system",
					new("review changed during recovery"))
				requirements.NoError(err, "change candidate to conflict")
			}

			pair := newParticipantPair(left, right)
			memoKey := newIdentityMatchMemoKey(
				pair, candidate.Basis, candidate.ServiceSlug, candidate.ScopeKind,
				candidate.ScopeValue, candidate.NormalizedValue,
			)
			var outcome matchOutcome
			err = matcher.handleStaleAcceptedMatch(ctx, candidate, memoKey, &outcome)
			requirements.NoError(err,
				"a concurrent review or participant merge must not fail the import")
			assertions.Len(outcome.Conflicts, test.wantConflicts)
			_, resolved := matcher.resolved[memoKey]
			assertions.True(resolved, "the stale pair is settled for this import run")
		})
	}
}

// installParticipantLinkFailure makes the real database reject participant
// link inserts until the returned release function removes the trigger.
func installParticipantLinkFailure(t *testing.T, st *store.Store) func() {
	t.Helper()
	require := require.New(t)

	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`CREATE FUNCTION fail_beeper_participant_link()
			RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'forced participant link failure';
			END;
			$$`)
		require.NoError(err, "create PostgreSQL failure function")
		_, err = st.DB().Exec(`CREATE TRIGGER fail_beeper_participant_link
			BEFORE INSERT ON participant_links
			FOR EACH ROW EXECUTE FUNCTION fail_beeper_participant_link()`)
		require.NoError(err, "create PostgreSQL failure trigger")
		release := func() {
			_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_participant_link
				ON participant_links`)
			require.NoError(err, "drop PostgreSQL failure trigger")
			_, err = st.DB().Exec(`DROP FUNCTION IF EXISTS fail_beeper_participant_link()`)
			require.NoError(err, "drop PostgreSQL failure function")
		}
		t.Cleanup(release)
		return release
	}

	_, err := st.DB().Exec(`CREATE TRIGGER fail_beeper_participant_link
		BEFORE INSERT ON participant_links
		FOR EACH ROW BEGIN
			SELECT RAISE(ABORT, 'forced participant link failure');
		END`)
	require.NoError(err, "create SQLite failure trigger")
	release := func() {
		_, err := st.DB().Exec(`DROP TRIGGER IF EXISTS fail_beeper_participant_link`)
		require.NoError(err, "drop SQLite failure trigger")
	}
	t.Cleanup(release)
	return release
}

func TestRejectedStableProviderIDCandidateIsNotRevived(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	cc := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	other, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_other:beeper.local", "Alice Example")
	require.NoError(err, "ensure other")

	// A reviewed rejection is durable. It is created before any automatic
	// application because an accepted candidate already has a retained link
	// and cannot be converted into a contradictory rejected row.
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: alice,
		RightKind: store.IdentityMatchParticipant, RightID: other,
		Basis:           store.IdentityMatchStableProviderID,
		ServiceSlug:     new("telegram"),
		NormalizedValue: new("provider:8:telegram:7:account:8:telegram:9:tg-shared"),
		State:           store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err, "UpsertIdentityMatchCandidateContext")
	rejected, err := st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateRejected, "user",
		new("different people despite the shared bridge ID"))
	require.NoError(err, "DecideIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateRejected, rejected.State)

	// The importer observes the stable ID. The rejection must hold.
	nextRun := newIdentityMatcher(st)
	nextRecorder := newObservationRecorder(st)
	captureAndMatch(t, nextRecorder, nextRun, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice",
		Raw: []byte(`{"providerID":"tg-shared"}`),
	}, cc)
	again := captureAndMatch(t, nextRecorder, nextRun, other, &User{
		ID: "@telegram_other:beeper.local", Username: "@other",
		Raw: []byte(`{"providerID":"tg-shared"}`),
	}, cc)
	assert.Empty(again.AutoResolved, "a rejected suggestion is never silently revived")

	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateRejected, reloaded.State)
}

func TestAutoResolveAcrossDurablePersonsIsRecordedAsAConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	cc := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, alice)
	require.NoError(err, "promote alice")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, bob)
	require.NoError(err, "promote bob")

	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice",
		Raw: []byte(`{"providerID":"tg-shared"}`),
	}, cc)
	outcome := captureAndMatch(t, recorder, matcher, bob, &User{
		ID: "@telegram_bob:beeper.local", Username: "@bob",
		Raw: []byte(`{"providerID":"tg-shared"}`),
	}, cc)

	assert.Empty(outcome.AutoResolved, "two curated people must never be merged automatically")
	require.Len(outcome.Conflicts, 1)
	assert.False(linked(t, st, alice, bob))

	candidate, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Conflicts[0])
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateConflict, candidate.State)
}

func TestImportReappliesAnAcceptedMatchWhoseLinkIsMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{"id": "@telegram_alice:beeper.local", "fullName": "Alice Example"},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")

	// Simulate a crash between the accept transaction and the link transaction.
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: alice,
		RightKind: store.IdentityMatchParticipant, RightID: bob,
		Basis:           store.IdentityMatchStableProviderID,
		NormalizedValue: new("provider:8:telegram:7:account:8:telegram:9:tg-shared"),
		State:           store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err, "UpsertIdentityMatchCandidateContext")
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	require.NoError(err, "DecideIdentityMatchCandidateContext")
	require.False(linked(t, st, alice, bob), "precondition: the link is missing")

	_, err = imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "Import")
	assert.True(linked(t, st, alice, bob),
		"an import re-applies an accepted match whose link never landed")
}

func TestFirstImportIdentityCandidateIncludesCurrentConversationEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{
			"id": "@telegram_alice:beeper.local", "fullName": "Alice Example",
			"username": "alice", "providerID": "native-id-one",
		},
		map[string]any{
			"id": "@telegram_alice_alt:beeper.local", "fullName": "Alice Example",
			"username": "alice", "providerID": "native-id-two",
		},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()

	summary, err := imp.Import(t.Context(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "Import")
	assert.EqualValues(1, summary.IdentityConflicts)

	candidates, err := st.ListIdentityMatchCandidatesContext(
		t.Context(), []store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0,
	)
	require.NoError(err, "list conflicting identity candidates")
	require.Len(candidates, 1)
	assert.Condition(func() bool {
		for _, evidence := range candidates[0].Evidence {
			if evidence.EvidenceKind == evidenceMembership && evidence.EvidenceRef != nil &&
				strings.HasPrefix(*evidence.EvidenceRef, "conversation:") {
				return true
			}
		}
		return false
	}, "the first import must include the conversation being imported as evidence")
}

func TestImportReportsAcceptedMatchReplayFailureWithoutAborting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{"id": "@telegram_alice:beeper.local", "fullName": "Alice Example"},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := t.Context()

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_replay_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_replay_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: alice,
			RightKind: store.IdentityMatchParticipant, RightID: bob,
			Basis:           store.IdentityMatchStableProviderID,
			NormalizedValue: new("telegram:replay-failure"),
			State:           store.IdentityMatchStateCandidate,
			Source:          store.ProvenanceArchiveObservation,
		})
	require.NoError(err, "create accepted candidate")
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	require.NoError(err, "accept candidate")

	releaseFailure := installParticipantLinkFailure(t, st)
	sum, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	releaseFailure()
	require.NoError(err, "a replay failure must not abort message archival")
	assert.EqualValues(1, sum.IdentityReplayErrors,
		"the summary must distinguish an unavailable replay store from a contested pair")
	assert.EqualValues(1, sum.Errors,
		"an accepted-link replay failure must be visible in the import diagnostics")
	assert.False(linked(t, st, alice, bob), "the failed replay must remain resumable")
}

func TestImportDoesNotReportContestedAcceptedMatchAsReplayFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := newFakeBeeper(t)
	f.addChat(observationChat("telegram", "Telegram", "@telegram_alice:beeper.local",
		map[string]any{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		map[string]any{"id": "@telegram_alice:beeper.local", "fullName": "Alice Example"},
	))
	imp, st, closeServer := newTestImporter(t, f)
	defer closeServer()
	ctx := t.Context()

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_contested_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_contested_bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, alice)
	require.NoError(err, "create Alice profile")
	_, _, err = st.CreatePersonFromParticipantContext(ctx, bob)
	require.NoError(err, "create Bob profile")
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx,
		store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: alice,
			RightKind: store.IdentityMatchParticipant, RightID: bob,
			Basis:           store.IdentityMatchStableProviderID,
			NormalizedValue: new("telegram:contested"),
			State:           store.IdentityMatchStateCandidate,
			Source:          store.ProvenanceArchiveObservation,
		})
	require.NoError(err, "create contested candidate")
	_, err = st.DecideIdentityMatchCandidateContext(
		ctx, candidate.ID, store.IdentityMatchStateAccepted, "system", nil)
	require.NoError(err, "accept candidate")

	sum, err := imp.Import(ctx, ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err, "a contested pair must not abort message archival")
	assert.Zero(sum.IdentityReplayErrors,
		"a durable-person contest is a handled decision, not an unavailable store")
	assert.Zero(sum.Errors, "a contested pair must not be reported as an import error")
	reloaded, err := st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err, "reload contested candidate")
	assert.Equal(store.IdentityMatchStateConflict, reloaded.State)
}

func TestCrossScopeUsernameBecomesAReviewCandidateNotALink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)

	// Slack's scope policy is required, so two bridge logins are two scopes.
	firstWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack"), AccountID: "slack", Network: "Slack",
	}
	secondWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack2"), AccountID: "slack2", Network: "Slack",
	}
	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	someoneElse, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack2_alice:beeper.local", "A. Example")
	require.NoError(err, "ensure the other alice")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@slack_alice:beeper.local", Username: "alice",
	}, firstWorkspace)
	outcome := captureAndMatch(t, recorder, matcher, someoneElse, &User{
		ID: "@slack2_alice:beeper.local", Username: "alice",
	}, secondWorkspace)

	assert.Empty(outcome.AutoResolved, "a username is scoped evidence, never identity proof")
	assert.Empty(outcome.Conflicts, "different scopes are different addresses, not a collision")
	require.Len(outcome.Suggested, 1)
	assert.False(linked(t, st, alice, someoneElse))

	candidate, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Suggested[0])
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchServiceScopeUsername, candidate.Basis)
	assert.Equal(store.IdentityMatchStateCandidate, candidate.State)
	require.NotNil(candidate.ServiceSlug)
	assert.Equal("slack", *candidate.ServiceSlug)
	assert.Nil(candidate.ScopeValue,
		"a cross-scope suggestion is addressed in neither scope")
	require.NotNil(candidate.NormalizedValue)
	assert.Equal("alice", *candidate.NormalizedValue)
	assert.Nil(candidate.DecidedBy, "nothing has decided it")

	require.NoError(st.RemoveSource(secondWorkspace.SourceID))
	_, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.ErrorIs(err, store.ErrIdentityMatchNotFound,
		"a cross-scope candidate requires current observations at both endpoints")
}

func TestSameScopeValueWithDifferentScopeKindsBecomesACandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	serviceSlug := "slack"
	scopeValue := "shared-scope"
	firstKind := "account"
	secondKind := "workspace"
	first, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_kind_first:beeper.local", "First")
	require.NoError(err, "ensure first participant")
	second, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_kind_second:beeper.local", "Second")
	require.NoError(err, "ensure second participant")
	input := store.ParticipantContactObservationInput{
		AddressKind:   store.ContactAddressUsername,
		ServiceSlug:   &serviceSlug,
		ScopeKind:     &firstKind,
		ScopeValue:    &scopeValue,
		OriginalValue: "same-user",
		Envelope: store.ValueEnvelopeInput{
			Source: store.ProvenanceArchiveObservation,
		},
	}
	firstResult, err := st.RecordContactObservationContext(ctx, first, input)
	require.NoError(err, "record first scoped observation")
	input.ScopeKind = &secondKind
	secondResult, err := st.RecordContactObservationContext(ctx, second, input)
	require.NoError(err, "record second scoped observation")
	require.NotNil(firstResult.Observation)
	require.NotNil(secondResult.Observation)

	matcher := newIdentityMatcher(st)
	outcome, err := matcher.match(ctx, second, secondResult)
	require.NoError(err, "match observations with different scope kinds")
	assert.Len(outcome.Suggested, 1,
		"scope identity is the (kind, value) pair, not value alone")
	assert.Empty(outcome.Conflicts)
	assert.False(linked(t, st, first, second), "a scope candidate must remain reviewable")
}

func TestCrossScopeUsernameWithOneUnscopedObservationSurvivesUnrelatedCleanup(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	service, _, err := st.EnsureCommunicationServiceContext(
		ctx, store.CommunicationServiceInput{
			Slug: "optional-scope-chat", DisplayLabel: "Optional Scope Chat",
			ScopePolicy: store.ScopePolicyOptional, DefaultScopeKind: new("workspace"),
			Normalization: store.NormalizationLower, NormalizationVersion: 1,
		},
	)
	require.NoError(err)
	unrelatedSource := newBeeperTestSource(t, st, "optional-scope-unrelated")
	supportSource := newBeeperTestSource(t, st, "optional-scope-support")
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@optional_unscoped:beeper.local", "Unscoped")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@optional_scoped:beeper.local", "Scoped")
	require.NoError(err)
	providerLeft := "optional-provider-left"
	providerRight := "optional-provider-right"
	_, err = st.RecordContactObservationContext(ctx, left, store.ParticipantContactObservationInput{
		SourceID: &supportSource, AddressKind: store.ContactAddressUsername,
		ServiceSlug: &service.Slug, ProviderUserID: &providerLeft, OriginalValue: "same-user",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	scopeKind := "workspace"
	scopeValue := "scoped-workspace"
	result, err := st.RecordContactObservationContext(ctx, right, store.ParticipantContactObservationInput{
		SourceID: &supportSource, AddressKind: store.ContactAddressUsername,
		ServiceSlug: &service.Slug, ScopeKind: &scopeKind, ScopeValue: &scopeValue,
		ProviderUserID: &providerRight, OriginalValue: "same-user",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	outcome, err := newIdentityMatcher(st).match(ctx, right, result)
	require.NoError(err)
	require.Len(outcome.Suggested, 1)

	require.NoError(st.RemoveSource(unrelatedSource))
	_, err = st.GetIdentityMatchCandidateContext(ctx, outcome.Suggested[0])
	require.NoError(err, "scoped and unscoped observations are distinct scopes")
}

func TestCrossScopeUsernameMatchesWhenUnscopedObservationArrivesSecond(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	service, _, err := st.EnsureCommunicationServiceContext(
		ctx, store.CommunicationServiceInput{
			Slug: "optional-scope-order", DisplayLabel: "Optional Scope Order",
			ScopePolicy: store.ScopePolicyOptional, DefaultScopeKind: new("workspace"),
			Normalization: store.NormalizationLower, NormalizationVersion: 1,
		},
	)
	require.NoError(err)
	first, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@scope_order_scoped:beeper.local", "Scoped")
	require.NoError(err)
	second, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@scope_order_unscoped:beeper.local", "Unscoped")
	require.NoError(err)
	scopeKind := "workspace"
	scopeValue := "first-workspace"
	firstProvider := "scope-order-first"
	_, err = st.RecordContactObservationContext(ctx, first, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: &service.Slug,
		ScopeKind: &scopeKind, ScopeValue: &scopeValue, ProviderUserID: &firstProvider,
		OriginalValue: "same-user",
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	secondProvider := "scope-order-second"
	result, err := st.RecordContactObservationContext(ctx, second, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: &service.Slug,
		ProviderUserID: &secondProvider, OriginalValue: "same-user",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)

	outcome, err := newIdentityMatcher(st).match(ctx, second, result)
	require.NoError(err)
	require.Len(outcome.Suggested, 1,
		"a null scope is distinct from a scoped observation regardless of arrival order")
}

func TestCrossScopeUsernameChecksEveryObservationForParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	service := "slack"
	other, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@all_scopes_other:beeper.local", "Other")
	require.NoError(err)
	incoming, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@all_scopes_incoming:beeper.local", "Incoming")
	require.NoError(err)
	record := func(participantID int64, scopeValue, providerID string) *store.RecordContactObservationResult {
		result, recordErr := st.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				AddressKind: store.ContactAddressUsername, ServiceSlug: &service,
				ScopeKind: new("workspace"), ScopeValue: &scopeValue,
				ProviderUserID: &providerID, OriginalValue: "same-user",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			},
		)
		require.NoError(recordErr)
		return result
	}
	record(other, "same-workspace", "other-same-scope")
	record(other, "different-workspace", "other-cross-scope")
	result := record(incoming, "same-workspace", "incoming-provider")

	outcome, err := newIdentityMatcher(st).match(ctx, incoming, result)
	require.NoError(err)
	assert.Len(outcome.Conflicts, 1, "the same-scope provider conflict remains visible")
	assert.Len(outcome.Suggested, 1,
		"a same-scope row must not hide a later qualifying row for that participant")
}

func TestCrossScopeEvidenceTracksItsExactObservationPair(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	service := "slack"
	left, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@scope_evidence_left:beeper.local", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@scope_evidence_right:beeper.local", "Right")
	require.NoError(err)
	sourceA := newBeeperTestSource(t, st, "scope-evidence-a")
	sourceB := newBeeperTestSource(t, st, "scope-evidence-b")
	sourceSame := newBeeperTestSource(t, st, "scope-evidence-same")
	sourceX := newBeeperTestSource(t, st, "scope-evidence-x")
	record := func(participantID, sourceID int64, scopeKind, scopeValue, providerID string) *store.RecordContactObservationResult {
		result, recordErr := st.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID: &sourceID, AddressKind: store.ContactAddressUsername,
				ServiceSlug: &service, ScopeKind: &scopeKind, ScopeValue: &scopeValue,
				ProviderUserID: &providerID, OriginalValue: "shared-user",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			},
		)
		require.NoError(recordErr)
		return result
	}
	record(left, sourceA, "account", "shared-scope", "provider-a")
	resultB := record(right, sourceB, "workspace", "shared-scope", "provider-b")
	firstOutcome, err := newIdentityMatcher(st).match(ctx, right, resultB)
	require.NoError(err)
	require.Len(firstOutcome.Suggested, 1)
	record(left, sourceSame, "workspace", "shared-scope", "provider-same")
	resultX := record(left, sourceX, "team", "scope-x", "provider-x")
	_, err = newIdentityMatcher(st).match(ctx, left, resultX)
	require.NoError(err)
	before, err := st.GetIdentityMatchCandidateContext(ctx, firstOutcome.Suggested[0])
	require.NoError(err)
	var staleDetail string
	for _, evidence := range before.Evidence {
		if evidence.EvidenceKind == evidenceUsername && evidence.Detail != nil &&
			strings.Contains(*evidence.Detail, "shared-scope and shared-scope") {
			staleDetail = *evidence.Detail
		}
	}
	require.NotEmpty(staleDetail)

	require.NoError(st.RemoveSource(sourceA))
	candidate, err := st.GetIdentityMatchCandidateContext(ctx, firstOutcome.Suggested[0])
	require.NoError(err, "the alternate scope-x/scope-b pair still supports the candidate")
	var details []string
	for _, evidence := range candidate.Evidence {
		if evidence.EvidenceKind == evidenceUsername && evidence.Detail != nil {
			details = append(details, *evidence.Detail)
		}
	}
	assert.NotContains(details, staleDetail)
	assert.Condition(func() bool {
		for _, detail := range details {
			if strings.Contains(detail, "scope-x") && strings.Contains(detail, "shared-scope") {
				return true
			}
		}
		return false
	}, "the current scope-x/shared-scope explanation must remain")
}

func TestSameUsernameOnDifferentServicesIsNeverMatched(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	stranger, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@x_alice:beeper.local", "Someone Else")
	require.NoError(err, "ensure stranger")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice",
	}, captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	})
	outcome := captureAndMatch(t, recorder, matcher, stranger, &User{
		ID: "@x_alice:beeper.local", Username: "@alice",
	}, captureContext{
		SourceID: newBeeperTestSource(t, st, "x"), AccountID: "x", Network: "X",
	})

	assert.Empty(outcome.AutoResolved)
	assert.Empty(outcome.Suggested, "the same handle on two services is two different addresses")
	assert.Empty(outcome.Conflicts)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 100, 0)
	require.NoError(err, "ListIdentityMatchCandidatesContext")
	assert.Empty(candidates)
}

func TestPhoneEmailNameAndMembershipOnlyAddEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)

	firstWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack"), AccountID: "slack", Network: "Slack",
	}
	secondWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack2"), AccountID: "slack2", Network: "Slack",
	}
	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack2_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure the second alice")

	// Both sightings carry the same phone and email, share a conversation, and
	// share a display name. None of that may confirm the username link.
	conversationSource, err := st.GetOrCreateSource(sourceTypeBeeper, "slack")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(
		conversationSource.ID, "!shared:beeper.local", "group_chat", "Shared")
	require.NoError(err, "EnsureConversationWithType")
	require.NoError(st.EnsureConversationParticipant(convID, alice, "member"), "add alice")
	require.NoError(st.EnsureConversationParticipant(convID, alsoAlice, "member"), "add the second alice")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@slack_alice:beeper.local", Username: "alice",
		PhoneNumber: "+12025550123", Email: "alice@example.com",
	}, firstWorkspace)
	outcome := captureAndMatch(t, recorder, matcher, alsoAlice, &User{
		ID: "@slack2_alice:beeper.local", Username: "alice",
		PhoneNumber: "+12025550123", Email: "Alice@Example.com",
	}, secondWorkspace)

	require.NotEmpty(outcome.Suggested)
	assert.Empty(outcome.AutoResolved,
		"correlated phone, email, name, and co-membership must not confirm a username link")
	assert.False(linked(t, st, alice, alsoAlice))

	candidate, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Suggested[0])
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateCandidate, candidate.State)

	kinds := map[string]bool{}
	for _, evidence := range candidate.Evidence {
		kinds[evidence.EvidenceKind] = true
	}
	assert.True(kinds[evidenceUsername], "the username match itself is evidence")
	assert.True(kinds[evidencePhone], "a matching phone is evidence")
	assert.True(kinds[evidenceEmail], "a matching email is evidence")
	assert.True(kinds[evidenceName], "a matching display name is evidence")
	assert.True(kinds[evidenceMembership], "a shared conversation is evidence")
}

func TestEvidenceIsNotDuplicatedAcrossRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()

	firstWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack"), AccountID: "slack", Network: "Slack",
	}
	secondWorkspace := captureContext{
		SourceID: newBeeperTestSource(t, st, "slack2"), AccountID: "slack2", Network: "Slack",
	}
	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@slack2_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure the second alice")

	first := newObservationRecorder(st)
	firstMatcher := newIdentityMatcher(st)
	captureAndMatch(t, first, firstMatcher, alice, &User{
		ID: "@slack_alice:beeper.local", Username: "alice", PhoneNumber: "+12025550123",
	}, firstWorkspace)
	outcome := captureAndMatch(t, first, firstMatcher, alsoAlice, &User{
		ID: "@slack2_alice:beeper.local", Username: "alice", PhoneNumber: "+12025550123",
	}, secondWorkspace)
	require.NotEmpty(outcome.Suggested)
	before, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Suggested[0])
	require.NoError(err, "first read")

	// A later run, fresh caches: the store must converge the same evidence row.
	second := newObservationRecorder(st)
	secondMatcher := newIdentityMatcher(st)
	captureAndMatch(t, second, secondMatcher, alsoAlice, &User{
		ID: "@slack2_alice:beeper.local", Username: "alice", PhoneNumber: "+12025550123",
	}, secondWorkspace)

	after, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Suggested[0])
	require.NoError(err, "second read")
	assert.Len(after.Evidence, len(before.Evidence))
}

func TestConversationMembershipEvidenceTracksConversationSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	firstWorkspace := captureContext{
		SourceID:  newBeeperTestSource(t, st, "membership-slack"),
		AccountID: "membership-slack", Network: "Slack",
	}
	secondWorkspace := captureContext{
		SourceID:  newBeeperTestSource(t, st, "membership-slack2"),
		AccountID: "membership-slack2", Network: "Slack",
	}
	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@membership_alice:beeper.local", "Alice")
	require.NoError(err)
	alsoAlice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@membership_alice2:beeper.local", "Alice")
	require.NoError(err)

	type sharedConversation struct {
		sourceID, conversationID int64
		ref                      string
	}
	conversationSources := make(map[string]int64)
	addSharedConversation := func(accountID, sourceConversationID string) sharedConversation {
		source, sourceErr := st.GetOrCreateSource(sourceTypeBeeper, accountID)
		require.NoError(sourceErr)
		conversationID, conversationErr := st.EnsureConversationWithType(
			source.ID, sourceConversationID, "group_chat", "Shared",
		)
		require.NoError(conversationErr)
		require.NoError(st.EnsureConversationParticipant(conversationID, alice, "member"))
		require.NoError(st.EnsureConversationParticipant(conversationID, alsoAlice, "member"))
		conversation := sharedConversation{
			sourceID: source.ID, conversationID: conversationID,
			ref: "conversation:" + strconv.FormatInt(conversationID, 10),
		}
		conversationSources[conversation.ref] = conversation.sourceID
		return conversation
	}
	firstConversation := addSharedConversation("membership-conversation-1", "!shared-1")
	secondConversation := addSharedConversation("membership-conversation-2", "!shared-2")

	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)
	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@membership_alice:beeper.local", Username: "alice",
	}, firstWorkspace)
	outcome := captureAndMatch(t, recorder, matcher, alsoAlice, &User{
		ID: "@membership_alice2:beeper.local", Username: "alice",
	}, secondWorkspace)
	require.NotEmpty(outcome.Suggested)
	candidateID := outcome.Suggested[0]

	membershipRefs := func() []string {
		candidate, candidateErr := st.GetIdentityMatchCandidateContext(ctx, candidateID)
		require.NoError(candidateErr)
		refs := make([]string, 0)
		for _, evidence := range candidate.Evidence {
			if evidence.EvidenceKind != evidenceMembership {
				continue
			}
			require.NotNil(evidence.EvidenceRef)
			refs = append(refs, *evidence.EvidenceRef)
			var sourceID int64
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT source_id FROM identity_match_evidence_sources
				 WHERE evidence_id = ?`), evidence.ID,
			).Scan(&sourceID))
			assert.Equal(conversationSources[*evidence.EvidenceRef], sourceID)
		}
		slices.Sort(refs)
		return refs
	}
	assert.Equal(
		[]string{firstConversation.ref, secondConversation.ref}, membershipRefs(),
	)

	thirdConversation := addSharedConversation("membership-conversation-3", "!shared-3")
	secondRecorder := newObservationRecorder(st)
	secondMatcher := newIdentityMatcher(st)
	captureAndMatch(t, secondRecorder, secondMatcher, alsoAlice, &User{
		ID: "@membership_alice2:beeper.local", Username: "alice",
	}, secondWorkspace)
	wantAll := []string{firstConversation.ref, secondConversation.ref, thirdConversation.ref}
	slices.Sort(wantAll)
	assert.Equal(wantAll, membershipRefs(),
		"a count change must add only the new immutable conversation evidence")

	require.NoError(st.RemoveSource(firstConversation.sourceID))
	wantRemaining := []string{secondConversation.ref, thirdConversation.ref}
	slices.Sort(wantRemaining)
	assert.Equal(wantRemaining, membershipRefs(),
		"removing a conversation source must remove only its evidence")
}

func TestConflictCandidateGainsEvidenceButNeverAccepts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	cc := captureContext{
		SourceID: newBeeperTestSource(t, st, "telegram"), AccountID: "telegram", Network: "Telegram",
	}
	recorder := newObservationRecorder(st)
	matcher := newIdentityMatcher(st)

	alice, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	impostor, err := st.EnsureParticipantByIdentifier(
		participantIdentifierType, "@telegram_impostor:beeper.local", "Alice Example")
	require.NoError(err, "ensure the impostor")

	captureAndMatch(t, recorder, matcher, alice, &User{
		ID: "@telegram_alice:beeper.local", Username: "@alice", PhoneNumber: "+12025550123",
	}, cc)
	outcome := captureAndMatch(t, recorder, matcher, impostor, &User{
		ID: "@telegram_impostor:beeper.local", Username: "@alice", PhoneNumber: "+12025550123",
	}, cc)

	require.NotEmpty(outcome.Conflicts)
	assert.Empty(outcome.AutoResolved)
	assert.False(linked(t, st, alice, impostor))

	candidate, err := st.GetIdentityMatchCandidateContext(ctx, outcome.Conflicts[0])
	require.NoError(err, "GetIdentityMatchCandidateContext")
	assert.Equal(store.IdentityMatchStateConflict, candidate.State)
	assert.NotEmpty(candidate.Evidence,
		"a conflict must be explainable, not just flagged")
}

func TestThreeParticipantCollisionExplainsAndReportsEveryConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	matcher := newIdentityMatcher(st)
	participants := make([]int64, 0, 3)
	for _, identifier := range []string{
		"@collision-a:beeper.local",
		"@collision-b:beeper.local",
		"@collision-c:beeper.local",
	} {
		participantID, err := st.EnsureParticipantByIdentifier(
			participantIdentifierType, identifier, "Collision User",
		)
		require.NoError(err)
		participants = append(participants, participantID)
	}

	conflictCount := 0
	for index, participantID := range participants {
		providerID := "provider-" + strconv.Itoa(index)
		result, err := st.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				AddressKind: store.ContactAddressUsername,
				ServiceSlug: new("x"), ProviderUserID: &providerID,
				OriginalValue: "@shared",
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(err)
		if index == len(participants)-1 {
			assert.Len(result.CandidateIDs, 2,
				"the third participant must expose both newly created conflict edges")
		}
		outcome, err := matcher.match(ctx, participantID, result)
		require.NoError(err)
		conflictCount += len(outcome.Conflicts)
	}

	assert.Equal(3, conflictCount,
		"a three-participant collision must report every edge in the conflict graph")
	candidates, err := st.ListIdentityMatchCandidatesContext(
		ctx, []store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0,
	)
	require.NoError(err)
	require.Len(candidates, 3)
	for _, candidate := range candidates {
		require.Len(candidate.Evidence, 1,
			"every reported conflict must carry its matching explanation")
		assert.Equal(evidenceName, candidate.Evidence[0].EvidenceKind)
	}
}
