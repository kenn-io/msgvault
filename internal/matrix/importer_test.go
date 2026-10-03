package matrix

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	matrixattachment "maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type matrixCryptoTestStateStore struct{}

func (matrixCryptoTestStateStore) IsEncrypted(context.Context, id.RoomID) (bool, error) {
	return true, nil
}

func (matrixCryptoTestStateStore) GetEncryptionEvent(context.Context, id.RoomID) (*event.EncryptionEventContent, error) {
	return &event.EncryptionEventContent{}, nil
}

func (matrixCryptoTestStateStore) GetHistoryVisibility(context.Context, id.RoomID) (*event.HistoryVisibilityEventContent, error) {
	return &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared}, nil
}

func (matrixCryptoTestStateStore) FindSharedRooms(context.Context, id.UserID) ([]id.RoomID, error) {
	return []id.RoomID{"!room:example.org"}, nil
}

func TestImporterBackfillsJoinedRoomAndPersistsCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("offline", r.URL.Query().Get("set_presence"),
			"every Matrix sync request must keep the archival account offline")
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[{"type":"m.room.name","state_key":"","content":{"name":"Example room"}}]},"timeline":{"events":[{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* updated","m.new_content":{"msgtype":"m.text","body":"updated"},"m.relates_to":{"rel_type":"m.replace","event_id":"$one"}}},{"type":"m.room.message","event_id":"$foreign-edit","sender":"@intruder:example.org","origin_server_ts":3500,"content":{"msgtype":"m.text","body":"* replaced by someone else","m.new_content":{"msgtype":"m.text","body":"replaced by someone else"},"m.relates_to":{"rel_type":"m.replace","event_id":"$one"}}},{"type":"m.reaction","event_id":"$reaction","sender":"@archive:example.org","origin_server_ts":4000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"👍"}}},{"type":"m.reaction","event_id":"$reaction-removed","sender":"@archive:example.org","origin_server_ts":4250,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"👎"}}},{"type":"m.room.redaction","event_id":"$redact-reaction","sender":"@archive:example.org","origin_server_ts":4500,"redacts":"$reaction-removed","content":{}},{"type":"m.room.redaction","event_id":"$redact-message","sender":"@archive:example.org","origin_server_ts":4750,"redacts":"$delete-me","content":{}},{"type":"org.matrix.msc4075.rtc.notification","event_id":"$unsupported-call","sender":"@member:example.org","origin_server_ts":4900,"content":{"notification_type":"ring"}},{"type":"m.room.encrypted","event_id":"$encrypted","sender":"@member:example.org","origin_server_ts":5000,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque","session_id":"session","sender_key":"key"}},{"type":"m.room.message","event_id":"$reply","sender":"@archive:example.org","origin_server_ts":5500,"content":{"msgtype":"m.text","body":"reply","m.relates_to":{"m.in_reply_to":{"event_id":"$one"}}}},{"type":"m.room.message","event_id":"$image","sender":"@member:example.org","origin_server_ts":6000,"content":{"msgtype":"m.image","body":"photo.jpg","url":"mxc://example.org/image","info":{"mimetype":"image/jpeg","size":1234}}},{"type":"m.room.message","event_id":"$orphan-reply","sender":"@member:example.org","origin_server_ts":6500,"content":{"msgtype":"m.text","body":"visible reply","m.relates_to":{"m.in_reply_to":{"event_id":"$not-visible"}}}}],"prev_batch":"older-1"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("older-1", r.URL.Query().Get("from"))
		assert.Equal("b", r.URL.Query().Get("dir"))
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$delete-me","sender":"@member:example.org","origin_server_ts":1500,"content":{"msgtype":"m.text","body":"remove this"}},{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}],"end":""}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: "@archive:example.org", NoMedia: true,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll},
	})
	require.NoError(err)
	assert.Equal(int64(6), sum.MessagesAdded)
	assert.Equal(int64(1), sum.Undecryptable)
	assert.Equal(int64(1), sum.EventsSkipped)
	assert.Equal(int64(1), sum.AttachmentsPending)
	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(5), count)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$one", "$delete-me", "$encrypted", "$reply", "$image", "$orphan-reply"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("updated", body)
	originalRaw, err := st.GetMessageRaw(messageIDs["$one"])
	require.NoError(err)
	var original event.Event
	require.NoError(json.Unmarshal(originalRaw, &original))
	require.NoError(original.Content.ParseRaw(original.Type))
	assert.Equal(id.EventID("$one"), original.ID)
	assert.Equal("original", original.Content.AsMessage().Body)
	body, err = st.GetMessageBodyText(messageIDs["$encrypted"])
	require.NoError(err)
	assert.Equal(encryptedPlaceholder, body)
	rawRows, err := st.ScanArchivedRawMessages(source.ID, rawFormat, 0, 10)
	require.NoError(err)
	var encrypted event.Event
	for _, row := range rawRows {
		var evt event.Event
		require.NoError(json.Unmarshal(row.RawData, &evt))
		if evt.ID == "$encrypted" {
			encrypted = evt
		}
	}
	assert.Equal(id.RoomID("!room:example.org"), encrypted.RoomID)
	var replyToMessageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageIDs["$reply"]).Scan(&replyToMessageID))
	assert.Equal(messageIDs["$one"], replyToMessageID)
	var orphanReplyTargetValid bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$orphan-reply"]).Scan(&orphanReplyTargetValid))
	assert.False(orphanReplyTargetValid)
	var deletedAtValid bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$delete-me"]).Scan(&deletedAtValid))
	assert.True(deletedAtValid)
	attachments, err := st.MessageMatrixAttachments(messageIDs["$image"])
	require.NoError(err)
	assert.Equal(attachmentpolicy.StatePending, attachments["matrix:mxc://example.org/image"].State)
	var reactions int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), messageIDs["$one"]).Scan(&reactions))
	assert.Equal(1, reactions)
	var sourceReactionID string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT rse.source_reaction_id
		FROM reaction_source_events rse
		JOIN reactions r ON r.id = rse.reaction_id
		WHERE r.message_id = ?`), messageIDs["$one"]).Scan(&sourceReactionID))
	assert.Equal("$reaction", sourceReactionID)
	assert.Equal(int64(1), sum.RelationsUnresolved)
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE source_id = ?`), source.ID).Scan(&conversationType))
	assert.Equal("direct_chat", conversationType)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal("next-1", state.NextBatch)
	assert.True(state.Rooms["!room:example.org"].Backfilled)
	assert.Empty(state.Undecryptable, "pending events stay out of the checkpoint")
	pendingEvent, ok := pendingUndecryptable(t, st, source.ID, "$encrypted")
	require.True(ok)
	assert.Equal("!room:example.org", pendingEvent.RoomID)

	fullSummary, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: "@archive:example.org", Full: true, NoMedia: true,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll},
	})
	require.NoError(err)
	assert.Equal(int64(0), fullSummary.MessagesAdded)
	assert.Equal(int64(1), fullSummary.RelationsUnresolved, "the orphan reply still has no target")
	body, err = st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("updated", body)
}

func TestImporterFullSyncIgnoresMalformedSavedCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.URL.Query().Get("since"))
		_, _ = w.Write([]byte(`{"next_batch":"fresh","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, "malformed saved cursor"))

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: source.Identifier, Full: true,
	})
	require.NoError(err)
}

func TestImporterDecryptsRealMegolmEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := t.Context()
	roomID := id.RoomID("!room:example.org")

	newMachine := func(userID id.UserID, deviceID id.DeviceID) (*crypto.OlmMachine, *crypto.MemoryStore) {
		client, err := mautrix.NewClient("https://example.invalid", userID, "token")
		require.NoError(err)
		client.DeviceID = deviceID
		memory := crypto.NewMemoryStore(nil)
		machine := crypto.NewOlmMachine(client, nil, memory, matrixCryptoTestStateStore{})
		require.NoError(machine.Load(ctx))
		return machine, memory
	}
	senderMachine, senderStore := newMachine("@sender:example.org", "SENDER")
	receiverMachine, receiverStore := newMachine("@archive:example.org", "ARCHIVE")
	outbound, err := crypto.NewOutboundGroupSession(roomID, &event.EncryptionEventContent{},
		&event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibilityShared})
	require.NoError(err)
	outbound.Shared = true
	require.NoError(senderStore.AddOutboundGroupSession(ctx, outbound))
	senderIdentity := senderMachine.OwnIdentity()
	inbound, err := crypto.NewInboundGroupSession(senderIdentity.IdentityKey, senderIdentity.SigningKey, roomID,
		outbound.Internal.Key(), 0, 0, outbound.SharedHistory, false)
	require.NoError(err)
	require.NoError(receiverStore.PutGroupSession(ctx, inbound))
	encryptedContent, err := senderMachine.EncryptMegolmEvent(ctx, roomID, event.EventMessage,
		&event.MessageEventContent{MsgType: event.MsgText, Body: "secret fixture"})
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, roomID.String(), "group_chat", "Encrypted")
	require.NoError(err)
	receiverClient, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	runtime := &Runtime{Client: receiverClient, decryptEvent: receiverMachine.DecryptMegolmEvent}
	encryptedEvent := &event.Event{
		ID: "$encrypted", RoomID: roomID, Sender: "@sender:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: encryptedContent},
	}
	decrypted, err := receiverMachine.DecryptMegolmEvent(ctx, encryptedEvent)
	require.NoError(err)
	require.Equal(event.EventMessage, decrypted.Type)
	encryptedContent, err = senderMachine.EncryptMegolmEvent(ctx, roomID, event.EventMessage,
		&event.MessageEventContent{MsgType: event.MsgText, Body: "secret fixture"})
	require.NoError(err)
	encryptedEvent.ID = "$encrypted-2"
	encryptedEvent.Content = event.Content{Parsed: encryptedContent}
	require.NoError(NewImporter(st, runtime).persistEvent(ctx, source.ID, conversationID, encryptedEvent,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$encrypted-2"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$encrypted-2"])
	require.NoError(err)
	assert.Equal("secret fixture", body)
}

func TestImporterDecryptFailurePreservesArchivedPlaintext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversationWithType(source.ID, roomID.String(), "group_chat", "Encrypted")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$replayed-encrypted", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$replayed-encrypted","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"retained plaintext"}}`)
	decrypted.RoomID = roomID
	success := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return decrypted, nil
	}})
	require.NoError(success.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	failing := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return nil, errors.New("synthetic missing session")
	}})
	sum := &ImportSummary{}
	require.NoError(failing.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs[encrypted.ID.String()])
	require.NoError(err)
	assert.Equal("retained plaintext", body)
	archivedRaw, err := st.GetMessageRaw(messageIDs[encrypted.ID.String()])
	require.NoError(err)
	var archived event.Event
	require.NoError(json.Unmarshal(archivedRaw, &archived))
	assert.Equal(event.EventMessage, archived.Type)
	assert.Zero(sum.Undecryptable, "an event decrypted before is already archived")
	_, ok := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.False(ok)
}

