package cmd

import (
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
	"go.kenn.io/msgvault/internal/youtube"
)

func retireScheduledSyncSourceForTest(t *testing.T, st *store.Store, sourceType string) {
	t.Helper()
	retireScheduledSourceForTest(t, st, sourceType, "history", "history-current")
}

func retireScheduledSourceForTest(t *testing.T, st *store.Store, sourceType, identifier, destinationIdentifier string) {
	t.Helper()
	retired, err := st.GetOrCreateSource(sourceType, identifier)
	require.NoError(t, err)
	destination, err := st.GetOrCreateSource(sourceType, destinationIdentifier)
	require.NoError(t, err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: retired.ID,
		IntoSourceID: destination.ID,
	})
	require.NoError(t, err)
}

func TestConfiguredScheduledSyncSkipsMergedSources(t *testing.T) {
	t.Run("Granola before credential validation", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, granola.SourceType)

		err := runConfiguredGranolaSync(t.Context(), st, config.GranolaSource{
			Identifier: "history",
		})

		require.NoError(t, err)
	})

	t.Run("YouTube before credential validation", func(t *testing.T) {
		require := require.New(t)
		st := testutil.NewTestStore(t)
		channelID := "UCfixture"
		retireScheduledSourceForTest(t, st, youtube.SourceType,
			youtube.SourceIdentifier(channelID), youtube.SourceIdentifier("UCcurrent"))
		for _, name := range []string{"TEST_YOUTUBE_CLIENT_ID", "TEST_YOUTUBE_CLIENT_SECRET", "TEST_YOUTUBE_REFRESH_TOKEN"} {
			t.Setenv(name, "")
		}
		ctx := testInvocationContext(t.Context(), &config.Config{}, invocationOptions{})

		err := runConfiguredYouTubeSync(ctx, st, config.YouTubeSource{
			Identifier: "history", ChannelID: channelID,
			ClientIDEnv: "TEST_YOUTUBE_CLIENT_ID", ClientSecretEnv: "TEST_YOUTUBE_CLIENT_SECRET",
			TokenEnv: "TEST_YOUTUBE_REFRESH_TOKEN",
		})

		require.NoError(err)
		result, err := syncYouTubeProfile(ctx, st, config.YouTubeSource{
			Identifier: "history", ChannelID: channelID,
			ClientIDEnv: "TEST_YOUTUBE_CLIENT_ID", ClientSecretEnv: "TEST_YOUTUBE_CLIENT_SECRET",
			TokenEnv: "TEST_YOUTUBE_REFRESH_TOKEN",
		}, 1000)
		require.ErrorIs(err, store.ErrSourceRetired)
		require.Nil(result)
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
		ctx := testInvocationContext(t.Context(), &config.Config{}, invocationOptions{})

		err := runConfiguredCirclebackSync(ctx, st, config.CirclebackSource{
			Identifier: "history", AccountEmail: "person@example.test", Endpoint: server.URL,
		})

		require.NoError(t, err)
		require.Zero(t, requests.Load(), "a retired scheduled source must not connect to Circleback")
	})

	t.Run("Notion Meetings before token validation", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		retireScheduledSyncSourceForTest(t, st, notionmeetings.SourceType)

		err := runConfiguredNotionMeetingsSync(t.Context(), st, config.NotionMeetingsSource{
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

		err := runConfiguredMuesliSync(t.Context(), st, config.MuesliSource{
			Identifier: "history", AccountEmail: "person@example.test",
			DBPath: filepath.Join(t.TempDir(), "missing-muesli.db"),
		}, t.TempDir())

		require.NoError(t, err)
	})
}
