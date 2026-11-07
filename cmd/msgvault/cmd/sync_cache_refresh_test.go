package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/youtube"
)

func TestManualCLICommandNeedsCacheRefresh(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "manual sync", args: []string{"sync-youtube"}, want: true},
		{name: "confirmed reply", args: []string{"youtube", "reply", "42", "--confirm", "token"}, want: true},
		{name: "confirmed reply with verbose logging", args: []string{"youtube", "reply", "42", "--confirm", "token", "--verbose"}, want: true},
		{name: "confirmed reply with log level", args: []string{"youtube", "reply", "42", "--confirm", "token", "--log-level", "debug"}, want: true},
		{name: "equals confirmation", args: []string{"youtube", "reply", "42", "--confirm=token"}, want: true},
		{name: "resolve accepted receipt", args: []string{"youtube", "reply", "42", "--confirm=token", "--resolve-comment", "reply-1"}, want: true},
		{name: "resolve accepted receipt with root logging flags", args: []string{"youtube", "reply", "42", "--confirm=token", "--resolve-comment", "reply-1", "--verbose", "--log-level", "debug"}, want: true},
		{name: "body resembles dry run flag", args: []string{"youtube", "reply", "42", "--body", "--dry-run", "--confirm", "token"}, want: true},
		{name: "confirmation resembles dry run flag", args: []string{"youtube", "reply", "42", "--body", "Example reply", "--confirm", "--dry-run"}, want: true},
		{name: "dry run", args: []string{"youtube", "reply", "42", "--dry-run"}},
		{name: "missing confirmation", args: []string{"youtube", "reply", "42", "--body", "Example reply"}},
		{name: "empty confirmation", args: []string{"youtube", "reply", "42", "--confirm="}},
		{name: "unrelated command", args: []string{"youtube", "comments"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, manualCLICommandNeedsCacheRefresh(test.args))
		})
	}
}

func TestManualSyncCacheFlagValuesIgnoreYouTubeReplyStringValues(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{
			name: "body resembles skip flag",
			args: []string{"youtube", "reply", "42", "--body", "--no-build-cache", "--confirm", "token"},
		},
		{
			name: "body resembles force flag",
			args: []string{"youtube", "reply", "42", "--body", "--build-cache", "--confirm", "token"},
		},
		{
			name: "confirmation resembles skip flag",
			args: []string{"youtube", "reply", "42", "--body", "Example reply", "--confirm", "--no-build-cache"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			force, skip := manualSyncCacheFlagValues(test.args)
			assert.False(t, force)
			assert.False(t, skip)
		})
	}
}

