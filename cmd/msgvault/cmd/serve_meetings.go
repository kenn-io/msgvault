package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) GetMeetingContextContext(
	ctx context.Context, scope store.MeetingQueryScope, options meetingcontent.PacketOptions,
) (*meetingcontent.PacketResult, error) {
	return a.store.GetMeetingContextContext(ctx, scope, options)
}

func (a *storeAPIAdapter) ListMeetingActionsContext(
	ctx context.Context, request store.MeetingActionsQuery,
) (*meetingcontent.ActionsPage, error) {
	return a.store.ListMeetingActionsContext(ctx, request)
}

func (a *storeAPIAdapter) GetMeetingMetricsContext(
	ctx context.Context, scope store.MeetingQueryScope,
) (*meetingcontent.Metrics, error) {
	return a.store.GetMeetingMetricsContext(ctx, scope)
}

var _ api.MeetingStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) LinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error {
	return a.store.LinkMeetingContext(ctx, memberID, ref)
}

func (a *storeAPIAdapter) UnlinkMeetingContext(ctx context.Context, memberID int64, ref store.OccurrenceRef) error {
	return a.store.UnlinkMeetingContext(ctx, memberID, ref)
}

func (a *storeAPIAdapter) SuggestMeetingLinksContext(ctx context.Context, ref store.MeetingLinkRef, limit int) ([]store.MeetingSuggestion, error) {
	return a.store.SuggestMeetingLinksContext(ctx, ref, limit)
}

var _ api.MeetingLinkStore = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) MeetingLinksContext(ctx context.Context, messageID int64) ([]store.MeetingLink, error) {
	return a.store.MeetingLinksContext(ctx, messageID)
}
