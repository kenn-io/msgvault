package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// FullConversationWhere rejects absent policy state for supported chat sources.
func FullConversationWhere(alias string) string {
	return `(EXISTS (SELECT 1 FROM conversation_policy policy_full WHERE policy_full.conversation_id=` + alias + `.id AND policy_full.effective_tier='full') OR (NOT EXISTS (SELECT 1 FROM conversation_policy policy_missing WHERE policy_missing.conversation_id=` + alias + `.id) AND NOT EXISTS (SELECT 1 FROM sources policy_source WHERE policy_source.id=` + alias + `.source_id AND policy_source.source_type IN ('beeper','slack','slackdump','discord','teams'))))`
}
func FullMessageWhere(alias string) string {
	return `(EXISTS (SELECT 1 FROM conversations policy_conversation WHERE policy_conversation.id=` + alias + `.conversation_id AND ` + FullConversationWhere("policy_conversation") + `) OR (` + alias + `.conversation_id IS NULL AND NOT EXISTS (SELECT 1 FROM sources policy_message_source WHERE policy_message_source.id=` + alias + `.source_id AND policy_message_source.source_type IN ('beeper','slack','slackdump','discord','teams'))))`
}

// FullActivityEventWhere hides a projected event as soon as its message's
// conversation becomes ineligible. Queue processing will then retract the
// stored event and refresh contact state; this predicate closes that interval.
func FullActivityEventWhere(alias string) string {
	return `EXISTS (SELECT 1 FROM messages policy_activity_message WHERE policy_activity_message.id=` + alias + `.message_id AND ` + FullMessageWhere("policy_activity_message") + `)`
}

// LexicalMessageWhere admits only full conversations, plus cold when explicitly requested.
// Source deletion visibility is an independent caller predicate.
func LexicalMessageWhere(alias string, includeCold bool) string {
	full := FullMessageWhere(alias)
	if !includeCold {
		return full
	}
	return "(" + full + " OR EXISTS (SELECT 1 FROM conversation_policy policy_cold WHERE policy_cold.conversation_id=" + alias + ".conversation_id AND policy_cold.effective_tier='cold'))"
}

var ErrConversationPolicyStale = errors.New("conversation policy work is no longer eligible")

// CheckConversationPolicyMessagesTx checks through the caller's transaction only.
// Caller owns the shared fence; this function never reacquires it.
func (s *Store) CheckConversationPolicyMessagesTx(ctx context.Context, tx ConversationPolicyTx, ids []int64) error {
	for _, id := range ids {
		var eligible bool
		if err := tx.QueryRowContext(ctx, s.Rebind(`SELECT EXISTS(SELECT 1 FROM messages m WHERE m.id=? AND `+FullMessageWhere("m")+`)`), id).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return fmt.Errorf("%w: message %d", ErrConversationPolicyStale, id)
		}
	}
	return nil
}
func (s *Store) CheckConversationPolicyWork(ctx context.Context, expectedRevision int64, ids []int64) error {
	if !s.IsPostgreSQL() {
		release, err := s.AcquireSQLiteConversationPolicyFence(ctx, ConversationPolicyShared)
		if err != nil {
			return err
		}
		defer release()
	}
	return s.withTxLockedContext(ctx, &sql.TxOptions{ReadOnly: true}, func(tx *loggedTx) error {
		if s.IsPostgreSQL() {
			return s.LockPostgresConversationPolicyTx(ctx, tx.Tx, ConversationPolicyShared)
		}
		return nil
	}, func(tx *loggedTx) error {
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(value AS BIGINT)),0) FROM archive_metadata WHERE key='conversation_policy_revision'`).Scan(&revision); err != nil {
			return err
		}
		if revision != expectedRevision {
			return ErrConversationPolicyStale
		}
		return s.CheckConversationPolicyMessagesTx(ctx, tx.Tx, ids)
	})
}
