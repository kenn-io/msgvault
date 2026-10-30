package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// RefreshMediaReference obtains a fresh, peer-checked file reference from the
// original message. It never substitutes a different attachment identity.
func RefreshMediaReference(ctx context.Context, client *tg.Client, dialog CatalogDialog, messageID int, sourceID string) (*MediaReference, error) {
	if client == nil || messageID <= 0 || int64(messageID) > math.MaxInt32 || sourceID == "" {
		return nil, errors.New("invalid Telegram media refresh request")
	}
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}}
	var response tg.MessagesMessagesClass
	var err error
	switch dialog.Kind {
	case "user", "group":
		response, err = client.MessagesGetMessages(ctx, ids)
	case "channel", "supergroup":
		if dialog.NativeID <= 0 {
			return nil, errors.New("invalid Telegram channel")
		}
		response, err = client.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: &tg.InputChannel{ChannelID: dialog.NativeID, AccessHash: dialog.AccessHash}, ID: ids})
	default:
		return nil, errors.New("unsupported Telegram dialog kind")
	}
	if err != nil {
		return nil, fmt.Errorf("refresh Telegram media message: %w", err)
	}
	var messages []tg.MessageClass
	switch result := response.(type) {
	case *tg.MessagesMessages:
		if result != nil {
			messages = result.Messages
		}
	case *tg.MessagesMessagesSlice:
		if result != nil {
			messages = result.Messages
		}
	case *tg.MessagesChannelMessages:
		if result != nil {
			messages = result.Messages
		}
	default:
		return nil, errors.New("unexpected Telegram media refresh response")
	}
	if len(messages) != 1 {
		return nil, errors.New("Telegram media message is unavailable")
	}
	message, ok := messages[0].(*tg.Message)
	if !ok || message == nil || message.ID != messageID {
		return nil, errors.New("Telegram media message is unavailable")
	}
	peer, err := telegram.MarkedPeerID(message.PeerID)
	if err != nil || peer != dialog.ChatID {
		return nil, errors.New("Telegram media message has an unexpected peer")
	}
	media, present := message.GetMedia()
	if !present {
		return nil, errors.New("Telegram media is unavailable")
	}
	ref, err := MediaReferenceFromMessage(media)
	if err != nil {
		return nil, err
	}
	if ref == nil {
		return nil, errors.New("Telegram media is unavailable")
	}
	live, err := ref.liveMedia()
	if err != nil {
		return nil, err
	}
	if live.SourceID != sourceID {
		return nil, errors.New("Telegram media identity changed")
	}
	return ref, nil
}
