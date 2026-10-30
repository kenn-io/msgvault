package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"
)

// CanonicalExportChatID converts Desktop's bare peer ID to the archive key.
// Negative marked IDs are accepted for compatibility with older fixtures.
func CanonicalExportChatID(kind string, id int64) (int64, error) {
	switch kind {
	case chatTypePersonalChat, chatTypeBotChat, chatTypeSavedMessages, chatTypeReplies, chatTypeVerificationCodes:
		if kind == chatTypePersonalChat && id >= constant.MinMTProtoMonoforumID && id <= constant.MaxMTProtoMonoforumID {
			return 0, errors.New("ambiguous Telegram personal chat may be a monoforum; peer evidence is required")
		}
		if id <= 0 {
			return 0, errors.New("invalid Telegram user peer ID")
		}
		return id, nil
	case chatTypePrivateGroup:
		if id < 0 {
			if constant.TDLibPeerID(id).IsChat() {
				return id, nil
			}
			return 0, errors.New("invalid marked Telegram group ID")
		}
		if id == 0 || id > constant.MaxTDLibChatID {
			return 0, errors.New("invalid Telegram group ID")
		}
		var marked constant.TDLibPeerID
		marked.Chat(id)
		return int64(marked), nil
	case chatTypePrivateSupergroup, chatTypePublicSupergroup, chatTypePrivateChannel, chatTypePublicChannel:
		if id < 0 {
			if constant.TDLibPeerID(id).IsChannel() {
				return id, nil
			}
			return 0, errors.New("invalid marked Telegram channel ID")
		}
		if id == 0 || (id > constant.MaxTDLibChannelID && (id < constant.MinMTProtoMonoforumID || id > constant.MaxMTProtoMonoforumID)) {
			return 0, errors.New("invalid Telegram channel ID")
		}
		var marked constant.TDLibPeerID
		marked.Channel(id)
		return int64(marked), nil
	default:
		return 0, fmt.Errorf("unsupported Telegram chat type %q", kind)
	}
}

// MarkedPeerID converts a Telegram API peer to the archive key.
func MarkedPeerID(peer tg.PeerClass) (int64, error) {
	switch p := peer.(type) {
	case *tg.PeerUser:
		if p != nil && p.UserID > 0 {
			return p.UserID, nil
		}
	case *tg.PeerChat:
		if p != nil && p.ChatID > 0 {
			return CanonicalExportChatID(chatTypePrivateGroup, p.ChatID)
		}
	case *tg.PeerChannel:
		if p != nil && p.ChannelID > 0 {
			return CanonicalExportChatID(chatTypePublicChannel, p.ChannelID)
		}
	}
	return 0, errors.New("unsupported Telegram API peer")
}

// ExportReplyPeerID resolves Desktop's explicitly typed reply-to peer.
func ExportReplyPeerID(raw string) (int64, error) {
	for _, entry := range []struct{ prefix, kind string }{{"user", chatTypePersonalChat}, {"chat", chatTypePrivateGroup}, {"channel", chatTypePublicChannel}} {
		if text, ok := strings.CutPrefix(raw, entry.prefix); ok {
			id, err := strconv.ParseInt(text, 10, 64)
			if err != nil || id <= 0 || strconv.FormatInt(id, 10) != text {
				return 0, errors.New("invalid Telegram reply peer")
			}
			// An explicit user prefix disambiguates the monoforum ID range.
			if entry.prefix == "user" {
				return id, nil
			}
			return CanonicalExportChatID(entry.kind, id)
		}
	}
	return 0, errors.New("unsupported Telegram reply peer")
}
