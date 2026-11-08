package importantdate

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
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

func TestRunCreatesOneNativePreparationIssue(t *testing.T) {
	st := storetest.New(t).Store
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier("email", "contact@example.test", "Example Contact")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	require.NoError(t, err)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
	require.NoError(t, err)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	kata, err := taskclient.ConnectKata(ctx, taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	require.NoError(t, err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: katatest.Project, Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	require.NoError(t, st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.Project, cfg.Recipient, true))
	count, err := RunOnce(ctx, st, kata, cfg, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	count, err = RunOnce(ctx, st, kata, cfg, time.Now())
	require.NoError(t, err)
	assert.Zero(t, count)
	deliveries, err := st.ListImportantDateDeliveriesContext(ctx)
	require.NoError(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, store.ImportantDateDelivered, deliveries[0].State)
	issue, err := kata.GetTask(ctx, cfg.Project, deliveries[0].IssueUID)
	require.NoError(t, err)
	assert.Equal(t, person.VCardUID, issue.Metadata["msgvault.person"])
}
