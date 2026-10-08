package chatwoot

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func contractArchivedMessageID(t *testing.T, st *store.Store, providerID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), providerID).Scan(&id))
	return id
}

func contractSender(t *testing.T, st *store.Store, messageID int64) sql.NullInt64 {
	t.Helper()
	var sender sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&sender))
	return sender
}

func contractRecipients(t *testing.T, st *store.Store, messageID int64, role string) []store.MessageRecipient {
	t.Helper()
	// The mail recovery helper intentionally returns only email-addressed
	// recipients. Inspect real rows to cover phone and provider-ID identities.
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT participant_id, COALESCE(email_address,''), COALESCE(display_name,'') FROM message_recipients WHERE message_id = ? AND recipient_type = ? ORDER BY id`), messageID, role)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var recipients []store.MessageRecipient
	for rows.Next() {
		var recipient store.MessageRecipient
		require.NoError(t, rows.Scan(&recipient.ParticipantID, &recipient.EmailAddress, &recipient.DisplayName))
		recipients = append(recipients, recipient)
	}
	require.NoError(t, rows.Err())
	return recipients
}

func TestImportContractActorsSendersAndRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"}
	agent := map[string]any{"id": int64(7), "type": "user", "name": "Example Agent", "email": "staffquartz@example.com"}
	bot := map[string]any{"id": int64(7), "type": "agent_bot", "name": "Example Bot"}
	owner := map[string]any{"id": int64(8), "type": "user", "name": "Example Owner"}
	assistant := map[string]any{"id": int64(7), "type": "captain_assistant", "name": "Example Assistant"}
	detached := contractMessage(108, 1767225608, nil)
	detached["sender_type"], detached["sender_id"] = "Captain::Assistant", int64(7)
	messages := []map[string]any{
		contractMessage(101, 1767225601, contact),
		contractMessage(102, 1767225602, agent),
		contractMessage(103, 1767225603, bot),
		contractMessage(104, 1767225604, nil),
		contractMessage(105, 1767225605, agent),
		contractMessage(106, 1767225606, owner),
		contractMessage(107, 1767225607, assistant),
		detached,
		contractMessage(109, 1767225609, map[string]any{"id": int64(50), "type": "user"}),
	}
	for _, index := range []int{1, 2, 4, 5, 6, 7, 8} {
		messages[index]["message_type"] = 1
	}
	messages[3]["message_type"] = 2
	messages[4]["private"] = true
	messages[4]["content"] = "Synthetic employee note"
	api := newContractAPI(t, 3, messages)
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, SelfAgentIDs: []int64{8}})
	require.NoError(err)
	assert.Equal([]int64{101, 102, 103, 104, 105, 106, 107, 108, 109}, contractMessageIDs(t, st))
	ids := map[int64]int64{}
	for _, providerID := range []int64{101, 102, 103, 104, 105, 106, 107, 108, 109} {
		ids[providerID] = contractArchivedMessageID(t, st, strconv.FormatInt(providerID, 10))
		fromMe, err := st.GetMessageIsFromMe(ids[providerID])
		require.NoError(err)
		assert.Equal(providerID == 106, fromMe, "outgoing is not personal ownership: provider message %d", providerID)
	}
	contactID, agentID, botID := contractSender(t, st, ids[101]), contractSender(t, st, ids[102]), contractSender(t, st, ids[103])
	require.True(contactID.Valid)
	require.True(agentID.Valid)
	require.True(botID.Valid)
	assert.NotEqual(contactID.Int64, agentID.Int64, "contact and user ID namespaces must differ")
	assert.NotEqual(contactID.Int64, botID.Int64)
	assert.NotEqual(agentID.Int64, botID.Int64)
	assistantID := contractSender(t, st, ids[107])
	require.True(assistantID.Valid, "assistant replies keep a sender")
	assert.NotContains([]int64{contactID.Int64, agentID.Int64, botID.Int64}, assistantID.Int64, "assistant IDs have their own namespace")
	assert.Equal(assistantID, contractSender(t, st, ids[108]), "a detached sender resolves by type and ID")
	assert.False(contractSender(t, st, ids[104]).Valid, "activity without an actor must not acquire the current assignee")
	assert.Equal(agentID, contractSender(t, st, ids[105]))
	for _, providerID := range []int64{101, 102, 103, 105, 106} {
		from := contractRecipients(t, st, ids[providerID], "from")
		require.Len(from, 1, "provider message %d", providerID)
		assert.Equal(contractSender(t, st, ids[providerID]).Int64, from[0].ParticipantID)
	}
	outgoing := contractRecipients(t, st, ids[102], "to")
	require.Len(outgoing, 1)
	assert.Equal(contactID.Int64, outgoing[0].ParticipantID)
	incoming := contractRecipients(t, st, ids[101], "to")
	require.Len(incoming, 1)
	assert.NotContains([]int64{contactID.Int64, agentID.Int64, botID.Int64}, incoming[0].ParticipantID)
	noteBody, err := st.GetMessageBodyText(ids[105])
	require.NoError(err)
	assert.Contains(strings.ToLower(noteBody), "private", "ordinary readers visibly mark private notes")
	note := contractRecipients(t, st, ids[105], "to")
	require.Len(note, 1)
	assert.Equal(incoming[0].ParticipantID, note[0].ParticipantID, "private notes address the shared inbox")

	api.Mu.Lock()
	api.Conversations[42] = append(api.Conversations[42], contractMessage(110, 1767225610, map[string]any{"id": int64(50), "type": "user", "name": "Late Agent"}))
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, SelfAgentIDs: []int64{8}})
	require.NoError(err)
	var name string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COALESCE(display_name, '') FROM participants WHERE id = ?`), contractSender(t, st, ids[109]).Int64).Scan(&name))
	assert.Equal("Late Agent", name, "a later sighting fills a blank name")
	for _, rebuilt := range []bool{false, true} {
		if rebuilt {
			_, err := st.RebuildFTSContext(t.Context(), nil)
			require.NoError(err)
		}
		_, matches, err := st.SearchMessages("staffquartz", 0, 10)
		require.NoError(err)
		assert.Zero(matches, "staff observations stay outside the sender-address index before and after rebuilding")
	}
}

