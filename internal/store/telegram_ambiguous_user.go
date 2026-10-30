package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"github.com/gotd/td/constant"
)

// ErrTelegramPeerUnverified indicates that an ambiguous peer has no
// corroborated identity in the archive.
var ErrTelegramPeerUnverified = errors.New("ambiguous Telegram personal chat requires verified peer evidence")

// MatchTelegramAmbiguousEvidenceContext checks snapshots without assigning a
// peer identity. The caller holds the source execution while comparing peers.
func (s *Store) MatchTelegramAmbiguousEvidenceContext(ctx context.Context, sourceID, bareID int64, evidence []TelegramExportKeyEvidence) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if bareID < constant.MinMTProtoMonoforumID || bareID > constant.MaxMTProtoMonoforumID {
		return errors.New("invalid ambiguous Telegram peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var convID int64
		var kind string
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(bareID, 10)).Scan(&convID, &kind)
		if err != nil {
			return err
		}
		if kind != "direct_chat" {
			return errors.New("ambiguous Telegram peer is not a direct conversation")
		}
		return verifyTelegramExportEvidence(ctx, tx, sourceID, convID, TelegramExportKeyChange{LegacyID: bareID, Evidence: evidence}, s.dialect.SelectForUpdate())
	})
}

// EnsureTelegramTypedUserContext records an API-typed user before its first
// message. An existing unverified conversation is never claimed by this path.
func (s *Store) EnsureTelegramTypedUserContext(ctx context.Context, sourceID, userID int64, title string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if userID < constant.MinMTProtoMonoforumID || userID > constant.MaxMTProtoMonoforumID {
		return errors.New("invalid ambiguous Telegram user peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram user verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(userID, 10)).Scan(&convID, &kind, &metadata)
		if err == nil {
			var fields struct {
				Verified bool `json:"telegram_verified_user"`
			}
			if kind != "direct_chat" || !metadata.Valid || json.Unmarshal([]byte(metadata.String), &fields) != nil || !fields.Verified {
				return errors.New("ambiguous Telegram conversation requires peer verification")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		query := fmt.Sprintf(`INSERT INTO conversations (source_id, source_conversation_id, conversation_type, title, metadata) VALUES (?, ?, ?, ?, %s) ON CONFLICT (source_id, source_conversation_id) DO NOTHING RETURNING id`, s.dialect.JSONBindExpr())
		err = tx.QueryRowContext(ctx, query, sourceID, strconv.FormatInt(userID, 10), "direct_chat", title, `{"telegram_verified_user":true}`).Scan(&convID)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("ambiguous Telegram conversation requires peer verification")
		}
		return err
	})
}

// VerifyTelegramAmbiguousUserContext marks a legacy direct conversation as a
// user only after two distinct message snapshots match a verified API user.
// The caller must obtain the evidence from that authenticated user peer.
func (s *Store) VerifyTelegramAmbiguousUserContext(ctx context.Context, sourceID, userID int64, evidence []TelegramExportKeyEvidence) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if userID < constant.MinMTProtoMonoforumID || userID > constant.MaxMTProtoMonoforumID {
		return errors.New("invalid ambiguous Telegram user peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram user verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(userID, 10)).Scan(&convID, &kind, &metadata)
		if err != nil {
			return fmt.Errorf("find ambiguous Telegram conversation: %w", err)
		}
		if kind != "direct_chat" {
			return errors.New("ambiguous Telegram user is not a direct conversation")
		}
		if err := verifyTelegramExportEvidence(ctx, tx, sourceID, convID, TelegramExportKeyChange{LegacyID: userID, Evidence: evidence}, s.dialect.SelectForUpdate()); err != nil {
			return err
		}
		fields := map[string]jsontext.Value{}
		if metadata.Valid && metadata.String != "" {
			if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil || fields == nil {
				return errors.New("invalid Telegram conversation metadata")
			}
		}
		if raw, ok := fields["telegram_verified_user"]; ok {
			var prior bool
			if err := json.Unmarshal(raw, &prior); err != nil || !prior {
				return errors.New("conflicting Telegram user verification")
			}
		}
		fields["telegram_verified_user"] = jsontext.Value("true")
		encoded, err := json.Marshal(fields, json.Deterministic(true))
		if err != nil {
			return err
		}
		query := fmt.Sprintf(`UPDATE conversations SET metadata = %s WHERE id = ? AND source_id = ?`, s.dialect.JSONBindExpr())
		_, err = tx.ExecContext(ctx, query, string(encoded), convID, sourceID)
		return err
	})
}

// CheckVerifiedTelegramUserExportContext checks an ambiguous Desktop dialog
// against a previously API-verified user and two matching archived messages.
func (s *Store) CheckVerifiedTelegramUserExportContext(ctx context.Context, sourceID, userID int64, evidence []TelegramExportKeyEvidence) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if userID < constant.MinMTProtoMonoforumID || userID > constant.MaxMTProtoMonoforumID {
		return errors.New("invalid ambiguous Telegram user peer")
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if sourceType != "telegram" {
			return errors.New("Telegram user verification requires a Telegram source")
		}
		var convID int64
		var kind string
		var metadata sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id, conversation_type, metadata FROM conversations WHERE source_id = ? AND source_conversation_id = ?`+s.dialect.SelectForUpdate(), sourceID, strconv.FormatInt(userID, 10)).Scan(&convID, &kind, &metadata)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTelegramPeerUnverified
		}
		if err != nil {
			return err
		}
		if kind != "direct_chat" || !metadata.Valid {
			return ErrTelegramPeerUnverified
		}
		var fields struct {
			Verified bool `json:"telegram_verified_user"`
		}
		if err := json.Unmarshal([]byte(metadata.String), &fields); err != nil {
			return errors.New("invalid Telegram conversation metadata")
		}
		if !fields.Verified {
			return ErrTelegramPeerUnverified
		}
		return verifyTelegramExportEvidence(ctx, tx, sourceID, convID, TelegramExportKeyChange{LegacyID: userID, Evidence: evidence}, s.dialect.SelectForUpdate())
	})
}
