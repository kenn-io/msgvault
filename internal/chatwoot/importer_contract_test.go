package chatwoot

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
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

func TestImportContractActualSendersAndPrivateRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	contact := map[string]any{"id": int64(7), "type": "contact", "name": "Example Contact", "phone_number": "+12025550101"}
	agent := map[string]any{"id": int64(7), "type": "user", "name": "Example Agent"}
	bot := map[string]any{"id": int64(7), "type": "agent_bot", "name": "Example Bot"}
	owner := map[string]any{"id": int64(8), "type": "user", "name": "Example Owner"}
	messages := []map[string]any{
		contractMessage(101, 1801526401, contact),
		contractMessage(102, 1801526402, agent),
		contractMessage(103, 1801526403, bot),
		contractMessage(104, 1801526404, nil),
		contractMessage(105, 1801526405, agent),
		contractMessage(106, 1801526406, owner),
	}
	for _, index := range []int{1, 2, 4, 5} {
		messages[index]["message_type"] = 1
	}
	messages[3]["message_type"] = 2
	messages[4]["private"] = true
	messages[4]["content"] = "Synthetic employee note"
	api := newContractAPI(t, 3, messages)
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, SelfAgentIDs: []int64{8}})
	require.NoError(err)
	assert.Equal([]int64{101, 102, 103, 104, 105, 106}, contractMessageIDs(t, st))
	ids := map[int64]int64{}
	for _, providerID := range []int64{101, 102, 103, 104, 105, 106} {
		ids[providerID] = contractArchivedMessageID(t, st, strconv.FormatInt(providerID, 10))
		fromMe, err := st.GetMessageIsFromMe(ids[providerID])
		require.NoError(err)
		assert.Equal(providerID == 106, fromMe, "outgoing is not personal ownership: provider message %d", providerID)
		raw, err := st.GetMessageRaw(ids[providerID])
		require.NoError(err)
		assert.NotContains(string(raw), "excluded-private-seed")
		assert.NotContains(string(raw), "excluded-channel-secret")
	}
	contactID, agentID, botID := contractSender(t, st, ids[101]), contractSender(t, st, ids[102]), contractSender(t, st, ids[103])
	require.True(contactID.Valid)
	require.True(agentID.Valid)
	require.True(botID.Valid)
	assert.NotEqual(contactID.Int64, agentID.Int64, "contact and user ID namespaces must differ")
	assert.NotEqual(contactID.Int64, botID.Int64)
	assert.NotEqual(agentID.Int64, botID.Int64)
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
	storedSource, err := st.GetSourceByID(source.ID)
	require.NoError(err)
	assert.NotContains(storedSource.SyncConfig.String, "excluded-channel-secret")
}

func TestImportContractPrivateExclusionKeepsActivityAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	messages := []map[string]any{contractMessage(101, 1801526400, nil), contractMessage(102, 1801526401, nil), contractMessage(103, 1801526402, nil)}
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
	message := contractMessage(201, 1801526400, contact)
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
	assert.Equal(time.Unix(1801526400, 0).UTC(), meeting.SentAt.UTC(), "unanswered call is dated from the timeline occurrence")
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
		metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{ParticipantIDs: []int64{*participant.ParticipantID}})
		require.NoError(err)
		assert.Equal(int64(1), metrics.Totals.MeetingCount)
	}
	after := time.Unix(1801526300, 0)
	metrics, err := st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{SourceIDs: []int64{source.ID}, After: &after})
	require.NoError(err)
	assert.Equal(int64(1), metrics.Totals.MeetingCount)
	assert.Equal(int64(1), metrics.Totals.KnownDurationCount, "explicit zero differs from unknown duration")
	assert.Zero(metrics.Totals.TotalKnownSeconds)

	api.mu.Lock()
	message["call"] = map[string]any{"id": 601, "provider_call_id": "CA_synthetic", "provider": "twilio", "direction": "incoming", "status": "completed", "duration_seconds": 45,
		"accepted_by_agent_id": 7, "accepted_by_agent_name": "Example Agent", "transcript": "Updated call transcript words"}
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	assert.Equal(meetingID, contractArchivedMessageID(t, st, "call:201"))
	metrics, err = st.GetMeetingMetricsContext(t.Context(), store.MeetingQueryScope{SourceIDs: []int64{source.ID}})
	require.NoError(err)
	assert.Equal(int64(1), metrics.Totals.MeetingCount)
	assert.InDelta(45, metrics.Totals.TotalKnownSeconds, 1e-9)
	body, err := st.GetMessageBodyText(meetingID)
	require.NoError(err)
	assert.Contains(body, "Updated call transcript words")
}

