package importantdate

import (
	"context"
	"errors"
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
	native.Endpoint(server).CloseIssue(t, issue.UID)
	count, more, err = RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Zero(count)
	checks.False(more)
	issue, err = kata.GetTask(ctx, cfg.Project, issue.UID)
	must.NoError(err)
	checks.NotEqual("open", issue.Status)
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

func TestRunRefusesChangedProjectIdentity(t *testing.T) {
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
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", ProjectUID: "original-project", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	kata := &deliveryKataFake{projectUID: "replacement-project"}
	_, _, err = RunOnce(ctx, st, kata, cfg, time.Now())
	must.NoError(err)
	checks.Zero(kata.creates)
	checks.Zero(kata.notifications)
}

// A retained issue whose date has changed must not consume the entire batch.
func TestRunSkipsCorrectedRetainedIssues(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	now := time.Now()
	tomorrow := now.UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", ProjectUID: "project-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	for i := range 26 {
		participant, err := st.EnsureParticipantByIdentifier("email", fmt.Sprintf("corrected-%d@example.test", i), "Example Contact")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		must.NoError(err)
		_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		must.NoError(err)
		must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	}
	now = time.Now()
	candidates, err := Candidates(ctx, st, cfg, now)
	must.NoError(err)
	must.Len(candidates, 26)
	for _, candidate := range candidates[:25] {
		claimed, err := st.ClaimImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, "old-fingerprint")
		must.NoError(err)
		must.True(claimed)
		changed, err := st.AdvanceImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
		must.NoError(err)
		must.True(changed)
		changed, err = st.AdvanceImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, "issue-example")
		must.NoError(err)
		must.True(changed)
	}
	preview, err := PreviewCandidates(ctx, st, cfg, now)
	must.NoError(err)
	checks.Equal(25, preview.Skipped["corrected_occurrence_requires_reconciliation"])
	must.Len(preview.Candidates, 1)
	kata := &deliveryKataFake{}
	count, more, err := RunOnce(ctx, st, kata, cfg, now)
	must.NoError(err)
	checks.Equal(1, count)
	checks.False(more)
	checks.Equal(1, kata.notifications)
}

type failingRetainedIssueKata struct{ deliveryKataFake }

func (f *failingRetainedIssueKata) GetTask(ctx context.Context, project, uid string) (taskclient.KataTask, error) {
	if uid == "unreadable-issue" {
		return taskclient.KataTask{}, errors.New("issue lookup unavailable")
	}
	return f.deliveryKataFake.GetTask(ctx, project, uid)
}

func TestRunContinuesAfterRetainedIssueLookupFailure(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	now := time.Now()
	tomorrow := now.UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", ProjectUID: "project-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	for i := range 2 {
		participant, err := st.EnsureParticipantByIdentifier("email", fmt.Sprintf("lookup-%d@example.test", i), "Example Contact")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		must.NoError(err)
		_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		must.NoError(err)
		must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	}
	now = time.Now()
	candidates, err := Candidates(ctx, st, cfg, now)
	must.NoError(err)
	must.Len(candidates, 2)
	first := candidates[0]
	claimed, err := st.ClaimImportantDateDeliveryContext(ctx, first.OccurrenceKey, first.Fingerprint)
	must.NoError(err)
	must.True(claimed)
	changed, err := st.AdvanceImportantDateDeliveryContext(ctx, first.OccurrenceKey, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
	must.NoError(err)
	must.True(changed)
	changed, err = st.AdvanceImportantDateDeliveryContext(ctx, first.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, "unreadable-issue")
	must.NoError(err)
	must.True(changed)
	kata := &failingRetainedIssueKata{}
	count, more, err := RunOnce(ctx, st, kata, cfg, now)
	must.ErrorContains(err, "issue lookup unavailable")
	checks.Equal(2, count)
	checks.False(more)
	checks.Equal(1, kata.notifications)
	state, uid, err := st.ImportantDateDeliveryContext(ctx, first.OccurrenceKey)
	must.NoError(err)
	checks.Equal(store.ImportantDateIssueCreated, state)
	checks.Equal("unreadable-issue", uid)
}

func TestRunBatchContinuesPastRepeatedPreflightFailures(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	now := time.Now()
	tomorrow := now.UTC().AddDate(0, 0, 1)
	month, day := int(tomorrow.Month()), tomorrow.Day()
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", ProjectUID: "project-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	cfg.ConsentScope = DestinationScope("https://example.test", cfg.Project, cfg.ProjectUID)
	for i := range 26 {
		participant, err := st.EnsureParticipantByIdentifier("email", fmt.Sprintf("batch-failure-%d@example.test", i), "Example Contact")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, participant)
		must.NoError(err)
		_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		must.NoError(err)
		_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: &month, Day: &day}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		must.NoError(err)
		must.NoError(st.SetImportantDateConsentContext(ctx, "person/"+person.VCardUID, cfg.ConsentScope, cfg.Recipient, true))
	}
	now = time.Now()
	candidates, err := Candidates(ctx, st, cfg, now)
	must.NoError(err)
	must.Len(candidates, 26)
	for _, candidate := range candidates[:25] {
		claimed, err := st.ClaimImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, candidate.Fingerprint)
		must.NoError(err)
		must.True(claimed)
		changed, err := st.AdvanceImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, store.ImportantDateClaimed, store.ImportantDateIssueInFlight, "")
		must.NoError(err)
		must.True(changed)
		changed, err = st.AdvanceImportantDateDeliveryContext(ctx, candidate.OccurrenceKey, store.ImportantDateIssueInFlight, store.ImportantDateIssueCreated, "unreadable-issue")
		must.NoError(err)
		must.True(changed)
	}
	kata := &failingRetainedIssueKata{}
	count, cursor, err := RunBatch(ctx, st, kata, cfg, now, "")
	must.ErrorContains(err, "issue lookup unavailable")
	checks.Equal(25, count)
	must.NotEmpty(cursor)
	count, cursor, err = RunBatch(ctx, st, kata, cfg, now, cursor)
	must.NoError(err)
	checks.Equal(1, count)
	checks.Empty(cursor)
	checks.Equal(1, kata.notifications)
}