func TestImportContractSelfAgentOwnershipFollowsIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	owner := map[string]any{"id": int64(8), "type": "user", "name": "Example Owner", "email": "owner@example.com"}
	call := contractMessage(201, 1767225600, owner)
	call["message_type"] = 1
	call["content_type"] = "voice_call"
	call["call"] = map[string]any{"id": 601, "direction": "outgoing", "status": "completed"}
	reply := contractMessage(202, 1767225601, owner)
	reply["message_type"] = 1
	employee := contractMessage(107, 1767225602, map[string]any{"id": int64(7), "type": "user"})
	employee["message_type"] = 1
	api := newContractAPI(t, 3, []map[string]any{call, reply, employee})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	providerIDs := []string{"202", "call:201"}
	owned := func(selfAgents []int64) []bool {
		_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, SelfAgentIDs: selfAgents})
		require.NoError(err)
		var flags []bool
		for _, providerID := range providerIDs {
			fromMe, err := st.GetMessageIsFromMe(contractArchivedMessageID(t, st, providerID))
			require.NoError(err)
			flags = append(flags, fromMe)
		}
		return flags
	}
	assert.Equal([]bool{true, true}, owned([]int64{8}))
	assert.Equal([]bool{false, false}, owned(nil), "dropping a self agent un-marks its messages and calls")
	identities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(identities)

	assert.Equal([]bool{true, true}, owned([]int64{8}))
	identities, err = st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	_, err = st.RemoveAccountIdentity(source.ID, identities[0].Address)
	require.NoError(err)
	for _, providerID := range []string{"202", "call:201"} {
		fromMe, err := st.GetMessageIsFromMe(contractArchivedMessageID(t, st, providerID))
		require.NoError(err)
		assert.False(fromMe, "removing the identity un-marks %s", providerID)
	}
	importer = NewImporter(st, api.client(t))
	assert.Equal([]bool{false, false}, owned([]int64{8}), "normal sync preserves removed call and reply ownership")
	for _, id := range []int64{203, 204} {
		message := contractMessage(id, now().Unix(), owner)
		message["message_type"] = 1
		if id == 204 {
			message["content_type"] = "voice_call"
			message["call"] = map[string]any{"id": 602, "direction": "outgoing", "status": "completed"}
		}
		api.Conversations[42] = append(api.Conversations[42], message)
	}
	providerIDs = append(providerIDs, "203", "call:204")
	assert.Equal([]bool{false, false, false, false}, owned([]int64{8}))

	seven, eight := importer.actorIdentifier(Actor{ID: 7, Type: actorUser}), importer.actorIdentifier(Actor{ID: 8, Type: actorUser})
	opts := ImportOptions{InboxID: 7}
	opts.SelfAgentIDs = []int64{7}
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	opts.SelfAgentIDs = []int64{7, 8}
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	fromMe, err := st.GetMessageIsFromMe(contractArchivedMessageID(t, st, "202"))
	require.NoError(err)
	assert.True(fromMe)
	require.NoError(st.AddAccountIdentity(source.ID, seven, "manual"))
	opts.SelfAgentIDs = []int64{8}
	_, err = importer.Import(t.Context(), opts)
	require.NoError(err)
	identities, err = st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Len(identities, 2, "manual evidence survives removal from configuration")
	before, err := st.GetSourceByTypeAndIdentifier(SourceType, source.Identifier)
	require.NoError(err)
	trigger := `CREATE TRIGGER reject_chatwoot_config BEFORE UPDATE OF sync_config ON sources BEGIN SELECT RAISE(ABORT, 'synthetic config failure'); END`
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		trigger = `CREATE FUNCTION reject_chatwoot_config() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic config failure'; END $$; CREATE TRIGGER reject_chatwoot_config BEFORE UPDATE OF sync_config ON sources FOR EACH ROW EXECUTE FUNCTION reject_chatwoot_config()`
	}
	_, err = st.DB().Exec(trigger)
	require.NoError(err)
	nine := importer.actorIdentifier(Actor{ID: 9, Type: actorUser})
	require.Error(st.SyncChatwootSelfAgents(t.Context(), source.ID, []string{nine}))
	after, err := st.GetSourceByTypeAndIdentifier(SourceType, source.Identifier)
	require.NoError(err)
	assert.Equal(before.SyncConfig, after.SyncConfig)
	afterIdentities, err := st.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Equal(identities, afterIdentities, "config-write failure rolls back grants and removals together")
	_, err = st.DB().Exec(`DROP TRIGGER reject_chatwoot_config` + func() string {
		if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
			return " ON sources"
		}
		return ""
	}())
	require.NoError(err)
	_, err = st.RemoveAccountIdentity(source.ID, eight)
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(source.ID, `{"unrelated":true}`))
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	afterIdentities, err = st.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(afterIdentities, 1, "legacy imported sources adopt config without restoring removed identities")
	assert.Equal(seven, afterIdentities[0].Address)
	fresh, err := st.GetOrCreateSource(SourceType, SourceIdentifier(api.server.URL, 3, 8))
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(fresh.ID, `{"no_default_identity":true,"unrelated":true}`))
	require.NoError(st.SyncChatwootSelfAgents(t.Context(), fresh.ID, []string{seven}))
	empty, err := st.ListAccountIdentities(fresh.ID)
	require.NoError(err)
	assert.Empty(empty)
	require.NoError(st.SyncChatwootSelfAgents(t.Context(), fresh.ID, []string{seven, eight}))
	confirmed, err := st.ListAccountIdentities(fresh.ID)
	require.NoError(err)
	require.Len(confirmed, 1)
	assert.Equal(eight, confirmed[0].Address, "a changed configured set explicitly grants a new owner")
	fresh, err = st.GetSourceByTypeAndIdentifier(SourceType, fresh.Identifier)
	require.NoError(err)
	var config map[string]any
	require.NoError(json.Unmarshal([]byte(fresh.SyncConfig.String), &config))
	assert.Equal(true, config["unrelated"])
}

