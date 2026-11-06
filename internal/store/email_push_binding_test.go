package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailPushBindingSurvivesNormalSyncButNotRemoval(t *testing.T) {
	for _, removal := range []string{"maintenance", "serialized"} {
		t.Run(removal, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t) // Also exercises PostgreSQL when configured.
			src, err := st.GetOrCreateSource("imap", "imap://owner@example.test")
			requirements.NoError(err)
			bound, token, err := st.BindEmailPushSource(t.Context(), src.ID)
			requirements.NoError(err)
			assertions.Equal(src.ID, bound.ID)
			requirements.NotEmpty(token)
			requirements.NoError(st.UpdateSourceSyncCursor(src.ID, "synthetic-cursor"))
			_, again, err := st.BindEmailPushSource(t.Context(), src.ID)
			requirements.NoError(err)
			assertions.Equal(token, again, "ordinary sync must not rotate the source incarnation")
			if removal == "serialized" {
				_, _, err = st.RemoveSourceSerialized(t.Context(), src.ID)
			} else {
				err = st.RemoveSource(src.ID)
			}
			requirements.NoError(err)
			_, err = st.EmailPushBoundSource(t.Context(), src.ID, token)
			requirements.Error(err)
			replacement, err := st.GetOrCreateSource("imap", src.Identifier)
			requirements.NoError(err)
			if !st.IsPostgreSQL() {
				assertions.Equal(src.ID, replacement.ID, "exercise SQLite reuse of the highest source rowid")
			}
			_, err = st.EmailPushBoundSource(t.Context(), replacement.ID, token)
			requirements.Error(err, "recreating the same tuple cannot restore the removed binding")
			_, fresh, err := st.BindEmailPushSource(t.Context(), replacement.ID)
			requirements.NoError(err)
			assertions.NotEqual(token, fresh)
			_, err = st.EmailPushBoundSource(t.Context(), replacement.ID, fresh)
			requirements.NoError(err)
			_, err = st.EmailPushBoundSource(t.Context(), replacement.ID, token)
			requirements.Error(err)
		})
	}
}

func TestEmailPushBindingRetiresWithSourceMerge(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("imap", "imap://old@example.test")
	requirements.NoError(err)
	into, err := st.GetOrCreateSource("imap", "imap://current@example.test")
	requirements.NoError(err)
	_, token, err := st.BindEmailPushSource(t.Context(), from.ID)
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	requirements.NoError(err)
	_, err = st.EmailPushBoundSource(t.Context(), from.ID, token)
	requirements.Error(err)
	_, _, err = st.BindEmailPushSource(t.Context(), from.ID)
	requirements.Error(err, "retired sources cannot receive a fresh binding")
}

func TestEmailPushBindingCanceledStartupDoesNotInstallToken(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://owner@example.test")
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = st.BindEmailPushSource(ctx, src.ID)
	requirements.ErrorIs(err, context.Canceled)
	var markers int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM archive_metadata WHERE key LIKE 'email_push.%'").Scan(&markers))
	assertions.Zero(markers, "canceled startup must not install an incarnation token")
}

func TestEmailPushBindingAndRemovalAcrossStoreHandles(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "synthetic.db")
	first := testutil.NewSQLiteTestStoreAt(t, path)
	second, err := store.OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(second.Close()) })
	for range 20 {
		src, err := first.GetOrCreateSource("imap", "imap://owner@example.test")
		requirements.NoError(err)
		start := make(chan struct{})
		type bound struct {
			token string
			err   error
		}
		binding, removal := make(chan bound, 1), make(chan error, 1)
		go func() {
			<-start
			_, token, err := first.BindEmailPushSource(t.Context(), src.ID)
			binding <- bound{token, err}
		}()
		go func() { <-start; _, _, err := second.RemoveSourceSerialized(t.Context(), src.ID); removal <- err }()
		close(start)
		result := <-binding
		requirements.NoError(<-removal)
		// Either startup linearized before removal, or it rejected the missing
		// source. Neither ordering can leave a live old token after deletion.
		if result.err == nil {
			_, err = first.EmailPushBoundSource(t.Context(), src.ID, result.token)
			requirements.Error(err)
		}
		replacement, err := first.GetOrCreateSource("imap", src.Identifier)
		requirements.NoError(err)
		assertions.Equal(src.ID, replacement.ID)
		if result.token != "" {
			_, err = first.EmailPushBoundSource(t.Context(), replacement.ID, result.token)
			requirements.Error(err)
		}
	}
}
