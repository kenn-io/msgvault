package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/telegram"
)

// RefreshCatalog saves a complete unfiltered discovery without advancing any
// history or update position. Missing dialogs retain their prior state.
func RefreshCatalog(ctx context.Context, state *telegram.SyncState, dialogs []CatalogDialog, observedAt time.Time, save func(context.Context, *telegram.SyncState) error) error {
	if state == nil || observedAt.IsZero() || save == nil {
		return errors.New("invalid Telegram catalog refresh")
	}
	if _, err := state.Marshal(); err != nil {
		return err
	}
	next := *state
	next.Dialogs = make(map[string]telegram.DialogState, len(state.Dialogs)+len(dialogs))
	for key, item := range state.Dialogs {
		next.Dialogs[key] = item
	}
	seen := make(map[string]bool, len(dialogs))
	for _, dialog := range dialogs {
		key := strconv.FormatInt(dialog.ChatID, 10)
		if seen[key] {
			return errors.New("duplicate Telegram catalog dialog")
		}
		seen[key] = true
		item := next.Dialogs[key]
		peer := &telegram.DialogPeer{NativeID: dialog.NativeID, Kind: dialog.Kind, AccessHash: dialog.AccessHash, Title: dialog.Title, TopMessage: dialog.TopMessage, EnabledByDefault: dialog.EnabledByDefault, Self: dialog.Self}
		if dialog.HasObservedPTS {
			pts := dialog.ObservedPTS
			peer.ObservedPTS = &pts
		}
		if dialog.Inaccessible {
			if item.Peer != nil && item.Peer.InaccessibleSince != nil {
				inaccessibleSince := *item.Peer.InaccessibleSince
				peer.InaccessibleSince = &inaccessibleSince
			} else {
				inaccessibleSince := observedAt.UTC()
				peer.InaccessibleSince = &inaccessibleSince
			}
		}
		item.Peer = peer
		next.Dialogs[key] = item
	}
	for key, item := range next.Dialogs {
		if seen[key] || item.Peer == nil || item.Peer.MissingSince != nil {
			continue
		}
		peer := *item.Peer
		missing := observedAt.UTC()
		peer.MissingSince = &missing
		item.Peer = &peer
		next.Dialogs[key] = item
	}
	if _, err := next.Marshal(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := save(ctx, &next); err != nil {
		return fmt.Errorf("save Telegram catalog: %w", err)
	}
	*state = next
	return nil
}

// CatalogDialog carries the stable peer information needed by later syncs.
type CatalogDialog struct {
	ChatID                int64                   `json:"chat_id"`
	NativeID              int64                   `json:"native_id"`
	Kind                  string                  `json:"kind"`
	UnsupportedReason     string                  `json:"unsupported_reason,omitempty"`
	AccessHash            int64                   `json:"access_hash,omitempty"`
	Title                 string                  `json:"title,omitempty"`
	TopMessage            int                     `json:"top_message"`
	MessageRanges         []telegram.HistoryRange `json:"-"`
	Inaccessible          bool                    `json:"-"`
	MigrationParentChatID int64                   `json:"-"`
	ObservedPTS           int                     `json:"observed_pts,omitempty"`
	HasObservedPTS        bool                    `json:"has_observed_pts"`
	EnabledByDefault      bool                    `json:"enabled_by_default"`
	Self                  bool                    `json:"self,omitempty"`
	ParticipantCount      int                     `json:"participant_count,omitempty"`
	ParticipantCountKnown bool                    `json:"participant_count_known,omitempty"`
}

// ListDialogsForRanges remembers the split ranges in which Telegram returned
// each dialog, as required for takeout history pagination.
func ListDialogsForRanges(ctx context.Context, client *tg.Client, ranges []telegram.HistoryRange) ([]CatalogDialog, error) {
	if client == nil || len(ranges) == 0 {
		return nil, errors.New("invalid Telegram takeout dialog ranges")
	}
	result := make([]CatalogDialog, 0)
	indices := make(map[int64]int)
	for _, messageRange := range ranges {
		if messageRange.MinID < 0 || messageRange.MaxID <= messageRange.MinID || int64(messageRange.MaxID) > math.MaxInt32 {
			return nil, errors.New("invalid Telegram takeout message range")
		}
		ranged := tg.NewClient(messagesRangeInvoker{
			next: client.Invoker(), range_: tg.MessageRange{MinID: messageRange.MinID, MaxID: messageRange.MaxID},
		})
		dialogs, err := ListDialogs(ctx, ranged)
		if err != nil {
			return nil, err
		}
		for _, dialog := range dialogs {
			if index, ok := indices[dialog.ChatID]; ok {
				result[index].MessageRanges = append(result[index].MessageRanges, messageRange)
				if dialog.TopMessage > result[index].TopMessage {
					result[index].TopMessage = dialog.TopMessage
				}
				continue
			}
			dialog.MessageRanges = []telegram.HistoryRange{messageRange}
			indices[dialog.ChatID] = len(result)
			result = append(result, dialog)
		}
	}
	return result, nil
}

// InputPeer reconstructs the request peer without a gotd peer database.
func (d CatalogDialog) InputPeer() (tg.InputPeerClass, error) {
	if d.NativeID <= 0 {
		return nil, errors.New("invalid Telegram dialog peer ID")
	}
	switch d.Kind {
	case "user":
		if d.Self {
			return &tg.InputPeerSelf{}, nil
		}
		return &tg.InputPeerUser{UserID: d.NativeID, AccessHash: d.AccessHash}, nil
	case "group":
		return &tg.InputPeerChat{ChatID: d.NativeID}, nil
	case "supergroup", "channel":
		return &tg.InputPeerChannel{ChannelID: d.NativeID, AccessHash: d.AccessHash}, nil
	default:
		return nil, errors.New("unsupported Telegram dialog kind")
	}
}

// MapDialog resolves one dialog using the peer entities returned by Telegram.
func MapDialog(dialog *tg.Dialog, users []tg.UserClass, chats []tg.ChatClass) (CatalogDialog, error) {
	var out CatalogDialog
	if dialog == nil {
		return out, errors.New("missing Telegram dialog")
	}
	id, err := telegram.MarkedPeerID(dialog.Peer)
	if err != nil {
		return out, err
	}
	out.ChatID = id
	out.TopMessage = dialog.TopMessage
	out.ObservedPTS, out.HasObservedPTS = dialog.GetPts()
	switch peer := dialog.Peer.(type) {
	case *tg.PeerUser:
		for _, item := range users {
			user, ok := item.(*tg.User)
			if !ok || user == nil || user.ID != peer.UserID {
				continue
			}
			hash, present := user.GetAccessHash()
			if !user.Self && (!present || user.Min) {
				return CatalogDialog{}, errors.New("Telegram user access hash is unavailable")
			}
			out.NativeID, out.Kind, out.AccessHash, out.Self = user.ID, "user", hash, user.Self
			out.ParticipantCount, out.ParticipantCountKnown = 2, true
			if user.Self {
				out.ParticipantCount = 1
			}
			out.Title = strings.TrimSpace(user.FirstName + " " + user.LastName)
			out.EnabledByDefault = true
			return out, nil
		}
	case *tg.PeerChat:
		for _, item := range chats {
			switch chat := item.(type) {
			case *tg.Chat:
				if chat == nil || chat.ID != peer.ChatID {
					continue
				}
				out.NativeID, out.Kind, out.Title = chat.ID, "group", chat.Title
				out.ParticipantCount, out.ParticipantCountKnown = chat.ParticipantsCount, chat.ParticipantsCount > 0
				out.EnabledByDefault = true
				return out, nil
			case *tg.ChatForbidden:
				if chat == nil || chat.ID != peer.ChatID {
					continue
				}
				out.NativeID, out.Kind, out.Title = chat.ID, "group", chat.Title
				out.Inaccessible = true
				out.UnsupportedReason = "Telegram dialog is inaccessible"
				return out, nil
			}
		}
	case *tg.PeerChannel:
		for _, item := range chats {
			switch channel := item.(type) {
			case *tg.Channel:
				if channel == nil || channel.ID != peer.ChannelID {
					continue
				}
				hash, present := channel.GetAccessHash()
				if !present || channel.Min {
					return CatalogDialog{}, errors.New("Telegram channel access hash is unavailable")
				}
				out.NativeID, out.AccessHash, out.Title = channel.ID, hash, channel.Title
				out.ParticipantCount, out.ParticipantCountKnown = channel.GetParticipantsCount()
				out.ParticipantCountKnown = out.ParticipantCountKnown && out.ParticipantCount > 0
				switch {
				case channel.Monoforum:
					rights, present := channel.GetAdminRights()
					if channel.Creator || (present && rights.ManageDirectMessages) {
						out.Kind = "supergroup"
						out.UnsupportedReason = "administrator monoforum requires topic-aware history"
						return out, nil
					}
					out.Kind, out.EnabledByDefault = "supergroup", true
				case channel.Broadcast:
					out.Kind = "channel"
				case channel.Megagroup || channel.Gigagroup:
					out.Kind, out.EnabledByDefault = "supergroup", true
				default:
					return CatalogDialog{}, errors.New("Telegram channel kind is unavailable")
				}
				return out, nil
			case *tg.ChannelForbidden:
				if channel == nil || channel.ID != peer.ChannelID {
					continue
				}
				out.NativeID, out.AccessHash, out.Title = channel.ID, channel.AccessHash, channel.Title
				if channel.Megagroup || channel.Monoforum {
					out.Kind = "supergroup"
				} else {
					out.Kind = "channel"
				}
				out.Inaccessible = true
				out.UnsupportedReason = "Telegram dialog is inaccessible"
				return out, nil
			}
		}
	}
	return CatalogDialog{}, errors.New("Telegram dialog peer is unavailable")
}

// ListDialogs walks both active and archived folders without assuming a short
// page means the catalog is complete.
func ListDialogs(ctx context.Context, client *tg.Client) ([]CatalogDialog, error) {
	var result []CatalogDialog
	seen := make(map[int64]bool)
	for _, folder := range []int{0, 1} {
		req := &tg.MessagesGetDialogsRequest{Limit: 100, OffsetPeer: &tg.InputPeerEmpty{}}
		req.SetFolderID(folder)
		anchors := make(map[struct {
			chatID          int64
			messageID, date int
		}]bool)
		for {
			response, err := client.MessagesGetDialogs(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("list Telegram dialogs: %w", err)
			}
			var dialogs []tg.DialogClass
			var messages []tg.MessageClass
			var users []tg.UserClass
			var chats []tg.ChatClass
			complete := false
			switch page := response.(type) {
			case *tg.MessagesDialogs:
				dialogs, messages, users, chats, complete = page.Dialogs, page.Messages, page.Users, page.Chats, true
			case *tg.MessagesDialogsSlice:
				dialogs, messages, users, chats = page.Dialogs, page.Messages, page.Users, page.Chats
			default:
				return nil, errors.New("unexpected Telegram dialog page")
			}
			var last *CatalogDialog
			for _, raw := range dialogs {
				dialog, ok := raw.(*tg.Dialog)
				if !ok {
					continue
				}
				mapped, err := MapDialog(dialog, users, chats)
				if err != nil {
					return nil, fmt.Errorf("map Telegram dialog: %w", err)
				}
				if !seen[mapped.ChatID] {
					result = append(result, mapped)
					seen[mapped.ChatID] = true
				}
				last = &mapped
			}
			if complete {
				break
			}
			if last == nil {
				return nil, errors.New("Telegram dialog page has no pagination anchor")
			}
			date, ok := dialogTopDate(last.ChatID, last.TopMessage, messages)
			if !ok {
				return nil, errors.New("Telegram dialog page is missing its pagination message")
			}
			peer, err := last.InputPeer()
			if err != nil {
				return nil, err
			}
			anchor := struct {
				chatID          int64
				messageID, date int
			}{last.ChatID, last.TopMessage, date}
			if anchors[anchor] {
				return nil, errors.New("Telegram dialog pagination did not advance")
			}
			anchors[anchor] = true
			req.OffsetID, req.OffsetDate, req.OffsetPeer = last.TopMessage, date, peer
			req.SetExcludePinned(true)
		}
	}
	return result, nil
}

func dialogTopDate(chatID int64, messageID int, messages []tg.MessageClass) (int, bool) {
	for _, raw := range messages {
		var peer tg.PeerClass
		var date, id int
		switch msg := raw.(type) {
		case *tg.Message:
			if msg == nil {
				continue
			}
			peer, date, id = msg.PeerID, msg.Date, msg.ID
		case *tg.MessageService:
			if msg == nil {
				continue
			}
			peer, date, id = msg.PeerID, msg.Date, msg.ID
		default:
			continue
		}
		marked, err := telegram.MarkedPeerID(peer)
		if err == nil && marked == chatID && id == messageID && date > 0 {
			return date, true
		}
	}
	return 0, false
}
