//go:build linux

package peoplesweep

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const codexProxySocketName = ".proxy.sock"

func defaultCodexServiceProxy() CodexServiceProxy { return newCodexHostServiceProxy(nil) }

// Pinned 0.156.0 uses the ChatGPT backend for subscription inference and the
// OpenAI OAuth issuer for login and refresh. Keep this list exact.
var codexProxyAuthorities = map[string]struct{}{
	"chatgpt.com:443":     {},
	"auth.openai.com:443": {},
}

type codexHostServiceProxy struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func newCodexHostServiceProxy(dial func(context.Context, string, string) (net.Conn, error)) CodexServiceProxy {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	return codexHostServiceProxy{dial: dial}
}

func (p codexHostServiceProxy) Attach(ctx context.Context, workRoot string) (CodexProxySession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(workRoot) || filepath.Clean(workRoot) != workRoot {
		return nil, errors.New("codex proxy requires an absolute work root")
	}
	info, err := os.Lstat(workRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(info) {
		return nil, errors.New("codex proxy requires a private daemon-owned work root")
	}
	socketPath := filepath.Join(workRoot, codexProxySocketName)
	if _, err := os.Lstat(socketPath); err == nil || !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("codex proxy socket path is occupied")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, errors.New("create codex service proxy socket")
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, errors.New("secure codex service proxy socket")
	}
	proxyCtx, cancel := context.WithCancel(ctx)
	session := &codexHostProxySession{
		path: socketPath, listener: listener, cancel: cancel,
		dial: p.dial, active: make(map[net.Conn]struct{}),
	}
	session.wg.Add(1)
	go session.accept(proxyCtx)
	go func() {
		<-proxyCtx.Done()
		_ = session.Close()
	}()
	return session, nil
}

type codexHostProxySession struct {
	path     string
	listener net.Listener
	cancel   context.CancelFunc
	dial     func(context.Context, string, string) (net.Conn, error)
	mu       sync.Mutex
	active   map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
	once     sync.Once
	closeErr error
}

func (s *codexHostProxySession) SocketPath() string { return s.path }

func (s *codexHostProxySession) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		s.closeErr = s.listener.Close()
		for conn := range s.active {
			_ = conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s.closeErr
}

func (s *codexHostProxySession) accept(ctx context.Context) {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if !s.track(conn) {
			_ = conn.Close()
			return
		}
		s.wg.Add(1)
		go s.serve(ctx, conn)
	}
}

func (s *codexHostProxySession) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.active[conn] = struct{}{}
	return true
}

func (s *codexHostProxySession) untrack(conn net.Conn) {
	s.mu.Lock()
	delete(s.active, conn)
	s.mu.Unlock()
	_ = conn.Close()
}

func (s *codexHostProxySession) serve(ctx context.Context, client net.Conn) {
	defer s.wg.Done()
	defer s.untrack(client)
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(client, 4096)
	authority, ok := readCodexConnect(reader)
	if !ok {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
		return
	}
	_ = client.SetReadDeadline(time.Time{})
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	upstream, err := s.dial(dialCtx, "tcp", authority)
	cancel()
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	if !s.track(upstream) {
		_ = upstream.Close()
		return
	}
	defer s.untrack(upstream)
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, reader)
		close(done)
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	_ = upstream.Close()
	<-done
}

func readCodexConnect(reader *bufio.Reader) (string, bool) {
	const maxHeaders = 8192
	total := 0
	line, ok := readCodexProxyLine(reader, &total, maxHeaders)
	if !ok {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(line, "\r\n"), " ")
	if len(parts) != 3 || parts[0] != "CONNECT" || parts[2] != "HTTP/1.1" {
		return "", false
	}
	authority := parts[1]
	if _, allowed := codexProxyAuthorities[authority]; !allowed {
		return "", false
	}
	hostSeen := false
	for {
		line, ok = readCodexProxyLine(reader, &total, maxHeaders)
		if !ok {
			return "", false
		}
		if line == "\r\n" {
			return authority, hostSeen
		}
		name, value, found := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
		if !found || strings.TrimSpace(name) == "" {
			return "", false
		}
		if strings.EqualFold(name, "Host") {
			if hostSeen || strings.TrimSpace(value) != authority {
				return "", false
			}
			hostSeen = true
		}
	}
}

func readCodexProxyLine(reader *bufio.Reader, total *int, maximum int) (string, bool) {
	line, err := reader.ReadSlice('\n')
	*total += len(line)
	if err != nil || *total > maximum || len(line) < 2 || line[len(line)-2] != '\r' {
		return "", false
	}
	return string(line), true
}
