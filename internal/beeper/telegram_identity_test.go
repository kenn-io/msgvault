package beeper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLinkTelegramUser(t *testing.T) {
	for _, exportFirst := range []bool{false, true} {
		name := "beeper first"
		if exportFirst {
			name = "export first"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			ctx := t.Context()
			export, err := st.GetOrCreateSource("telegram", "user1")
			require.NoError(err)
			participant, err := st.EnsureParticipantByIdentifier("telegram", "user482111", "Alice Example")
			require.NoError(err)
			bridge, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "account-a-user", "Alice Example")
			require.NoError(err)
			cc := captureContext{SourceID: newBeeperTestSource(t, st, "account-a"), AccountID: "account-a", Network: "Telegram"}
			user := &User{ID: "@telegram_482111:beeper.local"}
			if exportFirst {
				linkErr := LinkTelegramUser(ctx, st, export.ID, participant, "482111")
				require.NoError(linkErr)
			}
			captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), bridge, user, cc)
			if !exportFirst {
				linkErr := LinkTelegramUser(ctx, st, export.ID, participant, "482111")
				require.NoError(linkErr)
			}
			assert.True(linked(t, st, participant, bridge))

			secondBridge, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "account-b-user", "Alice Example")
			require.NoError(err)
			captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), secondBridge, user,
				captureContext{SourceID: newBeeperTestSource(t, st, "account-b"), AccountID: "account-b", Network: "Telegram"})
			members, err := st.ClusterMembers(participant)
			require.NoError(err)
			assert.ElementsMatch([]int64{participant, bridge, secondBridge}, members)

			require.NoError(st.RemoveSource(export.ID))
			assert.False(linked(t, st, participant, bridge))
			assert.True(linked(t, st, bridge, secondBridge))
		})
	}
}

func TestTelegramSharedKeyPreservesUsernameRename(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "bridge-user", "Alice Example")
	require.NoError(err)
	cc := captureContext{SourceID: newBeeperTestSource(t, st, "account-a"), AccountID: "account-a", Network: "Telegram"}
	oldKey := "provider:8:telegram:7:account:9:account-a:6:482111"
	old, err := st.RecordContactObservationContext(ctx, participant, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("telegram"),
		OriginalValue: "alice", ProviderUserID: &oldKey, SourceID: &cc.SourceID,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), participant,
		&User{ID: "@telegram_482111:beeper.local", Username: "alice2", Raw: []byte(`{"providerID":"482111"}`)}, cc)
	observations, err := st.ListParticipantObservationsContext(ctx, participant, false)
	require.NoError(err)
	require.Len(observations, 3)
	for _, observation := range observations {
		switch observation.OriginalValue {
		case "alice":
			assert.Equal(old.Observation.Envelope.ID, observation.Envelope.ID)
			assert.NotNil(observation.Envelope.SupersededAt)
		case "alice2":
			assert.Nil(observation.Envelope.SupersededAt)
			assert.Equal(oldKey, *observation.ProviderUserID)
		default:
			assert.Equal("provider:8:telegram:7:service:0::6:482111", *observation.ProviderUserID)
		}
	}
}

func TestTelegramNumericIDSyncPreservesUnlink(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	matcher := newIdentityMatcher(st)
	var participants []int64
	var accounts []captureContext
	var outcome matchOutcome

	// Before shared keys, two logins expose the same username with different account keys.
	for _, account := range []string{"account-a", "account-b"} {
		participant, err := st.EnsureParticipantByIdentifier(participantIdentifierType, account+"-user", "Alice Example")
		require.NoError(err)
		cc := captureContext{SourceID: newBeeperTestSource(t, st, account), AccountID: account, Network: "Telegram"}
		oldKey := "provider:8:telegram:7:account:9:" + account + ":6:482111"
		result, err := st.RecordContactObservationContext(ctx, participant, store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("telegram"),
			OriginalValue: "alice", ProviderUserID: &oldKey, SourceID: &cc.SourceID,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		})
		require.NoError(err)
		outcome, err = matcher.match(ctx, participant, result)
		require.NoError(err)
		participants = append(participants, participant)
		accounts = append(accounts, cc)
	}
	require.Len(outcome.Conflicts, 1)
	candidateID := outcome.Conflicts[0]
	_, _, err := st.AcceptIdentityMatchCandidateContext(ctx, candidateID, "user", nil)
	require.NoError(err)
	require.True(linked(t, st, participants[0], participants[1]))
	_, err = st.UnlinkParticipants(participants[0], participants[1])
	require.NoError(err)
	require.False(linked(t, st, participants[0], participants[1]))
	candidate, err := st.GetIdentityMatchCandidateContext(ctx, candidateID)
	require.NoError(err)
	require.Equal(store.IdentityMatchStateRejected, candidate.State)

	// A later sync introduces the shared numeric key for both logins.
	recorder := newObservationRecorder(st)
	matcher = newIdentityMatcher(st)
	for i, participant := range participants {
		captureAndMatch(t, recorder, matcher, participant,
			&User{ID: "@telegram_482111:beeper.local", Username: "alice", Raw: []byte(`{"providerID":"482111"}`)}, accounts[i])
	}
	assert.False(linked(t, st, participants[0], participants[1]), "sync must preserve the user's unlink decision")
}
