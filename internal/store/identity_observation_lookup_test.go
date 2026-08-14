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

// recordObservation is a local helper: the inputs in these tests differ only
// in a few fields, and spelling the whole struct out each time hides that.
func recordObservation(
	t *testing.T,
	st *store.Store,
	participantID int64,
	serviceSlug string,
	scopeValue *string,
	providerUserID string,
	value string,
) *store.RecordContactObservationResult {
	t.Helper()
	require := require.New(t)

	input := store.ParticipantContactObservationInput{
		AddressKind:    store.ContactAddressUsername,
		ServiceSlug:    &serviceSlug,
		ProviderUserID: &providerUserID,
		OriginalValue:  value,
		Envelope:       store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
	}
	if scopeValue != nil {
		kindName := "account"
		input.ScopeKind = &kindName
		input.ScopeValue = scopeValue
	}
	result, err := st.RecordContactObservationContext(context.Background(), participantID, input)
	require.NoError(err, "RecordContactObservationContext")
	return result
}

func TestFindObservationsByProviderUserIDGroupsBothParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@alice2:beeper.local", "Alice Example")
	require.NoError(err, "ensure second alice")

	recordObservation(t, st, alice, "telegram", nil, "telegram:alice-native", "@alice")
	second := recordObservation(t, st, bob, "telegram", nil, "telegram:alice-native", "@alice")

	found, err := st.FindObservationsByProviderUserIDContext(ctx, "telegram:alice-native", 0)
	require.NoError(err, "FindObservationsByProviderUserIDContext")
	require.Len(found, 2, "both participants share the provider ID")
	assert.Equal(alice, found[0].ParticipantID)
	assert.Equal(bob, found[1].ParticipantID)

	require.NoError(st.SupersedeParticipantObservationContext(
		ctx, bob, second.Observation.Envelope.ID, nil), "supersede")
	current, err := st.FindObservationsByProviderUserIDContext(ctx, "telegram:alice-native", 0)
	require.NoError(err, "second lookup")
	assert.Len(current, 1, "a superseded observation is history, not a current match")

	none, err := st.FindObservationsByProviderUserIDContext(ctx, "telegram:nobody", 0)
	require.NoError(err, "unknown provider id")
	assert.Empty(none)
}

func TestFindObservationsByServiceValueIgnoresScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@bob:beeper.local", "Bob Example")
	require.NoError(err, "ensure bob")

	recordObservation(t, st, alice, "slack", new("T0EXAMPLE"), "beeper:@alice:beeper.local", "alice")
	recordObservation(t, st, bob, "slack", new("T0OTHER"), "beeper:@bob:beeper.local", "alice")

	found, err := st.FindObservationsByServiceValueContext(
		ctx, store.ContactAddressUsername, "slack", "alice", 0)
	require.NoError(err, "FindObservationsByServiceValueContext")
	require.Len(found, 2, "the lookup spans scopes on purpose")
	assert.NotEqual(*found[0].ScopeValue, *found[1].ScopeValue)

	other, err := st.FindObservationsByServiceValueContext(
		ctx, store.ContactAddressUsername, "x", "alice", 0)
	require.NoError(err, "different service")
	assert.Empty(other, "the service partitions the lookup")
}

func TestFindObservationsByQueriesPreserveCompleteValueEnvelope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	participantID, err := st.EnsureParticipantByIdentifier(
		"beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	activeFrom := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	confidence := 0.85
	serviceSlug := "telegram"
	providerUserID := "telegram:alice-native"
	input := store.ParticipantContactObservationInput{
		AddressKind:    store.ContactAddressUsername,
		ServiceSlug:    &serviceSlug,
		ProviderUserID: &providerUserID,
		OriginalValue:  "@alice",
		Envelope: store.ValueEnvelopeInput{
			Pref:       new(3),
			Ordinal:    new(7),
			TypeLabel:  new("work"),
			TypeTokens: []string{"WORK", "PREF"},
			VCard: store.VCardIdentity{
				Property: "IMPP",
				Group:    new("item1"),
				PropID:   new("prop-1"),
				PID:      []string{"1.1", "2.1"},
				AltID:    new("1"),
			},
			Source:     store.ProvenanceArchiveObservation,
			SourceRef:  new("beeper:alice"),
			Confidence: &confidence,
			ActiveFrom: &activeFrom,
		},
	}
	recorded, err := st.RecordContactObservationContext(ctx, participantID, input)
	require.NoError(err, "record observation")

	providerMatches, err := st.FindObservationsByProviderUserIDContext(
		ctx, providerUserID, 10)
	require.NoError(err, "provider-ID lookup")
	serviceMatches, err := st.FindObservationsByServiceValueContext(
		ctx, store.ContactAddressUsername, serviceSlug, "alice", 10)
	require.NoError(err, "service-value lookup")

	for name, matches := range map[string][]store.ParticipantContactObservation{
		"provider ID":   providerMatches,
		"service value": serviceMatches,
	} {
		require.Len(matches, 1, name)
		got := matches[0].Envelope
		assert.Equal(recorded.Observation.Envelope.ID, got.ID, name)
		assert.Equal(input.Envelope.Pref, got.Pref, name)
		assert.Equal(*input.Envelope.Ordinal, got.Ordinal, name)
		assert.Equal(input.Envelope.TypeLabel, got.TypeLabel, name)
		assert.Equal(input.Envelope.TypeTokens, got.TypeTokens, name)
		assert.Equal(input.Envelope.VCard, got.VCard, name)
		assert.Equal(input.Envelope.Source, got.Source, name)
		assert.Equal(input.Envelope.SourceRef, got.SourceRef, name)
		assert.Equal(input.Envelope.Confidence, got.Confidence, name)
		if assert.NotNil(got.ActiveFrom, name) {
			assert.True(got.ActiveFrom.Equal(*input.Envelope.ActiveFrom), name)
		}
		assert.Nil(got.ActiveUntil, name)
		assert.False(got.CreatedAt.IsZero(), name)
		assert.False(got.UpdatedAt.IsZero(), name)
		assert.Nil(got.SupersededAt, name)
	}
}

