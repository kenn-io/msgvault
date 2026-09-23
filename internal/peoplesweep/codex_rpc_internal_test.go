package peoplesweep

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stuckCodexStderr struct {
	release <-chan struct{}
}

func (s stuckCodexStderr) Read([]byte) (int, error) {
	<-s.release
	return 0, io.EOF
}

func (stuckCodexStderr) Close() error { return nil }

type finishedCodexProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
}

type countingExitCodexProcess struct {
	finishedCodexProcess

	kills atomic.Int64
}

type killedExitCodexProcess struct {
	finishedCodexProcess

	killed chan struct{}
	kills  atomic.Int64
}

type failedExitCodexProcess struct {
	countingExitCodexProcess
}

func (*failedExitCodexProcess) Wait() error { return errors.New("synthetic nonzero exit") }

func (p *killedExitCodexProcess) Wait() error {
	<-p.killed
	return errors.New("synthetic killed exit")
}

func (p *killedExitCodexProcess) Kill() error {
	p.kills.Add(1)
	close(p.killed)
	return nil
}

func TestCodexKilledAfterExitGraceRejectsSkippedAuthRefresh(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	child := &killedExitCodexProcess{killed: make(chan struct{})}
	child.finishedCodexProcess = finishedCodexProcess{
		stdin:  discardCodexWriteCloser{Writer: io.Discard},
		stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
	}
	var commits atomic.Int64
	process := &codexOwnedProcess{
		RPCProcess: child, workRoot: workRoot, childExited: make(chan struct{}), commitAuth: true,
		refreshCommit: func() error { commits.Add(1); return nil },
	}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())

	err := finishCodexProcess(t.Context(), process, client, false)
	requireChecks.ErrorIs(err, ErrCodexAuthRefreshUnsafe)
	assertChecks.Equal(int64(1), child.kills.Load())
	assertChecks.Zero(commits.Load())
	assertChecks.NoDirExists(workRoot)
}

func TestCodexNaturalNonzeroExitRejectsSkippedAuthRefresh(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	child := &failedExitCodexProcess{}
	child.finishedCodexProcess = finishedCodexProcess{
		stdin:  discardCodexWriteCloser{Writer: io.Discard},
		stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
	}
	var commits atomic.Int64
	process := &codexOwnedProcess{
		RPCProcess: child, workRoot: workRoot, childExited: make(chan struct{}), commitAuth: true,
		refreshCommit: func() error { commits.Add(1); return nil },
	}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())

	err := finishCodexProcess(t.Context(), process, client, false)
	requireChecks.ErrorIs(err, ErrCodexAuthRefreshUnsafe)
	assertChecks.Zero(child.kills.Load())
	assertChecks.Zero(commits.Load())
	assertChecks.NoDirExists(workRoot)
}

func TestCodexKilledInferenceRejectsSkippedAuthRefresh(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	child := finishedCodexProcess{
		stdin:  discardCodexWriteCloser{Writer: io.Discard},
		stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
	}
	var commits atomic.Int64
	process := &codexOwnedProcess{
		RPCProcess: child, workRoot: workRoot, childExited: make(chan struct{}), commitAuth: true,
		refreshCommit: func() error { commits.Add(1); return nil },
	}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())
	requireChecks.NoError(process.Kill())

	err := finishCodexProcess(t.Context(), process, client, false)
	requireChecks.ErrorIs(err, ErrCodexAuthRefreshUnsafe)
	assertChecks.Zero(commits.Load())
	assertChecks.NoDirExists(workRoot)
}

func (p *countingExitCodexProcess) Kill() error {
	p.kills.Add(1)
	return nil
}

func TestCodexCleanExitWaitsForAuthCommitBeforeRemovingWorkRoot(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	child := &countingExitCodexProcess{}
	child.finishedCodexProcess = finishedCodexProcess{
		stdin:  discardCodexWriteCloser{Writer: io.Discard},
		stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
	}
	process := &codexOwnedProcess{
		RPCProcess: child, workRoot: workRoot, childExited: make(chan struct{}), commitAuth: true,
		refreshCommit: func() error {
			time.Sleep(150 * time.Millisecond)
			return nil
		},
	}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())
	requireChecks.NoError(finishCodexProcess(t.Context(), process, client, false))
	assertChecks.Zero(child.kills.Load())
	assertChecks.NoDirExists(workRoot)
}

func (p finishedCodexProcess) Stdin() io.WriteCloser { return p.stdin }
func (p finishedCodexProcess) Stdout() io.ReadCloser { return p.stdout }
func (p finishedCodexProcess) Stderr() io.ReadCloser { return p.stderr }
func (finishedCodexProcess) Wait() error             { return nil }
func (finishedCodexProcess) Kill() error             { return nil }

type stuckKillCodexProcess struct {
	finishedCodexProcess

	release <-chan struct{}
}

func (p stuckKillCodexProcess) Wait() error {
	<-p.release
	return nil
}

