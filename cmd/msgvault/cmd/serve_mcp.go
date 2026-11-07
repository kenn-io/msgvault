package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// nativeMCPTransport dispatches the private daemon client directly to the API
// router. A pipe bounds buffering and propagates consumer cancellation.
type nativeMCPTransport struct {
	handler http.Handler
}

const nativeMCPOrigin = "http://msgvault.internal"

func (t nativeMCPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	request := r.Clone(ctx)
	request.Host = "msgvault.internal"
	// Deliberately treat nested calls as same-host CLI requests and exempt them
	// from the per-IP rate limit. The outer /mcp request already passed owner-key
	// authentication and that rate limit with its real address.
	request.RemoteAddr = "127.0.0.1:0"
	reader, writer := io.Pipe()
	ready := make(chan *http.Response, 1)
	finished := make(chan struct{})
	response := &nativeMCPResponse{header: make(http.Header), writer: writer, ready: ready, request: request}
	go func() {
		defer close(finished)
		t.handler.ServeHTTP(response, request)
		response.WriteHeader(http.StatusOK)
		_ = writer.Close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = writer.CloseWithError(ctx.Err())
		case <-finished:
		}
	}()
	select {
	case result := <-ready:
		result.Body = &nativeMCPBody{PipeReader: reader, cancel: cancel, finished: finished}
		return result, nil
	case <-ctx.Done():
		cancel()
		_ = reader.Close()
		<-finished
		return nil, ctx.Err()
	}
}

type nativeMCPBody struct {
	*io.PipeReader

	cancel   context.CancelFunc
	finished <-chan struct{}
}

func (b *nativeMCPBody) Close() error {
	b.cancel()
	err := b.PipeReader.Close()
	<-b.finished
	return err
}

type nativeMCPResponse struct {
	header  http.Header
	writer  *io.PipeWriter
	ready   chan *http.Response
	request *http.Request
	once    sync.Once
}

func (w *nativeMCPResponse) Header() http.Header { return w.header }
func (w *nativeMCPResponse) WriteHeader(status int) {
	if status < 200 {
		return
	}
	w.once.Do(func() {
		w.ready <- &http.Response{StatusCode: status, Header: w.header.Clone(), Request: w.request, ContentLength: -1}
	})
}
func (w *nativeMCPResponse) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.writer.Write(p)
}
func (w *nativeMCPResponse) Flush() { w.WriteHeader(http.StatusOK) }

// startServeMCP discovers capabilities without delaying API or vector startup.
func startServeMCP(ctx context.Context, cmd *cobra.Command, server *api.Server, state *invocation) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ctx.Err() != nil {
			return
		}
		if err := installServeMCP(ctx, cmd, server, state); err != nil && ctx.Err() == nil {
			state.logger.Warn("native MCP unavailable", "error", err)
		}
	}()
	return done
}

func installServeMCP(ctx context.Context, cmd *cobra.Command, server *api.Server, state *invocation) error {
	key := state.cfg.Server.AuthenticationKey()
	// Keyless daemons retain their API policy but never expose native MCP.
	if key == "" {
		return nil
	}
	client, err := newDaemonCLIClient(context.WithoutCancel(ctx), daemonclient.Config{
		URL: nativeMCPOrigin, APIKey: key, AllowInsecure: true,
		HTTPClient: &http.Client{Transport: nativeMCPTransport{handler: server.Router()}},
	})
	if err != nil {
		return err
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	opts := daemonMCPServeOptions(discoveryCtx, client, state)
	if err := ctx.Err(); err != nil {
		_ = client.Close()
		return fmt.Errorf("initialize native MCP: %w", err)
	}
	read, closeRead := mcpserver.NewHTTPHandler(opts, mcpserver.HTTPOptions{})
	var write http.Handler
	closeWrite := func() {}
	allowWrites, _ := cmd.Flags().GetBool("mcp-allow-writes")
	if allowWrites {
		applyMCPWriteFlags(cmd, "mcp-", &opts)
		write, closeWrite = mcpserver.NewHTTPHandler(opts, mcpserver.HTTPOptions{AllowWrites: true})
	}
	cleanup := func() { closeRead(); closeWrite(); _ = client.Close() }
	if err := server.InstallMCPHandlers(read, write, cleanup); err != nil {
		cleanup()
		return err
	}
	return nil
}