func TestImportContractPrivateExclusionKeepsActivityAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	messages := []map[string]any{contractMessage(101, 1767225600, nil), contractMessage(102, 1767225601, nil), contractMessage(103, 1767225602, nil)}
	messages[1]["private"] = true
	messages[1]["content"] = "excluded-private-message"
	messages[2]["message_type"] = 2
	api := newContractAPI(t, 1, messages)
	st := testutil.NewTestStore(t)
	contractRegister(t, st, api)
	for range 4 {
		_, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: false, Limit: 1})
		require.NoError(err)
	}
	assert.Equal([]int64{101, 103}, contractMessageIDs(t, st), "private skips must advance and public activity must remain")
	for _, providerID := range []string{"101", "103"} {
		id := contractArchivedMessageID(t, st, providerID)
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		assert.NotContains(string(raw), "excluded-private-seed")
		assert.NotContains(string(raw), "excluded-private-message")
	}
}

func TestImportContractCallFallbackAndLifecycleKeepsOneLinkedMeeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"}
	message := contractMessage(201, 1767225600, contact)
	message["content_type"] = "voice_call"
	message["content"] = "Voice call"
	message["content_attributes"] = map[string]any{"data": map[string]any{
		"call_id": 601, "call_sid": "CA_synthetic", "call_source": "twilio", "call_direction": "inbound", "status": "no-answer", "duration_seconds": 0,
		"accepted_by": map[string]any{"id": 7, "name": "Example Agent"},
	}}
	api := newContractAPI(t, 3, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	chatID := contractArchivedMessageID(t, st, "201")
	meetingID := contractArchivedMessageID(t, st, "call:201")
	chat, err := st.GetMessage(chatID)
	require.NoError(err)
	meeting, err := st.GetMessage(meetingID)
	require.NoError(err)
	assert.Equal("chatwoot", chat.MessageType)
	assert.Equal("meeting_transcript", meeting.MessageType)
	assert.Equal(source.ID, meeting.SourceID)
	assert.NotEqual(chat.ConversationID, meeting.ConversationID)
	assert.Equal("call:42:201", meeting.SourceConversationID)
	assert.Equal(time.Unix(1767225600, 0).UTC(), meeting.SentAt.UTC(), "unanswered call is dated from the timeline occurrence")
	for _, link := range []struct {
		messageID int64
		key       string
		want      int64
	}{{chatID, "meeting_message_id", meetingID}, {meetingID, "chat_message_id", chatID}} {
		metadata, err := st.GetMessageMetadata(link.messageID)
		require.NoError(err)
		var decoded map[string]jsontext.Value
		require.NoError(json.Unmarshal([]byte(metadata.String), &decoded))
		var linked int64
		require.NoError(json.Unmarshal(decoded[link.key], &linked), "missing %s in %s", link.key, metadata.String)
		assert.Equal(link.want, linked)
	}
	packet, err := st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: new([]int64{meetingID})}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, IncludeTranscript: true, MaxBytes: 64 << 10})
	require.NoError(err)
	var decoded meetingcontent.Packet
	require.NoError(json.Unmarshal([]byte(packet.Content), &decoded))
	require.Len(decoded.Meetings, 1)
	entry := decoded.Meetings[0]
	require.Len(entry.Participants, 2)
	assert.Contains(packet.Content, "+12025550101")
	assert.Contains(packet.Content, "Example Agent")
	assert.NotContains(packet.Content, "Example Assignee")
	assert.Equal(meetingcontent.StateUnavailable, entry.Content.Transcript.State)
	for _, participant := range entry.Participants {
		require.NotNil(participant.ParticipantID)
	}
	after := time.Unix(1767225500, 0)
	metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{SourceIDs: []int64{source.ID}, After: &after})
	require.NoError(err)
	assert.Equal(int64(1), metrics.Totals.MeetingCount)
	assert.Equal(int64(1), metrics.Totals.KnownDurationCount, "explicit zero differs from unknown duration")
	assert.Zero(metrics.Totals.TotalKnownSeconds)

	api.Mu.Lock()
	message["attachments"] = []any{
		map[string]any{"id": 401, "message_id": 201, "file_type": "audio", "transcribed_text": "firstquartz recording words"},
		map[string]any{"id": 402, "message_id": 201, "file_type": "audio", "transcribed_text": "secondquartz recording words"},
	}
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	body, err := st.GetMessageBodyText(meetingID)
	require.NoError(err)
	assert.Contains(body, "firstquartz recording words\n\nsecondquartz recording words")
	assert.NotContains(body, "Voice call")
	chatBody, err := st.GetMessageBodyText(chatID)
	require.NoError(err)
	assert.Contains(chatBody, "Voice call\n\nfirstquartz recording words\n\nsecondquartz recording words")
	results, _, err := st.SearchMessages("secondquartz", 0, 10)
	require.NoError(err)
	var matchingIDs []int64
	for _, result := range results {
		matchingIDs = append(matchingIDs, result.ID)
	}
	assert.Contains(matchingIDs, meetingID, "all attachment transcripts remain searchable in the linked meeting")
	assert.Contains(matchingIDs, chatID)

	api.Mu.Lock()
	message["call"] = map[string]any{"id": 601, "provider_call_id": "CA_synthetic", "provider": "twilio", "direction": "incoming", "status": "completed", "duration_seconds": 45,
		"accepted_by_agent_id": 7, "accepted_by_agent_name": "Example Agent", "transcript": "Updated call transcript words"}
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Equal(meetingID, contractArchivedMessageID(t, st, "call:201"))
	metrics, err = st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{SourceIDs: []int64{source.ID}})
	require.NoError(err)
	assert.Equal(int64(1), metrics.Totals.MeetingCount)
	assert.InDelta(45, metrics.Totals.TotalKnownSeconds, 1e-9)
	body, err = st.GetMessageBodyText(meetingID)
	require.NoError(err)
	assert.Contains(body, "Updated call transcript words")
	assert.NotContains(body, "firstquartz")
	assert.NotContains(body, "secondquartz")
	aliasID, err := st.EnsureParticipantContext(t.Context(), "owner.alias@example.com", "Example Owner", "example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, "owner.alias@example.com", "manual"))
	require.NoError(st.MergeParticipants(contractSender(t, st, meetingID).Int64, aliasID))
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	var recipientRow int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT MIN(id) FROM message_recipients WHERE message_id = ?`), chatID).Scan(&recipientRow))
	for range 2 {
		sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
		require.NoError(err)
		assert.Zero(sum.Meetings, "unchanged calls keep Store's merged owner attribution")
		var currentRow int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT MIN(id) FROM message_recipients WHERE message_id = ?`), chatID).Scan(&currentRow))
		assert.Equal(recipientRow, currentRow, "unchanged call refresh leaves recipient rows in place")
		for _, id := range []int64{chatID, meetingID} {
			owned, err := st.GetMessageIsFromMe(id)
			require.NoError(err)
			assert.True(owned)
		}
	}
	packet, err = st.GetMeetingContextContext(t.Context(), store.MeetingQueryScope{MessageIDs: new([]int64{meetingID})}, meetingcontent.PacketOptions{Format: meetingcontent.FormatJSON, MaxBytes: 64 << 10})
	require.NoError(err)
	require.NoError(json.Unmarshal([]byte(packet.Content), &decoded))
	require.Len(decoded.Meetings, 1)
	assert.Len(decoded.Meetings[0].Participants, 2, "the merged customer and handling agent each appear once")

	liveCall, ok := message["call"].(map[string]any)
	require.True(ok)
	api.Mu.Lock()
	message["sender"] = map[string]any{"id": int64(7), "type": "user", "name": "Example Agent", "availability_status": "online"}
	message["message_type"] = 1
	liveCall["direction"] = "outgoing"
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	rawBefore, err := st.GetMessageRaw(chatID)
	require.NoError(err)
	api.Mu.Lock()
	api.Contact["name"] = "Renamed Contact"
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	chat, err = st.GetMessage(chatID)
	require.NoError(err)
	assert.Equal("Renamed Contact", chat.Subject)
	var conversationTitle string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT title FROM conversations WHERE id = ?`), chat.ConversationID).Scan(&conversationTitle))
	assert.Equal("Renamed Contact", conversationTitle)
	var previousContactID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), chatID).Scan(&previousContactID))
	api.Mu.Lock()
	api.Contact["id"], api.Contact["phone_number"] = int64(50), "+12025550102"
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	rawAfter, err := st.GetMessageRaw(chatID)
	require.NoError(err)
	assert.True(meetingarchive.JSONEvidenceEqual(rawBefore, rawAfter), "conversation contact changes leave the outgoing payload unchanged")
	var chatContactID, meetingContactID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), chatID).Scan(&chatContactID))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`), meetingID).Scan(&meetingContactID))
	assert.NotEqual(previousContactID, chatContactID)
	assert.Equal(chatContactID, meetingContactID, "the linked meeting follows the reassigned conversation contact")
	sender, ok := message["sender"].(map[string]any)
	require.True(ok)
	sender["availability_status"] = "offline"
	sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Zero(sum.Meetings)
	rawAfter, err = st.GetMessageRaw(chatID)
	require.NoError(err)
	assert.True(meetingarchive.JSONEvidenceEqual(rawBefore, rawAfter), "presence changes preserve message evidence")
	assert.NotContains(string(rawAfter), "availability_status")
}