func TestLegacyPendingCiphertextCheckpointPrecedesFullReplay(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client := relationsClient(t, nil)
	encrypted := &event.Event{
		ID: "$legacy-full", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(
		t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	messageID := messageIDs[encrypted.ID.String()]
	require.NotZero(messageID)

	require.NoError(st.DeleteMatrixUndecryptableEvent(source.ID, encrypted.ID.String()))
	state := newSyncState()
	state.Undecryptable = map[string]UndecryptableEvent{encrypted.ID.String(): {RoomID: roomID.String()}}
	importer := NewImporter(st, nil)
	require.NoError(importer.migrateLegacyUndecryptable(source.ID, state))
	assert.Empty(state.Undecryptable)
	migrated, ok := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	require.True(ok)
	assert.Empty(migrated.RawEvent)

	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$legacy-full","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"recovered"}}`)
	decrypted.RoomID = roomID
	runtime := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return decrypted, nil
	}}
	require.NoError(NewImporter(st, runtime).recordLegacyCiphertext(source.ID, encrypted))
	recorded, ok := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	require.True(ok)
	assert.NotEmpty(recorded.RawEvent,
		"the ciphertext must be durable before decrypted raw can replace it")

	decryptedRaw, err := json.Marshal(decrypted, json.Deterministic(true))
	require.NoError(err)
	require.NoError(st.UpsertMessageRawWithFormat(messageID, decryptedRaw, rawFormat))

	_, err = NewImporter(st, runtime).retryUndecryptable(t.Context(), source.ID, state,
		ImportOptions{}, &ImportSummary{})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	assert.Equal("recovered", body)
	_, ok = pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.False(ok)
}

func TestFullSyncRetriesArchivedCiphertextOmittedByHomeserver(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"fresh","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID(source.Identifier), "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$retained-ciphertext", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(
		t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2},
		ImportOptions{NoMedia: true}, &ImportSummary{}))
	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$retained-ciphertext","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"recovered after full sync"}}`)
	decrypted.RoomID = roomID
	runtime := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return decrypted, nil
	}}

	summary, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{
		UserID: source.Identifier, Full: true,
	})
	require.NoError(err)
	assert.Equal(int64(1), summary.UndecryptableRecovered)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs[encrypted.ID.String()])
	require.NoError(err)
	assert.Equal("recovered after full sync", body)
}

func TestUndecryptableRecoveryHonorsRoomFilters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!excluded:example.org")
	conversationID, err := st.EnsureConversationWithType(source.ID, roomID.String(), "group_chat", "Excluded")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	initial := NewImporter(st, &Runtime{Client: client})
	encrypted := &event.Event{
		ID: "$encrypted-excluded", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	require.NoError(initial.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}, ImportOptions{NoMedia: true}, &ImportSummary{}))
	pending, ok := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	require.True(ok)
	assert.NotEmpty(pending.RawEvent)
	state := newSyncState()
	decryptCalls := 0
	recovering := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		decryptCalls++
		return nil, errors.New("unexpected decrypt")
	}})

	_, err = recovering.retryUndecryptable(t.Context(), source.ID, state,
		ImportOptions{ExcludeRooms: []string{roomID.String()}}, &ImportSummary{})
	require.NoError(err)
	assert.Zero(decryptCalls)
	_, retained := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.True(retained)
}

func TestImporterDownloadsAndDecryptsEncryptedMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	plaintext := []byte("synthetic encrypted image bytes")
	ciphertext := append([]byte(nil), plaintext...)
	encryptedFile := matrixattachment.NewEncryptedFile()
	encryptedFile.EncryptInPlace(ciphertext)
	content := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    "fixture.jpg",
		File: &event.EncryptedFileInfo{
			EncryptedFile: *encryptedFile,
			URL:           "mxc://example.org/encrypted-image",
		},
		Info: &event.FileInfo{MimeType: "image/jpeg", Size: len(ciphertext)},
	}
	syncBody, err := json.Marshal(map[string]any{
		"next_batch": "next-1",
		"rooms": map[string]any{"join": map[string]any{"!room:example.org": map[string]any{
			"state": map[string]any{"events": []any{}},
			"timeline": map[string]any{"events": []any{map[string]any{
				"type": "m.room.message", "event_id": "$encrypted-image", "sender": "@member:example.org",
				"origin_server_ts": 1000, "content": content,
			}}},
		}}},
	})
	require.NoError(err)
	downloaded := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(syncBody)
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/encrypted-image", func(w http.ResponseWriter, _ *http.Request) {
		downloaded = true
		_, _ = w.Write(ciphertext)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	attachmentsDir := t.TempDir()
	mediaMutations := 0
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: "@archive:example.org", AttachmentsDir: attachmentsDir,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 1 << 20},
		MediaMutation: func(_ context.Context, write func() error) error {
			assert.True(downloaded, "the media download must finish before acquiring the mutation lease")
			mediaMutations++
			return write()
		},
	})
	require.NoError(err)
	assert.Equal(1, mediaMutations)
	assert.Equal(int64(1), sum.AttachmentsDownloaded)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$encrypted-image"})
	require.NoError(err)
	attachments, err := st.MessageMatrixAttachments(messages["$encrypted-image"])
	require.NoError(err)
	archived := attachments["matrix:mxc://example.org/encrypted-image"]
	assert.Equal(attachmentpolicy.StateStored, archived.State)
	var sizeEstimate int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messages["$encrypted-image"]).Scan(&sizeEstimate))
	assert.Equal(int64(len("[image]")+len(plaintext)), sizeEstimate)
	stored, err := os.ReadFile(filepath.Join(attachmentsDir, archived.StoragePath))
	require.NoError(err)
	assert.Equal(plaintext, stored)
}

func TestImporterArchivesStickerAsImageAttachment(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/sticker", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sticker bytes"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Stickers")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	opts := ImportOptions{AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	evt := matrixTestEvent(t, `{"type":"m.sticker","event_id":"$sticker","sender":"@member:example.org","origin_server_ts":1000,"content":{"body":"wave","url":"mxc://example.org/sticker","info":{"mimetype":"image/png","size":13}}}`)

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$sticker"})
	require.NoError(err)
	attachments, err := st.MessageMatrixAttachments(messageIDs["$sticker"])
	require.NoError(err)
	require.Contains(attachments, "matrix:mxc://example.org/sticker")
	assert.Equal(attachmentpolicy.StateStored, attachments["matrix:mxc://example.org/sticker"].State)
}

func TestImporterRetriesFailedMediaWithoutReplacingMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/retry", func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, "synthetic failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("recovered media"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	opts := ImportOptions{AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	evt := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$retry-media","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.file","body":"retry.txt","url":"mxc://example.org/retry","info":{"mimetype":"text/plain","size":15}}}`)

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$retry-media"})
	require.NoError(err)
	messageID := messageIDs["$retry-media"]
	require.NotZero(messageID)
	firstRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	attachments, err := st.MessageMatrixAttachments(messageID)
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateFailed, attachments["matrix:mxc://example.org/retry"].State)

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, &ImportSummary{}))
	replayedIDs, err := st.MessageExistsBatch(source.ID, []string{"$retry-media"})
	require.NoError(err)
	assert.Equal(messageID, replayedIDs["$retry-media"])
	secondRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Equal(firstRaw, secondRaw)
	attachments, err = st.MessageMatrixAttachments(messageID)
	require.NoError(err)
	assert.Equal(attachmentpolicy.StateStored, attachments["matrix:mxc://example.org/retry"].State)
	assert.Equal(2, requests)
}

