package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/gotd/td/constant"
	"go.kenn.io/msgvault/internal/store"
)

var errUnverifiedExportPeer = errors.New("unverified ambiguous Telegram export peer")

// normalizedExportID preserves already marked IDs from older fixtures while
// converting Desktop's positive IDs using the export's explicit peer kind.
func normalizedExportID(chat *Chat) (int64, error) {
	if chat == nil || chat.ID == 0 {
		return 0, errors.New("telegram: missing export chat ID")
	}
	if chat.ID < 0 {
		switch chat.Type {
		case chatTypePersonalChat, chatTypeBotChat, chatTypeSavedMessages, chatTypeReplies, chatTypeVerificationCodes:
			return 0, fmt.Errorf("telegram: %s %d carries no peer user id", chat.Type, chat.ID)
		default:
			return chat.ID, nil
		}
	}
	id, err := CanonicalExportChatID(chat.Type, chat.ID)
	if err != nil {
		return 0, fmt.Errorf("telegram: cannot identify export chat kind: %w", err)
	}
	return id, nil
}

func normalizedImportID(ctx context.Context, st *store.Store, sourceID int64, chat *Chat) (int64, error) {
	id, err := normalizedExportID(chat)
	if err == nil {
		return id, nil
	}
	if chat == nil || chat.Type != chatTypePersonalChat || chat.ID < constant.MinMTProtoMonoforumID || chat.ID > constant.MaxMTProtoMonoforumID {
		return 0, err
	}
	var evidence []store.TelegramExportKeyEvidence
	for _, message := range chat.Messages {
		if len(evidence) == 1000 {
			break
		}
		if message.ID != 0 && !message.Service && !message.Unsupported && !message.SentAt.IsZero() && message.Text != "" {
			evidence = append(evidence, store.TelegramExportKeyEvidence{MessageID: message.ID, SentAt: message.SentAt, Text: message.Text})
		}
	}
	userErr := st.CheckVerifiedTelegramUserExportContext(ctx, sourceID, chat.ID, evidence)
	var marked constant.TDLibPeerID
	marked.Channel(chat.ID)
	channelErr := st.CheckVerifiedTelegramChannelExportContext(ctx, sourceID, chat.ID, int64(marked), evidence)
	expected := func(err error) bool {
		return err == nil || errors.Is(err, store.ErrTelegramPeerUnverified) || errors.Is(err, store.ErrTelegramEvidenceInsufficient)
	}
	if !expected(userErr) {
		return 0, userErr
	}
	if !expected(channelErr) {
		return 0, channelErr
	}
	if userErr == nil && channelErr == nil {
		return 0, fmt.Errorf("telegram: ambiguous personal chat matches both a user and a channel: %w", errUnverifiedExportPeer)
	}
	if userErr == nil {
		return chat.ID, nil
	}
	if channelErr == nil {
		chat.Type = chatTypePrivateSupergroup
		return int64(marked), nil
	}
	return 0, fmt.Errorf("telegram: ambiguous personal chat needs peer verification; run sync-telegram with access to the chat, then retry import: %w", errors.Join(errUnverifiedExportPeer, userErr, channelErr))
}

// exportKeyChanges reads peer-kind evidence before mutating any legacy key.
// Retained evidence is bounded per chat; a source without enough matching
// messages is left unchanged and requires a more complete matching export.
func exportKeyChanges(ctx context.Context, st *store.Store, sourceID int64, export *ExportReader) ([]store.TelegramExportKeyChange, error) {
	var changes []store.TelegramExportKeyChange
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chat, err := export.NextChat()
		if errors.Is(err, io.EOF) {
			return changes, nil
		}
		if err != nil {
			return nil, err
		}
		if chat.ID <= 0 {
			continue
		}
		marked, err := normalizedImportID(ctx, st, sourceID, chat)
		if errors.Is(err, errUnverifiedExportPeer) {
			continue
		}
		if err != nil {
			return nil, err
		}
		change := store.TelegramExportKeyChange{LegacyID: chat.ID, MarkedID: marked, ConversationType: chat.ConvType()}
		if chat.Type == chatTypeReplies || chat.Type == chatTypeVerificationCodes {
			change.AllowRetype = true
		}
		if marked != chat.ID || change.AllowRetype {
			change.ReplyEvidence = make(map[int64]store.TelegramExportReplyEvidence)
			for _, msg := range chat.Messages {
				if msg.ReplyTo != 0 && msg.ReplyPeerID != "" {
					peer, err := ExportReplyPeerID(msg.ReplyPeerID)
					if err != nil {
						return nil, fmt.Errorf("telegram: invalid export reply peer: %w", err)
					}
					if peer != marked {
						change.HasCrossChatReplies = true
					}
				}
				if msg.ReplyTo != 0 && msg.ID != 0 && !msg.Service && !msg.Unsupported {
					evidence := store.TelegramExportReplyEvidence{ReplyID: msg.ReplyTo, SentAt: msg.SentAt, Text: msg.Text}
					if previous, exists := change.ReplyEvidence[msg.ID]; exists && (previous.ReplyID != evidence.ReplyID || !previous.SentAt.Equal(evidence.SentAt) || previous.Text != evidence.Text) {
						return nil, errors.New("telegram: conflicting export reply evidence")
					}
					if len(change.ReplyEvidence) >= 10000 {
						change.ReplyEvidenceOverflow = true
					} else {
						change.ReplyEvidence[msg.ID] = evidence
					}
				}
				if len(change.Evidence) < 1000 && !msg.Service && !msg.Unsupported && msg.ID != 0 && !msg.SentAt.IsZero() && msg.Text != "" {
					change.Evidence = append(change.Evidence, store.TelegramExportKeyEvidence{MessageID: msg.ID, SentAt: msg.SentAt, Text: msg.Text})
				}
			}
		}
		changes = append(changes, change)
	}
}
