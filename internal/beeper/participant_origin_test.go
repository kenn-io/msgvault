package beeper

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestResolveUserRejectsStaleFullOrigin(t *testing.T) {
	for _, phone := range []bool{false, true} {
		name := "email"
		if phone {
			name = "phone"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			source, err := st.GetOrCreateSource("beeper", "origin-account")
			requirements.NoError(err)
			room, err := st.EnsureConversation(t.Context(), source.ID, "!origin:example.test", "Synthetic origin")
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "!origin:example.test", conversationpolicy.Full))
			resolver := newParticipantResolver(st, "origin-account")
			weak, err := resolver.resolveID(t.Context(), "@synthetic:example.test", "")
			requirements.NoError(err)
			full, err := st.ConversationFullEligibleContext(t.Context(), room)
			requirements.NoError(err)
			requirements.True(full)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "!origin:example.test", conversationpolicy.Cold))
			user := &User{ID: "@synthetic:example.test", FullName: "Cold directory name", Email: "cold-only@example.test"}
			if phone {
				user.PhoneNumber = "+15550100991"
			}
			got, err := resolver.resolveUserForPolicy(t.Context(), user, room)
			requirements.NoError(err, "stale automatic work must fall back without aborting import")
			assertions.Equal(weak, got)
			assertions.Empty(resolver.rich)
			var richRows int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants WHERE email_address='cold-only@example.test' OR phone_number='+15550100991'`).Scan(&richRows))
			assertions.Zero(richRows, "no rich row may be created before a later merge guard")
			var name string
			requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name,'') FROM participants WHERE id=?`), weak).Scan(&name))
			assertions.Empty(name)
		})
	}
}

func TestBeeperRosterDemotionPreservesText(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	imp, st, sourceID, chat := servedPolicyRoom(t, "retained-account", "retained-room", 1,
		map[string]any{"id": "retained-user", "fullName": "Retained directory name", "email": "retained@example.test"})
	room, err := st.EnsureConversation(t.Context(), sourceID, chat.ID, "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Full))
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), sourceID, chat.ID, conversationpolicy.Cold))
	sum := &ImportSummary{}
	conversationID, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, chat, ImportOptions{AccountID: "retained-account"}, sum)
	requirements.NoError(err)
	assertions.Equal(room, conversationID)
	requirements.NoError(imp.persistMessage(t.Context(), &chatScope{convID: room, sourceID: sourceID, cs: &ChatState{}}, &Message{ID: "retained-message", SenderID: "message-only-user", SenderName: "Cold message name", Text: "Retained synthetic text"}, sum))
	var rich int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participants WHERE email_address='retained@example.test' OR display_name IN ('Retained directory name','Cold message name')`).Scan(&rich))
	assertions.Zero(rich)
	assertions.Empty(imp.res.rich)
	var id int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='retained-message'`).Scan(&id))
	var body string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
	assertions.Equal("Retained synthetic text", body)
}

// A cold room's incomplete roster still hands the resolver its members'
// contact details. Mentions and reactions must not use them to merge the
// member into an independent phone participant.
func TestColdRoomMentionsAndReactionsStayBare(t *testing.T) {
	for _, kind := range []string{"mention", "reaction"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "account")
			requirements.NoError(err)
			room, err := st.EnsureConversation(t.Context(), source.ID, "cold", "Cold")
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "cold", conversationpolicy.Cold))
			existing, err := st.EnsureParticipantByPhone(t.Context(), "+12025550130", "Independent Contact", "sms")
			requirements.NoError(err)
			chat := &Chat{ID: "cold", AccountID: "account", Network: "Signal", Type: "group"}
			chat.Participants.Items = []Participant{{User: User{ID: "member", PhoneNumber: "+12025550130"}}}
			chat.Participants.HasMore = true
			imp := NewImporter(st, nil)
			imp.res.accountID = "account"
			requirements.NoError(imp.res.useMessageRoster(t.Context(), source.ID, chat, true, true))
			message := &Message{ID: kind + "-message", SenderID: "sender", Text: "Synthetic text"}
			if kind == "mention" {
				message.Mentions = []string{"member"}
			} else {
				message.Reactions = []Reaction{{ID: "reaction", ReactionKey: "👍", ParticipantID: "member", Emoji: true}}
			}
			requirements.NoError(imp.persistMessage(t.Context(), &chatScope{convID: room, sourceID: source.ID, cs: &ChatState{}}, message, &ImportSummary{}))
			member, _, err := st.ParticipantByIdentifier("beeper", beeperidentity.Fallback("account", "member"))
			requirements.NoError(err)
			assertions.NotZero(member)
			assertions.NotEqual(existing, member, "cold-room roster details must not merge the member")
		})
	}
}
