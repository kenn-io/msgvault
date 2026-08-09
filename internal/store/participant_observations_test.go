package store_test

import (
	"context"
	"fmt"
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
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}},
		{AddressKind: store.ContactAddressEmail, ServiceSlug: new("google-chat"),
			ProviderUserID: new("wa-1"), OriginalValue: "Alice@Example.com",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}},
		{AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"),
			ProviderUserID: new("wa-1"), OriginalValue: "Alice",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation}},
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
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	first, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	second, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	assert.False(second.Created)
	assert.Equal(first.Observation.Envelope.ID, second.Observation.Envelope.ID)
}

func TestContactObservationOrdinalsPreserveExplicitAndAppendMissingValues(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@ordered:example.org", "Ordered Example",
	)
	require.NoError(err)
	explicitOrdinal := 7
	explicit, err := st.RecordContactObservationContext(
		t.Context(), participantID, store.ParticipantContactObservationInput{
			AddressKind:   store.ContactAddressEmail,
			OriginalValue: "ordered@example.org",
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceArchiveObservation, Ordinal: &explicitOrdinal,
			},
		},
	)
	require.NoError(err)
	assert.Equal(explicitOrdinal, explicit.Observation.Envelope.Ordinal)

	appendedEmail, err := st.RecordContactObservationContext(
		t.Context(), participantID, store.ParticipantContactObservationInput{
			AddressKind:   store.ContactAddressEmail,
			OriginalValue: "ordered.next@example.org",
			Envelope: store.ValueEnvelopeInput{
				Source: store.ProvenanceArchiveObservation,
			},
		},
	)
	require.NoError(err)
	assert.Equal(explicitOrdinal+1, appendedEmail.Observation.Envelope.Ordinal)

	for index, phone := range []string{"+12025550101", "+12025550102"} {
		result, err := st.RecordContactObservationContext(
			t.Context(), participantID, store.ParticipantContactObservationInput{
				AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"),
				OriginalValue: phone,
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(err)
		assert.Equal(index, result.Observation.Envelope.Ordinal)
	}
}

func TestRecordingTheSameObservationFromTwoSourcesKeepsBothProvenances(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	ctx := t.Context()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@multi-source:example.org", "Multi Source",
	)
	require.NoError(err)
	otherSource, err := st.GetOrCreateSource("gmail", "other-source@example.org")
	require.NoError(err)

	var ids []int64
	for _, sourceID := range []int64{f.Source.ID, otherSource.ID} {
		result, err := st.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID: &sourceID, AddressKind: store.ContactAddressEmail,
				OriginalValue: "shared@example.org",
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(err)
		assert.True(result.Created, "source %d", sourceID)
		ids = append(ids, result.Observation.Envelope.ID)
	}
	assert.NotEqual(ids[0], ids[1])

	require.NoError(st.RemoveSource(f.Source.ID))
	observations, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err)
	require.Len(observations, 1)
	require.NotNil(observations[0].SourceID)
	assert.Equal(otherSource.ID, *observations[0].SourceID)
}