func TestImportContractLateAudioTranscriptAndCredentialFreeCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	payload := []byte("synthetic recording A bytes")
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := contractMessage(301, 1767225600, nil)
	message["content"] = ""
	attachment := map[string]any{"id": 401, "message_id": 301, "file_type": "audio", "content_type": "audio/ogg", "extension": "ogg", "file_size": len(payload), "data_url": router.url(t, media.server, "/recording-a.ogg"), "transcribed_text": ""}
	message["attachments"] = []any{attachment}
	api := newContractAPI(t, 2, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true, Policy: attachmentpolicy.Policy{MaxBytes: 1024}, AttachmentsDir: t.TempDir()}
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	id := contractArchivedMessageID(t, st, "301")
	var storagePath, contentHash, state string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT storage_path, content_hash, attachment_state FROM attachments WHERE message_id = ?`), id).Scan(&storagePath, &contentHash, &state))
	wantHash := sha256.Sum256(payload)
	assert.Equal(hex.EncodeToString(wantHash[:]), contentHash)
	assert.Equal("stored", state)
	stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Equal(payload, stored)

	// The provider does not advance conversation activity when transcription
	// arrives or changes. A normal run revisits audio still waiting for one; a
	// transcript settles it, so a later correction needs a full sync.
	for index, transcript := range []string{"firstquartz source transcript", "revisedquartz source transcript"} {
		api.Mu.Lock()
		attachment["transcribed_text"] = transcript
		api.Mu.Unlock()
		runOpts := opts
		runOpts.Full = index > 0
		_, err = NewImporter(st, api.client(t)).Import(t.Context(), runOpts)
		require.NoError(err)
		body, err := st.GetMessageBodyText(id)
		require.NoError(err)
		assert.Contains(body, transcript)
		results, _, err := st.SearchMessages(transcript[:len(transcript)-len(" source transcript")], 0, 10)
		require.NoError(err)
		require.Len(results, 1)
		assert.Equal(id, results[0].ID)
	}
	stale, _, err := st.SearchMessages("firstquartz", 0, 10)
	require.NoError(err)
	assert.Empty(stale, "corrected source transcripts replace the old searchable projection")
	var refreshedHash, metadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT content_hash, attachment_metadata FROM attachments WHERE message_id = ?`), id).Scan(&refreshedHash, &metadata))
	assert.Equal(contentHash, refreshedHash)
	assert.Contains(metadata, "revisedquartz source transcript")
}

