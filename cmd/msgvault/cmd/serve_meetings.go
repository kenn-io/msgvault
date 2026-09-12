package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) GetMeetingContextContext(
	ctx context.Context, ids []int64, options meetingcontent.PacketOptions,
) (*meetingcontent.PacketResult, error) {
	return a.store.GetMeetingContextContext(ctx, ids, options)
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
