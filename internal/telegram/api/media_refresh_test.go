package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRefreshMediaReferenceChecksPeerAndIdentity(t *testing.T) {
	for _, channel := range []bool{false, true} {
		t.Run(map[bool]string{false: "group", true: "channel"}[channel], func(t *testing.T) {
			dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group"}
			var peer tg.PeerClass = &tg.PeerChat{ChatID: 123}
			if channel {
				dialog = CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", AccessHash: 42}
				peer = &tg.PeerChannel{ChannelID: 123}
			}
			message := &tg.Message{ID: 7, PeerID: peer, Date: 100}
			media := &tg.MessageMediaDocument{}
			media.SetDocument(&tg.Document{ID: 101, DCID: 1, Size: 9, FileReference: []byte("fresh")})
			message.SetMedia(media)
			mock := tgmock.NewRequire(t)
			mock.ExpectFunc(func(input bin.Encoder) {
				if channel {
					_, ok := input.(*tg.ChannelsGetMessagesRequest)
					assert.True(t, ok)
				} else {
					_, ok := input.(*tg.MessagesGetMessagesRequest)
					assert.True(t, ok)
				}
			}).ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
			ref, err := RefreshMediaReference(context.Background(), tg.NewClient(mock), dialog, 7, "document:101")
			require.NoError(t, err)
			assert.Equal(t, []byte("fresh"), ref.Location.(*tg.InputDocumentFileLocation).FileReference)
			assert.True(t, mock.AllWereMet())
		})
	}
}

func TestRefreshMediaReferenceRejectsWrongPeerOrAttachment(t *testing.T) {
	for _, wrongPeer := range []bool{true, false} {
		mock := tgmock.NewRequire(t)
		peer, documentID := int64(123), int64(102)
		wantError := "Telegram media identity changed"
		if wrongPeer {
			peer, documentID = 124, 101
			wantError = "Telegram media message has an unexpected peer"
		}
		message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: peer}, Date: 100}
		media := &tg.MessageMediaDocument{}
		media.SetDocument(&tg.Document{ID: documentID, DCID: 1, Size: 9})
		message.SetMedia(media)
		mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
		_, err := RefreshMediaReference(context.Background(), tg.NewClient(mock), CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group"}, 7, "document:101")
		require.EqualError(t, err, wantError)
		assert.True(t, mock.AllWereMet())
	}
}
