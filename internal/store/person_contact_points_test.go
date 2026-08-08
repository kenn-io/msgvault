package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonKeepsManyCurrentAndHistoricalContactPointsWithoutJSONBundling(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	inputs := []store.PersonContactPointInput{
		{AddressKind: store.ContactAddressPhone, ServiceSlug: new("whatsapp"),
			OriginalValue: "+1 (202) 555-0123",
			Envelope:      store.ValueEnvelope{Source: store.ProvenanceUser, Pref: new(1)}},
		{AddressKind: store.ContactAddressEmail, OriginalValue: "Alice@Example.com",
			Envelope: store.ValueEnvelope{Source: store.ProvenanceUser}},
		{AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
			ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"),
			OriginalValue: "Alice",
			Envelope:      store.ValueEnvelope{Source: store.ProvenanceArchiveObservation}},
	}
	created := make([]store.PersonContactPoint, 0, len(inputs))
	for _, input := range inputs {
		point, err := st.AddPersonContactPointContext(ctx, personID, input)
		require.NoError(err)
		created = append(created, *point)
	}
	assert.Equal("+12025550123", created[0].NormalizedValue)
	assert.Equal("alice@example.com", created[1].NormalizedValue)
	assert.Equal("alice", created[2].NormalizedValue)
	require.NoError(st.SupersedePersonContactPointContext(ctx, personID, created[0].Envelope.ID, nil))
	current, err := st.ListPersonContactPointsContext(ctx, personID, true)
	require.NoError(err)
	assert.Len(current, 2)
	all, err := st.ListPersonContactPointsContext(ctx, personID, false)
	require.NoError(err)
	assert.Len(all, 3)
}

func TestSameUsernameIsSafeAcrossServicesAndScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	for _, scope := range []string{"irc.example.net", "irc.example.org"} {
		_, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
			AddressKind: store.ContactAddressUsername, ServiceSlug: new("irc"),
			ScopeKind: new("network"), ScopeValue: new(scope), OriginalValue: "alice",
			Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
		})
		require.NoError(err)
	}
	found, err := st.FindPersonContactPointsContext(ctx, store.ContactPointQuery{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("irc"),
		ScopeKind: new("network"), ScopeValue: new("irc.example.net"),
		NormalizedValue: "alice",
	})
	require.NoError(err)
	require.Len(found, 1)
	assert.Equal("irc.example.net", *found[0].ScopeValue)
}

func TestContactPointScopeKindAndLanguageValidation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)

	_, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("slack"),
		OriginalValue: "alice", Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrServiceScopeRequired)
	_, err = st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: "pager", OriginalValue: "12345",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceUser},
	})
	require.ErrorIs(err, store.ErrInvalidContactAddressKind)
	point, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressLanguage, OriginalValue: "en-GB",
		Envelope: store.ValueEnvelope{Source: store.ProvenanceVCardImport,
			VCard: store.VCardIdentity{Property: "LANGUAGE"}},
	})
	require.NoError(err)
	assert.Equal("en-gb", point.NormalizedValue)
	assert.Equal("en-GB", point.OriginalValue)
}

func TestRetractedContactPointLeavesCurrentSetButStaysInHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := storetest.New(t).Store
	ctx := context.Background()
	personID := newTestPerson(t, st)
	_, err := st.AddPersonContactPointContext(ctx, personID, store.PersonContactPointInput{
		AddressKind: store.ContactAddressUsername, ServiceSlug: new("x"),
		OriginalValue: "@alice",
		Envelope:      store.ValueEnvelope{Source: store.ProvenanceExtraction, Confidence: new(0.6)},
	})
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, "UPDATE person_contact_points SET superseded_at = CURRENT_TIMESTAMP")
	require.NoError(err)
	current, err := st.ListPersonContactPointsContext(ctx, personID, true)
	require.NoError(err)
	assert.Empty(current)
	all, err := st.ListPersonContactPointsContext(ctx, personID, false)
	require.NoError(err)
	require.Len(all, 1)
	assert.Nil(all[0].Envelope.ActiveUntil)
	assert.NotNil(all[0].Envelope.SupersededAt)
}
