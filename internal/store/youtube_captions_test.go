package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestYouTubeCaptionCursorCompareAndSwap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
	requirements.NoError(err)
	got, err := f.Store.YouTubeCaptionStateContext(t.Context(), src.ID)
	requirements.NoError(err)
	assertions.Empty(got)
	requirements.NoError(f.Store.CompareAndSwapYouTubeCaptionStateContext(t.Context(), src.ID, "", `{"after":1}`))
	requirements.ErrorIs(f.Store.CompareAndSwapYouTubeCaptionStateContext(t.Context(), src.ID, "", `{"after":2}`), store.ErrYouTubeCaptionCursorChanged)
	got, err = f.Store.YouTubeCaptionStateContext(t.Context(), src.ID)
	requirements.NoError(err)
	assertions.Equal(`{"after":1}`, got)
	assertions.Error(f.Store.CompareAndSwapYouTubeCaptionStateContext(t.Context(), f.Source.ID, "", `{}`))
}

func TestYouTubeCaptionRootsSnapshotAndScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(src.ID, "video-a", "Example video")
	requirements.NoError(err)
	add := func(external string) int64 {
		id, err := f.Store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: external, MessageType: "post"})
		requirements.NoError(err)
		return id
	}
	first := add("video:video-a")
	add("comment-example")
	f.CreateMessage("video:wrong-source")
	upper, err := f.Store.YouTubeCaptionRootBoundContext(t.Context(), src.ID)
	requirements.NoError(err)
	assertions.Equal(first, upper)
	late := add("video:video-b")
	root, err := f.Store.NextYouTubeCaptionRootContext(t.Context(), src.ID, 0, upper)
	requirements.NoError(err)
	requirements.NotNil(root)
	assertions.Equal(first, root.MessageID)
	assertions.Equal("video-a", root.VideoID)
	root, err = f.Store.NextYouTubeCaptionRootContext(t.Context(), src.ID, first, upper)
	requirements.NoError(err)
	assertions.Nil(root)
	root, err = f.Store.NextYouTubeCaptionRootContext(t.Context(), src.ID, first, late)
	requirements.NoError(err)
	requirements.NotNil(root)
	assertions.Equal(late, root.MessageID)
}