func TestImporterMediaEditReplacesAttachmentAndStoresEmptyFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/original", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("original bytes"))
	})
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/replacement", func(http.ResponseWriter, *http.Request) {})
	// No edit survives each redaction, so the original is shown again.
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	opts := ImportOptions{AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	sum := &ImportSummary{}
	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.file","body":"original.txt","filename":"original.txt","url":"mxc://example.org/original","info":{"mimetype":"text/plain","size":14}}}`)
	edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.file","body":"* replacement.txt","m.new_content":{"msgtype":"m.file","body":"replacement.txt","filename":"replacement.txt","url":"mxc://example.org/replacement","info":{"mimetype":"text/plain","size":0}},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, original, conversation, opts, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, edit, conversation, opts, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$media"})
	require.NoError(err)
	attachments, err := st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	require.Len(attachments, 1)
	replacement := attachments["matrix:mxc://example.org/replacement"]
	assert.Equal("replacement.txt", replacement.Filename)
	assert.Equal(attachmentpolicy.StateStored, replacement.State)
	assert.NotEmpty(replacement.StoragePath, "a present zero-byte response retains a blob and occurrence")
	stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, replacement.StoragePath))
	require.NoError(err)
	assert.Empty(stored)
	redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact-media-edit","sender":"@member:example.org","origin_server_ts":3000,"redacts":"$media-edit","content":{}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, conversation,
		ImportOptions{NoMedia: true}, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	restored := attachments["matrix:mxc://example.org/original"]
	assert.Equal(attachmentpolicy.StateStored, restored.State)
	assert.NotEmpty(restored.StoragePath, "redacting a media edit reuses the archived original blob")
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM matrix_media_cache WHERE message_id = ?`), messageIDs["$media"])
	require.NoError(err, "simulate legacy media before a text replacement")
	textEdit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media-text","sender":"@member:example.org","origin_server_ts":3200,"content":{"msgtype":"m.text","body":"* now text","m.new_content":{"msgtype":"m.text","body":"now text"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, textEdit, conversation, opts, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	assert.Empty(attachments, "a selected text replacement clears the media occurrence")
	redactText := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact-media-text","sender":"@member:example.org","origin_server_ts":3300,"redacts":"$media-text","content":{}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redactText, conversation,
		ImportOptions{NoMedia: true}, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	restored = attachments["matrix:mxc://example.org/original"]
	assert.Equal(attachmentpolicy.StateStored, restored.State)
	assert.NotEmpty(restored.StoragePath, "a text replacement caches legacy media before clearing it")

	renamed := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media-renamed","sender":"@member:example.org","origin_server_ts":3500,"content":{"msgtype":"m.file","body":"* renamed.bin","m.new_content":{"msgtype":"m.file","body":"renamed.bin","filename":"renamed.bin","url":"mxc://example.org/original","info":{"mimetype":"application/octet-stream","size":999}} ,"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, renamed, conversation,
		ImportOptions{NoMedia: true}, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	restored = attachments["matrix:mxc://example.org/original"]
	assert.Equal("renamed.bin", restored.Filename, "cache hits retain the selected version's filename")
	assert.Equal("application/octet-stream", restored.MimeType, "cache hits retain the selected version's MIME type")
	assert.Equal(14, restored.Size, "cache hits retain the observed stored size")
	_, err = st.DB().Exec(st.Rebind(`DELETE FROM matrix_media_cache WHERE message_id = ?`), messageIDs["$media"])
	require.NoError(err, "simulate an archive created before the media cache existed")

	noURL := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$media-no-url","sender":"@member:example.org","origin_server_ts":4000,"content":{"msgtype":"m.file","body":"* unavailable.txt","m.new_content":{"msgtype":"m.file","body":"unavailable.txt","filename":"unavailable.txt"},"m.relates_to":{"rel_type":"m.replace","event_id":"$media"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, noURL, conversation, opts, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	assert.Empty(attachments, "a selected media version without a URL clears the previous attachment")
	redactNoURL := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact-no-url","sender":"@member:example.org","origin_server_ts":5000,"redacts":"$media-no-url","content":{}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redactNoURL, conversation,
		ImportOptions{NoMedia: true}, sum))
	attachments, err = st.MessageMatrixAttachments(messageIDs["$media"])
	require.NoError(err)
	restored = attachments["matrix:mxc://example.org/original"]
	assert.Equal(attachmentpolicy.StateStored, restored.State)
	assert.NotEmpty(restored.StoragePath, "a URL-less edit caches a legacy stored mapping before clearing it")
}

func TestImporterMediaReplayUsesObservedOversize(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var downloads int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/large", func(w http.ResponseWriter, _ *http.Request) {
		downloads++
		_, _ = w.Write([]byte("larger than the cap"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Media")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	opts := ImportOptions{AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 4}}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	evt := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$large","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.file","body":"large.bin","url":"mxc://example.org/large","info":{"mimetype":"application/octet-stream","size":0}}}`)

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, &ImportSummary{}))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, &ImportSummary{}))
	assert.Equal(1, downloads)
}

func TestImporterDoesNotRecreateRemovedSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID(source.Identifier), "token")
	require.NoError(err)

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: source.Identifier,
	})

	require.ErrorIs(err, store.ErrSourceNotFound)
	sources, listErr := st.ListSources(SourceType)
	require.NoError(listErr)
	assert.Empty(sources)
}

func TestRoomIncluded(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		want    bool
	}{
		{name: "all rooms by default", want: true},
		{name: "included room", include: []string{"!room:example.org"}, want: true},
		{name: "not in include list", include: []string{"!other:example.org"}},
		{name: "excluded room", exclude: []string{"!room:example.org"}},
		{name: "exclude wins", include: []string{"!room:example.org"}, exclude: []string{"!room:example.org"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, roomIncluded("!room:example.org", tt.include, tt.exclude))
		})
	}
}

func TestRoomTitleUsesLatestTimelineRename(t *testing.T) {
	assert := assert.New(t)
	first := matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-1","state_key":"","content":{"name":"First"}}`)
	second := matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-2","state_key":"","content":{"name":"Second"}}`)
	title, present := roomTitle([]*event.Event{first, second})
	assert.True(present)
	assert.Equal("Second", title)
	empty, present := roomTitle([]*event.Event{matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-3","state_key":"","content":{"name":""}}`)})
	assert.True(present)
	assert.Empty(empty)
}

func TestImporterPlaintextSyncDoesNotAcquireMediaMutation(t *testing.T) {
	require := require.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$plaintext","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"plain"}}]}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: "@archive:example.org",
		MediaMutation: func(context.Context, func() error) error {
			require.FailNow("plaintext sync must not acquire the attachment mutation lease")
			return nil
		},
	})
	require.NoError(err)
}

func TestImporterDecryptsEachTimelineEventOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.encrypted","event_id":"$encrypted-once","sender":"@member:example.org","origin_server_ts":1000,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque","session_id":"session","sender_key":"key"}}]}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$encrypted-once","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"decrypted once"}}`)
	decryptCalls := 0
	runtime := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		decryptCalls++
		return decrypted, nil
	}}

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Equal(1, decryptCalls)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$encrypted-once"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$encrypted-once"])
	require.NoError(err)
	assert.Equal("decrypted once", body)
}

func TestImporterReclassifiesInactiveRoomFromDirectAccountData(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[]}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	importer := NewImporter(st, &Runtime{Client: client})
	_, err = importer.Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE source_conversation_id = ?`), "!room:example.org").Scan(&conversationType))
	assert.Equal("direct_chat", conversationType)
}

func TestImporterExplicitEmptyRoomNameClearsArchivedTitle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		name := "Named room"
		if syncCalls > 1 {
			name = ""
		}
		_, _ = fmt.Fprintf(w, `{"next_batch":"next-%d","rooms":{"join":{"!room:example.org":{"state":{"events":[{"type":"m.room.name","state_key":"","content":{"name":%q}}]},"timeline":{"events":[]}}}}}`, syncCalls, name)
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member fallback"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	var title string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT title FROM conversations WHERE source_conversation_id = ?`), "!room:example.org").Scan(&title))
	assert.Empty(title)
}

func TestLatestEditWinsOriginalAtEqualTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$z-original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$a-edit","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$z-original"}}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$z-original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$z-original"])
	require.NoError(err)
	assert.Equal("edited", body)
}

func TestImporterSkipsFirstSeenStrippedMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$already-redacted","sender":"@member:example.org","origin_server_ts":1000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$already-redacted"})
	require.NoError(err)
	assert.Zero(messageIDs["$already-redacted"])
}

