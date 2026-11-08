package importantdate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestCandidatesDeduplicateChildWithTwoParents(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	makePerson := func(address string) *store.Person {
		id, err := st.EnsureParticipantByIdentifier("email", address, "Example Person")
		require.NoError(t, err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, id)
		require.NoError(t, err)
		return person
	}
	first := makePerson("parent-one@example.test")
	second := makePerson("parent-two@example.test")
	child := makePerson("child@example.test")
	for _, parent := range []*store.Person{first, second} {
		_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: parent.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		require.NoError(t, err)
		_, err = st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: parent.ID, TargetPersonID: child.ID, TypeSlug: "parent", Source: store.ProvenanceUser, Actor: "test"})
		require.NoError(t, err)
	}
	_, err := st.AddPersonDateContext(ctx, child.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	candidates, err := Candidates(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, "person/"+child.VCardUID, candidates[0].SubjectUID)
	assert.Equal(t, TierA, candidates[0].Tier)
}
