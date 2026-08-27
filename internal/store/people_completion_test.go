package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCompletePersonProfilesReturnsOnlyCurrentCuratedPrimitives(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	st := storetest.New(t).Store
	participantID, err := st.EnsureParticipant("alice@example.test", "Alice Observed", "example.test")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
	require.NoError(err)
	_, err = st.UpdatePersonDisplayNameContext(ctx, person.ID, person.Revision, new("Alice Profile"))
	require.NoError(err)

	for _, input := range []store.PersonNameInput{
		{NameKind: store.PersonNameFormatted, Formatted: new("Alice Example"), Envelope: completionEnvelope()},
		{NameKind: store.PersonNameNickname, Formatted: new("Ally"), Envelope: completionEnvelope()},
		{NameKind: store.PersonNamePhonetic, Formatted: new("Arisu"), Envelope: completionEnvelope()},
		{NameKind: store.PersonNameNickname, Formatted: new("Percent% Name"), Envelope: completionEnvelope()},
		{NameKind: store.PersonNameNickname, Formatted: new("Under_score"), Envelope: completionEnvelope()},
		{NameKind: store.PersonNameSort, SortAs: new("Example, Alice"), Envelope: completionEnvelope()},
	} {
		_, err = st.AddPersonNameContext(ctx, person.ID, input)
		require.NoError(err)
	}
	for _, input := range []store.PersonContactPointInput{
		{AddressKind: store.ContactAddressEmail, OriginalValue: "Alice@Example.test", Envelope: completionEnvelope()},
		{AddressKind: store.ContactAddressPhone, OriginalValue: "+1 (202) 555-0147", Envelope: completionEnvelope()},
		{AddressKind: store.ContactAddressUsername, ServiceSlug: new("telegram"), OriginalValue: "@alice", Envelope: completionEnvelope()},
		{AddressKind: store.ContactAddressIMPP, OriginalValue: "xmpp:alice@example.test", Envelope: completionEnvelope()},
		{AddressKind: store.ContactAddressURL, OriginalValue: "https://example.test/alice", Envelope: completionEnvelope()},
	} {
		_, err = st.AddPersonContactPointContext(ctx, person.ID, input)
		require.NoError(err)
	}

	currentOrg, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Example Industries", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	endedOrg, err := st.CreateOrganizationContext(ctx, store.OrganizationInput{
		Name: "Former Example", Kind: store.OrganizationKindCompany,
	})
	require.NoError(err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: currentOrg.ID,
		Title: new("Staff Engineer"), Role: new("Technical Lead"),
		Source: store.ProvenanceUser,
	})
	require.NoError(err)
	_, err = st.AddEmploymentContext(ctx, store.EmploymentInput{
		PersonID: person.ID, OrganizationID: endedOrg.ID,
		Title: new("Former Engineer"), Role: new("Former Role"),
		IsCurrent: new(false), Source: store.ProvenanceUser,
	})
	require.NoError(err)

	tests := []struct {
		query string
		want  []store.PersonCompletion
	}{
		{"ally", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "name", Value: "Ally", MatchValue: "ally", Source: "nickname"}}},
		{"arisu", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "name", Value: "Arisu", MatchValue: "arisu", Source: "phonetic"}}},
		{"%", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "name", Value: "Percent% Name", MatchValue: "percent% name", Source: "nickname"}}},
		{"_", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "name", Value: "Under_score", MatchValue: "under_score", Source: "nickname"}}},
		{"2025550147", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "phone", Value: "+1 (202) 555-0147", MatchValue: "+12025550147", Source: "profile"}}},
		{"@alice", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "username", Value: "@alice", MatchValue: "alice", Source: "telegram"}}},
		{"xmpp", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "impp", Value: "xmpp:alice@example.test", MatchValue: "xmpp:alice@example.test", Source: "profile"}}},
		{"industries", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "organization", Value: "Example Industries", MatchValue: "example industries", Source: "profile"}}},
		{"staff", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "title", Value: "Staff Engineer", MatchValue: "staff engineer", Source: "profile"}}},
		{"technical", []store.PersonCompletion{{ParticipantID: participantID, DisplayLabel: "Alice Profile", Kind: "role", Value: "Technical Lead", MatchValue: "technical lead", Source: "profile"}}},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			rows, err := st.CompletePersonProfilesContext(ctx, store.PersonCompletionQuery{
				Query: test.query, Limit: 20,
			})
			assert.NoError(t, err) //nolint:testifylint // The parent test's required local helper shadows require.
			assert.Equal(t, test.want, rows)
		})
	}

	for _, excluded := range []string{"example, alice", "former example", "former engineer", "former role", "https://"} {
		rows, err := st.CompletePersonProfilesContext(ctx, store.PersonCompletionQuery{
			Query: excluded, Limit: 20,
		})
		require.NoError(err)
		assert.Empty(t, rows, excluded)
	}
}

func TestCompletePersonProfilesValidatesAndCapsResults(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := storetest.New(t).Store
	for index := range 25 {
		participantID, err := st.EnsureParticipant(
			"person-"+string(rune('a'+index))+"@example.test", "Synthetic", "example.test")
		require.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participantID)
		require.NoError(err)
		_, err = st.AddPersonNameContext(ctx, person.ID, store.PersonNameInput{
			NameKind: store.PersonNameNickname, Formatted: new("Common Name"),
			Envelope: completionEnvelope(),
		})
		require.NoError(err)
	}

	rows, err := st.CompletePersonProfilesContext(ctx, store.PersonCompletionQuery{
		Query: "common", Limit: 20,
	})
	require.NoError(err)
	assert.Len(rows, 20)

	for _, query := range []store.PersonCompletionQuery{
		{Query: " ", Limit: 8}, {Query: "common", Limit: -1}, {Query: "common", Limit: 21},
	} {
		_, err := st.CompletePersonProfilesContext(ctx, query)
		assert.ErrorIs(err, store.ErrInvalidPersonCompletionQuery)
	}
}

func completionEnvelope() store.ValueEnvelopeInput {
	return store.ValueEnvelopeInput{Source: store.ProvenanceUser}
}
