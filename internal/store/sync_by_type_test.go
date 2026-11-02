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
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("facebook_comments", "app:9:page:100")
			require.NoError(t, err)
			run, err := st.StartSync(src.ID, kind)
			require.NoError(t, err)
			require.NoError(t, st.PauseSyncWithCheckpoint(run, &store.Checkpoint{PageToken: "A"}))
			other, err := st.StartSync(src.ID, "facebook_comments_webhook")
			require.NoError(t, err)
			require.NoError(t, st.CompleteSync(other, ""))
			checkpoint, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, kind)
			require.NoError(t, err)
			assert.Equal(t, run, checkpoint.ID)
			assert.Equal(t, "A", checkpoint.CursorBefore.String)
			_, err = st.GetLastSuccessfulSyncByTypeContext(t.Context(), src.ID, kind)
			assert.ErrorIs(t, err, store.ErrSyncRunNotFound)
			completed, err := st.StartSync(src.ID, kind)
			require.NoError(t, err)
			require.NoError(t, st.CompleteSync(completed, "B"))
			_, err = st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, kind)
			assert.ErrorIs(t, err, store.ErrSyncRunNotFound)
			successful, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), src.ID, kind)
			require.NoError(t, err)
			assert.Equal(t, completed, successful.ID)
			assert.Equal(t, "B", successful.CursorAfter.String)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = st.GetLastSuccessfulSyncByTypeContext(ctx, src.ID, kind)
			assert.ErrorIs(t, err, context.Canceled)
			_, err = st.GetLatestCheckpointedSyncByTypeContext(ctx, src.ID, kind)
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}
