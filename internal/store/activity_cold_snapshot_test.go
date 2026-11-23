package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
)

func TestColdActivityIdentitySnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	var st *Store
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		st = newPGStoreInternal(t)
	} else {
		var err error
		st, err = OpenForTest(filepath.Join(t.TempDir(), "cold.db"))
		requirements.NoError(err)
		t.Cleanup(func() { requirements.NoError(st.Close()) })
		requirements.NoError(st.InitSchema())
	}
	source, err := st.GetOrCreateSource("beeper", "snapshot")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	requirements.NoError(err)
	room, err := st.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), room)
	requirements.NoError(err)
	requirements.NoError(st.EnsureConversationParticipant(room, owner, "member"))
	requirements.NoError(st.EnsureConversationParticipant(room, peer, "member"))
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE conversation_policy SET effective_tier='cold' WHERE conversation_id=?`), room)
	requirements.NoError(err)
	id, err := st.UpsertMessage(&Message{
		SourceID: source.ID, ConversationID: room, SourceMessageID: "message", MessageType: "chat",
		SentAt:   sql.NullTime{Time: time.Date(2026, 11, 1, 8, 30, 0, 0, time.UTC), Valid: true},
		SenderID: sql.NullInt64{Int64: peer, Valid: true},
	})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))

	// Remove ownership through the real API between candidate classification
	// and counterpart hydration. Rebind supplies the existing query boundary.
	rebind := st.db.rebind
	fired := false
	st.db.rebind = func(query string) string {
		if !fired && strings.Contains(query, "cold_facts(message_id") {
			fired = true
			removed, err := st.RemoveAccountIdentity(source.ID, "owner@example.com")
			requirements.NoError(err)
			requirements.Equal(int64(1), removed)
		}
		return rebind(query)
	}
	defer func() { st.db.rebind = rebind }()
	q := BurstQuery{Limit: 10}
	page, err := st.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.True(fired)
	requirements.Len(page.Bursts, 1)
	assertions.Equal(1, page.Bursts[0].Inbound)
	byParticipant := map[int64]ActivityMessageCounterpart{}
	for _, cp := range page.Bursts[0].Counterparts {
		byParticipant[cp.ParticipantID] = cp
	}
	requirements.Contains(byParticipant, owner)
	assertions.Equal("owner", byParticipant[owner].Excluded)

	fresh, err := st.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(fresh.Bursts, 1)
	clear(byParticipant)
	for _, cp := range fresh.Bursts[0].Counterparts {
		byParticipant[cp.ParticipantID] = cp
	}
	requirements.Contains(byParticipant, owner)
	assertions.Empty(byParticipant[owner].Excluded)
}