func TestImporterKeepsNewestEditAndRecomputesAfterRedaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, map[id.EventID]string{
		"$original": `{"type":"m.room.message","event_id":"$older","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* old","m.new_content":{"msgtype":"m.text","body":"old"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
	})})
	sum := &ImportSummary{}

	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`)
	newer := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$newer","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* newer version","m.new_content":{"msgtype":"m.text","body":"newer version"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	older := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$older","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* old","m.new_content":{"msgtype":"m.text","body":"old"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":4000,"redacts":"$newer","content":{}}`)
	for _, evt := range []*event.Event{original, newer, older} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt,
			attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	}
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("newer version", body, "an older edit from a later sync must not win")
	var sizeEstimate int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&sizeEstimate))
	assert.Equal(int64(len("newer version")), sizeEstimate)
	labelID, err := st.EnsureLabel(source.ID, "local-review", "Local review", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(messageIDs["$original"], []int64{labelID}))
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET embed_gen = 7 WHERE id = ?`), messageIDs["$original"])
	require.NoError(err)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, original,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	var embedGen int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&embedGen))
	assert.Equal(int64(7), embedGen, "unchanged full replay must preserve the selected edit's embedding generation")
	labelIDs, err := st.MessageLabelIDsContext(t.Context(), messageIDs["$original"])
	require.NoError(err)
	assert.Equal([]int64{labelID}, labelIDs, "provider replay must preserve locally assigned labels")
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	body, err = st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("old", body, "redacting the winning edit selects the latest survivor")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&sizeEstimate))
	assert.Equal(int64(len("old")), sizeEstimate)
}

func TestImporterRejectsCrossRoomRelations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomA, err := st.EnsureConversationWithType(source.ID, "!room-a:example.org", "group_chat", "Room A")
	require.NoError(err)
	roomB, err := st.EnsureConversationWithType(source.ID, "!room-b:example.org", "group_chat", "Room B")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$room-a-message","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"room A"}}`)
	edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$room-a-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$room-a-message"}}}`)
	reaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$room-a-reaction","sender":"@member:example.org","origin_server_ts":3000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$room-a-message","key":"ok"}}}`)
	for _, evt := range []*event.Event{original, edit, reaction} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, roomA, evt,
			attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	}
	crossRoomReaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$room-b-reaction","sender":"@member:example.org","origin_server_ts":4000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$room-a-message","key":"no"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, roomB, crossRoomReaction,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	for i, target := range []string{"$room-a-message", "$room-a-edit", "$room-a-reaction"} {
		redaction := matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.redaction","event_id":"$room-b-redaction-%d","sender":"@member:example.org","origin_server_ts":%d,"redacts":%q,"content":{}}`, i, 5000+i, target))
		if err := imp.persistEvent(t.Context(), source.ID, roomB, redaction, attachmentpolicy.Conversation{}, ImportOptions{}, sum); err != nil {
			require.ErrorIs(err, errRelationTargetMissing)
		}
	}
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Equal(1, reactions)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$room-a-message"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$room-a-message"])
	require.NoError(err)
	assert.Equal("edited", body)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`),
		messageIDs["$room-a-message"]).Scan(&deleted))
	assert.False(deleted)
}

func TestImporterDoesNotRestoreRedactedRelationPayloads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	opts := ImportOptions{NoMedia: true}
	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`)
	reaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$reaction","sender":"@member:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$original","key":"ok"}}}`)
	edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	for _, evt := range []*event.Event{original, reaction, edit} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, conversation, opts, sum))
	}
	for _, target := range []string{"$reaction", "$edit"} {
		redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":4000,"redacts":"`+target+`","content":{}}`)
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, conversation, opts, sum))
	}
	// The homeserver serves redacted events stripped, with the redaction attached.
	for _, eventID := range []string{"$reaction", "$edit"} {
		stripped := matrixTestEvent(t, `{"type":"m.room.message","event_id":"`+eventID+`","sender":"@member:example.org","origin_server_ts":2000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","content":{}}}}`)
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, stripped, attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	}
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Zero(reactions)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterFullReplayPreservesRedactedMessageContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"retained"}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$one"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("retained", body)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$one"]).Scan(&deleted))
	assert.True(deleted)
}

func TestImporterAppliesTargetOfStrippedRedaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	opts := ImportOptions{NoMedia: true}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$message","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"retained"}}`), conversation, opts, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","origin_server_ts":2000,"content":{"redacts":"$message"},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redact-redaction","sender":"@member:example.org","content":{}}}}`), conversation, opts, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$message"})
	require.NoError(err)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$message"]).Scan(&deleted))
	assert.True(deleted)
}

func TestImporterFullReplayRedactedEditDoesNotCreateMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original-edit-target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$stripped-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original-edit-target"}}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$stripped-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`),
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original-edit-target", "$stripped-edit"})
	require.NoError(err)
	assert.Zero(messageIDs["$stripped-edit"])
	body, err := st.GetMessageBodyText(messageIDs["$original-edit-target"])
	require.NoError(err)
	assert.Equal("original", body)
}

func TestStrippedEncryptedReactionRemovesRetainedReaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$reaction-target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"target"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.reaction","event_id":"$encrypted-reaction","sender":"@archive:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$reaction-target","key":"ok"}}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.encrypted","event_id":"$encrypted-reaction","sender":"@archive:example.org","origin_server_ts":2000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@archive:example.org","content":{}}}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	var reactions int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM reaction_source_events
		WHERE source_id = ? AND source_reaction_id = ?`), source.ID, "$encrypted-reaction").Scan(&reactions))
	assert.Zero(reactions)
}

func matrixTestEvent(t *testing.T, raw string) *event.Event {
	t.Helper()
	var evt event.Event
	require.NoError(t, json.Unmarshal([]byte(raw), &evt))
	evt.RoomID = "!room:example.org"
	return &evt
}

func TestMessageBodyStripsMatrixReplyFallback(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "> <@other:example.org> quoted text\n>\nreply text",
		Format:        event.FormatHTML,
		FormattedBody: "<mx-reply><blockquote>quoted text</blockquote></mx-reply><p>reply text</p>",
		RelatesTo:     &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
	}
	originalBody := content.Body
	originalFormattedBody := content.FormattedBody

	body := messageBody(content)

	assert.Equal("reply text", body)
	assert.NotContains(body, "quoted text")
	assert.Equal(originalBody, content.Body, "raw event content must remain unchanged")
	assert.Equal(originalFormattedBody, content.FormattedBody,
		"raw formatted event content must remain unchanged")
}

func TestOnlyMutatingMessageRelationsAreDeferred(t *testing.T) {
	assert := assert.New(t)
	reply := &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType:   event.MsgText,
		RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
	}}}
	assert.False(shouldDeferRelation(reply), "reply messages must exist before reactions are replayed")

	edit := &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType:   event.MsgText,
		RelatesTo: &event.RelatesTo{Type: event.RelReplace, EventID: "$target"},
	}}}
	assert.True(shouldDeferRelation(edit))
}

func TestDeferredReplyResolutionDoesNotOverwriteEditedBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	targetID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$target", MessageType: SourceType,
	})
	require.NoError(err)
	replyID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: "$reply", MessageType: SourceType,
		},
		BodyText: sql.NullString{String: "newest edit", Valid: true},
	})
	require.NoError(err)
	reply := &event.Event{
		ID: "$reply", Type: event.EventMessage,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText, Body: "original reply",
			RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
		}},
	}
	rawReply, err := json.Marshal(reply, json.Deterministic(true))
	require.NoError(err)
	var restoredReply event.Event
	require.NoError(json.Unmarshal(rawReply, &restoredReply))
	assert.Nil(restoredReply.Content.Parsed)

	require.NoError(NewImporter(st, nil).replayDeferredRelation(
		t.Context(), source.ID, conversationID, &restoredReply, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{},
	))
	body, err := st.GetMessageBodyText(replyID)
	require.NoError(err)
	assert.Equal("newest edit", body)
	var replyTo int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), replyID).Scan(&replyTo))
	assert.Equal(targetID, replyTo)
}

func TestEncryptedReplyDeferredUntilBackfilledTargetRemainsVisible(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$encrypted-reply", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$encrypted-reply","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"reply","m.relates_to":{"m.in_reply_to":{"event_id":"$backfilled-target"}}}}`)
	imp := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return decrypted, nil
	}})
	sum := &ImportSummary{}
	err = imp.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum)
	require.ErrorIs(err, errRelationTargetMissing)
	deferred, err := imp.deferredRelationAfterPersist(source.ID, encrypted)
	require.NoError(err)
	assert.Equal(event.EventMessage, deferred.Type)

	targetID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$backfilled-target", MessageType: SourceType,
	})
	require.NoError(err)
	require.NoError(imp.replayDeferredRelation(t.Context(), source.ID, conversationID, deferred,
		attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.finalizeDeferredRecovery(source.ID, deferred, sum))

	found, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	replyID := found[encrypted.ID.String()]
	require.NotZero(replyID)
	var replyTo int64
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id, deleted_from_source_at IS NOT NULL
		FROM messages WHERE id = ?`), replyID).Scan(&replyTo, &deleted))
	assert.Equal(targetID, replyTo)
	assert.False(deleted)
}

func TestFailedReplayCannotReplaceRecoveredEncryptedEdit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	base := NewImporter(st, &Runtime{Client: client})
	require.NoError(base.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit-target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	encrypted := &event.Event{
		ID: "$encrypted-edit", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	decrypted := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$encrypted-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$edit-target"}}}`)
	recovered := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return decrypted, nil
	}})
	require.NoError(recovered.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	failing := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return nil, errors.New("synthetic missing session")
	}})
	require.NoError(failing.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	require.NoError(base.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit-target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	found, err := st.MessageExistsBatch(source.ID, []string{"$edit-target"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(found["$edit-target"])
	require.NoError(err)
	assert.Equal("edited", body)
}

func TestRecoveredEncryptedRelationsRetirePlaceholders(t *testing.T) {
	for _, tt := range []struct {
		name      string
		decrypted string
	}{
		{
			name:      "edit",
			decrypted: `{"type":"m.room.message","event_id":"$encrypted-relation","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$target"}}}`,
		},
		{
			name:      "reaction",
			decrypted: `{"type":"m.reaction","event_id":"$encrypted-relation","sender":"@member:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$target","key":"ok"}}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
			require.NoError(err)
			roomID := id.RoomID("!room:example.org")
			conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
			require.NoError(err)
			client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
			require.NoError(err)
			baseline := NewImporter(st, &Runtime{Client: client})
			require.NoError(baseline.persistEvent(t.Context(), source.ID, conversationID,
				matrixTestEvent(t, `{"type":"m.room.message","event_id":"$target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`),
				attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
			encrypted := &event.Event{ID: "$encrypted-relation", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
				Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}}}
			require.NoError(baseline.persistEvent(t.Context(), source.ID, conversationID, encrypted,
				attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
			recovered := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
				return matrixTestEvent(t, tt.decrypted), nil
			}})
			require.NoError(recovered.persistEvent(t.Context(), source.ID, conversationID, encrypted,
				attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))

			messageIDs, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
			require.NoError(err)
			var deleted bool
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs[encrypted.ID.String()]).Scan(&deleted))
			assert.True(deleted)
		})
	}
}

