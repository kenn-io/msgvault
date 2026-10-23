package inline

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func directChatImportClient(t *testing.T, transport string, peerID int64) (Client, func()) {
	t.Helper()
	if transport == "cli" {
		// The process supplies the stable external CLI read contract; decoding,
		// import, person resolution, and archive queries use production code.
		return NewCLIClient(cliProcessFixture(t, fmt.Sprintf(`
case "$*" in
  "--version") printf 'inline 0.7.16' ;;
  "doctor --json --compact") printf '%%s' '{"config":{"apiBaseUrl":"https://api.inline.chat/v1","realtimeUrl":"wss://api.inline.chat/realtime"}}' ;;
  "auth me --json --compact") printf '%%s' '{"id":1,"first_name":"Example","last_name":"Owner"}' ;;
  "chats get --chat-id 7 --json --compact") printf '%%s' '{"chat":{"id":7,"peer_id":{"type":{"User":{"user_id":%d}}}}}' ;;
  "messages list --chat-id 7 --limit 100 --json --compact") printf '%%s' '{"messages":[{"id":5,"chat_id":7,"from_id":1,"date":1788220800,"message":"outgoing needle","out":true}]}' ;;
  *) exit 9 ;;
esac
`, peerID))), nil
	}
	message := strings.NewReplacer(`"fromId":"2"`, `"fromId":"1"`, `"out":false`, `"out":true`).Replace(inlineMessageJSON(t, 5, "outgoing needle", ""))
	f := newInlineMCPFixture(t, map[string]string{
		"conversations.get": fmt.Sprintf(`{"chat":{"chatId":"7","kind":"dm","peer":{"userId":"%d"}},"details":{}}`, peerID),
		"messages.list":     `{"chat":{"chatId":"7"},"nextOffsetId":null,"messages":[` + message + `]}`,
	}, transport == "mcp structured")
	return connectInlineFixture(t, f), func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.payloads["conversations.get"] = `{"chat":{"chatId":"7","kind":"dm"},"details":{}}`
	}
}

func TestDirectChatImportPersistsKnownPeerPersonScope(t *testing.T) {
	for _, transport := range []string{"mcp structured", "mcp text", "cli"} {
		for _, selfChat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/self_%t", transport, selfChat), func(t *testing.T) {
				assertions := assert.New(t)
				requires := require.New(t)
				peerID := int64(2)
				participantCount := 2
				wantDirection, wantRole := personscope.ToPerson, personscope.RoleTo
				if selfChat {
					peerID = 1
					participantCount = 1
					wantDirection, wantRole = personscope.FromPerson, personscope.RoleFrom
				}
				st := testutil.NewTestStore(t)
				// A person already known from earlier archive activity must acquire
				// outgoing-only DM history without needing an incoming message.
				peer := Account{UserID: peerID}
				peerParticipantID, err := st.EnsureParticipantByIdentifier(SourceType, peer.Identifier(), "Example Peer")
				requires.NoError(err)
				person, _, err := st.CreatePersonFromParticipant(peerParticipantID)
				requires.NoError(err)
				client, omitPeer := directChatImportClient(t, transport, peerID)
				account, err := client.Me(t.Context())
				requires.NoError(err)
				_, err = st.GetOrCreateSource(SourceType, account.Identifier())
				requires.NoError(err)
				imp := NewImporter(st, client)
				opts := ImportOptions{Account: account, ChatIDs: []int64{7}, NoMedia: true}
				first, err := imp.Import(t.Context(), opts)
				requires.NoError(err)
				assertions.Equal(1, first.MessagesAdded)
				messageID := archivedMessage(t, st, first.SourceID, 7, 5)
				message, err := st.GetMessage(messageID)
				requires.NoError(err)

				steps := []string{"initial", "full"}
				if omitPeer != nil {
					steps = append(steps, "unavailable peer")
				}
				for _, step := range steps {
					if step == "unavailable peer" {
						omitPeer()
					}
					if step != "initial" {
						opts.Full = true
						full, err := imp.Import(t.Context(), opts)
						requires.NoError(err)
						assertions.Equal(1, full.MessagesUpdated)
						assertions.Zero(full.MessagesAdded)
					}
					results, err := st.SearchPersonSweepMessages(t.Context(), peoplesweep.ContextRequest{
						PersonID: person.ID, Target: personfacts.TargetDescriptor{Description: "needle"},
						SourceClasses: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, Limit: 10,
					})
					requires.NoError(err)
					requires.Len(results, 1)
					assertions.Equal(messageID, results[0].Ref.MessageID)
					assertions.Equal([]int64{peerParticipantID}, results[0].Provenance.ParticipantIDs)
					assertions.Equal([]personscope.Direction{wantDirection}, results[0].Provenance.Directions)
					assertions.Equal([]personscope.Role{wantRole}, results[0].Provenance.Roles)
					var archivedCount, rosterCount int
					err = st.DB().QueryRow(st.Rebind(`SELECT participant_count,
						(SELECT COUNT(*) FROM conversation_participants cp WHERE cp.conversation_id = c.id)
						FROM conversations c WHERE c.id = ?`), message.ConversationID).Scan(&archivedCount, &rosterCount)
					requires.NoError(err)
					assertions.Equal(participantCount, archivedCount)
					assertions.Equal(participantCount, rosterCount, "replay must preserve distinct owner and peer membership")
					if engine, ok := query.NewEngine(st.DB(), st.IsPostgreSQL()).(query.TextEngine); ok {
						conversations, err := engine.ListConversations(t.Context(), query.TextFilter{ParticipantIDs: person.ParticipantIDs})
						requires.NoError(err)
						requires.Len(conversations, 1)
						assertions.Equal(message.ConversationID, conversations[0].ConversationID)
					}
				}
			})
		}
	}
}