func TestFindObservationsByAndEvidenceQueriesHonorCancellation(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "provider-ID lookup", run: func() error {
			_, err := st.FindObservationsByProviderUserIDContext(ctx, "provider:alice", 10)
			return err
		}},
		{name: "service-value lookup", run: func() error {
			_, err := st.FindObservationsByServiceValueContext(
				ctx, store.ContactAddressUsername, "telegram", "alice", 10)
			return err
		}},
		{name: "identifier classification", run: func() error {
			return st.ClassifyParticipantIdentifierServiceContext(
				ctx, "beeper", "@alice:beeper.local", nil, nil, nil)
		}},
		{name: "display names", run: func() error {
			_, err := st.ParticipantDisplayNamesContext(ctx, []int64{1})
			return err
		}},
		{name: "shared conversations", run: func() error {
			_, err := st.SharedConversationCountContext(ctx, 1, 2)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(tt.run(), context.Canceled)
		})
	}
}

func TestClassifyParticipantIdentifierService(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	participantID, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	require.NotZero(participantID)
	service, err := st.ResolveCommunicationServiceContext(ctx, "signal")
	require.NoError(err, "resolve signal")

	require.NoError(st.ClassifyParticipantIdentifierServiceContext(
		ctx, "beeper", "@alice:beeper.local", &service.ID, new("account"), new("signal"),
	), "classify")

	var (
		gotServiceID int64
		gotKind      string
		gotValue     string
	)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT service_id, scope_kind, scope_value FROM participant_identifiers
		 WHERE identifier_type = ? AND identifier_value = ?`),
		"beeper", "@alice:beeper.local",
	).Scan(&gotServiceID, &gotKind, &gotValue), "read classification")
	assert.Equal(service.ID, gotServiceID)
	assert.Equal("account", gotKind)
	assert.Equal("signal", gotValue)

	err = st.ClassifyParticipantIdentifierServiceContext(
		ctx, "beeper", "@nobody:beeper.local", &service.ID, nil, nil)
	assert.ErrorIs(err, store.ErrParticipantIdentifierNotFound)
}

func TestParticipantDisplayNamesAndSharedConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()

	source, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err, "GetOrCreateSource")
	alice, err := st.EnsureParticipantByIdentifier("beeper", "@alice:beeper.local", "Alice Example")
	require.NoError(err, "ensure alice")
	bob, err := st.EnsureParticipantByIdentifier("beeper", "@bob:beeper.local", "Alice Example")
	require.NoError(err, "ensure bob")

	names, err := st.ParticipantDisplayNamesContext(ctx, []int64{alice, bob})
	require.NoError(err, "ParticipantDisplayNamesContext")
	assert.Equal("Alice Example", names[alice])
	assert.Equal("Alice Example", names[bob])

	shared, err := st.SharedConversationCountContext(ctx, alice, bob)
	require.NoError(err, "SharedConversationCountContext")
	assert.Equal(0, shared)

	convID, err := st.EnsureConversationWithType(source.ID, "!room:beeper.local", "group_chat", "Group")
	require.NoError(err, "EnsureConversationWithType")
	require.NoError(st.EnsureConversationParticipant(convID, alice, "member"), "add alice")
	require.NoError(st.EnsureConversationParticipant(convID, bob, "member"), "add bob")

	shared, err = st.SharedConversationCountContext(ctx, alice, bob)
	require.NoError(err, "second SharedConversationCountContext")
	assert.Equal(1, shared)
}