func TestRecoveredRelationWithoutTargetKeepsPendingRowUntilCursorStored(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{ID: "$encrypted-reaction", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}}}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	imp := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.reaction","event_id":"$encrypted-reaction","sender":"@member:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$not-archived","key":"ok"}}}`), nil
	}})
	placeholderDeleted := func() bool {
		ids, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
		require.NoError(err)
		var deleted bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), ids[encrypted.ID.String()]).Scan(&deleted))
		return deleted
	}

	// A run interrupted before the cursor update loses the in-memory state.
	state := newSyncState()
	_, err = imp.retryUndecryptable(t.Context(), source.ID, state, ImportOptions{}, &ImportSummary{})
	require.NoError(err)
	_, pending := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.True(pending, "the pending row survives until the relation is durably stored")
	assert.False(placeholderDeleted())

	state = newSyncState()
	settle, err := imp.retryUndecryptable(t.Context(), source.ID, state, ImportOptions{}, &ImportSummary{})
	require.NoError(err)
	require.Len(state.Rooms[roomID.String()].DeferredRelations, 1, "the next run recovers the relation again")
	require.NoError(settle())
	_, pending = pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.False(pending)
	assert.True(placeholderDeleted())
}

func TestCiphertextIsNotProofOfArchiveWhenPlaintextPersistFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{ID: "$encrypted-message", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failing := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		// The interruption arrives after decryption, before the plaintext is stored.
		cancel()
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$encrypted-message","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"secret"}}`), nil
	}})
	require.Error(failing.persistEvent(ctx, source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	_, _, err = st.MatrixEncryptedEvent(source.ID, encrypted.ID.String())
	require.ErrorIs(err, sql.ErrNoRows, "ciphertext is retained only once the plaintext is stored")

	// The next sync cannot decrypt, so the event must be kept as a placeholder
	// instead of being taken as already archived.
	sum := &ImportSummary{}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	assert.Equal(int64(1), sum.Undecryptable)
	_, pending := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.True(pending)
}

func TestKeylessReplayKeepsPlaintextStoredBeforeItsCiphertext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	// An earlier run stored the decrypted message and stopped before it
	// retained the ciphertext.
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$encrypted-message","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"secret"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	encrypted := &event.Event{ID: "$encrypted-message", RoomID: roomID, Sender: "@member:example.org", Timestamp: 2000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}}}

	sum := &ImportSummary{}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	assert.Zero(sum.Undecryptable)
	_, pending := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.False(pending)
	_, _, err = st.MatrixEncryptedEvent(source.ID, encrypted.ID.String())
	require.NoError(err, "the replay retains the ciphertext the earlier run did not")
	found, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	body, err := st.GetMessageBodyText(found[encrypted.ID.String()])
	require.NoError(err)
	assert.Equal("secret", body)
}

func TestFailedReplayCannotCreatePlaceholderForRecoveredReaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$reaction-target-replay","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"target"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	encrypted := &event.Event{
		ID: "$recovered-reaction", RoomID: "!room:example.org", Sender: "@archive:example.org",
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	decrypting := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.reaction","event_id":"$recovered-reaction","sender":"@archive:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$reaction-target-replay","key":"ok"}}}`), nil
	}})
	require.NoError(decrypting.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	failing := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return nil, errors.New("synthetic missing session")
	}})
	require.NoError(failing.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	found, err := st.MessageExistsBatch(source.ID, []string{encrypted.ID.String()})
	require.NoError(err)
	assert.Zero(found[encrypted.ID.String()])
}

func TestLegacyCursorSeedsArchivedEncryptedPlaceholders(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$legacy-untracked", RoomID: "!room:example.org", Sender: "@member:example.org",
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(
		t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	require.NoError(NewImporter(st, nil).seedLegacyUndecryptable(t.Context(), source.ID))
	pending, ok := pendingUndecryptable(t, st, source.ID, encrypted.ID.String())
	assert.True(ok)
	assert.Equal(encrypted.RoomID.String(), pending.RoomID)
	assert.NotEmpty(pending.RawEvent)
}

func TestDecryptedUnsupportedEventIsSkipped(t *testing.T) {
	evt := &event.Event{
		ID:   "$decrypted-call",
		Type: event.Type{Type: "org.matrix.msc4075.rtc.notification", Class: event.MessageEventType},
		Content: event.Content{Raw: map[string]any{
			"notification_type": "ring",
		}},
	}

	assert.False(t, parseContent(evt))
}

func TestRecoveredUnsupportedEventRemovesPlaceholder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "$decrypted-call",
		MessageType:     SourceType,
	})
	require.NoError(err)
	require.NoError(st.PutMatrixUndecryptableEvents(source.ID, []store.MatrixUndecryptableEvent{
		{EventID: "$decrypted-call", RoomID: "!room:example.org"},
	}))

	require.NoError(NewImporter(st, nil).discardRecoveredUnsupported(source.ID, "$decrypted-call"))
	_, pendingLeft := pendingUndecryptable(t, st, source.ID, "$decrypted-call")
	assert.False(pendingLeft)
	var deleted bool
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID,
	).Scan(&deleted))
	assert.True(deleted)
}

func TestImmediatelyDecryptedUnsupportedEventRetiresOrphanedPlaceholder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$unsupported-orphan", MessageType: SourceType,
	})
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$unsupported-orphan", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	unsupported := matrixTestEvent(t, `{"type":"org.matrix.msc4075.rtc.notification","event_id":"$unsupported-orphan","sender":"@member:example.org","origin_server_ts":1000,"content":{"notification_type":"ring"}}`)
	imp := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return unsupported, nil
	}})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	var deleted bool
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID,
	).Scan(&deleted))
	assert.True(deleted)
}

func TestDecryptedReplacementWithoutNewContentRetiresPlaceholder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$bare-replacement", MessageType: SourceType,
	})
	require.NoError(err)
	require.NoError(st.PutMatrixUndecryptableEvents(source.ID, []store.MatrixUndecryptableEvent{
		{EventID: "$bare-replacement", RoomID: roomID.String()},
	}))
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$bare-replacement", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	replacement := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$bare-replacement","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"* edited","m.relates_to":{"rel_type":"m.replace","event_id":"$target"}}}`)
	imp := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return replacement, nil
	}})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, encrypted,
		attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))

	_, pendingLeft := pendingUndecryptable(t, st, source.ID, "$bare-replacement")
	assert.False(pendingLeft)
	var deleted bool
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID,
	).Scan(&deleted))
	assert.True(deleted, "the placeholder must not outlive its pending entry")
}

func TestDeferredRecoveredEditRemovesPendingPlaceholder(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$older-edit", MessageType: SourceType,
	})
	require.NoError(err)
	imp := NewImporter(st, nil)
	require.NoError(st.PutMatrixUndecryptableEvents(source.ID, []store.MatrixUndecryptableEvent{
		{EventID: "$older-edit", RoomID: "!room:example.org"},
	}))
	sum := &ImportSummary{}
	edit := &event.Event{
		ID: "$older-edit", Type: event.EventMessage,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText, NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "older"},
			RelatesTo: &event.RelatesTo{Type: event.RelReplace, EventID: "$target"},
		}},
	}

	require.NoError(imp.finalizeDeferredRecovery(source.ID, edit, sum))
	_, pendingLeft := pendingUndecryptable(t, st, source.ID, "$older-edit")
	assert.False(pendingLeft)
	assert.Equal(int64(1), sum.UndecryptableRecovered)
	require.NoError(imp.finalizeDeferredRecovery(source.ID, edit, sum))
	assert.Equal(int64(1), sum.UndecryptableRecovered,
		"replaying an already retired placeholder is not a new recovery")
	var deleted bool
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageID,
	).Scan(&deleted))
	assert.True(deleted)

	orphanedID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$orphaned-edit", MessageType: SourceType,
	})
	require.NoError(err)
	orphaned := *edit
	orphaned.ID = "$orphaned-edit"
	require.NoError(imp.finalizeDeferredRecovery(source.ID, &orphaned, sum))
	require.NoError(st.DB().QueryRow(
		st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), orphanedID,
	).Scan(&deleted))
	assert.True(deleted, "a recovered relation retires its placeholder even when checkpoint state was lost")
	assert.Equal(int64(2), sum.UndecryptableRecovered)
}

func TestImporterResumesFailedRoomBackfillCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requestedFrom, requestedTo []string
	failedOnce := false
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$recent","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"recent"}},{"type":"m.room.message","event_id":"$edit-oldest","sender":"@member:example.org","origin_server_ts":3500,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$oldest"}}}],"prev_batch":"older-1"}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$new","sender":"@member:example.org","origin_server_ts":5000,"content":{"msgtype":"m.text","body":"new"}}],"limited":true,"prev_batch":"fresh-gap"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		requestedFrom = append(requestedFrom, from)
		requestedTo = append(requestedTo, r.URL.Query().Get("to"))
		if from == "fresh-gap" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$between","sender":"@member:example.org","origin_server_ts":4000,"content":{"msgtype":"m.text","body":"between"}}]}`))
			return
		}
		if from == "older-1" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$middle","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"middle"}}],"end":"older-2"}`))
			return
		}
		if !failedOnce {
			failedOnce = true
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"temporary test interruption"}`))
			return
		}
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$oldest","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"oldest"}}],"end":""}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	options := ImportOptions{UserID: "@archive:example.org", NoMedia: true,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.Error(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.NoError(err)
	require.Equal([]string{"older-1", "older-2", "fresh-gap", "older-2"}, requestedFrom)
	assert.Equal([]string{"", "", "next-1", ""}, requestedTo,
		"the gap after an interrupted first run stops at that run's sync token")

	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(5), count)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$oldest"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$oldest"])
	require.NoError(err)
	assert.Equal("edited", body, "an edit deferred before the interruption applies once its target arrives")
}

