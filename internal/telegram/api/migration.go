package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
)

type ambiguousPeerEvidence struct {
	dialog    CatalogDialog
	messages  []store.TelegramExportKeyEvidence
	replies   map[int64]store.TelegramExportReplyEvidence
	crossChat bool
}

// RepairAmbiguousPeers compares every typed catalog candidate for each legacy
// ID before repairing it. The caller verifies the account and owns the run.
func RepairAmbiguousPeers(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialogs []CatalogDialog) error {
	if st == nil || client == nil || sourceID <= 0 {
		return errors.New("invalid Telegram peer repair")
	}
	ids, err := st.AmbiguousTelegramDirectKeysContext(ctx, sourceID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var matches []ambiguousPeerEvidence
		blocked := false
		for _, dialog := range dialogs {
			if dialog.NativeID != id || (dialog.Kind != "user" && dialog.Kind != "supergroup" && dialog.Kind != "channel") {
				continue
			}
			if dialog.Inaccessible || dialog.UnsupportedReason != "" {
				blocked = true
				continue
			}
			candidate, err := collectAmbiguousPeerEvidence(ctx, client, dialog)
			if err != nil {
				return err
			}
			err = st.MatchTelegramAmbiguousEvidenceContext(ctx, sourceID, id, candidate.messages)
			if errors.Is(err, store.ErrTelegramEvidenceInsufficient) {
				continue
			}
			if err != nil {
				return err
			}
			matches = append(matches, candidate)
		}
		if blocked {
			continue
		}
		if len(matches) != 1 {
			return fmt.Errorf("ambiguous Telegram peer %d has %d matching candidates; peer identity was not resolved", id, len(matches))
		}
		match := matches[0]
		if match.dialog.Kind == "user" {
			err = st.VerifyTelegramAmbiguousUserContext(ctx, sourceID, id, match.messages)
		} else {
			err = st.VerifyTelegramAmbiguousChannelContext(ctx, sourceID, id, match.dialog.ChatID, match.messages, match.replies, match.crossChat)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func collectAmbiguousPeerEvidence(ctx context.Context, client *tg.Client, dialog CatalogDialog) (ambiguousPeerEvidence, error) {
	out := ambiguousPeerEvidence{dialog: dialog, replies: make(map[int64]store.TelegramExportReplyEvidence)}
	if dialog.TopMessage <= 0 {
		return out, errors.New("ambiguous Telegram peer has no verifiable history")
	}
	offset, examined := 0, 0
	for examined < 10000 {
		page, err := GetHistoryPage(ctx, client, dialog, offset, 0, dialog.TopMessage)
		if err != nil {
			return out, err
		}
		if page.Empty {
			return out, nil
		}
		for _, item := range page.Messages {
			examined++
			if examined > 10000 {
				return out, errors.New("ambiguous Telegram history exceeds verification limit")
			}
			message, ok := item.(*tg.Message)
			if !ok || message == nil || message.Date <= 0 {
				continue
			}
			sentAt := time.Unix(int64(message.Date), 0).UTC()
			if message.Message != "" {
				out.messages = append(out.messages, store.TelegramExportKeyEvidence{MessageID: int64(message.ID), SentAt: sentAt, Text: message.Message})
			}
			if raw, present := message.GetReplyTo(); present {
				header, ok := raw.(*tg.MessageReplyHeader)
				if !ok || header == nil {
					return out, errors.New("unsupported Telegram reply evidence")
				}
				id, present := header.GetReplyToMsgID()
				if !present {
					continue
				}
				if id <= 0 {
					return out, errors.New("invalid Telegram reply evidence")
				}
				if peer, present := header.GetReplyToPeerID(); present {
					peerID, err := telegram.MarkedPeerID(peer)
					if err != nil {
						return out, err
					}
					out.crossChat = out.crossChat || peerID != dialog.ChatID
				}
				out.replies[int64(message.ID)] = store.TelegramExportReplyEvidence{ReplyID: int64(id), SentAt: sentAt, Text: message.Message}
			}
		}
		offset = page.NextOffset
	}
	return out, errors.New("ambiguous Telegram history exceeds verification limit")
}

// VerifyAmbiguousUserFromHistory establishes a legacy direct conversation's
// user identity from matching API history. The caller verifies the logged-in
// account and owns the source sync execution for the entire operation.
func VerifyAmbiguousUserFromHistory(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialog CatalogDialog) error {
	if st == nil || client == nil || sourceID <= 0 || dialog.Kind != "user" || dialog.ChatID != dialog.NativeID || dialog.NativeID < constant.MinMTProtoMonoforumID || dialog.NativeID > constant.MaxMTProtoMonoforumID || dialog.TopMessage <= 0 {
		return errors.New("invalid ambiguous Telegram user dialog")
	}
	var evidence []store.TelegramExportKeyEvidence
	offset, examined := 0, 0
	for examined < 1000 {
		page, err := GetHistoryPage(ctx, client, dialog, offset, 0, dialog.TopMessage)
		if err != nil {
			return err
		}
		if page.Empty {
			break
		}
		for _, item := range page.Messages {
			if examined == 1000 {
				break
			}
			examined++
			message, ok := item.(*tg.Message)
			if !ok || message == nil || message.Date <= 0 || message.Message == "" {
				continue
			}
			evidence = append(evidence, store.TelegramExportKeyEvidence{MessageID: int64(message.ID), SentAt: time.Unix(int64(message.Date), 0).UTC(), Text: message.Message})
		}
		if len(evidence) >= 2 {
			err := st.VerifyTelegramAmbiguousUserContext(ctx, sourceID, dialog.NativeID, evidence)
			if err == nil {
				return nil
			}
			if !errors.Is(err, store.ErrTelegramEvidenceInsufficient) {
				return err
			}
		}
		offset = page.NextOffset
	}
	return fmt.Errorf("verify Telegram user from history: %w", store.ErrTelegramEvidenceInsufficient)
}

// VerifyAmbiguousChannelFromHistory establishes a legacy personal-chat
// conversation as a typed non-admin monoforum before moving its keys. The
// caller verifies the logged-in account and owns the source sync execution.
func VerifyAmbiguousChannelFromHistory(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialog CatalogDialog) error {
	if st == nil || client == nil || sourceID <= 0 || dialog.Kind != "supergroup" || dialog.NativeID < constant.MinMTProtoMonoforumID || dialog.NativeID > constant.MaxMTProtoMonoforumID || dialog.TopMessage <= 0 {
		return errors.New("invalid ambiguous Telegram channel dialog")
	}
	marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: dialog.NativeID})
	if err != nil || marked != dialog.ChatID {
		return errors.New("invalid ambiguous Telegram channel identity")
	}
	var evidence []store.TelegramExportKeyEvidence
	replies := make(map[int64]store.TelegramExportReplyEvidence)
	var crossChat bool
	offset, examined := 0, 0
	for examined < 10000 {
		page, err := GetHistoryPage(ctx, client, dialog, offset, 0, dialog.TopMessage)
		if err != nil {
			return err
		}
		if page.Empty {
			break
		}
		for _, item := range page.Messages {
			if examined == 10000 {
				break
			}
			examined++
			message, ok := item.(*tg.Message)
			if !ok || message == nil || message.Date <= 0 {
				continue
			}
			sentAt := time.Unix(int64(message.Date), 0).UTC()
			if message.Message != "" && len(evidence) < 1000 {
				evidence = append(evidence, store.TelegramExportKeyEvidence{MessageID: int64(message.ID), SentAt: sentAt, Text: message.Message})
			}
			if raw, present := message.GetReplyTo(); present {
				header, ok := raw.(*tg.MessageReplyHeader)
				if !ok || header == nil {
					return errors.New("unsupported Telegram reply evidence")
				}
				id, present := header.GetReplyToMsgID()
				if !present {
					continue
				}
				if id <= 0 {
					return errors.New("invalid Telegram reply evidence")
				}
				if peer, present := header.GetReplyToPeerID(); present {
					peerID, err := telegram.MarkedPeerID(peer)
					if err != nil {
						return err
					}
					crossChat = crossChat || peerID != marked
				}
				replies[int64(message.ID)] = store.TelegramExportReplyEvidence{ReplyID: int64(id), SentAt: sentAt, Text: message.Message}
			}
		}
		offset = page.NextOffset
	}
	return st.VerifyTelegramAmbiguousChannelContext(ctx, sourceID, dialog.NativeID, marked, evidence, replies, crossChat)
}

// RepairShiftedHistory resolves each affected channel from a complete catalog
// and applies only server-confirmed links. The caller verifies the logged-in
// account and owns the source sync execution for the entire operation.
func RepairShiftedHistory(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialogs []CatalogDialog) error {
	_, err := repairAndDiscoverFormerGroups(ctx, st, sourceID, client, dialogs, 0)
	return err
}

// repairAndDiscoverFormerGroups repairs existing shifted keys and records
// every verified migration link needed by this backfill. Discovery is
// unfiltered for ordinary runs, so each enabled supergroup contributes its
// former basic-group history before the archive walk starts.
func repairAndDiscoverFormerGroups(ctx context.Context, st *store.Store, sourceID int64, client *tg.Client, dialogs []CatalogDialog, selectedChatID int64) ([]CatalogDialog, error) {
	if st == nil || client == nil || sourceID <= 0 {
		return nil, errors.New("invalid Telegram shifted history repair")
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if source == nil || source.SourceType != telegram.SourceType {
		return nil, errors.New("Telegram history repair requires a Telegram source")
	}
	channels, err := st.TelegramShiftedHistoryChannelsContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]CatalogDialog, len(dialogs))
	for _, dialog := range dialogs {
		if _, exists := byID[dialog.ChatID]; exists {
			return nil, errors.New("duplicate Telegram catalog dialog")
		}
		byID[dialog.ChatID] = dialog
	}
	shifted := make(map[int64]bool, len(channels))
	for _, id := range channels {
		shifted[id] = true
	}
	verified := make([]CatalogDialog, 0, len(channels)+len(dialogs))
	seen := make(map[int64]bool, len(channels)+len(dialogs))
	for _, dialog := range dialogs {
		_, hasShiftedHistory := shifted[dialog.ChatID]
		discoverMigration := dialog.Kind == "supergroup" && ((selectedChatID == 0 && dialog.EnabledByDefault) || (selectedChatID != 0 && dialog.ChatID == selectedChatID))
		if !hasShiftedHistory && !discoverMigration {
			continue
		}
		if dialog.Kind != "supergroup" {
			if hasShiftedHistory {
				return nil, errors.New("shifted Telegram history requires access to its supergroup")
			}
			continue
		}
		verified = append(verified, dialog)
		seen[dialog.ChatID] = true
	}
	for _, id := range channels {
		if !seen[id] {
			if _, ok := byID[id]; !ok {
				return nil, errors.New("shifted Telegram history requires access to its supergroup")
			}
		}
	}
	formerGroups := make([]CatalogDialog, 0, len(verified))
	for _, dialog := range verified {
		_, hasShiftedHistory := shifted[dialog.ChatID]
		if dialog.Inaccessible || dialog.UnsupportedReason != "" {
			continue
		}
		priorLink, err := st.TelegramHistoryMigrationContext(ctx, sourceID, dialog.ChatID)
		if err != nil {
			return nil, fmt.Errorf("load Telegram migration linkage: %w", err)
		}
		var link *ChannelMigration
		if priorLink != nil && !hasShiftedHistory {
			link = &ChannelMigration{FormerChatID: priorLink.FormerChatID, FormerMaxID: priorLink.FormerMaxID}
		} else {
			link, err = GetChannelMigration(ctx, client, dialog)
			if err != nil {
				return nil, err
			}
		}
		if link == nil {
			if hasShiftedHistory {
				return nil, errors.New("Telegram supergroup has no verified former-group link")
			}
			continue
		}
		verifiedLink := store.TelegramHistoryMigration{FormerChatID: link.FormerChatID, FormerMaxID: link.FormerMaxID}
		if hasShiftedHistory {
			if err := st.ReconcileTelegramShiftedHistoryContext(ctx, sourceID, dialog.ChatID, link.FormerChatID, link.FormerMaxID); err != nil {
				return nil, fmt.Errorf("reconcile Telegram shifted history: %w", err)
			}
		} else if priorLink == nil || *priorLink != verifiedLink {
			if err := st.SaveTelegramHistoryMigrationContext(ctx, sourceID, dialog.ChatID, link.FormerChatID, link.FormerMaxID); err != nil {
				return nil, fmt.Errorf("save Telegram migration linkage: %w", err)
			}
		}
		if (selectedChatID != 0 && dialog.ChatID != selectedChatID) || (selectedChatID == 0 && !dialog.EnabledByDefault) {
			continue
		}
		formerID, err := telegram.MarkedPeerID(&tg.PeerChat{ChatID: link.FormerChatID})
		if err != nil {
			return nil, fmt.Errorf("map Telegram former group: %w", err)
		}
		formerGroups = append(formerGroups, CatalogDialog{
			ChatID: formerID, NativeID: link.FormerChatID, Kind: "group", Title: dialog.Title,
			TopMessage: link.FormerMaxID, EnabledByDefault: true, MigrationParentChatID: dialog.ChatID,
		})
	}
	return formerGroups, nil
}

