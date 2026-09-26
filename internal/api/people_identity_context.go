package api

import (
	"context"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// ParticipantIdentityContextStore supplies the live identity details omitted
// from the analytical person cache.
type ParticipantIdentityContextStore interface {
	GetParticipantIdentityContext(ctx context.Context, participantIDs []int64) (*store.ParticipantIdentityContext, error)
}

func enrichIdentityContext(person *query.PersonSummary, details *store.ParticipantIdentityContext) {
	if person == nil || details == nil {
		return
	}
	members := make(map[int64]store.ParticipantIdentityMember, len(details.Members))
	if person.Cluster != nil {
		person.Cluster.Members = make([]query.PersonClusterMember, 0, len(details.Members))
	}
	for _, member := range details.Members {
		members[member.ParticipantID] = member
		if person.Cluster != nil {
			person.Cluster.Members = append(person.Cluster.Members, query.PersonClusterMember{
				ParticipantID: member.ParticipantID,
				DisplayName:   member.DisplayName,
				Email:         member.Email,
				Phone:         member.Phone,
			})
		}
	}
	type identifierKey struct {
		participantID int64
		kind          string
		value         string
	}
	identifiers := make(map[identifierKey]store.ParticipantIdentifierContext, len(details.Identifiers))
	for _, identifier := range details.Identifiers {
		identifiers[identifierKey{identifier.ParticipantID, identifier.Type, identifier.Value}] = identifier
	}
	for i := range person.Identifiers {
		identifier := &person.Identifiers[i]
		if stored, ok := identifiers[identifierKey{identifier.ParticipantID, identifier.Type, identifier.Value}]; ok {
			identifier.ServiceSlug = stored.ServiceSlug
			identifier.ServiceLabel = stored.ServiceLabel
			identifier.ScopeKind = stored.ScopeKind
			identifier.ScopeValue = stored.ScopeValue
		}
		identifier.ParticipantDisplayName = members[identifier.ParticipantID].DisplayName
	}
	if person.Cluster == nil {
		return
	}
	links := make(map[[2]int64]store.ParticipantLinkContext, len(details.Links))
	for _, link := range details.Links {
		links[[2]int64{link.ParticipantA, link.ParticipantB}] = link
	}
	for i := range person.Cluster.Edges {
		edge := &person.Cluster.Edges[i]
		if link, ok := links[[2]int64{edge.ParticipantA, edge.ParticipantB}]; ok {
			edge.LinkOrigin = &query.PersonClusterLinkOrigin{
				Kind: link.OriginKind, Source: link.Source, Basis: link.Basis,
			}
		}
	}
}
