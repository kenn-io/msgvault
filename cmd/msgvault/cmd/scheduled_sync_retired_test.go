package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/twenty"
)

func retireScheduledSyncSourceForTest(t *testing.T, st *store.Store, sourceType string) {
	t.Helper()
	const identifier = "history"
	retired, err := st.GetOrCreateSource(sourceType, identifier)
	require.NoError(t, err)
	destination, err := st.GetOrCreateSource(sourceType, identifier+"-current")
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(context.Background(), store.MergeSourcesRequest{
		FromSourceID: retired.ID,
		IntoSourceID: destination.ID,
	})
	require.NoError(t, err)
}

func TestConfiguredScheduledSyncSkipsMergedSources(t *testing.T) {
	t.Run("Granola before credential validation", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, granola.SourceType)

		err := runConfiguredGranolaSync(context.Background(), st, config.GranolaSource{
			Identifier: "history",
		})

		require.NoError(t, err)
	})

	t.Run("Circleback before provider connection", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, circleback.SourceType)
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		ctx := testInvocationContext(context.Background(), &config.Config{}, invocationOptions{})

		err := runConfiguredCirclebackSync(ctx, st, config.CirclebackSource{
			Identifier: "history", AccountEmail: "person@example.test", Endpoint: server.URL,
		})

		require.NoError(t, err)
		require.Zero(t, requests.Load(), "a retired scheduled source must not connect to Circleback")
	})

	t.Run("Notion Meetings before token validation", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, notionmeetings.SourceType)

		err := runConfiguredNotionMeetingsSync(context.Background(), st, config.NotionMeetingsSource{
			Identifier: "history",
		})

		require.NoError(t, err)
	})

	t.Run("Twenty before credential validation", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, twenty.SourceType)

		err := runConfiguredTwentySync(t.Context(), st, config.TwentySource{
			Identifier: "history",
		})

		require.NoError(t, err)
	})

	t.Run("Muesli before local database access", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, muesli.SourceType)

		err := runConfiguredMuesliSync(context.Background(), st, config.MuesliSource{
			Identifier: "history", AccountEmail: "person@example.test",
			DBPath: filepath.Join(t.TempDir(), "missing-muesli.db"),
		})

		require.NoError(t, err)
	})
}
