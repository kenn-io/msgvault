package peoplesweep_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

type pipeRPCProcess struct {
	stdin                  *io.PipeWriter
	stdout                 *io.PipeReader
	stderr                 *io.PipeReader
	serverIn               *io.PipeReader
	serverOut              *io.PipeWriter
	serverErr              *io.PipeWriter
	done                   chan struct{}
	killOnce               sync.Once
	kills                  atomic.Int64
	waits                  atomic.Int64
	waitErr                error
	killErr                error
	stdinClosed            atomic.Bool
	stdoutClosed           atomic.Bool
	stderrClosed           atomic.Bool
	killedBeforeStdinClose atomic.Bool
	waitStarted            chan struct{}
	waitStartOnce          sync.Once
	onKill                 func()
}

func newPipeRPCProcess(
	t *testing.T,
	serve func(*bufio.Reader, io.Writer, io.Writer) error,
) *pipeRPCProcess {
	t.Helper()
	serverIn, stdin := io.Pipe()
	stdout, serverOut := io.Pipe()
	stderr, serverErr := io.Pipe()
	process := &pipeRPCProcess{
		stdin: stdin, stdout: stdout, stderr: stderr,
		serverIn: serverIn, serverOut: serverOut, serverErr: serverErr,
		done: make(chan struct{}), waitStarted: make(chan struct{}),
	}
	go func() {
		process.waitErr = serve(bufio.NewReader(serverIn), serverOut, serverErr)
		_ = serverOut.Close()
		_ = serverErr.Close()
		_ = serverIn.Close()
		close(process.done)
	}()
	t.Cleanup(func() {
		_ = process.Stdin().Close()
		_ = process.Kill()
		_ = process.Stdout().Close()
		_ = process.Stderr().Close()
	})
	return process
}

type trackedPipeWriteCloser struct {
	writer *io.PipeWriter
	closed *atomic.Bool
}

type trackedPipeReadCloser struct {
	reader *io.PipeReader
	closed *atomic.Bool
}

func (r trackedPipeReadCloser) Read(data []byte) (int, error) { return r.reader.Read(data) }
func (r trackedPipeReadCloser) Close() error {
	r.closed.Store(true)
	return r.reader.Close()
}

func (w trackedPipeWriteCloser) Write(data []byte) (int, error) { return w.writer.Write(data) }
func (w trackedPipeWriteCloser) Close() error {
	w.closed.Store(true)
	return w.writer.Close()
}

func (p *pipeRPCProcess) Stdin() io.WriteCloser {
	return trackedPipeWriteCloser{writer: p.stdin, closed: &p.stdinClosed}
}
func (p *pipeRPCProcess) Stdout() io.ReadCloser {
	return trackedPipeReadCloser{reader: p.stdout, closed: &p.stdoutClosed}
}
func (p *pipeRPCProcess) Stderr() io.ReadCloser {
	return trackedPipeReadCloser{reader: p.stderr, closed: &p.stderrClosed}
}

func (p *pipeRPCProcess) Wait() error {
	p.waits.Add(1)
	p.waitStartOnce.Do(func() { close(p.waitStarted) })
	<-p.done
	return p.waitErr
}

func (p *pipeRPCProcess) Kill() error {
	p.killOnce.Do(func() {
		p.kills.Add(1)
		if !p.stdinClosed.Load() {
			p.killedBeforeStdinClose.Store(true)
		}
		if p.onKill != nil {
			p.onKill()
		}
		if p.killErr == nil {
			_ = p.serverIn.CloseWithError(context.Canceled)
			_ = p.serverOut.CloseWithError(context.Canceled)
			_ = p.serverErr.CloseWithError(context.Canceled)
		}
	})
	return p.killErr
}

func writeRPCFrame(w io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = w.Write(encoded)
	return err
}

func TestCodexRPCRejectsMalformedOrUnknownResponse(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "raw-response-must-stay-private"
	tests := []struct {
		name     string
		response string
	}{
		{name: "malformed", response: "{not-json:" + secret + "}\n"},
		{name: "unknown id", response: `{"id":99,"result":{"secret":"` + secret + `"}}` + "\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			process := newPipeRPCProcess(t, func(reader *bufio.Reader, stdout, _ io.Writer) error {
				if _, err := reader.ReadBytes('\n'); err != nil {
					return fmt.Errorf("read malformed-response request: %w", err)
				}
				_, err := io.WriteString(stdout, test.response)
				return err
			})
			client := peoplesweep.CodexRPCClient{Process: process}
			var result map[string]any
			err := client.Call(t.Context(), "test/method", map[string]any{"safe": true}, &result)
			must.Error(err)
			checks.NotContains(err.Error(), secret)
			checks.NotContains(err.Error(), "not-json")
		})
	}
}

