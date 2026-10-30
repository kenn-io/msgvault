package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

// ArchiveAPIMessage writes one API message through the shared export writer.
func ArchiveAPIMessage(ctx context.Context, st *store.Store, sourceID, ownerID int64, ownerName string, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass) error {
	return archiveAPIMessage(ctx, ownerID, dialog, msg, users, chats, func(chat telegram.Chat, message telegram.Message) error {
		return telegram.WriteLiveMessage(ctx, st, sourceID, ownerID, ownerName, chat, message)
	})
}

// ArchiveAPIMessageWithWriter reuses a run's preflight and participant cache.
// The caller resolves page-local replies before committing the checkpoint.
func ArchiveAPIMessageWithWriter(ctx context.Context, writer *telegram.LiveWriter, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass) error {
	if writer == nil {
		return errors.New("missing Telegram live writer")
	}
	if msg == nil {
		return errors.New("missing Telegram API message")
	}
	if _, edited := msg.GetEditDate(); edited {
		return ApplyAPIEditAuthoritative(ctx, writer, dialog, msg, users, chats)
	}
	return archiveAPIMessage(ctx, writer.OwnerID(), dialog, msg, users, chats, func(chat telegram.Chat, message telegram.Message) error {
		return writer.Write(ctx, chat, message)
	})
}

// ApplyAPIEdit validates the typed peer before applying a live edit.
func ApplyAPIEdit(ctx context.Context, writer *telegram.LiveWriter, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass) error {
	return applyAPIEdit(ctx, writer, dialog, msg, users, chats, false)
}

// ApplyAPIEditAuthoritative applies an edited message fetched as a current
// history snapshot, which can resolve same-second edit ambiguity.
func ApplyAPIEditAuthoritative(ctx context.Context, writer *telegram.LiveWriter, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass) error {
	return applyAPIEdit(ctx, writer, dialog, msg, users, chats, true)
}

func applyAPIEdit(ctx context.Context, writer *telegram.LiveWriter, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass, authoritative bool) error {
	if writer == nil || msg == nil {
		return errors.New("invalid Telegram edit")
	}
	if editDate, ok := msg.GetEditDate(); !ok || editDate <= 0 {
		return errors.New("Telegram edit has no edit timestamp")
	}
	return archiveAPIMessage(ctx, writer.OwnerID(), dialog, msg, users, chats, func(chat telegram.Chat, message telegram.Message) error {
		if authoritative {
			return writer.WriteEditAuthoritative(ctx, chat, message)
		}
		return writer.WriteEdit(ctx, chat, message)
	})
}

// ApplyAPIEditWithClient retries a second-resolution timestamp conflict from
// the current server message so an older update cannot replace a newer
// archived snapshot or stall the update checkpoint.
func ApplyAPIEditWithClient(ctx context.Context, client *tg.Client, writer *telegram.LiveWriter, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass) error {
	if client == nil {
		return errors.New("missing Telegram client for edit refresh")
	}
	err := ApplyAPIEdit(ctx, writer, dialog, msg, users, chats)
	if !errors.Is(err, store.ErrTelegramEditTimestampConflict) {
		return err
	}
	current, currentUsers, currentChats, found, err := getTelegramMessage(ctx, client, dialog, msg.ID)
	if err != nil {
		return fmt.Errorf("refresh conflicting Telegram edit: %w", err)
	}
	if !found {
		// Telegram no longer returns this message. Keep the archived snapshot;
		// a deletion update can still mark it without replaying a stale edit.
		return nil
	}
	return ApplyAPIEditAuthoritative(ctx, writer, dialog, current, currentUsers, currentChats)
}

func getTelegramMessage(ctx context.Context, client *tg.Client, dialog CatalogDialog, messageID int) (*tg.Message, []tg.UserClass, []tg.ChatClass, bool, error) {
	if client == nil || messageID <= 0 {
		return nil, nil, nil, false, errors.New("invalid Telegram message refresh")
	}
	if int64(messageID) > math.MaxInt32 {
		return nil, nil, nil, false, errors.New("invalid Telegram message refresh ID")
	}
	offsetID := messageID + 1
	if int64(messageID) == math.MaxInt32 {
		offsetID = 0
	}
	page, err := GetHistoryPage(ctx, client, dialog, offsetID, messageID-1, messageID)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("get Telegram message: %w", err)
	}
	for _, item := range page.Messages {
		switch current := item.(type) {
		case *tg.Message:
			if current != nil && current.ID == messageID {
				return current, page.Users, page.Chats, true, nil
			}
		case *tg.MessageEmpty:
			if current != nil && current.ID == messageID {
				return nil, page.Users, page.Chats, false, nil
			}
		}
	}
	return nil, page.Users, page.Chats, false, nil
}