// ChannelMigration identifies a former basic group and its migration boundary.
type ChannelMigration struct {
	FormerChatID int64
	FormerMaxID  int
}

// GetChannelMigration reads the server-confirmed former group link. A nil
// result means the channel reports no migration; it is not evidence that
// unrelated archived negative message IDs are safe to rewrite.
func GetChannelMigration(ctx context.Context, client *tg.Client, dialog CatalogDialog) (*ChannelMigration, error) {
	if client == nil || dialog.Kind != "supergroup" || dialog.NativeID <= 0 {
		return nil, errors.New("invalid Telegram migration peer")
	}
	marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: dialog.NativeID})
	if err != nil || marked != dialog.ChatID {
		return nil, errors.New("invalid Telegram migration peer identity")
	}
	result, err := client.ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: dialog.NativeID, AccessHash: dialog.AccessHash})
	if err != nil {
		return nil, fmt.Errorf("read Telegram channel migration: %w", err)
	}
	if result == nil {
		return nil, errors.New("missing Telegram channel migration response")
	}
	full, ok := result.FullChat.(*tg.ChannelFull)
	if !ok || full == nil || full.ID != dialog.NativeID {
		return nil, errors.New("Telegram channel migration identity mismatch")
	}
	oldID, hasID := full.GetMigratedFromChatID()
	maxID, hasMax := full.GetMigratedFromMaxID()
	if !hasID && !hasMax {
		return nil, nil
	}
	if !hasID || !hasMax || maxID <= 0 || int64(maxID) > math.MaxInt32 {
		return nil, errors.New("invalid Telegram channel migration boundary")
	}
	if _, err := telegram.MarkedPeerID(&tg.PeerChat{ChatID: oldID}); err != nil {
		return nil, errors.New("invalid Telegram former group identity")
	}
	return &ChannelMigration{FormerChatID: oldID, FormerMaxID: maxID}, nil
}

