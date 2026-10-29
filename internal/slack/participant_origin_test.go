package slack

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestSlackRetainedRosterUsesLiveOrigin(t *testing.T) {
	for _, provider := range []string{"slack", "slackdump"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(1)
			src, err := st.GetOrCreateSource(provider, "T_SYNTHETIC:U_SELF")
			requirements.NoError(err)
			room, err := st.EnsureConversationWithType(src.ID, "D_ORIGIN", "direct_chat", "Synthetic")
			requirements.NoError(err)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "D_ORIGIN", conversationpolicy.Full))
			imp := NewImporter(st, nil, "T_SYNTHETIC")
			imp.res.loadUserSnapshot([]User{{ID: "U_OTHER", Profile: UserProfile{Email: "slack-origin@example.test", DisplayName: "Synthetic directory name"}}})
			full, err := st.ConversationFullEligibleContext(t.Context(), room)
			requirements.NoError(err)
			requirements.True(full)
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "D_ORIGIN", conversationpolicy.Cold))
			conversation := &Conversation{ID: "D_ORIGIN", IsIM: true, User: "U_OTHER"}
			replay := func() {
				t.Helper()
				if provider == "slackdump" {
					_, _, err = ensureSlackdumpMembership(t.Context(), st, imp.res, room, conversation, "U_SELF", attachmentpolicy.Policy{})
				} else {
					_, _, err = imp.ensureMembership(t.Context(), 0, room, conversation, ImportOptions{UserID: "U_SELF"}, &ImportSummary{})
				}
				require.NoError(t, err)
			}
			replay()
			var rich int
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='slack-origin@example.test' OR display_name='Synthetic directory name'`).Scan(&rich))
			assertions.Zero(rich)
			assertions.Empty(imp.res.rich)
			assertions.NotEmpty(imp.res.fallback)
			// Existing fetched profile and cached bare ID must promote without another fetch.
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "D_ORIGIN", ""))
			replay()
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='slack-origin@example.test'`).Scan(&rich))
			assertions.Equal(1, rich)
			assertions.NotEmpty(imp.res.rich)
			cached := imp.res.rich["U_OTHER"]
			imp.res.users["U_OTHER"] = User{ID: "U_OTHER", Profile: UserProfile{Email: "changed-cold@example.test", DisplayName: "Changed cold name"}}
			requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "D_ORIGIN", conversationpolicy.Cold))
			replay()
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address='changed-cold@example.test' OR display_name='Changed cold name'`).Scan(&rich))
			assertions.Zero(rich, "an earlier rich cache entry does not authorize new metadata")
			assertions.Equal(cached, imp.res.rich["U_OTHER"])
		})
	}
}

func TestSlackColdMessageIdentitiesPreserveText(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	src, err := st.GetOrCreateSource("slack", "T_SYNTHETIC:U_SELF")
	requirements.NoError(err)
	room, err := st.EnsureConversation(src.ID, "C_ORIGIN", "Synthetic")
	requirements.NoError(err)
	requirements.NoError(st.SetConversationPolicyOverride(t.Context(), src.ID, "C_ORIGIN", conversationpolicy.Cold))
	imp := NewImporter(st, nil, "T_SYNTHETIC")
	for _, id := range []string{"USENDER", "UMENTION", "UREACTION"} {
		imp.res.users[id] = User{ID: id, Profile: UserProfile{Email: id + "@example.test", DisplayName: "Directory " + id}}
	}
	cc := &convScope{channelID: "C_ORIGIN", convID: room, sourceID: src.ID, opts: ImportOptions{NoMedia: true}, cs: &ConvState{}}
	msg := &Message{Type: "message", TS: "1700000000.000001", User: "USENDER", Text: "Retained <@UMENTION>", Raw: []byte(`{"type":"message","text":"Retained <@UMENTION>"}`), Reactions: []Reaction{{Name: "wave", Users: []string{"UREACTION"}}}}
	requirements.NoError(imp.processMessage(t.Context(), cc, msg, &ImportSummary{}))
	bot := &Message{Type: "message", TS: "1700000001.000001", BotID: "B_SYNTHETIC", Username: "Cold bot name", Text: "Retained bot text", Raw: []byte(`{"type":"message","text":"Retained bot text"}`)}
	requirements.NoError(imp.processMessage(t.Context(), cc, bot, &ImportSummary{}))
	var rich int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM participants WHERE email_address IS NOT NULL OR COALESCE(display_name,'')<>''`).Scan(&rich))
	assertions.Zero(rich)
	assertions.Empty(imp.res.rich)
	archived, err := st.InspectMessage(sourceMessageID(cc.channelID, msg.TS))
	requirements.NoError(err)
	assertions.Contains(archived.BodyText, "Retained")
	var reactions int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assertions.Equal(1, reactions)
	var mentions int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE recipient_type='mention'`).Scan(&mentions))
	assertions.Equal(1, mentions)
}
