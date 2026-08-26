package peoplesweep

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

const (
	defaultCodexMaxFrameBytes          = 1 << 20
	defaultCodexMaxStderrBytes         = 64 << 10
	defaultCodexMaxQueuedNotifications = 64
)

var codexRPCMethodPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_./-]{0,127}$`)

var (
	errCodexStderrLimit = errors.New("codex app-server stderr limit exceeded")
	errCodexStderrRead  = errors.New("read codex app-server stderr")
)

// CodexRPCClient is a sequential, bounded JSONL client for one App Server
// process. App Server's JSON-RPC 2.0 wire format intentionally omits the
// jsonrpc member.
type CodexRPCClient struct {
	Process        RPCProcess
	MaxFrameBytes  int
	MaxStderrBytes int

	mu                sync.Mutex
	once              sync.Once
	initErr           error
	stdin             io.WriteCloser
	stdout            io.ReadCloser
	stderr            io.ReadCloser
	reader            *bufio.Reader
	nextID            int64
	stderrEvent       chan error
	stderrDone        chan struct{}
	stderrErr         error
	notifications     []codexRPCNotification
	notificationBytes int
	notificationErr   error
}

type codexRPCNotification struct {
	method string
	params json.RawMessage
	size   int
}

type codexRPCRequest struct {
	Method string `json:"method"`
	ID     int64  `json:"id"`
	Params any    `json:"params"`
}

type codexRPCEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

func (c *CodexRPCClient) initialize() error {
	c.once.Do(func() {
		if c.Process == nil {
			c.initErr = errors.New("codex app-server process is required")
			return
		}
		if c.MaxFrameBytes == 0 {
			c.MaxFrameBytes = defaultCodexMaxFrameBytes
		}
		if c.MaxStderrBytes == 0 {
			c.MaxStderrBytes = defaultCodexMaxStderrBytes
		}
		if c.MaxFrameBytes <= 0 || c.MaxStderrBytes <= 0 {
			c.initErr = errors.New("codex app-server stream limits must be positive")
			return
		}
		c.stdin = c.Process.Stdin()
		c.stdout = c.Process.Stdout()
		c.stderr = c.Process.Stderr()
		if c.stdin == nil || c.stdout == nil || c.stderr == nil {
			c.initErr = errors.New("codex app-server process streams are incomplete")
			return
		}
		c.reader = bufio.NewReaderSize(c.stdout, 32<<10)
		c.stderrEvent = make(chan error, 1)
		c.stderrDone = make(chan struct{})
		go c.drainStderr()
	})
	return c.initErr
}

func (c *CodexRPCClient) drainStderr() {
	defer close(c.stderrDone)
	buffer := make([]byte, 32<<10)
	total := 0
	reported := false
	for {
		n, err := c.stderr.Read(buffer)
		total += n
		if total > c.MaxStderrBytes && !reported {
			reported = true
			c.publishStderrEvent(errCodexStderrLimit)
		}
		if err != nil {
			if !reported && !errors.Is(err, io.EOF) {
				c.publishStderrEvent(errCodexStderrRead)
			}
			return
		}
	}
}

func (c *CodexRPCClient) publishStderrEvent(event error) {
	select {
	case c.stderrEvent <- event:
	default:
	}
}

// Call sends one request with the next monotonic ID and waits for its exact
// matching response. Interleaved notifications are retained in a bounded FIFO
// for the operation consumer.
func (c *CodexRPCClient) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initialize(); err != nil {
		return err
	}
	if !codexRPCMethodPattern.MatchString(method) {
		return errors.New("invalid codex app-server RPC method")
	}
	c.nextID++
	frame, err := json.Marshal(codexRPCRequest{Method: method, ID: c.nextID, Params: params})
	if err != nil {
		return fmt.Errorf("encode codex app-server %s request", method)
	}
	frame = append(frame, '\n')
	return c.callFrameLocked(ctx, method, c.nextID, frame, result)
}

// Notify sends one bounded notification. It does not consume a request ID.
func (c *CodexRPCClient) Notify(ctx context.Context, method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initialize(); err != nil {
		return err
	}
	if !codexRPCMethodPattern.MatchString(method) {
		return errors.New("invalid codex app-server RPC method")
	}
	frame, err := json.Marshal(struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}{Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("encode codex app-server %s notification", method)
	}
	frame = append(frame, '\n')
	return c.writeFrame(ctx, frame)
}

func (c *CodexRPCClient) callPrepared(
	ctx context.Context,
	frame []byte,
	result any,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initialize(); err != nil {
		return err
	}
	var request codexRPCRequest
	if err := decodeSingleJSON(frame, &request); err != nil ||
		!codexRPCMethodPattern.MatchString(request.Method) || request.ID != c.nextID+1 {
		return errors.New("prepared codex app-server request frame is invalid")
	}
	if len(frame) == 0 || frame[len(frame)-1] != '\n' {
		return errors.New("prepared codex app-server request is not a JSONL frame")
	}
	c.nextID = request.ID
	return c.callFrameLocked(ctx, request.Method, request.ID, frame, result)
}

func (c *CodexRPCClient) callFrameLocked(
	ctx context.Context,
	method string,
	id int64,
	frame []byte,
	result any,
) error {
	if len(frame) > c.MaxFrameBytes {
		return errors.New("codex app-server request frame limit exceeded")
	}
	if err := c.writeFrame(ctx, frame); err != nil {
		return err
	}
	for {
		raw, err := c.readFrame(ctx)
		if err != nil {
			return err
		}
		var envelope codexRPCEnvelope
		if err := decodeSingleJSON(raw, &envelope); err != nil {
			return errors.New("codex app-server returned a malformed response frame")
		}
		if len(envelope.ID) == 0 {
			if !codexRPCMethodPattern.MatchString(envelope.Method) {
				return errors.New("codex app-server returned a malformed response envelope")
			}
			if err := c.enqueueNotification(envelope.Method, envelope.Params, len(raw)); err != nil {
				return err
			}
			continue
		}
		var responseID int64
		if err := decodeSingleJSON(envelope.ID, &responseID); err != nil {
			return errors.New("codex app-server returned a malformed response ID")
		}
		if responseID != id {
			return errors.New("codex app-server returned an unknown response ID")
		}
		if envelope.Error != nil {
			return fmt.Errorf("codex app-server %s failed with code %d", method, envelope.Error.Code)
		}
		if len(envelope.Result) == 0 {
			return errors.New("codex app-server response is missing a result")
		}
		if result == nil {
			return nil
		}
		if err := decodeSingleJSON(envelope.Result, result); err != nil {
			return fmt.Errorf("decode codex app-server %s result", method)
		}
		return c.checkStderr()
	}
}

func (c *CodexRPCClient) nextNotification(
	ctx context.Context,
) (string, json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.initialize(); err != nil {
		return "", nil, err
	}
	if c.notificationErr != nil {
		return "", nil, c.notificationErr
	}
	if len(c.notifications) > 0 {
		notification := c.notifications[0]
		c.notifications[0] = codexRPCNotification{}
		c.notifications = c.notifications[1:]
		c.notificationBytes -= notification.size
		return notification.method, append(json.RawMessage(nil), notification.params...), c.checkStderr()
	}
	raw, err := c.readFrame(ctx)
	if err != nil {
		return "", nil, err
	}
	var envelope codexRPCEnvelope
	if err := decodeSingleJSON(raw, &envelope); err != nil {
		return "", nil, errors.New("codex app-server returned a malformed notification frame")
	}
	if len(envelope.ID) != 0 {
		return "", nil, errors.New("codex app-server returned an unknown response ID")
	}
	if !codexRPCMethodPattern.MatchString(envelope.Method) {
		return "", nil, errors.New("codex app-server returned a malformed notification method")
	}
	return envelope.Method, append(json.RawMessage(nil), envelope.Params...), c.checkStderr()
}

func (c *CodexRPCClient) enqueueNotification(method string, params json.RawMessage, frameBytes int) error {
	if c.notificationErr != nil {
		return c.notificationErr
	}
	if len(c.notifications) >= defaultCodexMaxQueuedNotifications ||
		frameBytes <= 0 || c.notificationBytes > c.MaxFrameBytes-frameBytes {
		c.notificationErr = errors.New("codex app-server notification queue limit exceeded")
		return c.notificationErr
	}
	c.notifications = append(c.notifications, codexRPCNotification{
		method: method, params: append(json.RawMessage(nil), params...), size: frameBytes,
	})
	c.notificationBytes += frameBytes
	return nil
}

func (c *CodexRPCClient) writeFrame(ctx context.Context, frame []byte) error {
	writeDone := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(frame)
		writeDone <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-writeDone:
		if err != nil {
			return errors.New("write codex app-server request")
		}
		return c.checkStderr()
	case err := <-c.stderrEvent:
		if err != nil {
			c.stderrErr = err
			return err
		}
		return nil
	}
}

func (c *CodexRPCClient) readFrame(ctx context.Context) ([]byte, error) {
	type readResult struct {
		frame []byte
		err   error
	}
	readDone := make(chan readResult, 1)
	go func() {
		frame, err := c.readFrameBlocking()
		readDone <- readResult{frame: frame, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case event := <-c.stderrEvent:
		if event != nil {
			c.stderrErr = event
			return nil, event
		}
		return nil, errors.New("codex app-server stderr stream ended unexpectedly")
	case got := <-readDone:
		if got.err != nil {
			return nil, got.err
		}
		return got.frame, c.checkStderr()
	}
}

func (c *CodexRPCClient) readFrameBlocking() ([]byte, error) {
	frame := make([]byte, 0, 1024)
	for {
		fragment, prefix, err := c.reader.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("codex app-server stdout closed before a complete frame")
			}
			return nil, errors.New("read codex app-server stdout")
		}
		if len(frame)+len(fragment) > c.MaxFrameBytes {
			return nil, errors.New("codex app-server stdout frame limit exceeded")
		}
		frame = append(frame, fragment...)
		if !prefix {
			if len(frame) == 0 {
				return nil, errors.New("codex app-server returned an empty response frame")
			}
			return frame, nil
		}
	}
}

func (c *CodexRPCClient) checkStderr() error {
	if c.stderrErr != nil {
		return c.stderrErr
	}
	select {
	case event := <-c.stderrEvent:
		if event != nil {
			c.stderrErr = event
		}
	default:
	}
	return c.stderrErr
}

func (c *CodexRPCClient) finalStderrError() error {
	if c.stderrDone == nil {
		return nil
	}
	<-c.stderrDone
	return c.checkStderr()
}

func (c *CodexRPCClient) waitForStderr(ctx context.Context, grace time.Duration) (bool, error) {
	if c == nil || c.stderrDone == nil {
		return true, nil
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-c.stderrDone:
		return true, c.checkStderr()
	case <-ctx.Done():
		return false, nil
	case <-timer.C:
		return false, nil
	}
}
