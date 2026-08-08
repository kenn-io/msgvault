package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestObservationsAttachManyAddressesToOneParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:example.org", "Alice Example",
	)
	require.NoError(err)
	inputs := []store.ParticipantContactObservationInput{
		{AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"),
			ProviderUserID: new("wa-1"), OriginalValue: "+1 202 555 0123",
			Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation}},
		{AddressKind: store.ContactAddressEmail, ServiceSlug: new("google-chat"),
			ProviderUserID: new("wa-1"), OriginalValue: "Alice@Example.com",
			Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation}},
		{AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"),
			ProviderUserID: new("wa-1"), OriginalValue: "Alice",
			Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation}},
	}
	for _, input := range inputs {
		result, err := st.RecordContactObservationContext(ctx, participantID, input)
		require.NoError(err)
		assert.True(result.Created)
		assert.False(result.Conflicting)
	}
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err)
	assert.Len(observations, 3)
}

func TestRecordingTheSameObservationTwiceIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:example.org", "Alice Example",
	)
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		ProviderUserID: new("x-1"), OriginalValue: "@alice",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	}
	first, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	second, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	assert.False(second.Created)
	assert.Equal(first.Observation.Envelope.ID, second.Observation.Envelope.ID)
}

func TestDuplicateUsernameUnderDifferentStableIDsBecomesAConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		OriginalValue: "@shared",
		Envelope:      store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	}
	input.ProviderUserID = new("x-left")
	first, err := st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	assert.False(first.Conflicting)
	input.ProviderUserID = new("x-right")
	second, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	assert.True(second.Conflicting)
	assert.NotNil(second.CandidateID)
	found, err := st.FindObservationsByAddressContext(ctx, store.ContactPointQuery{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		NormalizedValue: "shared",
	})
	require.NoError(err)
	assert.Len(found, 2)
	candidates, err := st.ListIdentityMatchCandidatesContext(
		ctx, []store.IdentityMatchState{store.IdentityMatchStateConflict}, 10, 0,
	)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(store.IdentityMatchServiceScopeUsername, candidates[0].Basis)
}

func TestSameUsernameOnDifferentScopesIsNotAConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	left, err := st.EnsureParticipantByIdentifier("beeper", "@alice:example.org", "Alice Example")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("beeper", "@bob:example.org", "Bob Example")
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
		ScopeKind: new("workspace"), OriginalValue: "alice",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	}
	input.ScopeValue, input.ProviderUserID = new("T0EXAMPLE"), new("slack-left")
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ScopeValue, input.ProviderUserID = new("T0OTHER"), new("slack-right")
	result, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	assert.False(result.Conflicting)
}

func TestRenameSupersedesWithoutMovingHistoryBetweenParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:example.org", "Alice Example",
	)
	require.NoError(err)
	old, err := st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		ProviderUserID: new("x-1"), OriginalValue: "@alice_old",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, participantID, old.Observation.Envelope.ID, nil,
	))
	_, err = st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		ProviderUserID: new("x-1"), OriginalValue: "@alice_new",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err)
	require.Len(current, 1)
	assert.Equal("alice_new", current[0].NormalizedValue)
	all, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err)
	assert.Len(all, 2)
}