func (stuckKillCodexProcess) Kill() error { return errors.New("synthetic kill failure") }

type discardCodexWriteCloser struct{ io.Writer }

func (discardCodexWriteCloser) Close() error { return nil }

func TestCodexRPCStderrEventPublicationNeverBlocks(t *testing.T) {
	client := CodexRPCClient{stderrEvent: make(chan error, 1)}
	client.stderrEvent <- errCodexStderrLimit
	done := make(chan struct{})
	go func() {
		client.publishStderrEvent(errCodexStderrRead)
		close(done)
	}()

	select {
	case <-done:
	case <-t.Context().Done():
		require.FailNow(t, "publishing a secondary stderr event blocked")
	}
	require.ErrorIs(t, <-client.stderrEvent, errCodexStderrLimit)
	require.NotErrorIs(t, client.checkStderr(), errCodexStderrRead)
}

func TestCodexCleanupBoundsStderrDrainAfterStreamClose(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	process := finishedCodexProcess{
		stdin:  discardCodexWriteCloser{Writer: io.Discard},
		stdout: io.NopCloser(bytes.NewReader(nil)),
		stderr: stuckCodexStderr{release: release},
	}
	client := &CodexRPCClient{Process: process}
	require.NoError(t, client.initialize())

	started := time.Now()
	err := finishCodexProcess(context.Background(), process, client, false)

	require.Error(t, err)
	assert.Less(t, time.Since(started), 500*time.Millisecond)
}

func TestCodexForceKillReportsAbandonedWaitAndRemovesAuthRoot(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	requireChecks.NoError(os.Mkdir(filepath.Join(workRoot, ".codex"), 0o700))
	requireChecks.NoError(os.WriteFile(filepath.Join(workRoot, ".codex", "auth.json"), []byte("SYNTHETIC_AUTH"), 0o600))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	process := &codexOwnedProcess{RPCProcess: stuckKillCodexProcess{
		finishedCodexProcess: finishedCodexProcess{
			stdin:  discardCodexWriteCloser{Writer: io.Discard},
			stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
		},
		release: release,
	}, workRoot: workRoot}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())

	err := finishCodexProcess(t.Context(), process, client, true)
	requireChecks.ErrorContains(err, "process termination failed")
	assertChecks.NoDirExists(workRoot)
	assertChecks.NotContains(err.Error(), "SYNTHETIC_AUTH")
}

func TestCodexForceKillReportsAuthRootRemovalFailure(t *testing.T) {
	assertChecks := assert.New(t)
	requireChecks := require.New(t)
	workRoot := t.TempDir()
	var attempts atomic.Int64
	process := &codexOwnedProcess{
		RPCProcess: finishedCodexProcess{
			stdin:  discardCodexWriteCloser{Writer: io.Discard},
			stdout: io.NopCloser(bytes.NewReader(nil)), stderr: io.NopCloser(bytes.NewReader(nil)),
		},
		workRoot: workRoot,
		removeRoot: func(string) error {
			attempts.Add(1)
			return errors.New("SYNTHETIC_PRIVATE_PATH")
		},
	}
	client := &CodexRPCClient{Process: process}
	requireChecks.NoError(client.initialize())

	err := finishCodexProcess(t.Context(), process, client, true)
	requireChecks.ErrorContains(err, "remove codex app-server work root")
	assertChecks.NotContains(err.Error(), "SYNTHETIC_PRIVATE_PATH")
	assertChecks.GreaterOrEqual(attempts.Load(), int64(2))
}

func TestCodexFinalReadsPinnedTotalUsage(t *testing.T) {
	assertChecks := assert.New(t)
	frames := []byte(`{"method":"item/completed","params":{"threadId":"thread-safe","turnId":"turn-safe","item":{"type":"agentMessage","phase":"final_answer","text":"{\"claims\":[]}"}}}` + "\n" +
		`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-safe","turnId":"turn-safe","tokenUsage":{"total":{"inputTokens":21,"outputTokens":4,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":25},"last":{"inputTokens":21,"outputTokens":4,"cachedInputTokens":0,"reasoningOutputTokens":0,"totalTokens":25}}}}` + "\n" +
		`{"method":"turn/completed","params":{"threadId":"thread-safe","turn":{"id":"turn-safe","status":"completed"}}}` + "\n")
	client := &CodexRPCClient{Process: finishedCodexProcess{
		stdin: discardCodexWriteCloser{Writer: io.Discard}, stdout: io.NopCloser(bytes.NewReader(frames)),
		stderr: io.NopCloser(bytes.NewReader(nil)),
	}}
	final, usage, known, err := readCodexFinal(t.Context(), client, "thread-safe", "turn-safe")
	require.NoError(t, err)
	assertChecks.JSONEq(`{"claims":[]}`, string(final))
	assertChecks.Equal(TokenUsage{InputTokens: 21, OutputTokens: 4}, usage)
	assertChecks.True(known)
}