func TestCodexRPCBoundsStdoutAndStderr(t *testing.T) {
	tests := []struct {
		name       string
		writeReply func(io.Writer, io.Writer) error
	}{
		{
			name: "stdout frame",
			writeReply: func(stdout, _ io.Writer) error {
				_, err := io.WriteString(stdout, strings.Repeat("stdout-secret", (1<<20)/13+2)+"\n")
				return err
			},
		},
		{
			name: "stderr stream",
			writeReply: func(stdout, stderr io.Writer) error {
				if _, err := io.WriteString(stderr, strings.Repeat("stderr-secret", (64<<10)/13+2)); err != nil {
					return err
				}
				// io.Pipe releases the oversized writer after copying the final
				// fragment but before the reader publishes the cap event. A
				// subsequent write cannot complete until that event is queued.
				if _, err := io.WriteString(stderr, "x"); err != nil {
					return err
				}
				return writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			process := newPipeRPCProcess(t, func(reader *bufio.Reader, stdout, stderr io.Writer) error {
				if _, err := reader.ReadBytes('\n'); err != nil {
					return fmt.Errorf("read bounded-stream request: %w", err)
				}
				return test.writeReply(stdout, stderr)
			})
			client := peoplesweep.CodexRPCClient{Process: process}
			var result map[string]any
			err := client.Call(t.Context(), "bounded/method", map[string]any{}, &result)
			must.Error(err)
			checks.NotContains(err.Error(), "stdout-secret")
			checks.NotContains(err.Error(), "stderr-secret")
		})
	}
}

func TestCodexRPCUsesMonotonicIDsAndSafeProviderErrors(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	const secret = "provider-error-payload"
	process := newPipeRPCProcess(t, func(reader *bufio.Reader, stdout, _ io.Writer) error {
		for wantID := int64(1); wantID <= 2; wantID++ {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return fmt.Errorf("read codex RPC request: %w", err)
			}
			var request struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(line, &request); err != nil {
				return err
			}
			if request.ID != wantID {
				return errors.New("non-monotonic request id")
			}
			if wantID == 2 {
				return writeRPCFrame(stdout, map[string]any{
					"id":    wantID,
					"error": map[string]any{"code": -32000, "message": secret, "data": secret},
				})
			}
			if err := writeRPCFrame(stdout, map[string]any{"id": wantID, "result": map[string]any{"ok": true}}); err != nil {
				return err
			}
		}
		return nil
	})
	client := peoplesweep.CodexRPCClient{Process: process}
	var first map[string]any
	must.NoError(client.Call(t.Context(), "first", nil, &first))
	var second map[string]any
	err := client.Call(t.Context(), "second", nil, &second)
	must.Error(err)
	checks.Contains(err.Error(), "second")
	checks.Contains(err.Error(), "-32000")
	checks.NotContains(err.Error(), secret)
}

func TestCodexRPCRejectsNotificationQueueOverflowSafely(t *testing.T) {
	const secret = "queued-notification-secret"
	tests := []struct {
		name          string
		notifications int
		payloadBytes  int
	}{
		{name: "count", notifications: 65, payloadBytes: 1},
		{name: "aggregate bytes", notifications: 2, payloadBytes: 600 << 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			process := newPipeRPCProcess(t, func(reader *bufio.Reader, stdout, _ io.Writer) error {
				if _, err := reader.ReadBytes('\n'); err != nil {
					return fmt.Errorf("read queue-overflow request: %w", err)
				}
				for range test.notifications {
					if err := writeRPCFrame(stdout, map[string]any{
						"method": "queued/event",
						"params": map[string]any{"payload": secret + strings.Repeat("x", test.payloadBytes)},
					}); err != nil {
						return err
					}
				}
				return writeRPCFrame(stdout, map[string]any{"id": 1, "result": map[string]any{}})
			})
			client := peoplesweep.CodexRPCClient{Process: process}
			var result map[string]any
			err := client.Call(t.Context(), "queue/test", nil, &result)
			must.Error(err)
			checks.Contains(err.Error(), "notification queue")
			checks.NotContains(err.Error(), secret)
		})
	}
}
