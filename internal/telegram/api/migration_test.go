package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChannelMigrationReadsVerifiedFormerGroup(t *testing.T) {
	mock := tgmock.NewRequire(t)
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(456)
	full.SetMigratedFromMaxID(789)
	mock.ExpectFunc(func(input bin.Encoder) {
		request, ok := input.(*tg.ChannelsGetFullChannelRequest)
		require.True(t, ok)
		assert.Equal(t, &tg.InputChannel{ChannelID: 123, AccessHash: 42}, request.Channel)
	}).ThenResult(&tg.MessagesChatFull{FullChat: full})
	link, err := GetChannelMigration(context.Background(), tg.NewClient(mock), CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", AccessHash: 42})
	{
		require := require.New(t)
		require.NoError(err)
	}
	require.NotNil(t, link)
	{
		assert := assert.New(t)
		assert.EqualValues(456, link.FormerChatID)
	}
	assert.Equal(t, 789, link.FormerMaxID)
}

func TestChannelMigrationRejectsWrongResponsePeer(t *testing.T) {
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: &tg.ChannelFull{ID: 999, ChatPhoto: &tg.PhotoEmpty{}}})
	_, err := GetChannelMigration(context.Background(), tg.NewClient(mock), CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"})
	require.ErrorContains(t, err, "identity")
}

func TestChannelMigrationDistinguishesAbsentAndInvalidLink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		oldID   int64
		maxID   int
		wantErr bool
	}{
		{name: "absent"}, {name: "missing boundary", oldID: 456, wantErr: true}, {name: "missing former group", maxID: 7, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := tgmock.NewRequire(t)
			full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
			if tc.oldID != 0 {
				full.SetMigratedFromChatID(tc.oldID)
			}
			if tc.maxID != 0 {
				full.SetMigratedFromMaxID(tc.maxID)
			}
			mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
			link, err := GetChannelMigration(context.Background(), tg.NewClient(mock), CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Nil(t, link)
		})
	}
}

func TestRepairShiftedHistoryUsesDiscoveredChannelLink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:-999999999", MessageType: "telegram"}})
	require.NoError(err)
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(456)
	full.SetMigratedFromMaxID(100)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
	dialogs := []CatalogDialog{{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", AccessHash: 42}}
	require.NoError(RepairShiftedHistory(ctx, st, source.ID, tg.NewClient(mock), dialogs))
	shifted, err := st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	assert.False(shifted)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-456:1").Scan(&count))
	assert.Equal(1, count)
	var formerConversationID, messageConversationID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), source.ID, "-456").Scan(&formerConversationID))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-456:1").Scan(&messageConversationID))
	assert.Equal(formerConversationID, messageConversationID)
}

func TestRepairShiftedHistoryPersistsLinkWithoutShiftedMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(456)
	full.SetMigratedFromMaxID(100)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", AccessHash: 42, EnabledByDefault: true}
	require.NoError(RepairShiftedHistory(ctx, st, source.ID, tg.NewClient(mock), []CatalogDialog{dialog}))
	link, err := st.TelegramHistoryMigrationContext(ctx, source.ID, dialog.ChatID)
	require.NoError(err)
	assert.Equal(&store.TelegramHistoryMigration{FormerChatID: 456, FormerMaxID: 100}, link)
	assert.True(mock.AllWereMet())

	cachedMock := tgmock.NewRequire(t)
	formerGroups, err := repairAndDiscoverFormerGroups(ctx, st, source.ID, tg.NewClient(cachedMock), []CatalogDialog{dialog}, 0)
	require.NoError(err)
	assert.Equal([]CatalogDialog{{
		ChatID: -456, NativeID: 456, Kind: "group", Title: dialog.Title,
		TopMessage: 100, EnabledByDefault: true, MigrationParentChatID: dialog.ChatID,
	}}, formerGroups)
	assert.True(cachedMock.AllWereMet())
}

func TestRepairShiftedHistoryRequiresAccessibleSupergroup(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:-999999999", MessageType: "telegram"}})
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	err = RepairShiftedHistory(ctx, st, source.ID, tg.NewClient(mock), nil)
	require.ErrorContains(err, "access to its supergroup")
	shifted, err := st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	assert.True(t, shifted)
}

func TestRepairShiftedHistoryDefersInaccessibleSupergroup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "-1000000000123", "group_chat", "Synthetic")
	require.NoError(err)
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "-1000000000123:-999999999", MessageType: "telegram"}})
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	dialog := CatalogDialog{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup", Inaccessible: true}
	require.NoError(RepairShiftedHistory(ctx, st, source.ID, tg.NewClient(mock), []CatalogDialog{dialog}))
	shifted, err := st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	assert.True(shifted, "the inaccessible conversation remains available for repair when access returns")
	assert.True(mock.AllWereMet())
}

func TestRepairShiftedReplyWithoutArchivedParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	conv, err := st.EnsureConversationWithType(source.ID, "456", "direct_chat", "Synthetic")
	require.NoError(err)
	meta := sql.NullString{String: `{"referenced_message_id":"-1000000000123:-999999999","reply_peer_verified":true}`, Valid: true}
	_, err = st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "456:3", MessageType: "telegram"}, Metadata: &meta})
	require.NoError(err)
	shifted, err := st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	require.True(shifted)
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(789)
	full.SetMigratedFromMaxID(100)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
	require.NoError(RepairShiftedHistory(ctx, st, source.ID, tg.NewClient(mock), []CatalogDialog{{ChatID: -1000000000123, NativeID: 123, Kind: "supergroup"}}))
	var stored string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT metadata FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "456:3").Scan(&stored))
	var recovery struct {
		Reference string `json:"referenced_message_id"`
	}
	require.NoError(json.Unmarshal([]byte(stored), &recovery))
	assert.Equal("-789:1", recovery.Reference)
	shifted, err = st.HasTelegramShiftedHistoryContext(ctx, source.ID)
	require.NoError(err)
	assert.False(shifted)
}

func TestVerifyAmbiguousUserFromAPIHistory(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	const userID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: userID}, Date: 100, Message: "synthetic two"},
		&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: userID}, Date: 100, Message: "synthetic one"},
	}})
	dialog := CatalogDialog{ChatID: userID, NativeID: userID, Kind: "user", TopMessage: 2}
	require.NoError(VerifyAmbiguousUserFromHistory(ctx, st, source.ID, tg.NewClient(mock), dialog))
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(t, ambiguous)
}

func TestVerifyAmbiguousUserRejectsDifferentSnapshots(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	const userID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 2, PeerID: &tg.PeerUser{UserID: userID}, Date: 100, Message: "different two"},
		&tg.Message{ID: 1, PeerID: &tg.PeerUser{UserID: userID}, Date: 100, Message: "different one"},
	}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	dialog := CatalogDialog{ChatID: userID, NativeID: userID, Kind: "user", TopMessage: 2}
	require.ErrorIs(VerifyAmbiguousUserFromHistory(ctx, st, source.ID, tg.NewClient(mock), dialog), store.ErrTelegramEvidenceInsufficient)
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.True(t, ambiguous)
}

func TestVerifyAmbiguousChannelFromAPIHistory(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("telegram", "user999")
	require.NoError(err)
	const peerID int64 = 1002147483649
	conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
	require.NoError(err)
	for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
		_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
		require.NoError(err)
	}
	marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: peerID})
	require.NoError(err)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 2, PeerID: &tg.PeerChannel{ChannelID: peerID}, Date: 100, Message: "synthetic two"},
		&tg.Message{ID: 1, PeerID: &tg.PeerChannel{ChannelID: peerID}, Date: 100, Message: "synthetic one"},
	}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	dialog := CatalogDialog{ChatID: marked, NativeID: peerID, Kind: "supergroup", TopMessage: 2}
	require.NoError(VerifyAmbiguousChannelFromHistory(ctx, st, source.ID, tg.NewClient(mock), dialog))
	ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
	require.NoError(err)
	assert.False(t, ambiguous)
}

func TestRepairAmbiguousPeersComparesAllCandidatesBeforeWriting(t *testing.T) {
	for _, mode := range []string{"user", "channel", "both", "neither"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			ctx := context.Background()
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("telegram", "user999")
			require.NoError(err)
			const id int64 = 1002147483649
			conv, err := st.EnsureConversationWithType(source.ID, "1002147483649", "direct_chat", "Synthetic")
			require.NoError(err)
			for _, row := range []struct{ key, text string }{{"1002147483649:1", "synthetic one"}, {"1002147483649:2", "synthetic two"}} {
				_, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: row.key, MessageType: "telegram", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}}, BodyText: sql.NullString{String: row.text, Valid: true}})
				require.NoError(err)
			}
			marked, err := telegram.MarkedPeerID(&tg.PeerChannel{ChannelID: id})
			require.NoError(err)
			mock := tgmock.NewRequire(t)
			for _, candidate := range []struct {
				peer    tg.PeerClass
				matches bool
			}{
				{&tg.PeerUser{UserID: id}, mode == "user" || mode == "both"},
				{&tg.PeerChannel{ChannelID: id}, mode == "channel" || mode == "both"},
			} {
				first, second := "different one", "different two"
				if candidate.matches {
					first, second = "synthetic one", "synthetic two"
				}
				mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{&tg.Message{ID: 2, PeerID: candidate.peer, Date: 100, Message: second}, &tg.Message{ID: 1, PeerID: candidate.peer, Date: 100, Message: first}}})
				mock.Expect().ThenResult(&tg.MessagesMessages{})
			}
			dialogs := []CatalogDialog{{ChatID: id, NativeID: id, Kind: "user", TopMessage: 2}, {ChatID: marked, NativeID: id, Kind: "supergroup", TopMessage: 2}}
			err = RepairAmbiguousPeers(ctx, st, source.ID, tg.NewClient(mock), dialogs)
			if mode == "both" || mode == "neither" {
				require.Error(err)
				ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
				require.NoError(err)
				assert.True(ambiguous)
				return
			}
			require.NoError(err)
			var key string
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_conversation_id FROM conversations WHERE id = ?`), conv).Scan(&key))
			if mode == "channel" {
				assert.Equal(strconv.FormatInt(marked, 10), key)
			} else {
				assert.Equal("1002147483649", key)
			}
			ambiguous, err := st.HasAmbiguousTelegramDirectKeysContext(ctx, source.ID)
			require.NoError(err)
			assert.False(ambiguous)
		})
	}
}
