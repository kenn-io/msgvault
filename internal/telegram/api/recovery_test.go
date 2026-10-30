package api

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/telegram"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTrailingRescanAppliesEditWithoutInferringDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", TopMessage: 7, EnabledByDefault: true}
	for _, msg := range []*tg.Message{{ID: 6, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "absent"}, {ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "original"}} {
		require.NoError(ArchiveAPIMessageWithWriter(ctx, writer, dialog, msg, nil, nil))
	}
	edited := &tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "edited synthetic"}
	edited.SetEditDate(150)
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{edited}})
	mock.Expect().ThenResult(&tg.MessagesMessages{})
	require.NoError(RescanDialogSince(ctx, tg.NewClient(mock), writer, dialog, time.Unix(50, 0).UTC()))
	var id int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:7").Scan(&id))
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&body))
	assert.Equal("edited synthetic", body)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE source_id = ? AND source_message_id = ?`), source.ID, "-123:6").Scan(&deleted))
	assert.False(deleted)
	assert.True(mock.AllWereMet())
}

func TestTrailingRescanStopsAtExclusiveDateBoundary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx := context.Background()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(telegram.SourceType, telegram.OwnerSourceIdentifier(999))
	require.NoError(err)
	writer, err := telegram.NewLiveWriter(ctx, st, source.ID, 999, "Synthetic")
	require.NoError(err)
	dialog := CatalogDialog{ChatID: -123, NativeID: 123, Kind: "group", Title: "Synthetic", TopMessage: 8}
	mock := tgmock.NewRequire(t)
	mock.Expect().ThenResult(&tg.MessagesMessages{Messages: []tg.MessageClass{
		&tg.Message{ID: 8, PeerID: &tg.PeerChat{ChatID: 123}, Date: 101, Message: "new"},
		&tg.Message{ID: 7, PeerID: &tg.PeerChat{ChatID: 123}, Date: 100, Message: "boundary"},
	}})
	require.NoError(RescanDialogSince(ctx, tg.NewClient(mock), writer, dialog, time.Unix(100, 0).UTC()))
	assert.True(mock.AllWereMet())
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&count))
	assert.Equal(1, count)
}
