package meta

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInstagramAdsStreamedYieldPreservesFailureQualification(t *testing.T) {
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "pure yield", true: "yield after refusal"}[joined], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewSQLiteTestStore(t)
			f, page, ad := newAdReadFixture(t)
			f.stream = true
			if joined {
				f.unresolved = "missing target"
			}
			imp := NewImporter(st, page)
			src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
			require.NoError(err)
			ctx, cancel := context.WithCancelCause(t.Context())
			t.Cleanup(func() { cancel(nil) })
			var reads atomic.Int32
			base := page.http.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			page.http.Transport = streamedGraphTransport{RoundTripper: base, cancel: cancel, reads: &reads, path: "/v26.0/900"}
			_, err = imp.ImportInstagramAds(ctx, "9", "100", "200", "700", ad, ImportOptions{})
			require.ErrorIs(err, context.Canceled)
			require.ErrorIs(context.Cause(ctx), jobctx.ErrYieldedToWaiter)
			assert.Positive(reads.Load(), "cancellation follows actual response bytes")
			assert.NotContains(err.Error(), "synthetic-page-secret")
			assert.NotContains(err.Error(), "private-body")
			run, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			want := store.SyncStatusCancelled
			if joined {
				want = store.SyncStatusFailed
			}
			assert.Equal(want, run.Status)
			if joined {
				assert.Positive(run.ErrorsCount)
			} else {
				assert.Zero(run.ErrorsCount)
			}
			var state syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
			assert.True(state.Phases[instagramMediaEdge].Done)
			assert.Equal(joined, state.Phases[instagramMediaEdge].Rescan)
			require.Len(state.Phases[adCreativesEdge].Posts, 1)
			assert.Equal("600", state.Phases[adCreativesEdge].Posts[0].ID)
			if joined {
				assert.Contains(run.ErrorMessage.String, `"599"`)
			}
			page.http.Transport, f.stream, f.unresolved = base, false, ""
			completed := false
			for range 3 {
				sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ad, ImportOptions{})
				if err != nil {
					require.ErrorContains(err, "unverified")
				}
				if !sum.Partial {
					completed = true
					break
				}
			}
			require.True(completed)
			message(t, st, src.ID, "1000")
		})
	}
}