func selectedFormerGroup(ctx context.Context, client *tg.Client, dialogs []CatalogDialog, selectedChatID int64) (*CatalogDialog, error) {
	if selectedChatID == 0 {
		return nil, nil
	}
	for _, dialog := range dialogs {
		if dialog.ChatID != selectedChatID || dialog.Kind != "supergroup" || dialog.Inaccessible || dialog.UnsupportedReason != "" {
			continue
		}
		link, err := GetChannelMigration(ctx, client, dialog)
		if err != nil {
			return nil, err
		}
		if link == nil {
			return nil, nil
		}
		chatID, err := telegram.MarkedPeerID(&tg.PeerChat{ChatID: link.FormerChatID})
		if err != nil {
			return nil, fmt.Errorf("map Telegram former group: %w", err)
		}
		return &CatalogDialog{
			ChatID: chatID, NativeID: link.FormerChatID, Kind: "group", Title: dialog.Title,
			TopMessage: link.FormerMaxID, EnabledByDefault: true, MigrationParentChatID: dialog.ChatID,
		}, nil
	}
	return nil, nil
}

func attachFormerGroup(dialogs []CatalogDialog, former *CatalogDialog, ranges []telegram.HistoryRange) []CatalogDialog {
	if former == nil {
		return dialogs
	}
	return attachFormerGroups(dialogs, []CatalogDialog{*former}, ranges)
}

func attachFormerGroups(dialogs, formers []CatalogDialog, ranges []telegram.HistoryRange) []CatalogDialog {
	for _, former := range formers {
		attached := false
		for index := range dialogs {
			if dialogs[index].ChatID != former.ChatID {
				continue
			}
			if dialogs[index].TopMessage < former.TopMessage {
				dialogs[index].TopMessage = former.TopMessage
			}
			dialogs[index].EnabledByDefault = true
			dialogs[index].MigrationParentChatID = former.MigrationParentChatID
			if len(dialogs[index].MessageRanges) == 0 && len(ranges) > 0 {
				dialogs[index].MessageRanges = append([]telegram.HistoryRange(nil), ranges...)
			}
			attached = true
			break
		}
		if attached {
			continue
		}
		copy := former
		copy.EnabledByDefault = true
		if len(copy.MessageRanges) == 0 && len(ranges) > 0 {
			copy.MessageRanges = append([]telegram.HistoryRange(nil), ranges...)
		}
		dialogs = append(dialogs, copy)
	}
	return dialogs
}
