//go:build goolm

package matrix

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
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
	require.NoError(NewImporter(st, runtime).persistEvent(ctx, source.ID, conversationID, encryptedEvent, &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$encrypted-2"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$encrypted-2"])
	require.NoError(err)
	assert.Equal("secret fixture", body)
}
