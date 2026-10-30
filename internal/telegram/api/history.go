package api

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// HistoryPage carries one page and the offset to use after its writes commit.
type HistoryPage struct {
	Messages   []tg.MessageClass
	Users      []tg.UserClass
	Chats      []tg.ChatClass
	NextOffset int
	Empty      bool
}

// GetHistoryPage fetches a page without advancing any durable state.
func GetHistoryPage(ctx context.Context, client *tg.Client, dialog CatalogDialog, offsetID, minID, ceiling int) (HistoryPage, error) {
	var page HistoryPage
	if offsetID < 0 || minID < 0 || int64(offsetID) > math.MaxInt32 || int64(minID) > math.MaxInt32 || ceiling <= 0 || int64(ceiling) > math.MaxInt32 {
		return page, errors.New("invalid Telegram history bounds")
	}
	peer, err := dialog.InputPeer()
	if err != nil {
		return page, err
	}
	var native tg.PeerClass
	switch dialog.Kind {
	case "user":
		native = &tg.PeerUser{UserID: dialog.NativeID}
	case "group":
		native = &tg.PeerChat{ChatID: dialog.NativeID}
	case "supergroup", "channel":
		native = &tg.PeerChannel{ChannelID: dialog.NativeID}
	}
	expected, err := telegram.MarkedPeerID(native)
	if err != nil || (dialog.ChatID != 0 && dialog.ChatID != expected) {
		return page, errors.New("invalid Telegram dialog peer")
	}
	maxID := 0
	if int64(ceiling) < math.MaxInt32 {
		maxID = ceiling + 1
	}
	response, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offsetID, MinID: minID, MaxID: maxID, Limit: 100})
	if err != nil {
		return page, fmt.Errorf("get Telegram history: %w", err)
	}
	switch result := response.(type) {
	case *tg.MessagesMessages:
		page.Messages, page.Users, page.Chats = result.Messages, result.Users, result.Chats
	case *tg.MessagesMessagesSlice:
		page.Messages, page.Users, page.Chats = result.Messages, result.Users, result.Chats
	case *tg.MessagesChannelMessages:
		page.Messages, page.Users, page.Chats = result.Messages, result.Users, result.Chats
	default:
		return page, errors.New("unexpected Telegram history page")
	}
	if len(page.Messages) == 0 {
		page.Empty = true
		return page, nil
	}
	for _, message := range page.Messages {
		var msgPeer tg.PeerClass
		var id int
		switch m := message.(type) {
		case *tg.Message:
			if m == nil {
				return HistoryPage{}, errors.New("Telegram history contains a nil message")
			}
			id, msgPeer = m.ID, m.PeerID
			if msgPeer == nil {
				return HistoryPage{}, errors.New("Telegram history message has no peer")
			}
		case *tg.MessageService:
			if m == nil {
				return HistoryPage{}, errors.New("Telegram history contains a nil message")
			}
			id, msgPeer = m.ID, m.PeerID
			if msgPeer == nil {
				return HistoryPage{}, errors.New("Telegram history message has no peer")
			}
		case *tg.MessageEmpty:
			if m == nil {
				return HistoryPage{}, errors.New("Telegram history contains a nil message")
			}
			id = m.ID
			msgPeer, _ = m.GetPeerID()
		default:
			return HistoryPage{}, errors.New("unsupported Telegram history message")
		}
		if msgPeer != nil {
			marked, err := telegram.MarkedPeerID(msgPeer)
			if err != nil || marked != expected {
				return HistoryPage{}, errors.New("Telegram history message has an unexpected peer")
			}
		}
		if id <= minID || id > ceiling || (offsetID > 0 && id >= offsetID) {
			return HistoryPage{}, errors.New("Telegram history pagination did not advance")
		}
		if page.NextOffset == 0 || id < page.NextOffset {
			page.NextOffset = id
		}
	}
	return page, nil
}
