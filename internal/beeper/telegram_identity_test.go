package beeper

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLinkTelegramUser(t *testing.T) {
	for _, username := range []string{"", "alice"} {
		for _, exportFirst := range []bool{false, true} {
			name := "beeper first"
			if exportFirst {
				name = "export first"
			}
			t.Run(name+"/"+username, func(t *testing.T) {
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
				user := &User{ID: "@telegram_482111:beeper.local", Username: username}
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

				otherNetwork, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "discord-user", "Other Example")
				require.NoError(err)
				captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), otherNetwork,
					&User{ID: "@discord_482111:beeper.local", Raw: []byte(`{"providerID":"482111"}`)},
					captureContext{SourceID: newBeeperTestSource(t, st, "discord"), AccountID: "discord", Network: "Discord"})
				assert.False(linked(t, st, participant, otherNetwork))

				secondBridge, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "account-b-user", "Alice Example")
				require.NoError(err)
				captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), secondBridge, user,
					captureContext{SourceID: newBeeperTestSource(t, st, "account-b"), AccountID: "account-b", Network: "Telegram"})
				members, err := st.ClusterMembers(participant)
				require.NoError(err)
				assert.ElementsMatch([]int64{participant, bridge, secondBridge}, members)

				unrelated, err := st.GetOrCreateSource("telegram", "user99")
				require.NoError(err)
				require.NoError(st.RemoveSource(unrelated.ID))
				assert.True(linked(t, st, participant, bridge))
				require.NoError(st.RemoveSource(export.ID))
				assert.False(linked(t, st, participant, bridge))
				assert.True(linked(t, st, bridge, secondBridge))
			})
		}
	}
}

func TestTelegramRejectedCandidateStaysRejected(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := t.Context()
	export, err := st.GetOrCreateSource("telegram", "user1")
	require.NoError(err)
	participant, err := st.EnsureParticipantByIdentifier("telegram", "user482111", "Alice Example")
	require.NoError(err)
	bridge, err := st.EnsureParticipantByIdentifier(participantIdentifierType, "bridge-user", "Alice Example")
	require.NoError(err)
	key := "provider:8:telegram:7:service:0::6:482111"
	candidate, _, err := st.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: participant,
		RightKind: store.IdentityMatchParticipant, RightID: bridge,
		Basis: store.IdentityMatchStableProviderID, ServiceSlug: new("telegram"),
		NormalizedValue: &key, State: store.IdentityMatchStateCandidate,
		Source: store.ProvenanceArchiveObservation,
	})
	require.NoError(err)
	_, err = st.DecideIdentityMatchCandidateContext(ctx, candidate.ID, store.IdentityMatchStateRejected, "user", nil)
	require.NoError(err)
	cc := captureContext{SourceID: newBeeperTestSource(t, st, "account-a"), AccountID: "account-a", Network: "Telegram"}
	for range 2 {
		require.NoError(LinkTelegramUser(ctx, st, export.ID, participant, "482111"))
		captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), bridge, &User{ID: "@telegram_482111:beeper.local"}, cc)
		assert.False(linked(t, st, participant, bridge))
	}
	candidate, err = st.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	require.NoError(err)
	assert.Equal(store.IdentityMatchStateRejected, candidate.State)
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

func TestLinkTelegramUserRejectsNonDigits(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user1")
	require.NoError(err)
	participant, err := st.EnsureParticipantByIdentifier("telegram", "user2", "Alice Example")
	require.NoError(err)
	for _, id := range []string{"", "channel2", "-2", "2:server", "２"} {
		require.NoError(LinkTelegramUser(t.Context(), st, source.ID, participant, id))
	}
	observations, err := st.ListParticipantObservationsContext(t.Context(), participant, false)
	require.NoError(err)
	assert.Empty(t, observations)
}
