package beeper

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperColdPolicyKeepsEstablishedIdentityAndSelfActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "account")
	requirements.NoError(err)
	imp := NewImporter(s, nil)
	imp.res.accountID = "account"
	user := User{ID: "synthetic-self", PhoneNumber: "+12025550124", IsSelf: true}
	existing, err := imp.res.resolveUser(&user)
	requirements.NoError(err)
	chat := &Chat{ID: "cold", AccountID: "account", Network: "Signal", Type: "group"}
	chat.Participants.Items = []Participant{{User: user}}
	_, err = s.EnsureConversation(source.ID, chat.ID, "Cold")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, chat.ID, conversationpolicy.Cold))
	room, _, _, err := imp.ensureConversation(t.Context(), 0, source.ID, chat, ImportOptions{AccountID: "account"}, &ImportSummary{})
	requirements.NoError(err)
	pid, _, err := s.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", user.ID))
	requirements.NoError(err)
	assertions.Equal(existing, pid)
	now := time.Now().UTC().Truncate(time.Second)
	_, err = s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "own", MessageType: "beeper", SenderID: sql.NullInt64{Int64: pid, Valid: true}, IsFromMe: true, SentAt: sql.NullTime{Time: now, Valid: true}})
	requirements.NoError(err)
	activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
	requirements.NoError(err)
	assertions.Equal(now, activity)
}
