package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTelegramKeyMigrationPreservesMessagesAndRecoveryManifest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}, Metadata: &sql.NullString{String: `{"referenced_message_id":"123:1","attachment_part_keys":["legacy-key"]}`, Valid: true}})
		require.NoError(err)
	}
	change := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat", ReplyEvidence: map[int64]store.TelegramExportReplyEvidence{1: {ReplyID: 1, SentAt: when, Text: "synthetic one"}, 2: {ReplyID: 1, SentAt: when, Text: "synthetic two"}}, Evidence: []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}}
	revisionBefore, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	require.NoError(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{change}))
	revisionAfter, err := st.DerivedDataRevisionContext(context.Background())
	require.NoError(err)
	assert.Equal(revisionBefore+1, revisionAfter, "message key changes invalidate cached shards")
	var convKey, msgKey, metadata string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id FROM conversations WHERE id = ?`), conv).Scan(&convKey))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_message_id, metadata FROM messages WHERE conversation_id = ? AND source_message_id = ?`), conv, "-123:2").Scan(&msgKey, &metadata))
	assert.Equal("-123", convKey)
	assert.Equal("-123:2", msgKey)
	var recovery struct {
		ReferencedMessageID string   `json:"referenced_message_id"`
		AttachmentPartKeys  []string `json:"attachment_part_keys"`
		LegacyMediaChatID   int64    `json:"legacy_media_chat_id"`
	}
	require.NoError(json.Unmarshal([]byte(metadata), &recovery))
	assert.Equal("-123:1", recovery.ReferencedMessageID)
	assert.Equal([]string{"legacy-key"}, recovery.AttachmentPartKeys)
	assert.EqualValues(123, recovery.LegacyMediaChatID)
}

func TestTelegramKeyMigrationRejectsAmbiguityWithoutChangingRows(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	change := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat"}
	err = st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{change})
	require.ErrorContains(err, "evidence")
	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id FROM conversations WHERE id = ?`), conv).Scan(&key))
	assert.Equal(t, "123", key)
}

func TestTelegramKeyMigrationRejectsMismatchedMarkedPeer(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(t, err)
	err = st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{{LegacyID: 123, MarkedID: -124, ConversationType: "group_chat"}})
	require.ErrorContains(t, err, "invalid")
}

func TestTelegramAmbiguousUserRequiresMatchingArchivedEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	const peerID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
	require.ErrorContains(st.VerifyTelegramAmbiguousUserContext(ctx, source.ID, peerID, evidence[:1]), "evidence")
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.True(ambiguous)
	require.NoError(st.VerifyTelegramAmbiguousUserContext(ctx, source.ID, peerID, evidence))
	ambiguous, err = st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(ambiguous)
}

func TestTelegramTypedUserDoesNotClaimExistingUnverifiedConversation(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	require.ErrorContains(st.EnsureTelegramTypedUserContext(ctx, source.ID, 1002147483649, "Synthetic"), "verification")
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.True(t, ambiguous)
}

func TestTelegramAmbiguousChannelRequiresEvidenceBeforeRekey(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	const peerID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	var marked constant.TDLibPeerID
	marked.Channel(peerID)
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
	require.ErrorContains(st.VerifyTelegramAmbiguousChannelContext(ctx, source.ID, peerID, int64(marked), evidence[:1], nil, false), "evidence")
	require.NoError(st.VerifyTelegramAmbiguousChannelContext(ctx, source.ID, peerID, int64(marked), evidence, nil, false))
	var key, kind string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id, conversation_type FROM conversations WHERE id = ?`), conv).Scan(&key, &kind))
	assert.Equal(strconv.FormatInt(int64(marked), 10), key)
	assert.Equal("group_chat", kind)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, key+":1").Scan(&count))
	assert.Equal(1, count)
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(ambiguous)
}

func TestTelegramAmbiguousChannelRejectsUnsafeMigrations(t *testing.T) {
	for _, mode := range []string{"collision", "verified user", "cross-chat reply", "external reply", "external link"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			ctx := context.Background()
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("telegram", "user999")
			require.NoError(err)
			const peerID int64 = 1002147483649
			conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
			require.NoError(err)
			when := time.Unix(100, 0).UTC()
			for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
				_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
				require.NoError(err)
			}
			var marked constant.TDLibPeerID
			marked.Channel(peerID)
			evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
			if mode == "collision" {
				_, err = st.EnsureConversationWithType(source.ID, strconv.FormatInt(int64(marked), 10), "group_chat", "Other")
				require.NoError(err)
			}
			if mode == "verified user" {
				require.NoError(st.VerifyTelegramAmbiguousUserContext(ctx, source.ID, peerID, evidence))
			}
			if mode == "external reply" || mode == "external link" {
				other, err := st.EnsureConversationWithType(source.ID, "456", "direct_chat", "Other")
				require.NoError(err)
				metadata := `{}`
				if mode == "external reply" {
					metadata = `{"referenced_message_id":"1002147483649:1","reply_peer_verified":true}`
				}
				_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: other, SourceMessageID: "456:1", MessageType: "telegram"}, Metadata: &sql.NullString{String: metadata, Valid: true}})
				require.NoError(err)
				if mode == "external link" {
					_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET reply_to_message_id = (SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?) WHERE source_id = ? AND source_message_id = ?`), source.ID, "1002147483649:1", source.ID, "456:1")
					require.NoError(err)
				}
			}
			require.Error(st.VerifyTelegramAmbiguousChannelContext(ctx, source.ID, peerID, int64(marked), evidence, nil, mode == "cross-chat reply"))
			var key, kind string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id, conversation_type FROM conversations WHERE id = ?`), conv).Scan(&key, &kind))
			assert.Equal("1002147483649", key)
			assert.Equal("direct_chat", kind)
		})
	}
}