func TestImportContractLateAudioTranscriptAndCredentialFreeCAS(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	payload := []byte("synthetic audio recording bytes")
	var router *chatwootMediaRouter
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"), "provider key must not accompany a media request")
		assert.Empty(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "audio/ogg")
		_, err := w.Write(payload)
		assert.NoError(err)
	}))
	t.Cleanup(media.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.Header.Get("Api_access_token"))
		http.Redirect(w, r, router.url(t, media, "/recording.ogg"), http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	router = newChatwootMediaRouter(t, redirect, media)
	message := contractMessage(301, 1801526400, nil)
	message["content"] = ""
	attachment := map[string]any{"id": 401, "message_id": 301, "file_type": "audio", "content_type": "audio/ogg", "extension": "ogg", "file_size": len(payload), "data_url": router.url(t, redirect, "/signed-audio"), "transcribed_text": ""}
	message["attachments"] = []any{attachment}
	api := newContractAPI(t, 2, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, IncludePrivate: true, Media: true, MaxMediaBytes: 1024, AttachmentsDir: t.TempDir()}
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

	// The provider does not advance conversation.updated_at when transcription
	// arrives or changes. A normal run must independently revisit old audio.
	for _, transcript := range []string{"firstquartz source transcript", "revisedquartz source transcript"} {
		api.mu.Lock()
		attachment["transcribed_text"] = transcript
		api.mu.Unlock()
		_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
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
		message := contractMessage(101, 1801526400, map[string]any{"id": int64(7), "type": "user", "name": "Example Agent"})
		message["message_type"] = 1
		api := newContractAPI(t, 2, []map[string]any{message})
		importer := NewImporter(st, api.client(t))
		sources, err := importer.Register(t.Context(), []int64{7, 8})
		require.NoError(err)
		require.Len(sources, 2)
		assert.NotEqual(sources[0].ID, sources[1].ID, "each inbox is a separate source")
		again, err := importer.Register(t.Context(), []int64{7, 8})
		require.NoError(err)
		require.Len(again, 2)
		assert.ElementsMatch([]int64{sources[0].ID, sources[1].ID}, []int64{again[0].ID, again[1].ID}, "registration is stable")
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

func TestImportContractDetachedSenderEvidenceAndUnknownSender(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	unknown := contractMessage(501, 1801526400, nil)
	unknown["message_type"] = 1
	detached := contractMessage(502, 1801526401, nil)
	detached["message_type"] = 1
	detached["sender_type"] = "User"
	detached["sender_id"] = int64(7)
	detached["future_evidence"] = map[string]any{"retained": "synthetic detached sender evidence"}
	known := contractMessage(503, 1801526402, map[string]any{"id": int64(7), "type": "user", "name": "Example Agent"})
	known["message_type"] = 1
	api := newContractAPI(t, 2, []map[string]any{unknown, detached, known})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	unknownID := contractArchivedMessageID(t, st, "501")
	assert.False(contractSender(t, st, unknownID).Valid, "outgoing direction cannot invent an employee")
	assert.Empty(contractRecipients(t, st, unknownID, "from"))
	detachedID := contractArchivedMessageID(t, st, "502")
	detachedSender := contractSender(t, st, detachedID)
	require.True(detachedSender.Valid)
	assert.Equal(contractSender(t, st, contractArchivedMessageID(t, st, "503")), detachedSender)
	raw, err := st.GetMessageRaw(detachedID)
	require.NoError(err)
	assert.Contains(string(raw), "synthetic detached sender evidence")
	assert.Contains(string(raw), "sender_id")
}

func TestImportContractMetadataOnlyAndUnknownAttachmentTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(601, 1801526400, nil)
	message["attachments"] = []any{
		map[string]any{"id": 701, "message_id": 601, "file_type": "future_type", "content_type": "application/vnd.example.future", "future_evidence": "synthetic retained metadata"},
		map[string]any{"id": 702, "message_id": 601, "file_type": "location", "coordinates_lat": 0.0, "coordinates_long": 0.0, "fallback_title": "Synthetic origin"},
		map[string]any{"id": 703, "message_id": 601, "file_type": "fallback", "fallback_title": "Synthetic fallback"},
	}
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true, Media: true, AttachmentsDir: t.TempDir()})
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
	var meetings int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type = 'meeting_transcript'`).Scan(&meetings))
	assert.Zero(meetings, "ordinary attachments do not create call meetings")
}

func TestImportContractOrdinaryKeywordAndEmbeddingEligibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	message := contractMessage(801, 1801526400, nil)
	message["content"] = "ordinaryquartz archived conversation text"
	api := newContractAPI(t, 2, []map[string]any{message})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	id := contractArchivedMessageID(t, st, "801")
	results, total, err := st.SearchMessages("ordinaryquartz", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(results, 1)
	assert.Equal(id, results[0].ID)
	assert.Equal(source.ID, results[0].SourceID)
	assert.Equal("chatwoot", results[0].MessageType)
	full, err := st.ScanForEmbeddingScoped(t.Context(), 1, 0, 100, nil, nil)
	require.NoError(err)
	assert.Equal([]int64{id}, full, "ordinary configured full-corpus embeddings include Chatwoot")
	scoped, err := st.ScanForEmbeddingScoped(t.Context(), 1, 0, 100, []string{"chatwoot"}, []int64{source.ID})
	require.NoError(err)
	assert.Equal([]int64{id}, scoped)
	otherType, err := st.ScanForEmbeddingScoped(t.Context(), 1, 0, 100, []string{"email"}, []int64{source.ID})
	require.NoError(err)
	assert.Empty(otherType, "provider classification is retained in scoped scans")
}
