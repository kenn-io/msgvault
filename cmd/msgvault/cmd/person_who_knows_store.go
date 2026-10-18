package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/whoknows"
)

var _ whoknows.DataReader = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) PeopleRolesForMessages(ctx context.Context, ids []int64) (store.PeopleRoles, error) {
	return a.store.PeopleRolesForMessages(ctx, ids)
}
func (a *storeAPIAdapter) PeopleRolesForMessagesWithRosterLimit(ctx context.Context, ids []int64, maxRosterEdges int) (store.PeopleRoles, error) {
	return a.store.PeopleRolesForMessagesWithRosterLimit(ctx, ids, maxRosterEdges)
}
func (a *storeAPIAdapter) PeopleNativeOwnersContext(ctx context.Context, refs []store.SourceParticipantRef) (map[store.SourceParticipantRef]bool, error) {
	return a.store.PeopleNativeOwnersContext(ctx, refs)
}
func (a *storeAPIAdapter) PeopleDeclaredTopics(ctx context.Context) ([]store.PersonDeclaredTopic, error) {
	return a.store.PeopleDeclaredTopics(ctx)
}
func (a *storeAPIAdapter) PeopleProfiles(ctx context.Context, ids []int64) (map[int64]store.PeopleProfile, error) {
	return a.store.PeopleProfiles(ctx, ids)
}
func (a *storeAPIAdapter) PeopleEmployments(ctx context.Context, ids []int64) ([]store.PeopleEmployment, error) {
	return a.store.PeopleEmployments(ctx, ids)
}
