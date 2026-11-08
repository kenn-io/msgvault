package importantdate

import (
	"fmt"
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
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier("email", "contact@example.test", "Example Contact")
	must.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	must.NoError(err)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
	must.NoError(err)
	_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ProjectUID = "project-example"
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	kata := &deliveryKataFake{}
	count, more, err := RunOnce(ctx, st, kata, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	must.NoError(err)
	checks.Zero(count)
	checks.False(more)
	checks.Zero(kata.creates)
}

func TestRunCreatesOneNativePreparationIssue(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	participant, err := st.EnsureParticipantByIdentifier("email", "contact@example.test", "Example Contact")
	must.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
	must.NoError(err)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
	must.NoError(err)
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	native := katatest.New(t)
	server := httptest.NewTLSServer(native.Service.Handler())
	t.Cleanup(server.Close)
	kata, err := taskclient.ConnectKata(ctx, taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, HTTPClient: server.Client(), DefaultProject: katatest.Project})
	must.NoError(err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: katatest.Project, Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ProjectUID, err = kata.ProjectUID(ctx, cfg.Project)
	must.NoError(err)
	cfg.ConsentScope = DestinationScope(server.URL, cfg.Project, cfg.ProjectUID)
	must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	count, more, err := RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Equal(1, count)
	checks.False(more)
	count, more, err = RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Zero(count)
	checks.False(more)
	deliveries, err := st.ListImportantDateDeliveriesContext(ctx)
	must.NoError(err)
	must.Len(deliveries, 1)
	checks.Equal(store.ImportantDateDelivered, deliveries[0].State)
	issue, err := kata.GetTask(ctx, cfg.Project, deliveries[0].IssueUID)
	must.NoError(err)
	checks.Equal(person.VCardUID, issue.Metadata["msgvault.person"])
}

func TestRunContinuesAfterBoundedBatch(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", ProjectUID: "project-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	for i := range 26 {
		participant, err := st.EnsureParticipantByIdentifier("email", fmt.Sprintf("contact-%d@example.test", i), "Example Contact")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		must.NoError(err)
		_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		must.NoError(err)
		must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	}
	kata := &deliveryKataFake{}
	count, more, err := RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Equal(25, count)
	checks.True(more)
	count, more, err = RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Equal(1, count)
	checks.False(more)
}
