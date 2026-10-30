package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchiveAPIMessageReplaysWithoutDuplicating(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic group"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	msg.SetFromID(&tg.PeerUser{UserID: 456})
	msg.SetEditDate(110)
	users := []tg.UserClass{&tg.User{ID: 456, FirstName: "Synthetic"}}
	for range 2 {
		require.NoError(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, users, nil))
	}
	var id int64
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), MIN(id) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&count, &id))
	assert.Equal(1, count)
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.Contains(string(raw), "synthetic")
}

func TestArchiveAPIMessageRefusesWrongPeerAndAcceptsEmptyMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 456}, Date: 100, Message: "synthetic"}
	require.ErrorContains(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil), "dialog")
	msg.PeerID = &tg.PeerChat{ChatID: 123}
	msg.SetMedia(&tg.MessageMediaEmpty{})
	require.NoError(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
}

func TestArchiveAPIMessagePreservesMetadataOnlyMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	msg.SetMedia(&tg.MessageMediaGeo{Geo: &tg.GeoPointEmpty{}})
	require.NoError(ArchiveAPIMessage(ctx, st, source.ID, 999, "Synthetic", dialog, msg, nil, nil))
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
	raw, err := st.GetMessageRaw(id)
	require.NoError(err)
	assert.NotEmpty(raw)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), id).Scan(&count))
	assert.Zero(count)
}

func TestArchiveAPIMessageRejectsUnsupportedMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	msg.SetMedia(&tg.MessageMediaUnsupported{})
	require.ErrorContains(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil), "unsupported Telegram media")
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Zero(count)
}

func TestArchiveAPIMessagePersistsPendingMediaMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "with attachment"}
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{
		ID: 101, AccessHash: 202, FileReference: []byte("synthetic-reference"),
		DCID: 3, Size: 42, MimeType: "application/pdf",
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "attachment.pdf"}},
	})
	msg.SetMedia(media)

	require.NoError(ArchiveAPIMessage(ctx, st, source.ID, 999, "Owner", dialog, msg, nil, nil))
	var state, metadata, sourcePartKey, sourceAttachmentID string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.attachment_state, CAST(a.attachment_metadata AS TEXT), a.source_part_key, a.source_attachment_id
		FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "-123:7").Scan(
		&state, &metadata, &sourcePartKey, &sourceAttachmentID,
	))
	assert.Equal("pending", state)
	assert.NotEmpty(sourcePartKey)
	assert.Equal("api:document:101", sourceAttachmentID)
	restored, err := mediaReferenceFromStored(&telegram.LiveMedia{SourceID: "document:101", Kind: "file", Metadata: metadata})
	require.NoError(err)
	document, ok := restored.Location.(*tg.InputDocumentFileLocation)
	require.True(ok)
	assert.Equal([]byte("synthetic-reference"), document.FileReference)
}

func TestArchiveAPIMessageInfersIncomingPrivateSenderAndRecipient(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	dialog := CatalogDialog{ChatID: 456, NativeID: 456, Kind: "user", Title: "Synthetic peer"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 456}, Date: 100, Message: "incoming"}
	require.NoError(ArchiveAPIMessage(ctx, st, source.ID, 999, "Synthetic owner", dialog, msg,
		[]tg.UserClass{&tg.User{ID: 456, FirstName: "Synthetic", LastName: "Peer"}}, nil))

	peerPID, _, err := st.ParticipantByIdentifier("telegram", "456")
	require.NoError(err)
	ownerPID, _, err := st.ParticipantByIdentifier("telegram", "999")
	require.NoError(err)
	var senderID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.sender_id FROM messages m
		WHERE m.source_id = ? AND m.source_message_id = ?`), source.ID, "456:7").Scan(&senderID))
	assert.Equal(peerPID, senderID)
	var toID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.participant_id FROM message_recipients mr
		JOIN messages m ON m.id = mr.message_id
		WHERE m.source_id = ? AND m.source_message_id = ? AND mr.recipient_type = 'to'`),
		source.ID, "456:7").Scan(&toID))
	assert.Equal(ownerPID, toID)
}

func TestArchiveAPIMessageRefusesUnmigratedExportKeys(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	require.ErrorContains(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil), "legacy")
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Zero(t, count)
}

func TestArchiveAPIMessageRefusesAmbiguousLegacyMonoforum(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	_, err = st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(t, err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	require.ErrorContains(t, ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil), "ambiguous")
}