func TestImporterAppliesBackfillEditsOldestFirst(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[],"prev_batch":"newer-page"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("from") == "newer-page" {
			_, _ = w.Write([]byte(`{"chunk":[],"end":"middle-page"}`))
			return
		}
		if r.URL.Query().Get("from") == "middle-page" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$latest-edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* latest","m.new_content":{"msgtype":"m.text","body":"latest"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}],"end":"older-page"}`))
			return
		}
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$older-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* older","m.new_content":{"msgtype":"m.text","body":"older"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}},{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}],"end":""}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$original"])
	require.NoError(err)
	assert.Equal("latest", body)
}

func TestImporterFillsLimitedIncrementalTimelineGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var gapRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "next-1" {
			_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[{"type":"m.room.message","event_id":"$new","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"new"}}],"prev_batch":"gap-1"}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$known","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"known"}}],"prev_batch":"initial-backfill"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch query.Get("from") {
		case "initial-backfill":
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$old","sender":"@member:example.org","origin_server_ts":500,"content":{"msgtype":"m.text","body":"old"}}]}`))
		case "gap-1":
			gapRequests = append(gapRequests, query.Get("to"))
			// An empty page with an end token is not the end of the gap.
			_, _ = w.Write([]byte(`{"chunk":[],"end":"gap-2"}`))
		case "gap-2":
			gapRequests = append(gapRequests, query.Get("to"))
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$gap","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"gap"}}]}`))
		default:
			http.Error(w, "unexpected history cursor", http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	opts := ImportOptions{UserID: "@archive:example.org", NoMedia: true,
		MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	second, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), second.MessagesAdded)
	assert.Equal([]string{"next-1", "next-1"}, gapRequests, "the gap walk stops at the previous sync token")

	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(4), count)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(&RoomState{Backfilled: true, SyncedTo: "next-2"}, state.Rooms["!room:example.org"])
}

func TestImporterResumesInterruptedGapFromSavedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	msg := func(eventID string, ts int) string {
		return fmt.Sprintf(`{"type":"m.room.message","event_id":"%s","sender":"@member:example.org","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"%s"}}`, eventID, ts, eventID)
	}
	var historyRequests []string
	var resumedSyncs int
	failed := map[string]bool{}
	interruptOnce := func(w http.ResponseWriter, page string) bool {
		if failed[page] {
			return false
		}
		failed[page] = true
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"temporary test interruption"}`))
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") != "next-1" {
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s],"prev_batch":"initial-backfill"}}}}}`, msg("$known", 1000))
			return
		}
		// A failed run never saves its token, so every later run syncs from next-1.
		resumedSyncs++
		switch resumedSyncs {
		case 1:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"gap-1"}}}}}`, msg("$new", 4000))
		case 2:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-3","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"fresh-gap"}}}}}`, msg("$newer", 6000))
		default:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-4","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"last-gap"}}}}}`, msg("$newest", 8000))
		}
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		from := query.Get("from")
		if from != "initial-backfill" {
			historyRequests = append(historyRequests, from+"->"+query.Get("to"))
		}
		switch from {
		case "initial-backfill":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s]}`, msg("$old", 500))
		case "gap-1":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s],"end":"gap-2"}`, msg("$gap-newer", 3000))
		case "gap-2":
			if !interruptOnce(w, from) {
				_, _ = fmt.Fprintf(w, `{"chunk":[%s]}`, msg("$gap-older", 2000))
			}
		case "fresh-gap":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s],"end":"fresh-2"}`, msg("$fresh-newer", 5500))
		case "fresh-2":
			if !interruptOnce(w, from) {
				_, _ = fmt.Fprintf(w, `{"chunk":[%s,%s]}`, msg("$fresh-older", 5000), msg("$new", 4000))
			}
		case "last-gap":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s,%s]}`, msg("$late", 7000), msg("$newer", 6000))
		default:
			http.Error(w, "unexpected history cursor", http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	opts := ImportOptions{UserID: "@archive:example.org"}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.Error(err)
	checkpoint, err := st.GetLatestCheckpointedSyncByType(source.ID, SourceType)
	require.NoError(err)
	interrupted, err := loadSyncState(checkpoint.CursorBefore.String)
	require.NoError(err)
	assert.Equal("gap-2", interrupted.Rooms["!room:example.org"].GapFrom)
	assert.Equal("next-2", interrupted.Rooms["!room:example.org"].GapTo)

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.Error(err, "the next gap is interrupted too")
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]string{
		"gap-1->next-1", "gap-2->next-1",
		"gap-2->next-1", "fresh-gap->next-2", "fresh-2->next-2",
		"fresh-2->next-2", "last-gap->next-3",
	}, historyRequests, "each run finishes the saved gap from its page before walking newer events")

	ids := []string{"$known", "$old", "$new", "$gap-newer", "$gap-older", "$fresh-newer", "$fresh-older", "$newer", "$late", "$newest"}
	found, err := st.MessageExistsBatch(source.ID, ids)
	require.NoError(err)
	for _, eventID := range ids {
		assert.NotZero(found[eventID], eventID)
	}
	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(len(ids)), count)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(&RoomState{Backfilled: true, SyncedTo: "next-4"}, state.Rooms["!room:example.org"])
}

func TestImporterEditMustKeepEventType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.sticker","event_id":"$sticker","sender":"@member:example.org","origin_server_ts":1000,"content":{"body":"party parrot","url":"mxc://example.org/parrot"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* replaced","m.new_content":{"msgtype":"m.text","body":"replaced"},"m.relates_to":{"rel_type":"m.replace","event_id":"$sticker"}}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$sticker"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$sticker"])
	require.NoError(err)
	assert.Equal("party parrot", body)
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), messageIDs["$sticker"]).Scan(&edited))
	assert.False(edited)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$file","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.file","body":"report.pdf","url":"mxc://example.org/report"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$file-edit","sender":"@member:example.org","origin_server_ts":4000,"content":{"msgtype":"m.text","body":"* gone","m.new_content":{"msgtype":"m.text","body":"gone"},"m.relates_to":{"rel_type":"m.replace","event_id":"$file"}}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err = st.MessageExistsBatch(source.ID, []string{"$file"})
	require.NoError(err)
	body, err = st.GetMessageBodyText(messageIDs["$file"])
	require.NoError(err)
	assert.Equal("gone", body, "Matrix lets an edit change a file into text")
}

// relationsClient serves each original's surviving m.replace events.
func relationsClient(t *testing.T, edits map[id.EventID]string) *mautrix.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[` + edits[id.EventID(r.PathValue("event"))] + `]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(t, err)
	return client
}

func TestImporterReplaysRedactionsAfterReactionsDespiteClockSkew(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		// The redaction's server timestamp is older than the reaction it removes.
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"one"}},{"type":"m.reaction","event_id":"$late-reaction","sender":"@member:example.org","origin_server_ts":5000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"ok"}}},{"type":"m.room.redaction","event_id":"$early-redaction","sender":"@member:example.org","origin_server_ts":4000,"redacts":"$late-reaction","content":{}}]}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Zero(sum.RelationsUnresolved)
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Zero(reactions)
}

func TestImporterReappliesEditWhosePointerWasNotSaved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	messageID := messageIDs["$original"]
	// An interruption after the text and flag but before the pointer.
	require.NoError(imp.setBody(messageID, "edited"))
	require.NoError(st.SetMessageEdited(messageID))

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	edit, err := imp.appliedEdit(messageID)
	require.NoError(err)
	assert.Equal(appliedEdit{EventID: "$edit", TS: 2000}, edit)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":3000,"redacts":"$edit","content":{}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterIgnoresInvalidReplacements(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	for _, raw := range []string{
		`{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`,
		`{"type":"m.room.message","event_id":"$bare-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* bare","m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
		`{"type":"m.room.message","event_id":"$state-edit","state_key":"","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* state","m.new_content":{"msgtype":"m.text","body":"state"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
	} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, raw), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	}
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original", "$bare-edit"})
	require.NoError(err)
	assert.Zero(messageIDs["$bare-edit"], "a replacement without new content is not a message")
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterReplayRecoversReplyLinkLostBeforeCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	reply := `{"type":"m.room.message","event_id":"$reply","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"reply","m.relates_to":{"m.in_reply_to":{"event_id":"$target"}}}}`
	// The reply was saved, but the run stopped before its deferral was checkpointed.
	require.ErrorIs(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), attachmentpolicy.Conversation{}, ImportOptions{}, sum), errRelationTargetMissing)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"target"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	ids, err := st.MessageExistsBatch(source.ID, []string{"$reply", "$target"})
	require.NoError(err)
	var replyTo int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$reply"]).Scan(&replyTo))
	assert.Equal(ids["$target"], replyTo)
}

