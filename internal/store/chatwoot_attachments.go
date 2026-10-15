package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

func (s *Store) ReplaceMessageChatwootAttachments(messageID int64, refs []AttachmentRef) error {
	return s.withTx(func(tx *loggedTx) error {
		if err := s.requireSyncMessageSourceTx(tx, messageID); err != nil {
			return err
		}
		if err := s.replaceMessageAttachmentsWhereTx(tx, messageID, `source_attachment_id LIKE ?`, refs, "chatwoot:%"); err != nil {
			return err
		}
		return recomputeMessageAttachmentStatsWith(tx, messageID)
	})
}

func (s *Store) ChatwootHasPrivateNotes(ctx context.Context, sourceID int64) (bool, error) {
	private := "json_extract(metadata, '$.private') = 1"
	if s.IsPostgreSQL() {
		private = "metadata ->> 'private' = 'true'"
	}
	var hasPrivate bool
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(
		SELECT 1 FROM messages WHERE source_id = ? AND `+private+`)`), sourceID).Scan(&hasPrivate); err != nil {
		return false, fmt.Errorf("check archived Chatwoot private notes: %w", err)
	}
	return hasPrivate, nil
}

// ChatwootConversationHead returns the highest archived Chatwoot message ID in
// one conversation, or zero when none exist.
func (s *Store) ChatwootConversationHead(ctx context.Context, sourceID int64, sourceConversationID string) (int64, error) {
	var head int64
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT COALESCE(MAX(CAST(m.source_message_id AS BIGINT)), 0)
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_id = ? AND c.source_id = ? AND c.source_conversation_id = ?`),
		sourceID, sourceID, sourceConversationID).Scan(&head); err != nil {
		return 0, fmt.Errorf("load Chatwoot conversation head: %w", err)
	}
	return head, nil
}

// chatwootSelfAgentConfig decodes a Chatwoot source's sync_config and the
// self-agent list a previous sync applied.
func chatwootSelfAgentConfig(raw sql.NullString) (config map[string]jsontext.Value, prior []string, applied bool, err error) {
	config = map[string]jsontext.Value{}
	if raw.Valid && strings.TrimSpace(raw.String) != "" {
		if err = json.Unmarshal([]byte(raw.String), &config); err != nil {
			return nil, nil, false, fmt.Errorf("decode sync_config: %w", err)
		}
	}
	if config == nil {
		config = map[string]jsontext.Value{}
	}
	priorRaw, applied := config["chatwoot_self_agents"]
	if applied {
		if err = json.Unmarshal(priorRaw, &prior); err != nil {
			return nil, nil, false, fmt.Errorf("decode applied self agents: %w", err)
		}
	}
	return config, prior, applied, nil
}

// SyncChatwootSelfAgents applies configuration and ownership under the same
// identity lock. An unchanged configuration returns without taking the lock.
func (s *Store) SyncChatwootSelfAgents(ctx context.Context, sourceID int64, wanted []string) error {
	wanted = slices.Clone(wanted)
	slices.Sort(wanted)
	wanted = slices.Compact(wanted)
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, s.Rebind(`SELECT sync_config FROM sources WHERE id = ? AND source_type = 'chatwoot'`), sourceID).Scan(&raw); err != nil {
		return fmt.Errorf("load Chatwoot source %d self agents: %w", sourceID, err)
	}
	_, prior, applied, err := chatwootSelfAgentConfig(raw)
	if err != nil {
		return fmt.Errorf("load Chatwoot source %d self agents: %w", sourceID, err)
	}
	if applied && slices.Equal(prior, wanted) {
		return nil
	}
	if err = s.withAttributionTxContext(ctx, attributionLock{Exclusive: true}, func(tx *loggedTx) error {
		return s.syncChatwootSelfAgentsTx(ctx, tx, sourceID, wanted)
	}); err != nil {
		return fmt.Errorf("sync Chatwoot source %d self agents: %w", sourceID, err)
	}
	return nil
}

func (s *Store) syncChatwootSelfAgentsTx(ctx context.Context, tx *loggedTx, sourceID int64, wanted []string) error {
	const signal = "chatwoot_self_agent"
	var raw sql.NullString
	var imported bool
	if err := tx.QueryRowContext(ctx, `SELECT sync_config,
   EXISTS(SELECT 1 FROM sync_runs WHERE source_id = sources.id AND status = 'completed') OR
   EXISTS(SELECT 1 FROM messages WHERE source_id = sources.id)
   FROM sources WHERE id = ? AND source_type = 'chatwoot'`, sourceID).Scan(&raw, &imported); err != nil {
		return fmt.Errorf("load source: %w", err)
	}
	config, prior, applied, err := chatwootSelfAgentConfig(raw)
	if err != nil {
		return err
	}
	var optedOut bool
	if value := config["no_default_identity"]; len(value) > 0 {
		if err := json.Unmarshal(value, &optedOut); err != nil {
			return fmt.Errorf("decode no_default_identity: %w", err)
		}
	}
	changed := false
	for _, address := range wanted {
		if slices.Contains(prior, address) {
			continue
		}
		added, _, err := s.mergeAccountIdentitySignalsTxWith(ctx, tx, sourceID, address, []string{signal}, newIdentifierMatch(address), applied || (!imported && !optedOut))
		if err != nil {
			return fmt.Errorf("add self agent identity: %w", err)
		}
		changed = changed || added
	}
	rows, err := tx.QueryContext(ctx, `SELECT address, source_signal FROM account_identities WHERE source_id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("list account identities: %w", err)
	}
	var identities []AccountIdentity
	for rows.Next() {
		var identity AccountIdentity
		if err = rows.Scan(&identity.Address, &identity.SourceSignal); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan account identity: %w", err)
		}
		identities = append(identities, identity)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("list account identities: %w", err)
	}
	for _, identity := range identities {
		if slices.Contains(wanted, identity.Address) {
			continue
		}
		signals := strings.Split(identity.SourceSignal, ",")
		if !slices.Contains(signals, signal) {
			continue
		}
		signals = slices.DeleteFunc(signals, func(value string) bool { return value == signal })
		if len(signals) == 0 {
			if _, err = tx.ExecContext(ctx, `DELETE FROM account_identities WHERE source_id = ? AND address = ?`, sourceID, identity.Address); err != nil {
				return fmt.Errorf("remove self agent identity: %w", err)
			}
			changed = true
		} else if _, err = tx.ExecContext(ctx, `UPDATE account_identities SET source_signal = ? WHERE source_id = ? AND address = ?`, strings.Join(signals, ","), sourceID, identity.Address); err != nil {
			return fmt.Errorf("remove self agent signal: %w", err)
		}
	}
	config["chatwoot_self_agents"], err = json.Marshal(wanted)
	if err != nil {
		return fmt.Errorf("encode self agents: %w", err)
	}
	encoded, err := json.Marshal(config, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode sync_config: %w", err)
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE sources SET sync_config = %s, updated_at = %s WHERE id = ?`, s.dialect.JSONBindExpr(), s.dialect.Now()), string(encoded), sourceID); err != nil {
		return fmt.Errorf("save sync_config: %w", err)
	}
	if !changed {
		return nil
	}
	if _, err = s.bumpIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	if err = s.bumpAccountIdentityRevisionContext(ctx, tx); err != nil {
		return err
	}
	return refreshSourceMessageAttributionContext(ctx, tx, sourceID)
}