func TestArchiveAPIMessageRetainsNewTypedUserIdentity(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	const userID int64 = 1002147483649
	dialog := CatalogDialog{ChatID: userID, NativeID: userID, Kind: "user", Title: "Synthetic"}
	for id := 1; id <= 2; id++ {
		msg := &tg.Message{ID: id, PeerID: &tg.PeerUser{UserID: userID}, Date: 100, Message: "synthetic"}
		require.NoError(ArchiveAPIMessage(ctx, st, source.ID, 999, "Owner", dialog, msg, nil, nil))
	}
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(t, ambiguous)
}

func TestArchiveAPIMessageRetainsNewTypedMonoforumIdentity(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	const peerID int64 = 1002147483649
	marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: peerID})
	require.NoError(err)
	dialog := CatalogDialog{ChatID: marked, NativeID: peerID, Kind: "supergroup", Title: "Synthetic"}
	for id := 1; id <= 2; id++ {
		msg := &tg.Message{ID: id, PeerID: &tg.PeerChannel{ChannelID: peerID}, Date: 100, Message: "synthetic"}
		require.NoError(ArchiveAPIMessage(ctx, st, source.ID, 999, "Owner", dialog, msg, nil, nil))
	}
	when := time.Unix(100, 0).UTC()
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic"}, {MessageID: 2, SentAt: when, Text: "synthetic"}}
	require.NoError(st.CheckVerifiedTelegramChannelExportContext(ctx, source.ID, peerID, marked, evidence))
}

func TestArchiveAPIMessageRefusesUnverifiedMigratedHistory(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:-999999999", MessageType: telegram.MessageType}, BodyText: sql.NullString{String: "synthetic", Valid: true}})
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	require.ErrorContains(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", dialog, msg, nil, nil), "history")
}

func TestArchiveAPIMessageResolvesCrossChatReply(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	childDialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic"}
	child := &tg.Message{ID: 2, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "child"}
	header := &tg.MessageReplyHeader{}
	header.SetReplyToMsgID(1)
	header.SetReplyToPeerID(&tg.PeerUser{UserID: 456})
	child.SetReplyTo(header)
	require.NoError(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", childDialog, child, nil, nil))
	parentDialog := CatalogDialog{ChatID: 456, NativeID: 456, Kind: "user", Title: "Synthetic"}
	parent := &tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: 456}, Date: 90, Message: "parent"}
	require.NoError(ArchiveAPIMessage(context.Background(), st, source.ID, 999, "Owner", parentDialog, parent, nil, nil))
	var parentID, linkedID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "456:1").Scan(&parentID))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:2").Scan(&linkedID))
	assert.Equal(t, parentID, linkedID)
}

func TestMapAPIMessagePreservesSenderReplyAndEdit(t *testing.T) {
	assert := assert.New(t)

	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "synthetic"}
	msg.SetFromID(&tg.PeerUser{UserID: 456})
	msg.SetEditDate(110)
	reply := &tg.MessageReplyHeader{}
	reply.SetReplyToMsgID(4)
	msg.SetReplyTo(reply)
	mapped, err := MapAPIMessage(msg, 999, []tg.UserClass{&tg.User{ID: 456, FirstName: "Synthetic", LastName: "Person"}}, nil)
	require.NoError(t, err)
	assert.EqualValues(7, mapped.Message.ID)
	assert.Equal("user456", mapped.Message.FromID)
	assert.Equal("Synthetic Person", mapped.Message.FromName)
	assert.Equal("synthetic", mapped.Message.Text)
	assert.EqualValues(4, mapped.Message.ReplyTo)
	assert.Equal(int64(-123), mapped.ReplyChatID)
	assert.True(mapped.Message.Edited)
	assert.Equal(time.Unix(110, 0).UTC(), mapped.EditedAt)
	assert.Contains(mapped.RawJSON, "synthetic")
}

func TestMapAPIMessageUsesOutgoingOwnerWhenSenderAbsent(t *testing.T) {
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerUser{UserID: 456}, Date: 100, Out: true}
	mapped, err := MapAPIMessage(msg, 999, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "user999", mapped.Message.FromID)
}

func TestMapAPIMessagePreservesCrossChatReply(t *testing.T) {
	msg := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	reply := &tg.MessageReplyHeader{}
	reply.SetReplyToMsgID(4)
	reply.SetReplyToPeerID(&tg.PeerUser{UserID: 456})
	msg.SetReplyTo(reply)
	mapped, err := MapAPIMessage(msg, 999, nil, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 4, mapped.Message.ReplyTo)
	assert.EqualValues(t, 456, mapped.ReplyChatID)
}

func TestMapAPIMessageRejectsInvalidIdentity(t *testing.T) {
	_, err := MapAPIMessage(&tg.Message{ID: 7, Date: 100}, 999, nil, nil)
	require.Error(t, err)
}