func TestManualSyncRefreshVerifiesConversationOnlyChangesInBackground(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	c, s := openTestDaemonAnalyticsStore(t)
	c.Analytics.AutoBuildCache = true
	c.Analytics.MinRebuildInterval = 0
	source, err := s.GetOrCreateSource("gmail", "user@example.test")
	require.NoError(err)
	conversationID, err := s.EnsureConversationWithType(source.ID, "thread-1", "email_thread", "Original title")
	require.NoError(err)
	_, err = s.UpsertMessage(&store.Message{
		SourceID: source.ID, SourceMessageID: "message-1", ConversationID: conversationID,
		MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	require.NoError(err)
	_, err = buildCache(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(), true)
	require.NoError(err)
	_, err = s.EnsureConversationWithType(source.ID, "thread-1", "email_thread", "Updated title")
	require.NoError(err)
	light, err := cacheNeedsBuildForServing(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
	require.NoError(err)
	require.False(light.NeedsBuild, "conversation titles have no indexed staleness signal")

	ctx, cancel := context.WithCancel(t.Context())
	verified := make(chan cacheStaleness, 1)
	jobs := newCacheBuildJobs(ctx, nil, func(context.Context, buildCacheMode) error {
		verified <- cacheNeedsBuildContext(t.Context(), c.DatabaseDSN(), c.AnalyticsDir())
		return nil
	})
	// Cleanup runs after t.Context() is canceled.
	t.Cleanup(func() {
		cancel()
		waitCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), serveLifecycleTestTimeout)
		defer stop()
		require.True(jobs.waitContext(waitCtx), "background verification must finish before store cleanup")
	})
	adapter := &storeAPIAdapter{store: s, config: c, cacheJobs: jobs}
	require.NoError(adapter.queueCacheRefreshAfterManualSync(false, false))
	waitCtx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer stop()
	require.True(jobs.waitContext(waitCtx))
	select {
	case full := <-verified:
		assert.True(full.NeedsBuild)
		assert.True(full.HasConversationTypeDrift)
		assert.Contains(full.Reason, "conversation metadata changed")
	default:
		require.FailNow("manual sync did not queue full cache verification")
	}
}

func TestManualSyncRefreshChecksFilesBeforeThrottling(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		name := "usable"
		if damaged {
			name = "missing shard"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			c, s := openPublishedQueryTestStore(t)
			c.Analytics.AutoBuildCache = true
			c.Analytics.MinRebuildInterval = 6 * time.Hour
			_, err := s.DB().ExecContext(t.Context(), `INSERT INTO messages
				(id, source_id, source_message_id, conversation_id, message_type, sent_at)
				VALUES (2, 1, 'message-2', 1, 'email', '2024-01-02 00:00:00')`)
			require.NoError(err)
			if damaged {
				shards, err := filepath.Glob(filepath.Join(c.AnalyticsDir(), "messages", "*", "*.parquet"))
				require.NoError(err)
				require.Len(shards, 1)
				require.NoError(os.Remove(shards[0]))
			}
			ctx, cancel := context.WithCancel(t.Context())
			completed := make(chan *buildResult, 1)
			jobs := newCacheBuildJobs(ctx, nil, func(context.Context, buildCacheMode) error {
				result, err := buildCacheScheduled(t.Context(), c.DatabaseDSN(), c.AnalyticsDir(),
					c.Analytics.MinRebuildInterval, time.Now)
				completed <- result
				return err
			})
			// Cleanup runs after t.Context() is canceled.
			t.Cleanup(func() {
				cancel()
				waitCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), serveLifecycleTestTimeout)
				defer stop()
				require.True(jobs.waitContext(waitCtx))
			})
			adapter := &storeAPIAdapter{store: s, config: c, cacheJobs: jobs}
			require.NoError(adapter.queueCacheRefreshAfterManualSync(false, false))
			waitCtx, stop := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
			defer stop()
			require.True(jobs.waitContext(waitCtx))
			select {
			case result := <-completed:
				require.NotNil(result)
				assert.Equal(!damaged, result.Skipped)
			default:
				require.FailNow("manual sync did not queue cache verification")
			}
			engine, err := openDaemonDuckDBEngine(t.Context(), c, s)
			require.NoError(err)
			t.Cleanup(func() { _ = engine.Close() })
			result, err := engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
			require.NoError(err)
			require.Len(result.Rows, 1)
			wantCount := 1
			if damaged {
				wantCount = 2
			}
			assert.EqualValues(wantCount, result.Rows[0][0])
		})
	}
}

