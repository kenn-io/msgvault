package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	messagesync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type checkpointSignalAPI struct{ *gmail.MockAPI }

func (a checkpointSignalAPI) ListMessages(ctx context.Context, query, pageToken string) (*gmail.MessageListResponse, error) {
	if pageToken != "" {
		fmt.Println("checkpoint-ready")
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.MockAPI.ListMessages(ctx, query, pageToken)
}

func TestSyncResumeCheckpointChild(t *testing.T) {
	mode := os.Getenv("MSGVAULT_SYNC_CHECKPOINT_MODE")
	if mode == "" {
		return
	}
	ctx, cancel := signal.NotifyContext(t.Context(), os.Interrupt)
	defer cancel()
	s, err := store.OpenForTest(ctx, os.Getenv("MSGVAULT_SYNC_CHECKPOINT_DB"))
	require.NoError(t, err)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: "reader@example.test", MessagesTotal: 2, HistoryID: 1000}
	for _, id := range []string{"one", "two"} {
		mock.AddMessage(id, testemail.NewMessage().Bytes(), []string{"INBOX"})
	}
	mock.MessagePages = [][]string{{"one"}, {"two"}}
	opts := messagesync.DefaultOptions()
	for _, arg := range os.Args {
		if value, ok := strings.CutPrefix(arg, "--query="); ok {
			opts.Query = value
		}
	}
	var api gmail.API = mock
	if mode == "interrupt" {
		api = checkpointSignalAPI{mock}
	}
	summary, err := messagesync.New(api, s, opts).Full(ctx, "reader@example.test")
	if mode == "interrupt" {
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, s.Close())
		os.Exit(130)
	}
	require.NoError(t, err)
	fmt.Printf("resumed=%t token=%s\n", summary.WasResumed, summary.ResumedFromToken)
	require.NoError(t, s.Close())
}

func TestInterruptedSyncCopiedCommandResumesCheckpoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal and shell fixture")
	}
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	s := testutil.NewSQLiteTestStoreAt(t, dbPath)
	require.NoError(t, s.Close())
	root := &cobra.Command{Use: "msgvault"}
	root.PersistentFlags().String("test.run", "", "")
	require.NoError(t, root.PersistentFlags().Set("test.run", "^TestSyncResumeCheckpointChild$"))
	command := &cobra.Command{Use: "sync-full"}
	root.AddCommand(command)
	command.Flags().String("query", "", "")
	query := "subject:first\nsecond\x1b[31m\tend\n"
	require.NoError(t, command.ParseFlags([]string{"--query", query}))
	oldResolver := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return os.Args[0], nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = oldResolver })
	line, err := syncResumeCommand(command, nil)
	require.NoError(t, err)
	var hint bytes.Buffer
	command.SetErr(&hint)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err = runSyncFullWithHint(command, line, func() (bool, error) {
		child := exec.CommandContext(ctx, os.Args[0], "--test.run=^TestSyncResumeCheckpointChild$", "sync-full", "--query="+query)
		child.Env = append(os.Environ(), "MSGVAULT_SYNC_CHECKPOINT_MODE=interrupt", "MSGVAULT_SYNC_CHECKPOINT_DB="+dbPath)
		var stderr bytes.Buffer
		child.Stderr = &stderr
		stdout, err := child.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, child.Start())
		waited := false
		defer func() {
			if !waited {
				_ = child.Process.Kill()
				_ = child.Wait()
			}
		}()
		ready := make(chan bool, 1)
		go func() {
			scanner := bufio.NewScanner(stdout)
			for scanner.Scan() {
				if scanner.Text() == "checkpoint-ready" {
					ready <- true
					return
				}
			}
			ready <- false
		}()
		select {
		case ok := <-ready:
			require.True(t, ok, "child exited before checkpoint: %s", stderr.String())
		case <-ctx.Done():
			require.FailNow(t, "child did not reach checkpoint")
		}
		require.NoError(t, child.Process.Signal(os.Interrupt))
		waitErr := child.Wait()
		waited = true
		var exitErr *exec.ExitError
		require.ErrorAs(t, waitErr, &exitErr, "%s", stderr.String())
		require.Equal(t, 130, exitErr.ExitCode(), "%s", stderr.String())
		interrupted := false
		classified := classifyDaemonCLIWaitErr(waitErr, []string{"sync-full"}, func(stream, data string) error {
			interrupted = stream == "outcome" && data == "interrupted"
			return nil
		})
		return interrupted, classified
	})
	require.ErrorIs(t, err, ErrSyncInterrupted)
	assert.NotContains(t, hint.String(), "\x1b")
	copied := strings.TrimSpace(strings.SplitN(hint.String(), "\n", 2)[1])
	replay := exec.CommandContext(ctx, "sh", "-c", copied)
	replay.Env = append(os.Environ(), "MSGVAULT_SYNC_CHECKPOINT_MODE=resume", "MSGVAULT_SYNC_CHECKPOINT_DB="+dbPath)
	output, err := replay.CombinedOutput()
	require.NoError(t, err, "%s", output)
	assert.Contains(t, string(output), "resumed=true token=page_1")
}