func archiveAPIMessage(ctx context.Context, ownerID int64, dialog CatalogDialog, msg *tg.Message, users []tg.UserClass, chats []tg.ChatClass, write func(telegram.Chat, telegram.Message) error) error {
	if msg == nil {
		return errors.New("missing Telegram API message")
	}
	mapped, err := MapAPIMessage(msg, ownerID, users, chats)
	if err != nil {
		return err
	}
	peerID, err := telegram.MarkedPeerID(msg.PeerID)
	if err != nil || peerID != dialog.ChatID {
		return errors.New("Telegram message does not match dialog")
	}
	var peer tg.PeerClass
	var kind string
	switch dialog.Kind {
	case "user":
		peer, kind = &tg.PeerUser{UserID: dialog.NativeID}, "personal_chat"
	case "group":
		peer, kind = &tg.PeerChat{ChatID: dialog.NativeID}, "private_group"
	case "supergroup":
		peer, kind = &tg.PeerChannel{ChannelID: dialog.NativeID}, "private_supergroup"
	case "channel":
		peer, kind = &tg.PeerChannel{ChannelID: dialog.NativeID}, "private_channel"
	default:
		return errors.New("unsupported Telegram dialog kind")
	}
	expected, err := telegram.MarkedPeerID(peer)
	if err != nil || expected != peerID {
		return errors.New("invalid Telegram dialog identity")
	}
	mapped.Message.RawJSON = mapped.RawJSON
	mapped.Message.EditedAt = mapped.EditedAt
	mapped.Message.ReplyMarkedPeerID = mapped.ReplyChatID
	if media, present := msg.GetMedia(); present {
		ref, err := MediaReferenceFromMessage(media)
		if err != nil {
			return fmt.Errorf("map Telegram media: %w", err)
		}
		if ref != nil {
			mapped.Message.LiveMedia, err = ref.liveMedia()
			if err != nil {
				return err
			}
		}
	}
	return write(telegram.Chat{ID: peerID, Type: kind, Name: dialog.Title}, mapped.Message)
}

// MappedMessage carries transport-independent fields and Telegram-only metadata.
type MappedMessage struct {
	Message     telegram.Message
	ReplyChatID int64
	EditedAt    time.Time
	RawJSON     string
}

// MapAPIMessage maps a regular API message without writing it to the archive.
func MapAPIMessage(msg *tg.Message, ownerID int64, users []tg.UserClass, chats []tg.ChatClass) (MappedMessage, error) {
	var out MappedMessage
	if msg == nil || msg.ID <= 0 || msg.Date <= 0 || ownerID <= 0 {
		return out, errors.New("invalid Telegram message")
	}
	chatID, err := telegram.MarkedPeerID(msg.PeerID)
	if err != nil {
		return out, fmt.Errorf("map Telegram message peer: %w", err)
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return out, fmt.Errorf("encode Telegram message metadata: %w", err)
	}
	out.RawJSON = string(raw)
	out.Message.ID = int64(msg.ID)
	out.Message.SentAt = time.Unix(int64(msg.Date), 0).UTC()
	out.Message.Text = msg.Message
	if editDate, ok := msg.GetEditDate(); ok {
		if editDate <= 0 {
			return MappedMessage{}, errors.New("invalid Telegram edit date")
		}
		out.Message.Edited = true
		out.EditedAt = time.Unix(int64(editDate), 0).UTC()
	}
	sender, hasSender := msg.GetFromID()
	if !hasSender {
		if msg.Out {
			sender, hasSender = &tg.PeerUser{UserID: ownerID}, true
		} else if peer, ok := msg.PeerID.(*tg.PeerUser); ok && peer != nil && peer.UserID > 0 && peer.UserID != ownerID {
			sender, hasSender = peer, true
		}
	}
	if hasSender {
		switch p := sender.(type) {
		case *tg.PeerUser:
			if p == nil || p.UserID <= 0 {
				return MappedMessage{}, errors.New("invalid Telegram sender")
			}
			out.Message.FromID = fmt.Sprintf("user%d", p.UserID)
			for _, item := range users {
				if u, ok := item.(*tg.User); ok && u != nil && u.ID == p.UserID {
					out.Message.FromName = strings.TrimSpace(u.FirstName + " " + u.LastName)
					break
				}
			}
		case *tg.PeerChat:
			if p == nil || p.ChatID <= 0 {
				return MappedMessage{}, errors.New("invalid Telegram sender")
			}
			out.Message.FromID = fmt.Sprintf("chat%d", p.ChatID)
			for _, item := range chats {
				if c, ok := item.(*tg.Chat); ok && c != nil && c.ID == p.ChatID {
					out.Message.FromName = c.Title
					break
				}
			}
		case *tg.PeerChannel:
			if p == nil || p.ChannelID <= 0 {
				return MappedMessage{}, errors.New("invalid Telegram sender")
			}
			out.Message.FromID = fmt.Sprintf("channel%d", p.ChannelID)
			for _, item := range chats {
				if c, ok := item.(*tg.Channel); ok && c != nil && c.ID == p.ChannelID {
					out.Message.FromName = c.Title
					break
				}
			}
		default:
			return MappedMessage{}, errors.New("unsupported Telegram sender")
		}
	}
	if reply, ok := msg.GetReplyTo(); ok {
		if header, ok := reply.(*tg.MessageReplyHeader); ok && header != nil {
			if id, present := header.GetReplyToMsgID(); present {
				if id <= 0 {
					return MappedMessage{}, errors.New("invalid Telegram reply")
				}
				out.Message.ReplyTo, out.ReplyChatID = int64(id), chatID
				if peer, present := header.GetReplyToPeerID(); present {
					out.ReplyChatID, err = telegram.MarkedPeerID(peer)
					if err != nil {
						return MappedMessage{}, fmt.Errorf("map Telegram reply peer: %w", err)
					}
				}
			}
		}
	}
	return out, nil
}
