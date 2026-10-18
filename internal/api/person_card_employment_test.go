package api

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonCardEmploymentFactUsesNormalizedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		normalized string
		want       string
	}{
		{"missing", `{"organization":{"name":"Example Company"}}`, "Example Company"},
		{"empty", `{"organization":{"name":"Example Company"},"title":""}`, "Example Company"},
		{"present", `{"organization":{"name":"Example Company"},"title":"Engineer"}`, "Engineer @ Example Company"},
		{"no normalized value", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var normalized *personfacts.NormalizedValue
			if tc.normalized != "" {
				normalized = &personfacts.NormalizedValue{JSON: jsontext.Value(tc.normalized)}
			}
			got, err := personCardEmploymentFact(personfacts.Claim{Normalized: normalized})
			requirements.NoError(err)
			assertions.Equal(tc.want, got)
		})
	}
}

func TestPersonCardEmploymentFactKeepsClaimValueAfterProjectionEdit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st, personID, targets := newPersonFactAPIFixtureWithoutClaims(t)
	target, ok := targets["employment"]
	requirements.True(ok)
	applyPersonFactAPIGeneration(t, st, personID, target, "employment-snapshot",
		`{"organization":{"name":"Codec Company"},"title":"Engineer"}`)

	decisions, err := st.ListPersonFactDecisionsContext(t.Context(), personID,
		personfacts.DecisionFilter{Limit: 50})
	requirements.NoError(err)
	requirements.Len(decisions, 1)
	requirements.Equal(personfacts.DecisionApplied, decisions[0].Action)
	requirements.NotNil(decisions[0].Projection)
	employment, err := st.GetEmploymentContext(t.Context(), decisions[0].Projection.RowID)
	requirements.NoError(err)
	updated, err := st.UpdateEmploymentContext(t.Context(), employment.ID, employment.Revision,
		store.EmploymentInput{
			PersonID: personID, OrganizationID: employment.OrganizationID,
			Title: new("Director"), Source: store.ProvenanceUser,
		})
	requirements.NoError(err)
	requirements.NotNil(updated.Title)
	assertions.Equal("Director", *updated.Title)

	opts := personcard.Options{AsOf: time.Now().UTC().Add(time.Minute)}
	in := personcard.Inputs{
		CollectedAt: opts.AsOf,
		Person:      personcard.Person{PersonID: personID, Tracked: true},
	}
	srv.personCardFacts(t.Context(), personID, &in, opts)
	card := personcard.Assemble(in, opts)
	requirements.Len(card.Items, 1)
	assertions.Equal("fact_change", card.Items[0].Kind)
	assertions.Equal("fact_decision", card.Items[0].Cite.Kind)
	assertions.Equal("Engineer @ Codec Company", card.Items[0].UntrustedText.Text)
}