func TestMergeParticipantsPreservesContactObservations(t *testing.T) {
	t.Run("repoints current and historical rows", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := storetest.New(t).Store
		ctx := context.Background()
		absorbed, err := st.EnsureParticipantByIdentifier(
			"beeper", "@absorbed:example.org", "Absorbed",
		)
		require.NoError(err)
		survivor, err := st.EnsureParticipantByIdentifier(
			"beeper", "@survivor:example.org", "Survivor",
		)
		require.NoError(err)

		_, err = st.RecordContactObservationContext(ctx, absorbed,
			store.ParticipantContactObservationInput{
				AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"),
				ProviderUserID: new("wa-absorbed"), OriginalValue: "+1 202 555 0142",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			})
		require.NoError(err)
		historical, err := st.RecordContactObservationContext(ctx, absorbed,
			store.ParticipantContactObservationInput{
				AddressKind: store.ContactAddressEmail, OriginalValue: "old@example.com",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			})
		require.NoError(err)
		require.NoError(st.SupersedeParticipantObservationContext(
			ctx, absorbed, historical.Observation.Envelope.ID, nil,
		))

		require.NoError(st.MergeParticipants(absorbed, survivor))
		current, err := st.ListParticipantObservationsContext(ctx, survivor, true)
		require.NoError(err)
		require.Len(current, 1)
		assert.Equal("+12025550142", current[0].NormalizedValue)
		all, err := st.ListParticipantObservationsContext(ctx, survivor, false)
		require.NoError(err)
		assert.Len(all, 2)
	})

	t.Run("deduplicates current rows and retains stable provider ID", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := storetest.New(t).Store
		ctx := context.Background()
		absorbed, err := st.EnsureParticipantByIdentifier(
			"beeper", "@absorbed:example.org", "Absorbed",
		)
		require.NoError(err)
		survivor, err := st.EnsureParticipantByIdentifier(
			"beeper", "@survivor:example.org", "Survivor",
		)
		require.NoError(err)

		input := store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
			OriginalValue: "@Shared",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		}
		surviving, err := st.RecordContactObservationContext(ctx, survivor, input)
		require.NoError(err)
		input.OriginalValue = "shared"
		input.ProviderUserID = new("x-stable-user")
		absorbedResult, err := st.RecordContactObservationContext(ctx, absorbed, input)
		require.NoError(err)

		require.NoError(st.MergeParticipants(absorbed, survivor))
		current, err := st.ListParticipantObservationsContext(ctx, survivor, true)
		require.NoError(err)
		require.Len(current, 1)
		require.NotNil(current[0].ProviderUserID)
		assert.Equal("x-stable-user", *current[0].ProviderUserID)
		assert.Equal(surviving.Observation.Envelope.ID, current[0].Envelope.ID)

		all, err := st.ListParticipantObservationsContext(ctx, survivor, false)
		require.NoError(err)
		require.Len(all, 2)
		var historical *store.ParticipantContactObservation
		for index := range all {
			if all[index].Envelope.ID == absorbedResult.Observation.Envelope.ID {
				historical = &all[index]
			}
		}
		require.NotNil(historical)
		assert.Equal("shared", historical.OriginalValue)
		assert.NotNil(historical.Envelope.ActiveUntil)
		assert.NotNil(historical.Envelope.SupersededAt)
	})

	t.Run("keeps equal values from distinct sources", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := storetest.New(t)
		st := f.Store
		ctx := t.Context()
		absorbed := f.EnsureParticipant("absorbed@example.com", "Absorbed", "example.com")
		survivor := f.EnsureParticipant("survivor@example.com", "Survivor", "example.com")
		otherSource, err := st.GetOrCreateSource("gmail", "merge-source@example.org")
		require.NoError(err)

		for participantID, sourceID := range map[int64]int64{
			absorbed: otherSource.ID,
			survivor: f.Source.ID,
		} {
			_, err := st.RecordContactObservationContext(
				ctx, participantID, store.ParticipantContactObservationInput{
					SourceID: &sourceID, AddressKind: store.ContactAddressEmail,
					OriginalValue: "shared@example.org",
					Envelope: store.ValueEnvelopeInput{
						Source: store.ProvenanceArchiveObservation,
					},
				},
			)
			require.NoError(err)
		}

		require.NoError(st.MergeParticipants(absorbed, survivor))
		observations, err := st.ListParticipantObservationsContext(ctx, survivor, true)
		require.NoError(err)
		require.Len(observations, 2)
		assert.ElementsMatch(
			[]int64{f.Source.ID, otherSource.ID},
			[]int64{*observations[0].SourceID, *observations[1].SourceID},
		)
	})
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
		Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
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

func TestProviderIDEnrichmentRemovesGeneratedConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right", "Right")
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.org",
		ProviderUserID: new("provider-stable"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = nil
	conflicting, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(conflicting.Conflicting)

	input.ProviderUserID = new("provider-stable")
	enriched, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	assert.False(enriched.Created)
	require.NotNil(enriched.Observation.ProviderUserID)
	assert.Equal("provider-stable", *enriched.Observation.ProviderUserID)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	assert.Empty(candidates)
}

func TestContradictoryProviderIDSupersedesCurrentObservation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:example.org", "Alice Example",
	)
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		ProviderUserID: new("x-old"), OriginalValue: "@alice",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	first, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	input.ProviderUserID = new("x-new")
	second, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err)
	assert.True(second.Created, "a contradictory provider ID must record a new observation")
	assert.NotEqual(first.Observation.Envelope.ID, second.Observation.Envelope.ID)

	current, err := st.ListParticipantObservationsContext(ctx, participantID, true)
	require.NoError(err)
	require.Len(current, 1)
	require.NotNil(current[0].ProviderUserID)
	assert.Equal("x-new", *current[0].ProviderUserID)

	all, err := st.ListParticipantObservationsContext(ctx, participantID, false)
	require.NoError(err)
	require.Len(all, 2, "the contradicted observation must be retained as history")
	var historical *store.ParticipantContactObservation
	for index := range all {
		if all[index].Envelope.ID == first.Observation.Envelope.ID {
			historical = &all[index]
		}
	}
	require.NotNil(historical)
	assert.NotNil(historical.Envelope.ActiveUntil)
	assert.NotNil(historical.Envelope.SupersededAt)
	require.NotNil(historical.ProviderUserID)
	assert.Equal("x-old", *historical.ProviderUserID)
}