func TestImportContractSourceAndActorIsolationAcrossInstances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var sourceIDs, senderIDs []int64
	for range 2 {
		message := contractMessage(101, 1767225600, map[string]any{"id": int64(7), "type": "user", "name": "Example Agent"})
		message["message_type"] = 1
		api := newContractAPI(t, 2, []map[string]any{message})
		importer := NewImporter(st, api.client(t))
		sources, err := importer.Register(t.Context(), []Inbox{{ID: 7}, {ID: 8}})
		require.NoError(err)
		require.Len(sources, 2)
		assert.NotEqual(sources[0].ID, sources[1].ID, "each inbox is a separate source")
		_, err = importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
		require.NoError(err)
		var sourceID, senderID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT m.source_id, m.sender_id FROM messages m JOIN sources s ON s.id = m.source_id WHERE s.identifier = ? AND m.source_message_id = '101'`), api.server.URL+"/accounts/3/inboxes/7").Scan(&sourceID, &senderID))
		sourceIDs = append(sourceIDs, sourceID)
		senderIDs = append(senderIDs, senderID)
	}
	assert.NotEqual(sourceIDs[0], sourceIDs[1])
	assert.NotEqual(senderIDs[0], senderIDs[1], "bare agent IDs cannot cross instance boundaries")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE source_message_id = '101'`).Scan(&count))
	assert.Equal(2, count, "message IDs are deduplicated within their source")
}

