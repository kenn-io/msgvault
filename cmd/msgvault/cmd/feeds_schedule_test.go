package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/feeds"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestScheduledFeedSyncFailsOnlyWhenEveryFeedFails(t *testing.T) {
	require := require.New(t)

	var mu sync.Mutex
	broken := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := broken[r.URL.Path]
		mu.Unlock()
		if fail {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(feedCommandsAtom))
	}))
	t.Cleanup(server.Close)
	breakFeed := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		broken[path] = true
	}

	st := testutil.NewTestStore(t)
	_, err := feeds.NewSyncer(st, feeds.NewFetcher("msgvault-test"), nil).
		Follow(t.Context(), []string{server.URL + "/a.atom", server.URL + "/b.atom"})
	require.NoError(err)
	cfg := testConfigValue()
	cfg.Analytics.AutoBuildCache = false
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	breakFeed("/a.atom")
	require.NoError(runConfiguredFeedSync(ctx, st), "one broken feed is recorded on the run, not an outage")

	breakFeed("/b.atom")
	require.ErrorIs(runConfiguredFeedSync(ctx, st), feeds.ErrEveryFeedFailed, "every feed failing is an outage")
}
