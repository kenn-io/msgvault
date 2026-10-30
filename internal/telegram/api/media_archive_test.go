package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/telegram"
)

func TestArchiveMediaStoresDownloadedBytes(t *testing.T) {
	assert := assert.New(t)

	ref := &MediaReference{DCID: 1, Size: 9, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	dir := secureTempDir(t)
	var tempPath string
	media, err := archiveMedia(context.Background(), ref, attachmentpolicy.Policy{MaxBytes: 20}, attachmentpolicy.Conversation{Type: "direct_chat"}, t.TempDir(), dir, func(_ context.Context, _ *MediaReference, dir string, _ int64) (*os.File, error) {
		file, err := os.CreateTemp(dir, "synthetic-*")
		require.NoError(t, err)
		tempPath = file.Name()
		_, err = file.WriteString("synthetic")
		require.NoError(t, err)
		_, err = file.Seek(0, 0)
		return file, err
	})
	require.NoError(t, err)
	assert.NotEmpty(media.ContentHash)
	assert.NotEmpty(media.StoragePath)
	assert.Equal(int64(9), media.Size)
	_, err = os.Stat(tempPath)
	assert.True(os.IsNotExist(err))
}

func TestArchiveMediaSkipsOversizeWithoutDownloading(t *testing.T) {
	assert := assert.New(t)

	ref := &MediaReference{DCID: 1, Size: 21, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	called := false
	media, err := archiveMedia(context.Background(), ref, attachmentpolicy.Policy{MaxBytes: 20}, attachmentpolicy.Conversation{Type: "direct_chat"}, t.TempDir(), secureTempDir(t), func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		called = true
		return nil, errors.New("unexpected download")
	})
	require.NoError(t, err)
	assert.False(called)
	assert.Equal(attachmentpolicy.SkipSizeCap, media.SkipReason)
	assert.Empty(media.StoragePath)
}

func TestArchiveMediaRetainsRetryReferenceAfterFailure(t *testing.T) {
	ref := &MediaReference{DCID: 1, Size: 9, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	media, err := archiveMedia(context.Background(), ref, attachmentpolicy.Policy{MaxBytes: 20}, attachmentpolicy.Conversation{Type: "direct_chat"}, filepath.Join(t.TempDir(), "attachments"), secureTempDir(t), func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		return nil, errors.New("synthetic network failure")
	})
	require.NoError(t, err)
	assert.True(t, media.Failed)
	assert.Contains(t, media.Metadata, `"id":123`)
}

func TestArchiveMediaPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ref := &MediaReference{DCID: 1, Size: 9, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	media, err := archiveMedia(ctx, ref, attachmentpolicy.Policy{MaxBytes: 20}, attachmentpolicy.Conversation{Type: "direct_chat"}, t.TempDir(), secureTempDir(t), func(ctx context.Context, _ *MediaReference, _ string, _ int64) (*os.File, error) {
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, media)
}

func TestMediaProcessorDoesNotDownloadWhenMembershipIsUnknown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ref := &MediaReference{DCID: 1, Size: 9, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	media, err := ref.liveMedia()
	require.NoError(err)
	called := false
	process := newMediaProcessor(attachmentpolicy.Policy{MaxBytes: 20, MaxParticipants: 20}, []CatalogDialog{{ChatID: -123, Kind: "group"}}, t.TempDir(), secureTempDir(t), func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		called = true
		return nil, errors.New("unexpected download")
	})
	require.NoError(process(context.Background(), telegram.Chat{ID: -123, Type: "private_group"}, media))
	assert.False(called)
	assert.True(media.Failed)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(process(ctx, telegram.Chat{ID: -123, Type: "private_group"}, media), context.Canceled)
}

func TestMediaProcessorAppliesKnownMembershipLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ref := &MediaReference{DCID: 1, Size: 9, Kind: "file", Location: &tg.InputDocumentFileLocation{ID: 123}}
	media, err := ref.liveMedia()
	require.NoError(err)
	called := false
	process := newMediaProcessor(attachmentpolicy.Policy{MaxBytes: 20, MaxParticipants: 20}, []CatalogDialog{{ChatID: -123, Kind: "group", ParticipantCount: 21, ParticipantCountKnown: true}}, t.TempDir(), secureTempDir(t), func(context.Context, *MediaReference, string, int64) (*os.File, error) {
		called = true
		return nil, errors.New("unexpected download")
	})
	require.NoError(process(context.Background(), telegram.Chat{ID: -123, Type: "private_group"}, media))
	assert.False(called)
	assert.Equal(attachmentpolicy.SkipParticipantThreshold, media.SkipReason)
}