func TestImporterSkipsMalformedEventAndKeepsGoing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	malformed := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$bad","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":42}}`)
	assert.False(shouldDeferRelation(malformed))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, malformed, attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$good","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"good"}}`), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	assert.Equal(int64(1), sum.EventsSkipped)
	ids, err := st.MessageExistsBatch(source.ID, []string{"$bad", "$good"})
	require.NoError(err)
	assert.Zero(ids["$bad"])
	assert.NotZero(ids["$good"])
}

func TestMessageBodyKeepsCaptionsAndDropsReplyFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"image caption":        {`{"msgtype":"m.image","body":"Meter reading 12345","filename":"photo.jpg","url":"mxc://example.org/p"}`, "[image] Meter reading 12345"},
		"file without caption": {`{"msgtype":"m.file","body":"report.pdf","url":"mxc://example.org/r"}`, "[file: report.pdf]"},
		"plain reply fallback": {`{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nmy answer","m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`, "my answer"},
		"html reply fallback":  {`{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nmy answer","format":"org.matrix.custom.html","formatted_body":"<mx-reply><blockquote>their words</blockquote></mx-reply>my answer","m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`, "my answer"},
	} {
		t.Run(name, func(t *testing.T) {
			evt := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$e","sender":"@member:example.org","origin_server_ts":1,"content":`+tc.raw+`}`)
			require.NoError(t, evt.Content.ParseRaw(evt.Type))
			assert.Equal(t, tc.want, messageBody(evt.Content.AsMessage()))
		})
	}
}

func TestImporterEditedReplyKeepsQuotedFallbackOut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	reply := `{"type":"m.room.message","event_id":"$reply","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"answer","m.relates_to":{"m.in_reply_to":{"event_id":"$quoted"}}}}`
	edit := `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* fixed","m.new_content":{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nfixed answer"},"m.relates_to":{"rel_type":"m.replace","event_id":"$reply"}}}`
	if err := imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), attachmentpolicy.Conversation{}, ImportOptions{}, sum); err != nil {
		require.ErrorIs(err, errRelationTargetMissing)
	}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, edit), attachmentpolicy.Conversation{}, ImportOptions{}, sum))
	ids, err := st.MessageExistsBatch(source.ID, []string{"$reply"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(ids["$reply"])
	require.NoError(err)
	assert.Equal("fixed answer", body)
}

func pendingUndecryptable(t *testing.T, st *store.Store, sourceID int64, eventID string) (store.MatrixUndecryptableEvent, bool) {
	t.Helper()
	evt, found, err := st.MatrixUndecryptableEvent(sourceID, eventID)
	require.NoError(t, err)
	return evt, found
}

func encryptedTestEvents(prefix string, count int, firstTS int64) string {
	events := make([]string, 0, count)
	for i := range count {
		events = append(events, fmt.Sprintf(
			`{"type":"m.room.encrypted","event_id":"$%s-%03d","sender":"@member:example.org","origin_server_ts":%d,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque-%s-%03d","session_id":"session","sender_key":"key"}}`,
			prefix, i, firstTS+int64(i), prefix, i))
	}
	return strings.Join(events, ",")
}

func TestUndecryptableEventsStayOutOfCheckpointsAndRecoverAcrossRuns(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const perPage = 30
	var syncCalls int
	failedHistory := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		if syncCalls <= 2 {
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s],"prev_batch":"older-1"}}}}}`,
				encryptedTestEvents("recent", perPage, 10000))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, _ *http.Request) {
		if !failedHistory {
			failedHistory = true
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"temporary test interruption"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"chunk":[%s],"end":""}`, encryptedTestEvents("old", perPage, 1000))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	options := ImportOptions{UserID: "@archive:example.org"}

	countPending := func() int {
		var n int
		require.NoError(st.DB().QueryRow(
			st.Rebind(`SELECT COUNT(*) FROM matrix_undecryptable_events WHERE source_id = ?`), source.ID).Scan(&n))
		return n
	}
	assertCheckpointsOmitCiphertext := func() {
		rows, err := st.DB().Query(st.Rebind(`SELECT COALESCE(cursor_before, ''), COALESCE(cursor_after, '') FROM sync_runs WHERE source_id = ?`), source.ID)
		require.NoError(err)
		defer func() { _ = rows.Close() }()
		blobs := 0
		for rows.Next() {
			var before, after string
			require.NoError(rows.Scan(&before, &after))
			for _, blob := range []string{before, after} {
				if blob == "" {
					continue
				}
				blobs++
				assert.NotContains(blob, "ciphertext")
				assert.NotContains(blob, "encrypted_event")
				state, err := loadSyncState(blob)
				require.NoError(err)
				assert.Empty(state.Undecryptable)
			}
		}
		require.NoError(rows.Err())
		assert.Positive(blobs)
	}

	// The first run is interrupted while backfilling history.
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.Error(err)
	assert.Equal(perPage, countPending(), "events seen before the interruption are already pending")
	assertCheckpointsOmitCiphertext()

	// The second run finishes the backfill without losing earlier entries.
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.NoError(err)
	assert.Equal(int64(2*perPage), sum.Undecryptable, "the replayed timeline page is counted again")
	assert.Equal(2*perPage, countPending(), "replayed events do not duplicate pending rows")
	assertCheckpointsOmitCiphertext()

	// A later run with keys recovers every pending event from the table.
	decryptCalls := 0
	runtime := &Runtime{Client: client, decryptEvent: func(_ context.Context, evt *event.Event) (*event.Event, error) {
		decryptCalls++
		return matrixTestEvent(t, fmt.Sprintf(
			`{"type":"m.room.message","event_id":%q,"sender":"@member:example.org","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"recovered %s"}}`,
			evt.ID, evt.Timestamp, evt.ID)), nil
	}}
	sum, err = NewImporter(st, runtime).Import(t.Context(), options)
	require.NoError(err)
	assert.Equal(int64(2*perPage), sum.UndecryptableRecovered)
	assert.Equal(2*perPage, decryptCalls)
	assert.Zero(countPending())
	messages, err := st.MessageExistsBatch(source.ID, []string{"$old-007", "$recent-007"})
	require.NoError(err)
	for _, eventID := range []string{"$old-007", "$recent-007"} {
		body, err := st.GetMessageBodyText(messages[eventID])
		require.NoError(err)
		assert.Equal("recovered "+eventID, body)
	}
	assertCheckpointsOmitCiphertext()
}

func TestLegacyCheckpointUndecryptableEntriesMigrateToStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$legacy-state", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
			Algorithm: id.AlgorithmMegolmV1,
		}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(
		t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	require.NoError(st.DeleteMatrixUndecryptableEvent(source.ID, encrypted.ID.String()))
	encryptedRaw, err := json.Marshal(encrypted, json.Deterministic(true))
	require.NoError(err)

	// A cursor written before pending events moved to the store.
	legacyBlob, err := json.Marshal(map[string]any{
		"next_batch": "next-1",
		"rooms":      map[string]any{},
		"undecryptable": map[string]any{
			encrypted.ID.String(): map[string]any{"room_id": roomID.String(), "encrypted_event": string(encryptedRaw)},
		},
	})
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSyncAndUpdateSourceCursor(syncID, source.ID, string(legacyBlob)))

	runtime := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$legacy-state","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"recovered from legacy state"}}`), nil
	}}
	sum, err := NewImporter(st, runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Equal(1, syncCalls)
	assert.Equal(int64(1), sum.UndecryptableRecovered)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$legacy-state"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$legacy-state"])
	require.NoError(err)
	assert.Equal("recovered from legacy state", body)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	assert.NotContains(run.CursorAfter.String, "undecryptable\"")
	assert.NotContains(run.CursorAfter.String, "encrypted_event")
}

// emptySyncServer answers an incremental sync that carries no room activity.
func emptySyncServer(t *testing.T, edits map[id.EventID]string) *mautrix.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[` + edits[id.EventID(r.PathValue("event"))] + `]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(t, err)
	return client
}

func TestCheckpointSizeDoesNotGrowWithUndecryptableEvents(t *testing.T) {
	cursorAfterSync := func(t *testing.T, undecryptable int) string {
		t.Helper()
		require := require.New(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s],"prev_batch":"older-1"}}}}}`,
				encryptedTestEvents("pending", undecryptable, 1000))
		})
		mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"}}}`))
		})
		mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"chunk":[],"end":""}`))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
		require.NoError(err)
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
		require.NoError(err)
		sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
		require.NoError(err)
		require.Equal(int64(undecryptable), sum.Undecryptable)
		run, err := st.GetLastSuccessfulSync(source.ID)
		require.NoError(err)
		return run.CursorAfter.String
	}

	assert.Equal(t, cursorAfterSync(t, 1), cursorAfterSync(t, 200),
		"pending events live in the store, so the checkpoint is the same size")
}

func TestRecoveredPlaceholderTakesEditFromRelations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	roomID := id.RoomID("!room:example.org")
	encrypted := func(eventID string, ts int64) *event.Event {
		return &event.Event{
			ID: id.EventID(eventID), RoomID: roomID, Sender: "@member:example.org", Timestamp: ts,
			Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{
				Algorithm: id.AlgorithmMegolmV1, SessionID: id.SessionID("session-" + eventID),
			}},
		}
	}
	original, edit := encrypted("$original", 1000), encrypted("$edit", 2000)
	encryptedEdit, err := json.Marshal(edit, json.Deterministic(true))
	require.NoError(err)
	client := emptySyncServer(t, map[id.EventID]string{"$original": string(encryptedEdit)})
	plaintext := map[id.EventID]string{
		"$original": `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`,
		"$edit":     `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
	}
	runtimeWithKeys := func(known ...id.EventID) *Runtime {
		return &Runtime{Client: client, decryptEvent: func(_ context.Context, evt *event.Event) (*event.Event, error) {
			if !slices.Contains(known, evt.ID) {
				return nil, errors.New("synthetic missing session")
			}
			return matrixTestEvent(t, plaintext[evt.ID]), nil
		}}
	}
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)

	// The edit decrypts while its original is still a placeholder.
	require.NoError(NewImporter(st, runtimeWithKeys()).persistEvent(t.Context(), source.ID, conversationID, original, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	require.NoError(NewImporter(st, runtimeWithKeys("$edit")).persistEvent(t.Context(), source.ID, conversationID, edit, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal(encryptedPlaceholder, body)

	// A later run recovers the original and applies the edit the homeserver lists.
	sum, err := NewImporter(st, runtimeWithKeys("$original", "$edit")).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Equal(int64(1), sum.UndecryptableRecovered)
	body, err = st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("edited", body)
	_, pending := pendingUndecryptable(t, st, source.ID, "$original")
	assert.False(pending)
}

func TestRedactedPlaceholderIsNotRestoredByLaterKeys(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	roomID := id.RoomID("!room:example.org")
	client := emptySyncServer(t, nil)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$secret", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	imp := NewImporter(st, &Runtime{Client: client})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","origin_server_ts":2000,"redacts":"$secret","content":{}}`)
	redaction.RoomID = roomID
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	_, pending := pendingUndecryptable(t, st, source.ID, "$secret")
	assert.False(pending, "a redacted event leaves the pending set")

	withKeys := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$secret","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"redacted text"}}`), nil
	}}
	sum, err := NewImporter(st, withKeys).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Zero(sum.UndecryptableRecovered)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$secret"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$secret"])
	require.NoError(err)
	assert.Equal(encryptedPlaceholder, body)
}