func TestProviderIDChangeRemovesStaleGeneratedConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-change", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-change", "Right")
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.org",
		ProviderUserID: new("provider-left"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	conflicting, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(conflicting.Conflicting)

	input.ProviderUserID = new("provider-left")
	converged, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	assert.False(converged.Conflicting)

	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	assert.Empty(candidates,
		"a generated conflict must not survive provider ID convergence")
}

func TestMergeParticipantsRemovesConflictAfterProviderIDConvergence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	left := f.EnsureParticipant("left@example.org", "Left", "example.org")
	survivor := f.EnsureParticipant("survivor@example.org", "Survivor", "example.org")
	absorbed := f.EnsureParticipant("absorbed@example.org", "Absorbed", "example.org")
	otherSource, err := f.Store.GetOrCreateSource("gmail", "merge-convergence@example.org")
	require.NoError(err)

	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: "shared@example.org",
		ProviderUserID: new("provider-stable"), SourceID: &f.Source.ID,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = f.Store.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.SourceID = &otherSource.ID
	input.ProviderUserID = nil
	_, err = f.Store.RecordContactObservationContext(ctx, survivor, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-stable")
	_, err = f.Store.RecordContactObservationContext(ctx, absorbed, input)
	require.NoError(err)

	candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 2)

	require.NoError(f.Store.MergeParticipants(absorbed, survivor))
	candidates, err = f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	assert.Empty(candidates)
}

func TestCrossKindObservationPairDoesNotSupportGeneratedConflict(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "left-kind", "Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "right-kind", "Right")
	require.NoError(err)
	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, OriginalValue: "alice",
		ProviderUserID: new("provider-left"),
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	_, err = st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	conflicting, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(conflicting.Conflicting)

	input.AddressKind = store.ContactAddressSocial
	social, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.False(social.Conflicting,
		"generation must not pair a social handle with a username")

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, right, conflicting.Observation.Envelope.ID, nil,
	))
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	assert.Empty(candidates,
		"a username conflict must not stay supported by a cross-kind social pair")
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
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
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
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	})
	require.NoError(err)
	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, participantID, old.Observation.Envelope.ID, nil,
	))
	_, err = st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		ProviderUserID: new("x-1"), OriginalValue: "@alice_new",
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
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

func TestSupersedeParticipantObservationRecomputesGeneratedConflicts(t *testing.T) {
	t.Run("removes conflict after the last endpoint support is superseded", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := storetest.New(t).Store
		ctx := t.Context()
		left, err := st.EnsureParticipantByIdentifier("example", "left", "Left")
		require.NoError(err)
		right, err := st.EnsureParticipantByIdentifier("example", "right", "Right")
		require.NoError(err)

		input := store.ParticipantContactObservationInput{
			AddressKind: store.ContactAddressSocial, OriginalValue: "social:shared",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		}
		input.ProviderUserID = new("provider-left")
		leftResult, err := st.RecordContactObservationContext(ctx, left, input)
		require.NoError(err)
		input.ProviderUserID = new("provider-right")
		rightResult, err := st.RecordContactObservationContext(ctx, right, input)
		require.NoError(err)
		require.True(rightResult.Conflicting)

		require.NoError(st.SupersedeParticipantObservationContext(
			ctx, left, leftResult.Observation.Envelope.ID, nil,
		))
		candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
		require.NoError(err)
		assert.Empty(candidates)
	})

	t.Run("keeps conflict while another current observation supports the endpoint", func(t *testing.T) {
		require := require.New(t)
		fixture := storetest.New(t)
		st := fixture.Store
		ctx := t.Context()
		left, err := st.EnsureParticipantByIdentifier("example", "left", "Left")
		require.NoError(err)
		right, err := st.EnsureParticipantByIdentifier("example", "right", "Right")
		require.NoError(err)

		input := store.ParticipantContactObservationInput{
			AddressKind:   store.ContactAddressURL,
			OriginalValue: "https://example.org/shared",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		}
		var leftObservationID int64
		for _, participantID := range []int64{left, left, right} {
			result, recordErr := st.RecordContactObservationContext(ctx, participantID, input)
			require.NoError(recordErr)
			if participantID == left && leftObservationID == 0 {
				leftObservationID = result.Observation.Envelope.ID
				input.SourceID = &fixture.Source.ID
			}
		}

		require.NoError(st.SupersedeParticipantObservationContext(
			ctx, left, leftObservationID, nil,
		))
		candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
		require.NoError(err)
		require.Len(candidates, 1)
	})
}

