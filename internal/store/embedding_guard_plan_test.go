package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMembershipEmbeddingGuardSeeksBeeperIndex(t *testing.T) {
	requirements := require.New(t)
	s, err := OpenForTest(filepath.Join(t.TempDir(), "embedding-guard.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(s.Close()) })
	requirements.NoError(s.InitSchema())
	seedLiveMessages(t, s, 2000)
	conversationID, err := s.EnsureConversation(1, "beeper-chat", "Beeper chat")
	requirements.NoError(err)
	for _, id := range []string{"beeper-1", "beeper-2", "beeper-3"} {
		messageID, err := s.UpsertMessage(&Message{SourceID: 1, SourceMessageID: id, ConversationID: conversationID, MessageType: "beeper"})
		requirements.NoError(err)
		requirements.NoError(s.UpsertMessageBody(messageID, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
	}
	for _, stats := range []string{"no-stats", "analyzed"} {
		t.Run(stats, func(t *testing.T) {
			if stats == "analyzed" {
				_, err := s.db.Exec("ANALYZE")
				require.NoError(t, err)
			}
			for _, event := range []string{"insert", "delete"} {
				t.Run(event, func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					var trigger string
					requirements.NoError(s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`, "trg_embedding_changes_membership_"+event).Scan(&trigger))
					_, guard, found := strings.Cut(trigger, "WHEN")
					requirements.True(found)
					guard, _, found = strings.Cut(guard, "BEGIN")
					requirements.True(found)
					guard = strings.NewReplacer("NEW.conversation_id", "?", "OLD.conversation_id", "?").Replace(guard)
					plan := explainPlan(t, s, "SELECT "+guard, 1)
					assertions.Contains(plan, "SEARCH m USING INDEX idx_messages_beeper_conversation")
					assertions.NotContains(plan, "idx_messages_conversation (")
					assertions.NotContains(plan, "idx_messages_type")
				})
			}
		})
	}
}