func TestYouTubeReplyAcceptedRecoveryRefreshesCommittedCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg, st := openTestDaemonAnalyticsStore(t)
	cfg.Analytics.AutoBuildCache = true
	cfg.Analytics.MinRebuildInterval = 0
	cfg.YouTube = []config.YouTubeSource{{
		Identifier: "example", ChannelID: "owner-1", TokenEnv: "EXAMPLE_YOUTUBE_REFRESH",
		ClientIDEnv: "EXAMPLE_YOUTUBE_CLIENT", ClientSecretEnv: "EXAMPLE_YOUTUBE_SECRET",
	}}
	parentID := seedYouTubeReplyArchive(t, st)
	_, err := buildCache(t.Context(), cfg.DatabaseDSN(), cfg.AnalyticsDir(), true)
	require.NoError(err)
	engine, err := openDaemonDuckDBEngine(t.Context(), cfg, st)
	require.NoError(err)
	rows, err := engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	require.NoError(err)
	require.Len(rows.Rows, 1)
	assert.EqualValues(2, rows.Rows[0][0])
	require.NoError(engine.Close())

	target, err := youtube.ResolveReplyTarget(t.Context(), st, parentID)
	require.NoError(err)
	var providerComment youtube.Comment
	require.NoError(json.Unmarshal([]byte(`{"id":"sent-1","snippet":{"parentId":"parent-1","videoId":"video-1","textOriginal":"--dry-run","authorChannelId":{"value":"owner-1"},"publishedAt":"2026-01-03T00:00:00Z"}}`), &providerComment))
	service := &youtube.ReplyService{
		Store: st, Provider: acceptedYouTubeReplyProvider{comment: providerComment},
		Persist: func(context.Context, youtube.Comment) (int64, error) {
			return 0, errors.New("synthetic archive failure")
		},
	}
	preview, err := service.Preview(t.Context(), target, "--dry-run")
	require.NoError(err)
	accepted, err := service.Confirm(t.Context(), target, "--dry-run", preview.Token)
	require.Error(err)
	require.NotNil(accepted)
	assert.Equal("accepted", accepted.State)
	require.NoError(st.Close())

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_YOUTUBE_REFRESH", "")
	t.Setenv("EXAMPLE_YOUTUBE_CLIENT", "")
	t.Setenv("EXAMPLE_YOUTUBE_SECRET", "")
	cacheCtx, cancel := context.WithCancel(t.Context())
	jobs := newCacheBuildJobs(cacheCtx, nil, func(ctx context.Context, _ buildCacheMode) error {
		_, err := buildCacheScheduled(ctx, cfg.DatabaseDSN(), cfg.AnalyticsDir(), cfg.Analytics.MinRebuildInterval, time.Now)
		return err
	})
	t.Cleanup(func() {
		cancel()
		waitCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), serveLifecycleTestTimeout)
		defer stop()
		require.True(jobs.waitContext(waitCtx), "reply cache refresh must retire before store cleanup")
	})
	adapter := &storeAPIAdapter{store: st, config: cfg, cacheJobs: jobs}
	var output bytes.Buffer
	// Run the real Cobra handler with the daemon-child marker so this exercises
	// accepted-receipt recovery across the same parent/child cache boundary.
	runReply := func(ctx context.Context, args []string, _ map[string]string, _ string, emit func(string, string) error) error {
		if len(args) < 3 {
			return errors.New("missing youtube reply command arguments")
		}
		child := newYouTubeReplyCmd()
		registerRootFlags(child)
		child.SetContext(withInvocation(ctx, testInvocationWithConfig(cfg)))
		var childOutput bytes.Buffer
		child.SetOut(&childOutput)
		if err := child.ParseFlags(args[2:]); err != nil {
			return fmt.Errorf("parse reply flags: %w", err)
		}
		if err := child.RunE(child, child.Flags().Args()); err != nil {
			return fmt.Errorf("run reply handler: %w", err)
		}
		if err := emit(cliStreamStdout, childOutput.String()); err != nil {
			return fmt.Errorf("emit reply output: %w", err)
		}
		return nil
	}
	forward := func(event api.CLIRunEvent) error {
		if event.Type == cliStreamStdout {
			_, err := output.WriteString(event.Data)
			if err != nil {
				return fmt.Errorf("capture reply command output: %w", err)
			}
		}
		return nil
	}
	err = adapter.runCLICommandWithRunner(t.Context(), api.CLIRunRequest{Args: []string{
		"youtube", "reply", strconv.FormatInt(parentID, 10), "--body", "--dry-run", "--confirm", preview.Token,
		"--verbose", "--log-level", "debug",
	}}, forward, runReply)
	require.NoError(err)
	var receipt youtube.ReplyReceipt
	require.NoError(json.Unmarshal(output.Bytes(), &receipt))
	assert.Equal("archived", receipt.State)
	require.Positive(receipt.MessageID)
	require.True(jobs.waitContext(t.Context()), "daemon parent must finish its queued cache refresh")

	st, err = store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	engine, err = openDaemonDuckDBEngine(t.Context(), cfg, st)
	require.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	rows, err = engine.QuerySQL(t.Context(), "SELECT COUNT(*) FROM messages")
	require.NoError(err)
	require.Len(rows.Rows, 1)
	assert.EqualValues(3, rows.Rows[0][0])
}
