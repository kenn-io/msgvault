package importantdate

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRunSkipsCandidatesWithoutConsent(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier("email", "contact@example.test", "Example Contact")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	require.NoError(t, err)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
	require.NoError(t, err)
	_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	kata := &deliveryKataFake{}
	count, err := RunOnce(ctx, st, kata, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	assert.Zero(t, count)
	assert.Zero(t, kata.creates)
}