func TestSupersedeParticipantObservationDemotesPromotedCandidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	left, err := st.EnsureParticipantByIdentifier("example", "promoted-left", "Promoted Left")
	require.NoError(err)
	right, err := st.EnsureParticipantByIdentifier("example", "promoted-right", "Promoted Right")
	require.NoError(err)
	normalized := "shared@example.org"
	sourceRef := "manual-review-import"
	notes := "keep this review context"
	candidate, created, err := st.UpsertIdentityMatchCandidateContext(
		ctx, store.IdentityMatchCandidateInput{
			LeftKind: store.IdentityMatchParticipant, LeftID: left,
			RightKind: store.IdentityMatchParticipant, RightID: right,
			Basis: store.IdentityMatchEmail, NormalizedValue: &normalized,
			State: store.IdentityMatchStateCandidate, Source: store.ProvenanceUser,
			SourceRef: &sourceRef, Notes: &notes,
		},
	)
	require.NoError(err)
	require.True(created)
	_, err = st.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: "manual_review", Detail: new("preserve this evidence"),
		Source: store.ProvenanceUser,
	})
	require.NoError(err)

	input := store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressEmail, OriginalValue: normalized,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	input.ProviderUserID = new("provider-left")
	leftResult, err := st.RecordContactObservationContext(ctx, left, input)
	require.NoError(err)
	input.ProviderUserID = new("provider-right")
	rightResult, err := st.RecordContactObservationContext(ctx, right, input)
	require.NoError(err)
	require.True(rightResult.Conflicting)

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, left, leftResult.Observation.Envelope.ID, nil,
	))
	candidates, err := st.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(candidate.ID, candidates[0].ID)
	assert.Equal(store.IdentityMatchStateCandidate, candidates[0].State)
	assert.Equal(store.ProvenanceUser, candidates[0].Source)
	assert.Equal(&sourceRef, candidates[0].SourceRef)
	assert.Equal(&notes, candidates[0].Notes)
	require.Len(candidates[0].Evidence, 1)
	assert.Equal("manual_review", candidates[0].Evidence[0].EvidenceKind)
}

func TestSupersedeParticipantObservationKeepsConflictsBetweenOtherParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	participants := []int64{
		f.EnsureParticipant("first@example.com", "First", "example.com"),
		f.EnsureParticipant("second@example.com", "Second", "example.com"),
		f.EnsureParticipant("third@example.com", "Third", "example.com"),
	}
	sources := []int64{f.Source.ID}
	for _, identifier := range []string{"second-source@example.org", "third-source@example.org"} {
		source, err := f.Store.GetOrCreateSource("gmail", identifier)
		require.NoError(err)
		sources = append(sources, source.ID)
	}

	var firstObservationID int64
	for index, participantID := range participants {
		result, err := f.Store.RecordContactObservationContext(
			ctx, participantID, store.ParticipantContactObservationInput{
				SourceID: &sources[index], AddressKind: store.ContactAddressEmail,
				ProviderUserID: new(fmt.Sprintf("provider-%d", index)),
				OriginalValue:  "shared@example.org",
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation,
				},
			},
		)
		require.NoError(err)
		if index == 0 {
			firstObservationID = result.Observation.Envelope.ID
		}
	}
	candidates, err := f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 3, "three participants require a complete conflict graph")

	require.NoError(f.Store.SupersedeParticipantObservationContext(
		ctx, participants[0], firstObservationID, nil,
	))
	candidates, err = f.Store.ListIdentityMatchCandidatesContext(ctx, nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	assert.ElementsMatch(
		[]int64{participants[1], participants[2]},
		[]int64{candidates[0].LeftID, candidates[0].RightID},
	)
}
