package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedMembershipEmbeddingGuard(tb testing.TB, s *Store, size int) {
	tb.Helper()
	seedLiveMessages(tb, s, size)
	conversationID, err := s.EnsureConversation(1, "beeper-chat", "Beeper chat")
	require.NoError(tb, err)
	for _, id := range []string{"beeper-1", "beeper-2", "beeper-3"} {
		messageID, err := s.UpsertMessage(&Message{SourceID: 1, SourceMessageID: id, ConversationID: conversationID, MessageType: "beeper"})
		require.NoError(tb, err)
		require.NoError(tb, s.UpsertMessageBody(messageID, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	}
}

func TestMembershipEmbeddingGuardSeeksBeeperIndex(t *testing.T) {
	requirements := require.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "embedding-guard.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())
	seedMembershipEmbeddingGuard(t, s, 2000)
	for _, stats := range []string{"no-stats", "analyzed"} {
		t.Run(stats, func(t *testing.T) {
			if stats == "analyzed" {
				_, err := s.db.Exec("ANALYZE")
				require.NoError(t, err)
			}
			for _, event := range []struct {
				name  string
				args  []any
				seeks int
			}{
				{"insert", []any{1}, 1},
				{"update", []any{1, 1, 1, 1, "member", "admin", nil, nil, nil, nil, 1, 1}, 2},
				{"delete", []any{1}, 1},
			} {
				t.Run(event.name, func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					var trigger string
					requirements.NoError(s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, "trg_embedding_changes_membership_"+event.name).Scan(&trigger))
					_, guard, found := strings.Cut(trigger, "WHEN")
					requirements.True(found)
					guard, _, found = strings.Cut(guard, "BEGIN")
					requirements.True(found)
					for _, field := range []string{"conversation_id", "participant_id", "role", "joined_at", "left_at"} {
						guard = strings.NewReplacer("NEW."+field, "?", "OLD."+field, "?").Replace(guard)
					}
					plan := explainPlan(t, s, "SELECT "+guard, event.args...)
					assertions.Equal(event.seeks, strings.Count(plan, "SEARCH m USING INDEX idx_messages_beeper_conversation"), plan)
					assertions.NotContains(plan, "idx_messages_conversation (")
					assertions.NotContains(plan, "idx_messages_type")
				})
			}
		})
	}
}
