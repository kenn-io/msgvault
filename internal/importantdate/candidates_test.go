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
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	makePerson := func(address string) *store.Person {
		id, err := st.EnsureParticipantByIdentifier("email", address, "Example Person")
		must.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(ctx, id)
		must.NoError(err)
		return person
	}
	first := makePerson("parent-one@example.test")
	second := makePerson("parent-two@example.test")
	child := makePerson("child@example.test")
	for _, parent := range []*store.Person{first, second} {
		_, err := st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: parent.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(21))}, Source: store.ProvenanceUser})
		must.NoError(err)
		_, err = st.AddPersonRelationshipContext(ctx, store.PersonRelationshipInput{SourcePersonID: parent.ID, TargetPersonID: child.ID, TypeSlug: "parent", Source: store.ProvenanceUser, Actor: "test"})
		must.NoError(err)
	}
	_, err := st.AddPersonDateContext(ctx, child.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", Recipient: "owner/agent", UseCadencePresets: true}
	cfg.ApplyDefaults()
	candidates, err := Candidates(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	must.NoError(err)
	must.Len(candidates, 1)
	checks.Equal("person/"+child.VCardUID, candidates[0].SubjectUID)
	checks.Equal(TierA, candidates[0].Tier)
	targeted, err := CandidateForSubject(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC), candidates[0].SubjectUID, candidates[0].OwnerUID)
	must.NoError(err)
	checks.Equal(candidates, targeted)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: child.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(365))}, Source: store.ProvenanceUser})
	must.NoError(err)
	candidates, err = Candidates(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	must.NoError(err)
	checks.Empty(candidates, "direct tier D must win before the lead window is applied")
	targeted, err = CandidateForSubject(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC), "person/"+child.VCardUID, "person/"+first.VCardUID)
	must.NoError(err)
	checks.Empty(targeted)
	preview, err := PreviewCandidates(ctx, st, cfg, time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC))
	must.NoError(err)
	checks.Empty(preview.Candidates)
	checks.Equal(1, preview.Skipped["outside_lead_window"])
}

func TestRelationshipStartRequiresCompletePastDate(t *testing.T) {
	checks := assert.New(t)
	now := time.Date(2027, 10, 1, 12, 0, 0, 0, time.UTC)
	checks.True(relationshipStarted(nil, now, time.UTC))
	checks.True(relationshipStarted(&store.PartialDate{Year: new(2027), Month: new(9), Day: new(30)}, now, time.UTC))
	checks.False(relationshipStarted(&store.PartialDate{Year: new(2027), Month: new(10), Day: new(2)}, now, time.UTC))
	checks.False(relationshipStarted(&store.PartialDate{Year: new(2027)}, now, time.UTC))
}

func TestCandidatesRequireExplicitCustomOrUnassignedPolicy(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	st := storetest.New(t).Store
	ctx := t.Context()
	id, err := st.EnsureParticipantByIdentifier("email", "policy@example.test", "Example Person")
	must.NoError(err)
	person, _, err := st.CreatePersonFromParticipantContext(ctx, id)
	must.NoError(err)
	_, err = st.AddPersonDateContext(ctx, person.ID, store.PersonDateInput{DateKind: store.PersonDateBirthday, Date: store.PartialDate{Month: new(10), Day: new(23)}, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	must.NoError(err)
	cfg := Config{Enabled: true, PrivateDestination: true, Project: "private-example", Recipient: "owner/agent"}
	cfg.ApplyDefaults()
	now := time.Date(2027, 10, 10, 12, 0, 0, 0, time.UTC)
	preview, err := PreviewCandidates(ctx, st, cfg, now)
	must.NoError(err)
	checks.Empty(preview.Candidates)
	checks.Equal(1, preview.Skipped["unassigned_cadence"])
	cfg.UnassignedTier = TierB
	preview, err = PreviewCandidates(ctx, st, cfg, now)
	must.NoError(err)
	must.Len(preview.Candidates, 1)
	checks.Equal(TierB, preview.Candidates[0].Tier)
	_, err = st.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: new(int64(45))}, Source: store.ProvenanceUser})
	must.NoError(err)
	preview, err = PreviewCandidates(ctx, st, cfg, now)
	must.NoError(err)
	checks.Empty(preview.Candidates)
	checks.Equal(1, preview.Skipped["custom_cadence"])
	cfg.CustomTier = TierC
	preview, err = PreviewCandidates(ctx, st, cfg, now)
	must.NoError(err)
	must.Len(preview.Candidates, 1)
	checks.Equal(TierC, preview.Candidates[0].Tier)
}
