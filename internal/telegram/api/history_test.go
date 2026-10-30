package api

import (
	"context"
	"strconv"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryPageUsesResumableBounds(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.ExpectFunc(func(input bin.Encoder) {
		req, ok := input.(*tg.MessagesGetHistoryRequest)
		require.True(t, ok)
		assert.Equal(t, &tg.InputPeerChat{ChatID: 123}, req.Peer)
		assert.Equal(t, 50, req.OffsetID)
		assert.Equal(t, 10, req.MinID)
		assert.Equal(t, 100, req.Limit)
		assert.Equal(t, 61, req.MaxID)
	}).ThenResult(&tg.MessagesMessagesSlice{Count: 2, Messages: []tg.MessageClass{
		&tg.Message{ID: 40, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"},
		&tg.MessageEmpty{ID: 39},
	}})
	page, err := GetHistoryPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, 50, 10, 60)
	{
		require := require.New(t)
		require.NoError(err)
	}
	{
		assert := assert.New(t)
		assert.Equal(39, page.NextOffset)
	}
	assert.False(t, page.Empty)
	require.Len(t, page.Messages, 2)
	assert.True(t, mock.AllWereMet())
}

func TestHistoryPageEmptyAndStalled(t *testing.T) {
	dialog := CatalogDialog{Kind: "group", NativeID: 123}
	t.Run("empty", func(t *testing.T) {
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.MessagesMessages{})
		page, err := GetHistoryPage(context.Background(), tg.NewClient(mock), dialog, 50, 0, 60)
		require.NoError(t, err)
		assert.True(t, page.Empty)
	})
	t.Run("stalled", func(t *testing.T) {
		mock := tgmock.NewRequire(t)
		mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.MessageEmpty{ID: 50}}})
		_, err := GetHistoryPage(context.Background(), tg.NewClient(mock), dialog, 50, 0, 60)
		require.ErrorContains(t, err, "did not advance")
	})
}

func TestHistoryPageRejectsMessagesFromAnotherPeer(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 12, PeerID: &tg.PeerUser{UserID: 999}, Date: 100}}})
	_, err := GetHistoryPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123, ChatID: -123}, 50, 0, 60)
	require.ErrorContains(t, err, "peer")
}

func TestHistoryPageRejectsMissingMessagePeer(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 12}}})
	_, err := GetHistoryPage(context.Background(), tg.NewClient(mock), CatalogDialog{Kind: "group", NativeID: 123}, 50, 0, 60)
	require.ErrorContains(t, err, "peer")
}

func TestHistoryPageRejectsOutOfRangeBounds(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot represent out-of-range Telegram IDs")
	}
	tooLarge := int64(1) << 31
	dialog := CatalogDialog{Kind: "group", NativeID: 123}
	for _, bounds := range [][3]int{{int(tooLarge), 0, 60}, {0, int(tooLarge), 60}} {
		_, err := GetHistoryPage(context.Background(), tg.NewClient(tgmock.NewRequire(t)), dialog, bounds[0], bounds[1], bounds[2])
		require.ErrorContains(t, err, "bounds")
	}
}
