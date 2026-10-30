package api

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBackfillMediaRefreshesAndFillsExistingOccurrence(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	{
		require := require.New(t)
		require.NoError(err)
	}
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	old := &MediaReference{Location: &tg.InputDocumentFileLocation{ID: 101, FileReference: []byte("old")}, DCID: 1, Size: 9, Kind: "file"}
	live, err := old.liveMedia()
	require.NoError(t, err)
	require.NoError(t, writer.Write(ctx, telegram.Chat{ID: -123, Type: "private_group"}, telegram.Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: live}))
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{ID: 101, DCID: 1, Size: 9, FileReference: []byte("fresh")})
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	message.SetMedia(media)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
	opts := MediaOptions{AttachmentsDir: t.TempDir(), TempDir: t.TempDir(), Policy: attachmentpolicy.Policy{MaxBytes: 20, MaxParticipants: 20}, Download: func(_ context.Context, ref *MediaReference, dir string, _ int64) (*os.File, error) {
		assert.Equal(t, []byte("fresh"), ref.Location.(*tg.InputDocumentFileLocation).FileReference)
		f, err := os.CreateTemp(dir, "download-")
		require.NoError(t, err)
		_, err = f.WriteString("synthetic")
		require.NoError(t, err)
		_, err = f.Seek(0, 0)
		require.NoError(t, err)
		return f, nil
	}}
	summary, err := backfillMedia(ctx, st, source.ID, tg.NewClient(mock), []CatalogDialog{{ChatID: -123, NativeID: 123, Kind: "group", ParticipantCount: 2, ParticipantCountKnown: true}}, opts)
	require.NoError(t, err)
	{
		assert := assert.New(t)
		assert.Equal(1, summary.Stored)
	}
	page, err := st.ListTelegramPendingMediaAfterContext(ctx, source.ID, 0, 100)
	require.NoError(t, err)
	assert.Empty(t, page)
	assert.True(t, mock.AllWereMet())
}

func TestBackfillMediaDoesNotSubstituteChangedAttachment(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	second, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(998))
	require.NoError(err)
	ref := &MediaReference{Location: &tg.InputDocumentFileLocation{ID: 101, FileReference: []byte("old")}, DCID: 1, Size: 9, Kind: "file"}
	for _, source := range []struct{ id, owner int64 }{{first.ID, 999}, {second.ID, 998}} {
		live, err := ref.liveMedia()
		require.NoError(err)
		writer, err := telegram.NewLiveWriter(ctx, st, source.id, source.owner, "Synthetic")
		require.NoError(err)
		require.NoError(writer.Write(ctx, telegram.Chat{ID: -123, Type: "private_group"}, telegram.Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: live}))
	}
	changed := &tg.MessageMediaDocument{}
	changed.SetDocument(&tg.Document{ID: 102, DCID: 1, Size: 9, FileReference: []byte("fresh")})
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100}
	message.SetMedia(changed)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
	opts := MediaOptions{AttachmentsDir: t.TempDir(), TempDir: t.TempDir(), Policy: attachmentpolicy.Policy{MaxBytes: 20}, Download: func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		assert.Fail(t, "changed attachment must not be downloaded")
		return nil, nil
	}}
	summary, err := backfillMedia(ctx, st, first.ID, tg.NewClient(mock), []CatalogDialog{{ChatID: -123, NativeID: 123, Kind: "group"}}, opts)
	require.NoError(err)
	{
		assert := assert.New(t)
		assert.Equal(1, summary.Failed)
	}
	for _, sourceID := range []int64{first.ID, second.ID} {
		page, err := st.ListTelegramPendingMediaAfterContext(ctx, sourceID, 0, 100)
		require.NoError(err)
		require.Len(page, 1)
		refs, err := st.MessageProviderAttachmentsContext(ctx, page[0].MessageID, "api:")
		require.NoError(err)
		assert.Empty(t, refs["api:document:101"].StoragePath)
	}
	assert.True(t, mock.AllWereMet())
}

