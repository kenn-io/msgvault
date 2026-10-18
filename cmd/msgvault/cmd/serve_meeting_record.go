package cmd

import (
	"context"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) GetMeetingRecordContext(ctx context.Context, scope store.SourceScope, ref string) (store.MeetingRecord, error) {
	return a.store.GetMeetingRecordContext(ctx, scope, ref)
}
func (a *storeAPIAdapter) ListPersonMeetingsContext(ctx context.Context, scope store.SourceScope, q store.PersonMeetingsQuery) ([]store.MeetingRecordSummary, string, error) {
	return a.store.ListPersonMeetingsContext(ctx, scope, q)
}
func (a *storeAPIAdapter) SuggestMeetingLinksContext(ctx context.Context, scope store.SourceScope, ref string, limit int) ([]store.MeetingSuggestion, error) {
	return a.store.SuggestMeetingLinksContext(ctx, scope, ref, limit)
}
func (a *storeAPIAdapter) GetMeetingPersonContext(ctx context.Context, id int64) (*store.Person, error) {
	return a.store.GetMeetingPersonContext(ctx, id)
}
func (a *storeAPIAdapter) ListMeetingPersonContactPointsContext(ctx context.Context, id int64) ([]store.PersonContactPoint, error) {
	return a.store.ListMeetingPersonContactPointsContext(ctx, id)
}
func (a *storeAPIAdapter) MeetingClusterMembersContext(ctx context.Context, id int64) ([]int64, error) {
	return a.store.MeetingClusterMembersContext(ctx, id)
}
func (a *storeAPIAdapter) MeetingPersonForParticipantsContext(ctx context.Context, ids []int64) (*store.Person, error) {
	return a.store.MeetingPersonForParticipantsContext(ctx, ids)
}
func (a *storeAPIAdapter) MeetingParticipantEmailsContext(ctx context.Context, ids []int64) ([]string, error) {
	return a.store.MeetingParticipantEmailsContext(ctx, ids)
}
func (a *storeAPIAdapter) LinkMeetingContext(ctx context.Context, ref string, id int64) error {
	return a.store.LinkMeetingContext(ctx, ref, id)
}
func (a *storeAPIAdapter) UnlinkMeetingContext(ctx context.Context, ref string, id int64) error {
	return a.store.UnlinkMeetingContext(ctx, ref, id)
}

var _ api.MeetingRecordsBackend = (*storeAPIAdapter)(nil)
