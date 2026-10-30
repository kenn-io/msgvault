package beeper

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			room, err := st.EnsureConversation(source.ID, "!origin:example.test", "Synthetic origin")
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "!origin:example.test", conversationpolicy.Full))
			resolver := newParticipantResolver(st, "origin-account")
			weak, err := resolver.resolveID("@synthetic:example.test", "")
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
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='cold-only@example.test' OR phone_number='+15550100991'`).Scan(&richRows))
			assertions.Zero(richRows, "no rich row may be created before a later merge guard")
			var name string
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COALESCE(display_name,'') FROM participants WHERE id=?`), weak).Scan(&name))
			assertions.Empty(name)
		})
	}
}

func TestBeeperRetainedRosterDemotionPreservesText(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	source, err := st.GetOrCreateSource("beeper", "retained-account")
	requirements.NoError(err)
	room, err := st.EnsureConversation(source.ID, "retained-room", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, "retained-room", conversationpolicy.Full))
	// The fetched detail survives demotion; replay must not need a provider client.
	chat := &Chat{ID: "retained-room", AccountID: "retained-account", Type: "group", Network: "Signal"}
	retainedUser := User{ID: "retained-user", FullName: "Retained directory name", Email: "retained@example.test"}
	chat.Participants.Items = []Participant{{User: retainedUser}}
	imp := NewImporter(st, nil)
	opts := ImportOptions{AccountID: "retained-account"}
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), source.ID, chat.ID, conversationpolicy.Cold))
	sum := &ImportSummary{}
	requirements.NoError(imp.replayConversationRoster(t.Context(), room, source.ID, chat, true, routeEvidenceFor(chat, opts), opts, sum))
	requirements.NoError(imp.persistMessage(t.Context(), &chatScope{convID: room, sourceID: source.ID}, &Message{ID: "retained-message", SenderID: "message-only-user", SenderName: "Cold message name", Text: "Retained synthetic text"}, sum))
	var rich int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='retained@example.test' OR display_name IN ('Retained directory name','Cold message name')`).Scan(&rich))
	assertions.Zero(rich)
	assertions.Empty(imp.res.rich)
	var id int64
	requirements.NoError(st.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='retained-message'`).Scan(&id))
	var body string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
	assertions.Equal("Retained synthetic text", body)
}