func TestAuthenticatedMediaBackfillIncludesVerifiedFormerGroup(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(t, err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(t, err)
	old := &MediaReference{Location: &tg.InputDocumentFileLocation{ID: 101, FileReference: []byte("old")}, DCID: 1, Size: 9, Kind: "file"}
	live, err := old.liveMedia()
	require.NoError(t, err)
	require.NoError(t, writer.Write(ctx, telegram.Chat{ID: -456, Type: "private_group"}, telegram.Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: live}))

	channel := &tg.Channel{ID: 123, Title: "Synthetic", Megagroup: true, Photo: &tg.ChatPhotoEmpty{}}
	channel.SetAccessHash(42)
	full := &tg.ChannelFull{ID: 123, ChatPhoto: &tg.PhotoEmpty{}}
	full.SetMigratedFromChatID(456)
	full.SetMigratedFromMaxID(7)
	message := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 456}, Date: 100}
	media := &tg.MessageMediaDocument{}
	media.SetDocument(&tg.Document{ID: 101, DCID: 1, Size: 9, FileReference: []byte("fresh")})
	message.SetMedia(media)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.UsersUserFull{FullUser: tg.UserFull{ID: 999}, Users: []tg.UserClass{&tg.User{ID: 999, Self: true}}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{Dialogs: []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 123}, TopMessage: 1}}, Chats: []tg.ChatClass{channel}})
	mock.Expect().ThenResult(&tg.MessagesDialogs{})
	mock.Expect().ThenResult(&tg.MessagesChatFull{FullChat: full})
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{message}})
	options := MediaOptions{AttachmentsDir: t.TempDir(), TempDir: t.TempDir(), Policy: attachmentpolicy.Policy{MaxBytes: 20}, Download: func(_ context.Context, ref *MediaReference, dir string, _ int64) (*os.File, error) {
		assert.Equal(t, []byte("fresh"), ref.Location.(*tg.InputDocumentFileLocation).FileReference)
		f, err := os.CreateTemp(dir, "download-")
		require.NoError(t, err)
		_, err = f.WriteString("synthetic")
		require.NoError(t, err)
		_, err = f.Seek(0, 0)
		require.NoError(t, err)
		return f, nil
	}}
	summary, err := RunAuthenticatedMediaBackfill(ctx, st, source.ID, tg.NewClient(mock), options)
	require.NoError(t, err)
	assert.Equal(t, MediaBackfillSummary{Stored: 1}, summary)
	pending, err := st.ListTelegramPendingMediaAfterContext(ctx, source.ID, 0, 100)
	require.NoError(t, err)
	assert.Empty(t, pending)
	assert.True(t, mock.AllWereMet())
}

func TestBackfillMediaStopsWhenRefreshExceedsFloodLimit(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	ref := &MediaReference{Location: &tg.InputDocumentFileLocation{ID: 101}, DCID: 1, Size: 9, Kind: "file"}
	live, err := ref.liveMedia()
	require.NoError(err)
	require.NoError(writer.Write(ctx, telegram.Chat{ID: -123, Type: "private_group"}, telegram.Message{ID: 7, SentAt: time.Unix(100, 0), RawJSON: `{}`, LiveMedia: live}))
	options := MediaOptions{AttachmentsDir: t.TempDir(), TempDir: t.TempDir(), Policy: attachmentpolicy.Policy{MaxBytes: 20}, Download: func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		assert.Fail(t, "unexpected download")
		return nil, nil
	}}
	_, err = backfillMedia(ctx, st, source.ID, tg.NewClient(&stubInvoker{err: ErrFloodWaitLimit}), []CatalogDialog{{ChatID: -123, NativeID: 123, Kind: "group"}}, options)
	require.ErrorIs(err, ErrFloodWaitLimit)
}