func TestImportContractMetadataOnlyAndUnknownAttachmentTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(601, 1767225600, nil)
	message["attachments"] = []any{
		map[string]any{"id": 701, "message_id": 601, "file_type": "future_type", "content_type": "application/vnd.example.future", "future_evidence": "synthetic retained metadata"},
		map[string]any{"id": 702, "message_id": 601, "file_type": "location", "coordinates_lat": 0.0, "coordinates_long": 0.0, "fallback_title": "Synthetic origin"},
		map[string]any{"id": 703, "message_id": 601, "file_type": "fallback", "fallback_title": "Synthetic fallback"},
	}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, AttachmentsDir: t.TempDir()})
	require.NoError(err)
	id := contractArchivedMessageID(t, st, "601")
	rows, err := st.DB().Query(st.Rebind(`SELECT COALESCE(mime_type, ''), COALESCE(content_hash, ''), attachment_metadata FROM attachments WHERE message_id = ? ORDER BY source_attachment_id`), id)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var metadata []string
	var mimeTypes []string
	for rows.Next() {
		var mimeType, hash, itemMetadata string
		require.NoError(rows.Scan(&mimeType, &hash, &itemMetadata))
		assert.Empty(hash, "metadata-only attachments must not invent bytes")
		metadata = append(metadata, itemMetadata)
		mimeTypes = append(mimeTypes, mimeType)
	}
	require.NoError(rows.Err())
	require.Len(metadata, 3)
	assert.Contains(mimeTypes, "application/vnd.example.future")
	assert.Contains(metadata[0], "synthetic retained metadata")
	assert.Contains(metadata[1], "coordinates_lat")
	assert.Contains(metadata[1], "Synthetic origin")
	assert.Contains(metadata[2], "Synthetic fallback")
}

