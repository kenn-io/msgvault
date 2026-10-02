package query

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

func TestAccountScopesFilterIdentityRollups(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	builder := NewTestDataBuilder(t)
	selectedSource := builder.AddSource("owner@example.org")
	otherSource := builder.AddSource("other-owner@example.net")
	owner := builder.AddParticipant("owner@example.org", "example.org", "Owner")
	otherOwner := builder.AddParticipant("other-owner@example.net", "example.net", "Other Owner")
	builder.AddOwnerParticipant(selectedSource, owner)
	builder.AddOwnerParticipant(otherSource, otherOwner)
	shared := builder.AddParticipant("shared@people.example", "people.example", "Shared Person")
	other := builder.AddParticipant("other@other.example", "other.example", "Other Person")
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	selectedMessage := builder.AddMessage(MessageOpt{SourceID: selectedSource, SentAt: now.Add(-time.Hour)})
	builder.AddFrom(selectedMessage, shared, "Shared Person")
	builder.AddTo(selectedMessage, owner, "Owner")
	otherMessage := builder.AddMessage(MessageOpt{SourceID: otherSource, SentAt: now.Add(-time.Hour)})
	builder.AddFrom(otherMessage, other, "Other Person")
	builder.AddTo(otherMessage, shared, "Shared Person")
	builder.AddTo(otherMessage, otherOwner, "Other Owner")
	engine := builder.BuildEngine()
	wide, err := engine.GetPerson(t.Context(), shared, Context{}, nil)
	requirements.NoError(err)
	requirements.NotNil(wide)
	assertions.Equal(int64(2), wide.ActivityCount, "negative control: whole-archive rollups contain both messages")

	for _, tc := range []struct {
		name    string
		context Context
		count   int64
	}{
		{"virtual unattributed account", Context{AccountScopes: []search.AccountScope{{SourceID: &selectedSource, Unattributed: true}}}, 1},
		{"unmatched address", Context{AccountScopes: []search.AccountScope{{Addresses: []string{"missing@example.org"}}}}, 0},
		{"source intersects virtual account", Context{SourceIDs: []int64{selectedSource}, AccountScopes: []search.AccountScope{{SourceID: &otherSource, Unattributed: true}}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			explore := ExploreRequest{Context: tc.context}
			people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: explore, Page: PageSpec{Limit: 25}})
			requirements.NoError(err)
			var personIDs []int64
			for _, person := range people.Rows {
				personIDs = append(personIDs, person.ID)
			}
			assertions.NotContains(personIDs, other)
			domains, err := engine.SearchDomains(t.Context(), DomainSearchRequest{Explore: explore, Page: PageSpec{Limit: 25}})
			requirements.NoError(err)
			var domainNames []string
			for _, domain := range domains.Rows {
				domainNames = append(domainNames, domain.Domain)
			}
			assertions.NotContains(domainNames, "other.example")
			relationships, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: tc.context, ShowAll: true, Now: now, Limit: 25})
			requirements.NoError(err)
			var relationshipIDs []int64
			for _, relationship := range relationships.Rows {
				relationshipIDs = append(relationshipIDs, relationship.CanonicalID)
			}
			assertions.NotContains(relationshipIDs, other)
			person, err := engine.GetPerson(t.Context(), shared, tc.context, nil)
			requirements.NoError(err)
			if tc.count == 0 {
				assertions.Empty(people.Rows)
				assertions.Empty(domains.Rows)
				assertions.Empty(relationships.Rows)
				assertions.Nil(person)
				return
			}
			assertions.Contains(personIDs, shared)
			assertions.Contains(domainNames, "people.example")
			assertions.Contains(relationshipIDs, shared)
			requirements.NotNil(person)
			assertions.Equal(tc.count, person.ActivityCount)
		})
	}
}
