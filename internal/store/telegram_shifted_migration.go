package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/gotd/td/constant"
)

const telegramDesktopHistoryShift int64 = 1_000_000_000

// TelegramHistoryMigration records server-confirmed basic-group linkage.
type TelegramHistoryMigration struct {
	FormerChatID int64 `json:"former_chat_id"`
	FormerMaxID  int   `json:"former_max_id"`
}

// TelegramShiftedHistoryChannelsContext lists the channel keys which still
// contain shifted Desktop message IDs, without loading message bodies.
func (s *Store) TelegramShiftedHistoryChannelsContext(ctx context.Context, sourceID int64) ([]int64, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`SELECT m.source_message_id, m.metadata FROM messages m
		WHERE m.source_id = ? AND m.message_type = ?
		AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
		AND (m.source_message_id LIKE ? OR CAST(m.metadata AS TEXT) LIKE ?)`), sourceID, "telegram", "%:-%", "%:-%")
	if err != nil {
		return nil, fmt.Errorf("list Telegram shifted history: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[int64]bool)
	var channels []int64
	for rows.Next() {
		var key string
		var metadata sql.NullString
		if err := rows.Scan(&key, &metadata); err != nil {
			return nil, err
		}
		keys := []string{key}
		if metadata.Valid && strings.Contains(metadata.String, ":-") {
			var recovery struct {
				Reference string `json:"referenced_message_id"`
			}
			if err := json.Unmarshal([]byte(metadata.String), &recovery); err != nil {
				return nil, errors.New("invalid Telegram recovery metadata")
			}
			keys = append(keys, recovery.Reference)
		}
		for _, candidate := range keys {
			peer, suffix, ok := strings.Cut(candidate, ":")
			if !ok || !strings.HasPrefix(suffix, "-") {
				continue
			}
			id, err := strconv.ParseInt(peer, 10, 64)
			shifted, msgErr := strconv.ParseInt(suffix, 10, 64)
			if err != nil || msgErr != nil || strconv.FormatInt(id, 10) != peer || strconv.FormatInt(shifted, 10) != suffix || !constant.TDLibPeerID(id).IsChannel() || shifted >= 0 {
				return nil, errors.New("invalid shifted Telegram history key")
			}
			if !seen[id] {
				seen[id] = true
				channels = append(channels, id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(channels)
	return channels, nil
}

// TelegramHistoryMigrationContext returns a conversation's verified linkage.
func (s *Store) TelegramHistoryMigrationContext(ctx context.Context, sourceID, channelID int64) (*TelegramHistoryMigration, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return nil, err
	}
	if !constant.TDLibPeerID(channelID).IsChannel() {
		return nil, nil
	}
	var metadata sql.NullString
	err := s.db.QueryRowContext(ctx, s.dialect.Rebind(`SELECT metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, strconv.FormatInt(channelID, 10)).Scan(&metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Telegram history migration: %w", err)
	}
	if !metadata.Valid || metadata.String == "" {
		return nil, nil
	}
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
		return nil, errors.New("invalid Telegram conversation metadata")
	}
	raw, ok := fields["telegram_history_migration"]
	if !ok {
		return nil, nil
	}
	var link TelegramHistoryMigration
	if err := json.Unmarshal(raw, &link); err != nil || link.FormerChatID <= 0 || link.FormerChatID > constant.MaxTDLibChatID || link.FormerMaxID <= 0 || int64(link.FormerMaxID) > math.MaxInt32 {
		return nil, errors.New("invalid Telegram history migration linkage")
	}
	return &link, nil
}

// TelegramHistoryMessageKey maps a Desktop shifted ID only when a verified
// linkage is available; all other message IDs retain their ordinary key.
func TelegramHistoryMessageKey(channelID, messageID int64, link *TelegramHistoryMigration) (string, error) {
	key := fmt.Sprintf("%d:%d", channelID, messageID)
	if link == nil || messageID >= 0 {
		return key, nil
	}
	if !constant.TDLibPeerID(channelID).IsChannel() || link.FormerChatID <= 0 || link.FormerChatID > constant.MaxTDLibChatID || link.FormerMaxID <= 0 || int64(link.FormerMaxID) > math.MaxInt32 {
		return "", errors.New("invalid Telegram history migration linkage")
	}
	resolved, _, err := telegramUnshiftedKey(key, channelID, link.FormerChatID, link.FormerMaxID)
	return resolved, err
}

// SaveTelegramHistoryMigrationContext persists server-confirmed linkage without
// walking messages. Reconciliation is a separate operation for archives that
// still contain shifted Desktop keys.
func (s *Store) SaveTelegramHistoryMigrationContext(ctx context.Context, sourceID, channelID, formerID int64, formerMax int) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if !constant.TDLibPeerID(channelID).IsChannel() || formerID <= 0 || formerID > constant.MaxTDLibChatID || formerMax <= 0 || int64(formerMax) > math.MaxInt32 {
		return errors.New("invalid Telegram history migration linkage")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram history migration requires a Telegram source")
		}
		_, err := s.saveTelegramHistoryMigrationTx(ctx, tx, sourceID, channelID, formerID, formerMax)
		return err
	})
}

func (s *Store) saveTelegramHistoryMigrationTx(ctx context.Context, tx *loggedTx, sourceID, channelID, formerID int64, formerMax int) (int64, error) {
	var convID int64
	var convType sql.NullString
	var metadata sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(channelID, 10)).Scan(&convID, &convType, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		convID, err = ensureConversationWithType(boundQuerier{ctx: ctx, q: tx}, s.dialect, sourceID, strconv.FormatInt(channelID, 10), "group_chat", "")
		convType = sql.NullString{String: "group_chat", Valid: true}
	}
	if err != nil {
		return 0, fmt.Errorf("find Telegram migrated conversation: %w", err)
	}
	if !convType.Valid || convType.String != "group_chat" {
		return 0, errors.New("Telegram migrated conversation is not a group")
	}
	fields := map[string]jsontext.Value{}
	if metadata.Valid && metadata.String != "" {
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
			return 0, errors.New("invalid Telegram conversation metadata")
		}
	}
	link := TelegramHistoryMigration{FormerChatID: formerID, FormerMaxID: formerMax}
	if prior, ok := fields["telegram_history_migration"]; ok {
		var existing TelegramHistoryMigration
		if err := json.Unmarshal(prior, &existing); err != nil || existing != link {
			return 0, errors.New("conflicting Telegram history migration linkage")
		}
	}
	encodedLink, err := json.Marshal(link, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	fields["telegram_history_migration"] = encodedLink
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	if err != nil {
		return 0, err
	}
	if !metadata.Valid || metadata.String != string(encoded) {
		query := fmt.Sprintf(`UPDATE conversations SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		if _, err := tx.ExecContext(ctx, query, string(encoded), convID, sourceID); err != nil {
			return 0, err
		}
	}
	return convID, nil
}

// ReconcileTelegramShiftedHistoryContext rekeys Desktop's shifted former-group
// messages and their references atomically. The caller must first verify the
// channel and former-group linkage from Telegram's full-channel response.
func (s *Store) ReconcileTelegramShiftedHistoryContext(ctx context.Context, sourceID, channelID, formerID int64, formerMax int) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if !constant.TDLibPeerID(channelID).IsChannel() || formerID <= 0 || formerID > constant.MaxTDLibChatID || formerMax <= 0 || int64(formerMax) > math.MaxInt32 {
		return errors.New("invalid Telegram history migration linkage")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram history migration requires a Telegram source")
		}
		convID, err := s.saveTelegramHistoryMigrationTx(ctx, tx, sourceID, channelID, formerID, formerMax)
		if err != nil {
			return err
		}
		type row struct {
			id, convID int64
			key        sql.NullString
			senderID   sql.NullInt64
			metadata   sql.NullString
		}
		rowsByID := make(map[int64]row)
		load := func(query string, args ...any) error {
			rows, err := tx.QueryContext(ctx, query+s.dialect.SelectForUpdate(), args...)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var item row
				if err := rows.Scan(&item.id, &item.convID, &item.key, &item.senderID, &item.metadata); err != nil {
					return err
				}
				rowsByID[item.id] = item
			}
			return rows.Err()
		}
		keyPrefix := strconv.FormatInt(channelID, 10) + ":-"
		if err := load(`SELECT id, conversation_id, source_message_id, sender_id, metadata
			FROM messages m
			WHERE m.source_id = ? AND m.message_type = ? AND m.source_message_id LIKE ?
			AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
			ORDER BY id`, sourceID, "telegram", keyPrefix+"%"); err != nil {
			return fmt.Errorf("list shifted Telegram history messages: %w", err)
		}
		if err := load(`SELECT id, conversation_id, source_message_id, sender_id, metadata
			FROM messages m
			WHERE m.source_id = ? AND m.message_type = ? AND CAST(m.metadata AS TEXT) LIKE ?
			AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id = m.id)
			ORDER BY id`, sourceID, "telegram", "%"+keyPrefix+"%"); err != nil {
			return fmt.Errorf("list shifted Telegram reply references: %w", err)
		}
		items := make([]row, 0, len(rowsByID))
		for _, item := range rowsByID {
			items = append(items, item)
		}
		slices.SortFunc(items, func(a, b row) int {
			if a.id < b.id {
				return -1
			}
			if a.id > b.id {
				return 1
			}
			return 0
		})
		var formerConvID int64
		movedMessages := false
		for _, item := range items {
			key, changedKey, err := telegramUnshiftedKey(item.key.String, channelID, formerID, formerMax)
			if err != nil {
				return err
			}
			if changedKey && item.convID != convID {
				return errors.New("shifted Telegram message belongs to another conversation")
			}
			if changedKey {
				var existing int64
				err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`+s.dialect.SelectForUpdate(), sourceID, key).Scan(&existing)
				if err == nil {
					return errors.New("Telegram former-group message key collision; archive was not changed")
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if formerConvID == 0 {
					formerConvID, err = ensureConversationWithType(boundQuerier{ctx: ctx, q: tx}, s.dialect, sourceID, strconv.FormatInt(-formerID, 10), "group_chat", "")
					if err != nil {
						return fmt.Errorf("ensure Telegram former-group conversation: %w", err)
					}
				}
			}
			newMetadata, changedMetadata, err := unshiftTelegramReplyMetadata(item.metadata, channelID, formerID, formerMax)
			if err != nil {
				return err
			}
			switch {
			case changedKey && changedMetadata:
				query := fmt.Sprintf(`UPDATE messages SET source_message_id = ?, conversation_id = ?, metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
				if _, err := tx.ExecContext(ctx, query, key, formerConvID, newMetadata, item.id, sourceID); err != nil {
					return err
				}
			case changedKey:
				if _, err := tx.ExecContext(ctx, `UPDATE messages SET source_message_id = ?, conversation_id = ? WHERE id = ? AND source_id = ?`, key, formerConvID, item.id, sourceID); err != nil {
					return err
				}
			case changedMetadata:
				query := fmt.Sprintf(`UPDATE messages SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
				if _, err := tx.ExecContext(ctx, query, newMetadata, item.id, sourceID); err != nil {
					return err
				}
			}
			if changedKey {
				if item.senderID.Valid {
					if _, err := tx.ExecContext(ctx, s.dialect.InsertOrIgnore(`INSERT OR IGNORE INTO conversation_participants (conversation_id, participant_id, role) VALUES (?, ?, 'member')`), formerConvID, item.senderID.Int64); err != nil {
						return fmt.Errorf("add Telegram former-group sender membership: %w", err)
					}
				}
				movedMessages = true
			}
		}
		if movedMessages {
			q := boundQuerier{ctx: ctx, q: tx}
			if err := s.recomputeConversationStatsWith(q, "id = ?", convID); err != nil {
				return err
			}
			if err := s.recomputeConversationStatsWith(q, "id = ?", formerConvID); err != nil {
				return err
			}
			if err := s.bumpDerivedDataRevisionContext(ctx, tx); err != nil {
				return fmt.Errorf("invalidate Telegram message caches after shifted-history repair: %w", err)
			}
		}
		return nil
	})
}

func telegramUnshiftedKey(key string, channelID, formerID int64, formerMax int) (string, bool, error) {
	prefix := strconv.FormatInt(channelID, 10) + ":"
	suffix, ok := strings.CutPrefix(key, prefix)
	if !ok || !strings.HasPrefix(suffix, "-") {
		return key, false, nil
	}
	shifted, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil || strconv.FormatInt(shifted, 10) != suffix || shifted >= 0 || shifted <= -telegramDesktopHistoryShift {
		return "", false, errors.New("invalid shifted Telegram message key")
	}
	native := shifted + telegramDesktopHistoryShift
	if native > int64(formerMax) {
		return "", false, errors.New("shifted Telegram message exceeds migration boundary")
	}
	return fmt.Sprintf("-%d:%d", formerID, native), true, nil
}

func unshiftTelegramReplyMetadata(value sql.NullString, channelID, formerID int64, formerMax int) (string, bool, error) {
	if !value.Valid || value.String == "" {
		return "", false, nil
	}
	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal([]byte(value.String), &fields); err != nil || fields == nil {
		return "", false, errors.New("invalid Telegram recovery metadata")
	}
	raw, exists := fields["referenced_message_id"]
	if !exists {
		return "", false, nil
	}
	var reference string
	if err := json.Unmarshal(raw, &reference); err != nil {
		return "", false, errors.New("invalid Telegram reply reference")
	}
	key, changed, err := telegramUnshiftedKey(reference, channelID, formerID, formerMax)
	if err != nil || !changed {
		return "", false, err
	}
	fields["referenced_message_id"], err = json.Marshal(key)
	if err != nil {
		return "", false, err
	}
	encoded, err := json.Marshal(fields, json.Deterministic(true))
	return string(encoded), true, err
}
