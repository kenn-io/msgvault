package beeper

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

// servedPolicyRoom serves one Beeper group so ensureConversation can verify
// its roster. Synthetic members pad the room to size.
func servedPolicyRoom(t *testing.T, accountID, chatID string, size int, members ...map[string]any) (*Importer, *store.Store, int64, *Chat) {
	t.Helper()
	for i := len(members); i < size; i++ {
		members = append(members, map[string]any{"id": fmt.Sprintf("@member%d:beeper.local", i), "fullName": fmt.Sprintf("Member %d", i)})
	}
	f := newFakeBeeper(t)
	f.addChat(&fakeChat{ID: chatID, AccountID: accountID, Network: "Signal", Title: "Room", Type: "group", Participants: members})
	imp, st, closeServer := newTestImporter(t, f)
	t.Cleanup(closeServer)
	sourceID := newBeeperTestSource(t, st, accountID)
	chat, err := imp.client.GetChat(t.Context(), chatID)
	require.NoError(t, err)
	return imp, st, sourceID, chat
}

func TestBeeperColdPolicyKeepsEstablishedIdentityAndSelfActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, s, sourceID, chat := servedPolicyRoom(t, "account", "cold", 1,
		map[string]any{"id": "synthetic-self", "phoneNumber": "+12025550124", "isSelf": true})
	imp.res.accountID = "account"
	existing, err := imp.res.resolveUser(t.Context(), &chat.Participants.Items[0].User)
	requirements.NoError(err)
	_, err = s.EnsureConversation(t.Context(), sourceID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Cold))
	room, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, ImportOptions{AccountID: "account"}, &ImportSummary{})
	requirements.NoError(err)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "synthetic-self"))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: room, SourceMessageID: "own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
}