func TestRedactionFromAnotherRoomKeepsPendingEncryptedEvent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	roomID, otherRoomID := id.RoomID("!room:example.org"), id.RoomID("!other:example.org")
	client := emptySyncServer(t, nil)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	otherConversationID, err := st.EnsureConversation(source.ID, otherRoomID.String(), "Other room")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$secret", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	imp := NewImporter(st, &Runtime{Client: client})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","origin_server_ts":2000,"redacts":"$secret","content":{}}`)
	redaction.RoomID = otherRoomID

	require.NoError(imp.persistEvent(t.Context(), source.ID, otherConversationID, redaction, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	_, pending := pendingUndecryptable(t, st, source.ID, "$secret")
	assert.True(pending, "a redaction from another room cannot retire the pending event")

	redaction.RoomID = roomID
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	_, pending = pendingUndecryptable(t, st, source.ID, "$secret")
	assert.False(pending, "a redaction from the event's own room retires it")
}

func TestFirstSyncRecoversPlaceholdersWithoutPendingRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	roomID := id.RoomID("!room:example.org")
	client := emptySyncServer(t, nil)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	encrypted := &event.Event{
		ID: "$copied", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
	// A subset export copies the placeholder but neither the pending row nor
	// the Matrix cursor.
	require.NoError(st.DeleteMatrixUndecryptableEvent(source.ID, "$copied"))

	withKeys := &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$copied","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"recovered"}}`), nil
	}}
	sum, err := NewImporter(st, withKeys).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Equal(int64(1), sum.UndecryptableRecovered)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$copied"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$copied"])
	require.NoError(err)
	assert.Equal("recovered", body)
}

func TestRecoveredPlaceholderDownloadsItsMedia(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/media/download/example.org/photo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("photo bytes"))
	})
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversationWithType(source.ID, roomID.String(), "group_chat", "Media")
	require.NoError(err)
	opts := ImportOptions{AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll}}
	conversation := attachmentpolicy.Conversation{Type: "group_chat", ParticipantCount: 2}
	encrypted := &event.Event{
		ID: "$photo", RoomID: roomID, Sender: "@member:example.org", Timestamp: 1000,
		Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}},
	}
	require.NoError(NewImporter(st, &Runtime{Client: client}).persistEvent(t.Context(), source.ID, conversationID, encrypted, conversation, opts, &ImportSummary{}))

	withKeys := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$photo","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.image","body":"photo.jpg","url":"mxc://example.org/photo","info":{"mimetype":"image/jpeg","size":11}}}`), nil
	}})
	sum := &ImportSummary{}
	_, err = withKeys.retryUndecryptable(t.Context(), source.ID, newSyncState(), opts, sum)
	require.NoError(err)
	assert.Equal(int64(1), sum.UndecryptableRecovered)
	assert.Equal(int64(1), sum.AttachmentsDownloaded)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$photo"})
	require.NoError(err)
	attachments, err := st.MessageMatrixAttachments(messageIDs["$photo"])
	require.NoError(err)
	photo := attachments["matrix:mxc://example.org/photo"]
	assert.Equal(attachmentpolicy.StateStored, photo.State)
	stored, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, photo.StoragePath))
	require.NoError(err)
	assert.Equal([]byte("photo bytes"), stored)
}

func TestSyncedCiphertextIsNotProofOfArchiveWhenPersistFails(t *testing.T) {
	require := require.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s],"prev_batch":"older-1"}}}}}`,
			encryptedTestEvents("sync", 1, 1000))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failing := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		// The interruption arrives after decryption, before the plaintext is stored.
		cancel()
		return matrixTestEvent(t, `{"type":"m.room.message","event_id":"$sync-000","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"secret"}}`), nil
	}})
	_, err = failing.Import(ctx, ImportOptions{UserID: "@archive:example.org"})
	require.Error(err)
	_, _, err = st.MatrixEncryptedEvent(source.ID, "$sync-000")
	require.ErrorIs(err, sql.ErrNoRows, "ciphertext is retained only once the decrypted event is stored")
}

func TestDeferredRelationCiphertextWaitsForCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client, decryptEvent: func(context.Context, *event.Event) (*event.Event, error) {
		return matrixTestEvent(t, `{"type":"m.reaction","event_id":"$sync-000","sender":"@member:example.org","origin_server_ts":1000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$not-archived","key":"ok"}}}`), nil
	}})
	encrypted := matrixTestEvent(t, encryptedTestEvents("sync", 1, 1000))
	encrypted.RoomID = "!room:example.org"

	candidate, ciphertext, deferRelation, err := imp.relationForDeferral(t.Context(), encrypted)
	require.NoError(err)
	assert.True(deferRelation)
	assert.Equal(event.EventReaction, candidate.Type)
	assert.NotEmpty(ciphertext)
	_, _, err = st.MatrixEncryptedEvent(source.ID, "$sync-000")
	require.ErrorIs(err, sql.ErrNoRows, "decrypting a relation retains nothing until it is stored")
}

func TestRecoveryPersistsEachPageBeforeReadingTheNext(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomID := id.RoomID("!room:example.org")
	conversationID, err := st.EnsureConversation(source.ID, roomID.String(), "Example room")
	require.NoError(err)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@archive:example.org", "token")
	require.NoError(err)
	placeholders := NewImporter(st, &Runtime{Client: client})
	decryptedByID := map[string]string{}
	archive := func(eventID string, ts int64, plaintext string) {
		encrypted := &event.Event{ID: id.EventID(eventID), RoomID: roomID, Sender: "@member:example.org", Timestamp: ts,
			Type: event.EventEncrypted, Content: event.Content{Parsed: &event.EncryptedEventContent{Algorithm: id.AlgorithmMegolmV1}}}
		require.NoError(placeholders.persistEvent(t.Context(), source.ID, conversationID, encrypted, attachmentpolicy.Conversation{}, ImportOptions{}, &ImportSummary{}))
		decryptedByID[eventID] = plaintext
	}
	// "$a-edit" sorts before "$z-original", and more than one page lies between them.
	archive("$a-edit", 3000, `{"type":"m.room.message","event_id":"$a-edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$z-original"}}}`)
	for i := range undecryptableBatchSize + 20 {
		eventID := fmt.Sprintf("$m-%04d", i)
		archive(eventID, 1000, fmt.Sprintf(`{"type":"m.room.message","event_id":%q,"sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"filler"}}`, eventID))
	}
	archive("$z-original", 2000, `{"type":"m.room.message","event_id":"$z-original","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"original"}}`)

	// Pending events are read in ID order, so $m-0499 opens the second page.
	checked, recoveredBeforeSecondPage := false, false
	recovering := NewImporter(st, &Runtime{Client: client, decryptEvent: func(_ context.Context, evt *event.Event) (*event.Event, error) {
		if evt.ID == "$m-0499" && !checked {
			checked = true
			_, _, err := st.MatrixEncryptedEvent(source.ID, "$m-0000")
			recoveredBeforeSecondPage = err == nil
		}
		return matrixTestEvent(t, decryptedByID[evt.ID.String()]), nil
	}})
	sum := &ImportSummary{}
	settle, err := recovering.retryUndecryptable(t.Context(), source.ID, newSyncState(), ImportOptions{}, sum)
	require.NoError(err)
	require.NoError(settle())

	assert.Equal(int64(undecryptableBatchSize+22), sum.UndecryptableRecovered)
	assert.True(recoveredBeforeSecondPage, "page one is stored before page two is read")
	found, err := st.MessageExistsBatch(source.ID, []string{"$z-original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(found["$z-original"])
	require.NoError(err)
	assert.Equal("edited", body, "an edit recovered before its original is still applied")
}
