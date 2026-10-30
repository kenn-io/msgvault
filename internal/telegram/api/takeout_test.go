package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTakeoutWrapsOnlyHistoryRequests(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.AccountInitTakeoutSessionRequest)
		require.True(t, ok)
		assert.True(t, req.MessageUsers)
		assert.True(t, req.MessageChats)
		assert.True(t, req.MessageMegagroups)
		assert.True(t, req.MessageChannels)
		assert.False(t, req.Contacts)
		assert.False(t, req.Files)
	}).ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(t, ok)
		assert.Equal(t, int64(42), wrapped.TakeoutID)
		_, ok = wrapped.Query.(*tg.MessagesGetHistoryRequest)
		assert.True(t, ok)
	}).ThenResult(&tg.MessagesMessages{})
	client, err := startTakeout(context.Background(), tg.NewClient(mock))
	{
		require := require.New(t)
		require.NoError(err)
	}
	page, err := GetHistoryPage(context.Background(), client, CatalogDialog{Kind: "group", NativeID: 123}, 0, 0, 5)
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.True(page.Empty)
	}
	assert.True(t, mock.AllWereMet())
}

func TestTakeoutDelayIsReportedWithoutRetry(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenRPCErr(tgerr.New(420, "TAKEOUT_INIT_DELAY_60"))
	_, err := startTakeout(context.Background(), tg.NewClient(mock))
	require.ErrorContains(t, err, "1m")
	assert.True(t, mock.AllWereMet())
}

func TestTakeoutWrapsSplitRangeDiscoveryAndFinish(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(t, ok)
		assert.Equal(t, int64(42), wrapped.TakeoutID)
		assert.IsType(t, &tg.MessagesGetSplitRangesRequest{}, wrapped.Query)
	}).ThenResult(&tg.MessageRangeVector{Elems: []tg.MessageRange{{MinID: 1, MaxID: 10}}})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(t, ok)
		assert.Equal(t, int64(42), wrapped.TakeoutID)
		finish, ok := wrapped.Query.(*tg.AccountFinishTakeoutSessionRequest)
		require.True(t, ok)
		assert.True(t, finish.Success)
	}).ThenResult(&tg.BoolTrue{})
	err := WithTakeout(context.Background(), tg.NewClient(mock), func(ctx context.Context, client *tg.Client) error {
		ranges, err := client.MessagesGetSplitRanges(ctx)
		require.NoError(t, err)
		assert.Equal(t, []tg.MessageRange{{MinID: 1, MaxID: 10}}, ranges)
		return nil
	})
	require.NoError(t, err)
	assert.True(t, mock.AllWereMet())
}

func TestTakeoutFinishesAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(t, ok)
		assert.Equal(t, int64(42), wrapped.TakeoutID)
		req, ok := wrapped.Query.(*tg.AccountFinishTakeoutSessionRequest)
		require.True(t, ok)
		assert.False(t, req.Success)
	}).ThenResult(&tg.BoolTrue{})
	err := WithTakeout(ctx, tg.NewClient(mock), func(ctx context.Context, _ *tg.Client) error {
		cancel()
		return ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, mock.AllWereMet())
}

func TestTakeoutFinishFailureIsReturned(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.AccountTakeout{ID: 42})
	mock.ExpectFunc(func(input bin.Encoder) {
		wrapped, ok := input.(*tg.InvokeWithTakeoutRequest)
		require.True(t, ok)
		assert.Equal(t, int64(42), wrapped.TakeoutID)
		req, ok := wrapped.Query.(*tg.AccountFinishTakeoutSessionRequest)
		require.True(t, ok)
		assert.True(t, req.Success)
	}).ThenRPCErr(tgerr.New(500, "INTERNAL"))
	err := WithTakeout(context.Background(), tg.NewClient(mock), func(context.Context, *tg.Client) error { return nil })
	require.ErrorContains(t, err, "finish Telegram takeout")
	assert.True(t, mock.AllWereMet())
}
