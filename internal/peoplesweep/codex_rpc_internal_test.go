package peoplesweep

import (
	"bytes"
	"context"
	"io"
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

func (p finishedCodexProcess) Stdin() io.WriteCloser { return p.stdin }
func (p finishedCodexProcess) Stdout() io.ReadCloser { return p.stdout }
func (p finishedCodexProcess) Stderr() io.ReadCloser { return p.stderr }
func (finishedCodexProcess) Wait() error             { return nil }
func (finishedCodexProcess) Kill() error             { return nil }

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