func TestImportContractRelatedMessageAvailability(t *testing.T) {
	checks, must := assert.New(t), require.New(t)
	message := contractMessage(201, 1767225600, nil)
	message["content_type"] = "voice_call"
	message["call"] = map[string]any{"id": 601, "direction": "incoming", "status": "completed", "transcript": "Synthetic transcript"}
	neighbor := contractMessage(200, 1767225500, nil)
	neighbor["content"] = strings.Repeat("x", store.ConversationInlineBodyBudget+1)
	api := newContractAPI(t, 3, []map[string]any{neighbor, message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	must.NoError(err)
	chatID, meetingID := contractArchivedMessageID(t, st, "201"), contractArchivedMessageID(t, st, "call:201")
	neighborID := contractArchivedMessageID(t, st, "200")
	detail, err := st.GetMessage(neighborID)
	must.NoError(err)
	checks.Nil(detail.RelatedMessageID)
	window, err := st.GetConversationWindowContext(t.Context(), detail.ConversationID, neighborID, 10, 10, nil, nil)
	must.NoError(err)
	must.Len(window.Messages, 2)
	for _, item := range window.Messages {
		if item.ID == chatID {
			checks.True(item.BodyOmitted)
			checks.Equal(new(meetingID), item.RelatedMessageID)
		}
	}
	var dialect query.Dialect = query.SQLiteQueryDialect{}
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		dialect = query.PostgreSQLQueryDialect{}
	}
	engine := query.NewEngineWithDialect(st.DB(), dialect)
	for _, pair := range [][2]int64{{chatID, meetingID}, {meetingID, chatID}} {
		detail, err := st.GetMessageContext(t.Context(), pair[0])
		must.NoError(err)
		checks.Equal(new(pair[1]), detail.RelatedMessageID)
		queried, err := engine.GetMessage(t.Context(), pair[0])
		must.NoError(err)
		checks.Equal(new(pair[1]), queried.RelatedMessageID)
		window, err := st.GetConversationWindowContext(t.Context(), detail.ConversationID, pair[0], 10, 10, nil, nil)
		must.NoError(err)
		var related *int64
		for _, item := range window.Messages {
			if item.ID == pair[0] {
				related = item.RelatedMessageID
			}
		}
		checks.Equal(new(pair[1]), related)
	}

	invalidMetadata := []string{`{"meeting_message_id":-1}`, `{"meeting_message_id":` + strconv.FormatInt(chatID, 10) + `}`, `{"meeting_message_id":` + strconv.FormatInt(neighborID, 10) + `}`, `{"meeting_message_id":999999}`}
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		invalidMetadata = append(invalidMetadata, "broken JSON")
	}
	for _, metadata := range invalidMetadata {
		must.NoError(st.SetMessageMetadata(chatID, sql.NullString{String: metadata, Valid: true}))
		got, err := st.GetMessage(chatID)
		must.NoError(err)
		checks.Nil(got.RelatedMessageID, metadata)
		queried, err := engine.GetMessage(t.Context(), chatID)
		must.NoError(err)
		checks.Nil(queried.RelatedMessageID, metadata)
	}
	must.NoError(st.SetMessageMetadata(chatID, sql.NullString{String: `{"meeting_message_id":` + strconv.FormatInt(meetingID, 10) + `}`, Valid: true}))
	other, err := st.GetOrCreateSource("chatwoot", "synthetic-other-inbox")
	must.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), other.ID, meetingID)
	must.NoError(err)
	got, err := st.GetMessage(chatID)
	must.NoError(err)
	checks.Nil(got.RelatedMessageID)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_id = ? WHERE id = ?`), source.ID, meetingID)
	must.NoError(err)
	must.NoError(st.MarkMessageDeleted(source.ID, "call:201"))
	got, err = st.GetMessage(chatID)
	must.NoError(err)
	checks.Nil(got.RelatedMessageID)
	queried, err := engine.GetMessage(t.Context(), chatID)
	must.NoError(err)
	checks.Nil(queried.RelatedMessageID)
}

func TestImportContractRelatedMessageSubset(t *testing.T) {
	must, checks := require.New(t), assert.New(t)
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	st, err := store.Open(sourcePath)
	must.NoError(err)
	must.NoError(st.InitSchema())
	message := contractMessage(201, 1767225600, nil)
	message["content_type"] = "voice_call"
	message["call"] = map[string]any{"id": 601, "direction": "incoming", "status": "completed", "transcript": "Synthetic transcript"}
	api := newContractAPI(t, 3, []map[string]any{message})
	importer, _ := contractRegister(t, st, api)
	_, err = importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	must.NoError(err)
	must.NoError(st.Close())
	destination := t.TempDir()
	_, err = store.CopySubset(sourcePath, destination, 1, false)
	must.NoError(err)
	subset, err := store.Open(filepath.Join(destination, "msgvault.db"))
	must.NoError(err)
	t.Cleanup(func() { must.NoError(subset.Close()) })
	var id int64
	must.NoError(subset.DB().QueryRow(`SELECT id FROM messages`).Scan(&id))
	detail, err := subset.GetMessage(id)
	must.NoError(err)
	checks.Nil(detail.RelatedMessageID)
	engine := query.NewEngineWithDialect(subset.DB(), query.SQLiteQueryDialect{})
	queried, err := engine.GetMessage(t.Context(), id)
	must.NoError(err)
	checks.Nil(queried.RelatedMessageID)
}
