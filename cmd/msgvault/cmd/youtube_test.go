package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"testing"
)

func TestConfiguredYouTubeCredentialsMissingWithoutLeak(t *testing.T) {
	t.Setenv("FIXTURE_YT_CLIENT", "fixture-value")
	t.Setenv("FIXTURE_YT_SECRET", "")
	_, err := configuredYouTubeClient(t.Context(), config.YouTubeSource{Identifier: "fixture", ClientIDEnv: "FIXTURE_YT_CLIENT", ClientSecretEnv: "FIXTURE_YT_SECRET", TokenEnv: "FIXTURE_YT_REFRESH"})
	require.ErrorContains(t, err, "FIXTURE_YT_SECRET")
	assert.NotContains(t, err.Error(), "fixture-value")
}

func TestYouTubeNestedDaemonCommands(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	leaf, _, err := rootCmd.Find([]string{"source", "youtube", "sync"})
	require.NoError(err)
	require.NoError(leaf.Flags().Set("max-requests", "7"))
	t.Cleanup(func() {
		_ = leaf.Flags().Set("max-requests", "1000")
		leaf.Flags().Lookup("max-requests").Changed = false
	})
	forwarded, err := daemonCLIArgsFromCobra(leaf, []string{"example"})
	require.NoError(err)
	assert.Equal([]string{"source", "youtube", "sync", "--max-requests=7", "example"}, forwarded)
}

func TestYouTubeScheduledJobRegistration(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	sched := scheduler.New(nil)
	t.Cleanup(func() { <-sched.Stop().Done() })
	profile := config.YouTubeSource{ChannelID: "UCfixture", Schedule: "0 * * * *"}
	require.NoError(registerScheduledYouTubeJob(sched, profile, nil, func(context.Context) error { return nil }))
	assert.True(sched.IsJobScheduled("youtube:UCfixture"))
}

func TestRunServeWarnsAboutYouTubeWithoutSchedule(t *testing.T) {
	require := require.New(t)
	clearServerKeyEnvironment(t)
	t.Setenv("MSGVAULT_REMOTE_URL", "")
	home := t.TempDir()
	port := freeTCPPort(t)
	path := filepath.Join(home, "config.toml")
	text := fmt.Sprintf("[server]\nbind_addr='127.0.0.1'\napi_port=%d\ndaemon_auto_start=false\ndaemon_auto_restart='never'\n[analytics]\nengine='sql'\n[[youtube]]\nidentifier='fixture'\nchannel_id='UCfixture'\ntoken_env='FIXTURE_REFRESH'\nclient_id_env='FIXTURE_CLIENT'\nclient_secret_env='FIXTURE_SECRET'\nenabled=true\n", port)
	require.NoError(os.WriteFile(path, []byte(text), 0o600))
	cfg, err := config.Load(path, home)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	command := &cobra.Command{Use: "serve"}
	command.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	var logs bytes.Buffer
	invocationFromCommand(command).logger = slog.New(slog.NewTextHandler(&logs, nil))
	done := make(chan error, 1)
	go func() { done <- runServe(command, nil) }()
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			select {
			case <-done:
			case <-time.After(serveLifecycleTestTimeout):
				assert.Fail(t, "daemon did not stop")
			}
		}
	})
	waitForServeHealthBounded(t, port, done)
	cancel()
	select {
	case err := <-done:
		finished = true
		require.NoError(err)
	case <-time.After(serveLifecycleTestTimeout):
		require.FailNow("daemon did not stop")
	}
	require.Contains(logs.String(), "YouTube profile is enabled but has no schedule")
	require.Contains(logs.String(), "profile=fixture")
}