func TestTelegramKeyMigrationVerifiesCaptionlessReply(t *testing.T) {
	for _, mode := range []string{"matching", "wrong time", "unexpected body"} {
		t.Run(mode, func(t *testing.T) {
			require := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("telegram", "user999")
			require.NoError(err)
			conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
			require.NoError(err)
			when := time.Unix(100, 0).UTC()
			for _, row := range []struct{ key, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
				_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
				require.NoError(err)
			}
			body := sql.NullString{}
			if mode == "unexpected body" {
				body = sql.NullString{String: "caption", Valid: true}
			}
			_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "123:3", MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: body, Metadata: &sql.NullString{String: `{"referenced_message_id":"123:1"}`, Valid: true}})
			require.NoError(err)
			replyTime := when
			if mode == "wrong time" {
				replyTime = when.Add(time.Second)
			}
			change := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat", Evidence: []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}, ReplyEvidence: map[int64]store.TelegramExportReplyEvidence{3: {ReplyID: 1, SentAt: replyTime}}}
			err = st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{change})
			if mode == "matching" {
				require.NoError(err)
			} else {
				require.Error(err)
			}
		})
	}
}

func TestTelegramKeyMigrationRollsBackAllChangesOnCollision(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	first, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic one")
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "124", "group_chat", "Synthetic two")
	require.NoError(err)
	_, err = st.EnsureConversationWithType(source.ID, "-124", "group_chat", "Existing target")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: first, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
	changes := []store.TelegramExportKeyChange{{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat", Evidence: evidence}, {LegacyID: 124, MarkedID: -124, ConversationType: "group_chat"}}
	require.ErrorContains(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, changes), "collision")
	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id FROM conversations WHERE id = ?`), first).Scan(&key))
	assert.Equal(t, "123", key)
}

func TestTelegramKeyMigrationRollsBackOnMessageKeyCollision(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	legacy, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	other, err := st.EnsureConversationWithType(source.ID, "456", "direct_chat", "Other")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: legacy, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	_, err = st.UpsertMessage(&store.Message{ConversationID: other, SourceID: source.ID, SourceMessageID: "-123:2", MessageType: "telegram"})
	require.NoError(err)
	change := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat", Evidence: []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}}
	require.Error(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{change}))
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE conversation_id = ? AND source_message_id IN (?, ?)`), legacy, "123:1", "123:2").Scan(&count))
	assert.Equal(t, 2, count)
}

func TestTelegramKeyMigrationRejectsDirectClaimOnUnresolvedLegacyGroup(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	claim := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: 123, ConversationType: "direct_chat"}
	err = st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{claim})
	require.ErrorContains(err, "collision")
	var kind string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE id = ?`), conv).Scan(&kind))
	assert.Equal(t, "group_chat", kind)
}

func TestTelegramKeyMigrationRetypesVerifiedUserDialog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
	claim := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: 123, ConversationType: "direct_chat", AllowRetype: true, Evidence: evidence}
	require.NoError(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{claim}))
	var kind, key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type, source_conversation_id FROM conversations WHERE id = ?`), conv).Scan(&kind, &key))
	assert.Equal("direct_chat", kind)
	assert.Equal("123", key)
}

func TestTelegramKeyMigrationRejectsTwoPeerKindsForLegacyGroup(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text string }{{"123:1", "synthetic one"}, {"123:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	evidence := []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}
	changes := []store.TelegramExportKeyChange{{LegacyID: 123, MarkedID: -123, ConversationType: "group_chat", Evidence: evidence}, {LegacyID: 123, MarkedID: -1000000000123, ConversationType: "group_chat", Evidence: evidence}}
	require.ErrorContains(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, changes), "ambiguous")
	var key string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id FROM conversations WHERE id = ?`), conv).Scan(&key))
	assert.Equal(t, "123", key)
}

func TestTelegramKeyMigrationPreservesShiftedMessageAndReplyIDs(t *testing.T) {
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "123", "group_chat", "Synthetic")
	require.NoError(err)
	when := time.Unix(100, 0).UTC()
	for _, row := range []struct{ id, text, meta string }{
		{"123:1", "synthetic one", `{}`}, {"123:2", "synthetic two", `{}`},
		{"123:-999999997", "older synthetic", `{"referenced_message_id":"123:-999999998"}`},
	} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{ConversationID: conv, SourceID: source.ID, SourceMessageID: row.id, MessageType: "telegram", SentAt: sql.NullTime{Time: when, Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}, Metadata: &sql.NullString{String: row.meta, Valid: true}})
		require.NoError(err)
	}
	change := store.TelegramExportKeyChange{LegacyID: 123, MarkedID: -1000000000123, ConversationType: "group_chat", ReplyEvidence: map[int64]store.TelegramExportReplyEvidence{-999999997: {ReplyID: -999999998, SentAt: when, Text: "older synthetic"}}, Evidence: []store.TelegramExportKeyEvidence{{MessageID: 1, SentAt: when, Text: "synthetic one"}, {MessageID: 2, SentAt: when, Text: "synthetic two"}}}
	require.NoError(st.MigrateTelegramExportKeysContext(context.Background(), source.ID, []store.TelegramExportKeyChange{change}))
	var raw string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-1000000000123:-999999997").Scan(&raw))
	var meta struct {
		ReferencedMessageID string `json:"referenced_message_id"`
	}
	require.NoError(json.Unmarshal([]byte(raw), &meta))
	assert.Equal(t, "-1000000000123:-999999998", meta.ReferencedMessageID)
}
