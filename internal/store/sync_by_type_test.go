package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGetSyncByTypeContextIgnoresOtherTypes(t *testing.T) {
	for _, kind := range []string{"facebook_comments", "instagram_comments", ""} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
			require.NoError(err)
			run, err := st.StartSync(src.ID, kind)
			require.NoError(err)
			require.NoError(st.PauseSyncWithCheckpoint(t.Context(), run, &store.Checkpoint{PageToken: "A"}))
			other, err := st.StartSync(src.ID, "facebook_comments_webhook")
			require.NoError(err)
			require.NoError(st.CompleteSync(other, ""))
			checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), src.ID, kind)
			require.NoError(err)
			assert.Equal(run, checkpoint.ID)
			assert.Equal("A", checkpoint.CursorBefore.String)
			_, err = st.GetLastSuccessfulSyncByType(t.Context(), src.ID, kind)
			require.ErrorIs(err, store.ErrSyncRunNotFound)
			completed, err := st.StartSync(src.ID, kind)
			require.NoError(err)
			require.NoError(st.CompleteSync(completed, "B"))
			_, err = st.GetLatestCheckpointedSyncByType(t.Context(), src.ID, kind)
			require.ErrorIs(err, store.ErrSyncRunNotFound)
			successful, err := st.GetLastSuccessfulSyncByType(t.Context(), src.ID, kind)
			require.NoError(err)
			assert.Equal(completed, successful.ID)
			assert.Equal("B", successful.CursorAfter.String)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = st.GetLastSuccessfulSyncByType(ctx, src.ID, kind)
			require.ErrorIs(err, context.Canceled)
			_, err = st.GetLatestCheckpointedSyncByType(ctx, src.ID, kind)
			require.ErrorIs(err, context.Canceled)
		})
	}
}
