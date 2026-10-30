package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelDifferenceKeepsResponseAndFinalFlag(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.UpdatesGetChannelDifferenceRequest)
		require.True(t, ok)
		assert.Equal(t, &tg.InputChannel{ChannelID: 123, AccessHash: 456}, req.Channel)
		assert.Equal(t, 10, req.Pts)
		assert.Equal(t, 100, req.Limit)
	}).ThenResult(&tg.UpdatesChannelDifference{Pts: 12, Final: false, NewMessages: []tg.MessageClass{&tg.MessageEmpty{ID: 2}}})
	page, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123, AccessHash: 456}, 10)
	{
		require := require.New(t)
		require.NoError(err)
	}
	{
		assert := assert.New(t)
		assert.True(page.More)
	}
	require.NotNil(t, page.NextPTS)
	assert.Equal(t, 12, *page.NextPTS)
	result, ok := page.Response.(*tg.UpdatesChannelDifference)
	require.True(t, ok)
	assert.Len(t, result.NewMessages, 1)
}

func TestChannelDifferenceTooLongRetainsMessagesWithoutCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	dialog := &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}}
	dialog.SetPts(30)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceTooLong{Dialog: dialog, Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 5}}})
	page, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123, AccessHash: 456}, 10)
	require.NoError(err)
	assert.True(page.TooLong)
	assert.Nil(page.NextPTS)
	require.NotNil(page.RecoveryPTS)
	assert.Equal(30, *page.RecoveryPTS)
	response, ok := page.Response.(*tg.UpdatesChannelDifferenceTooLong)
	require.True(ok)
	assert.Len(response.Messages, 1)
}

func TestChannelDifferenceTooLongWithMissingPTSRequiresRecovery(t *testing.T) {
	assert := assert.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceTooLong{Dialog: &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}}})
	page, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "supergroup", NativeID: 123, AccessHash: 456}, 10)
	require.NoError(t, err)
	assert.True(page.TooLong)
	assert.Nil(page.NextPTS)
	assert.Nil(page.RecoveryPTS)
}

func TestChannelDifferenceRejectsUnexpectedRecoveryPeer(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifferenceTooLong{Dialog: &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 999}}})
	_, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123, AccessHash: 456}, 10)
	require.ErrorContains(t, err, "peer")
}

func TestChannelDifferenceRejectsNonAdvancingPage(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 10, Final: false})
	_, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123}, 10)
	require.ErrorContains(t, err, "advance")
}

func TestCommitChannelDifferenceWritesBeforeCheckpoint(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 12, Final: true})
	page, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123}, 10)
	require.NoError(t, err)
	pts := 10
	wrote := false
	err = CommitChannelDifference(context.Background(), page, page.ChatID, &pts,
		func(context.Context, tg.UpdatesChannelDifferenceClass) error { wrote = true; return nil },
		func(_ context.Context, next int) error {
			assert.True(t, wrote)
			assert.Equal(t, 12, next)
			assert.Equal(t, 10, pts)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 12, pts)
}

func TestCommitChannelDifferenceFailureOrWrongPeerLeavesCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UpdatesChannelDifference{Pts: 12, Final: true})
	page, err := GetChannelDifferencePage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "channel", NativeID: 123}, 10)
	require.NoError(err)
	fail := errors.New("synthetic failure")
	pts := 10
	saved := false
	err = CommitChannelDifference(context.Background(), page, page.ChatID, &pts,
		func(context.Context, tg.UpdatesChannelDifferenceClass) error { return fail },
		func(context.Context, int) error { saved = true; return nil })
	require.ErrorIs(err, fail)
	assert.False(saved)
	assert.Equal(10, pts)
	applied := false
	err = CommitChannelDifference(context.Background(), page, page.ChatID-1, &pts,
		func(context.Context, tg.UpdatesChannelDifferenceClass) error { applied = true; return nil },
		func(context.Context, int) error { return nil })
	require.Error(err)
	assert.False(applied)
}
