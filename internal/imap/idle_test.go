package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	"hegel.dev/go/hegel"
)

// This wire peer checks real EXAMINE/IDLE/DONE traffic. It deliberately never
// implements a writable mailbox or an ingestion command.
type idleWire struct {
	address  string
	commands chan string
	idling   chan struct{}
	updates  chan string
	done     chan struct{}
	exited   chan struct{}
	stop     func()
}

func startIdleWire(t *testing.T, capability, mode string) *idleWire {
	t.Helper()
	requirements := require.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	requirements.NoError(err)
	w := &idleWire{address: listener.Addr().String(), commands: make(chan string, 64), idling: make(chan struct{}), updates: make(chan string, 16), done: make(chan struct{}), exited: make(chan struct{})}
	w.stop = sync.OnceFunc(func() { _ = listener.Close(); close(w.done); <-w.exited })
	t.Cleanup(w.stop)
	caps := "IMAP4rev1"
	if capability != "" {
		caps += " " + capability
	}
	go func() {
		defer close(w.exited)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		go func() { <-w.done; _ = conn.Close() }()
		_, _ = fmt.Fprintf(conn, "* OK [CAPABILITY %s] synthetic\r\n", caps)
		scan := bufio.NewScanner(conn)
		for scan.Scan() {
			line := scan.Text()
			w.commands <- line
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return
			}
			tag, command := fields[0], strings.ToUpper(fields[1])
			switch command {
			case "CAPABILITY":
				if mode == "capability-disconnect" {
					return
				}
				if mode == "capability-unavailable" {
					_, _ = fmt.Fprintf(conn, "%s NO [UNAVAILABLE] synthetic temporary failure\r\n", tag)
					continue
				}
				_, _ = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK caps\r\n", caps, tag)
			case "LOGIN":
				if mode == "login-unavailable" {
					_, _ = fmt.Fprintf(conn, "%s NO [UNAVAILABLE] synthetic temporary failure\r\n", tag)
					continue
				}
				if mode == "bad-auth" {
					_, _ = fmt.Fprintf(conn, "%s NO [AUTHENTICATIONFAILED] synthetic-private-provider-text\r\n", tag)
				} else {
					_, _ = fmt.Fprintf(conn, "%s OK login\r\n", tag)
				}
			case "EXAMINE":
				if mode == "examine-unavailable" {
					_, _ = fmt.Fprintf(conn, "%s NO [UNAVAILABLE] synthetic temporary failure\r\n", tag)
					continue
				}
				_, _ = fmt.Fprintf(conn, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] synthetic\r\n%s OK [READ-ONLY] examined\r\n", tag)
			case "IDLE":
				if mode == "idle-serverbug" {
					_, _ = fmt.Fprintf(conn, "%s NO [SERVERBUG] synthetic temporary failure\r\n", tag)
					continue
				}
				select {
				case <-w.idling:
				default:
					close(w.idling)
				}
				if mode == "stall-continuation" {
					<-w.done
					return
				}
				_, _ = io.WriteString(conn, "+ idling\r\n")
				idleDone := make(chan struct{})
				go func() {
					for {
						select {
						case update := <-w.updates:
							_, _ = io.WriteString(conn, update+"\r\n")
						case <-idleDone:
							return
						case <-w.done:
							return
						}
					}
				}()
				if !scan.Scan() {
					close(idleDone)
					return
				}
				w.commands <- scan.Text()
				close(idleDone)
				if mode == "stall-done" {
					<-w.done
					return
				}
				if mode == "done-serverbug" {
					_, _ = fmt.Fprintf(conn, "%s NO [SERVERBUG] synthetic temporary failure\r\n", tag)
					continue
				}
				_, _ = fmt.Fprintf(conn, "%s OK idle complete\r\n", tag)
			default:
				_, _ = fmt.Fprintf(conn, "%s BAD unexpected command\r\n", tag)
			}
		}
	}()
	return w
}

func (w *idleWire) client(t *testing.T) *Client {
	t.Helper()
	requirements := require.New(t)
	host, portText, err := net.SplitHostPort(w.address)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	return NewClient(&Config{Host: host, Port: port, Username: "owner@example.test"}, "synthetic-password", WithLogger(slog.New(slog.DiscardHandler)))
}

func TestWatchInboxReadOnlyHints(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	w := startIdleWire(t, "IDLE", "")
	client := w.client(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wake := make(chan struct{}, 8)
	result := make(chan error, 1)
	go func() { result <- client.WatchInbox(ctx, func() { wake <- struct{}{} }) }()
	<-w.idling
	<-wake // listener is established before initial catchup
	for _, update := range []string{"* 2 EXISTS", "* 1 FETCH (FLAGS (\\Seen))", "* 1 EXPUNGE"} {
		w.updates <- update
		<-wake
	}
	cancel()
	requirements.ErrorIs(<-result, context.Canceled)
	requirements.NoError(client.Close(), "dedicated watcher must hard-close before ordinary Close")
	close(w.commands)
	var commands []string
	for line := range w.commands {
		commands = append(commands, line)
	}
	assertions.Condition(func() bool {
		for _, line := range commands {
			if strings.Contains(line, " EXAMINE ") {
				return true
			}
		}
		return false
	})
	assertions.Contains(commands, "DONE")
	for _, line := range commands {
		assertions.NotContains(line, " SELECT ")
		assertions.NotContains(line, " STORE ")
		assertions.NotContains(line, " LOGOUT")
	}
}

func TestWatchInboxUnsupported(t *testing.T) {
	requirements := require.New(t)
	w := startIdleWire(t, "", "")
	client := w.client(t)
	err := client.WatchInbox(t.Context(), func() { requirements.FailNow("unsupported listener must not announce ready") })
	requirements.ErrorIs(err, ErrIdleUnsupported)
	requirements.NoError(client.Close())
}

func TestWatchInboxCanceledProtocolStall(t *testing.T) {
	for _, mode := range []string{"stall-continuation", "stall-done"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			w := startIdleWire(t, "IDLE", mode)
			client := w.client(t)
			client.idleDoneTimeout = 20 * time.Millisecond //nolint:kennlint // deadlinetest: asserts timeout of DONE against a real stalled TCP peer.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			ready := make(chan struct{}, 1)
			go func() { result <- client.WatchInbox(ctx, func() { ready <- struct{}{} }) }()
			<-w.idling
			if mode == "stall-done" {
				<-ready
			}
			cancel()
			requirements.ErrorIs(<-result, context.Canceled)
			requirements.NoError(client.Close())
		})
	}
}

func TestWatchInboxAuthenticationIsTerminalAndRedacted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	w := startIdleWire(t, "IDLE", "bad-auth")
	client := w.client(t)
	err := client.WatchInbox(t.Context(), func() { requirements.FailNow("failed authentication emitted hint") })
	requirements.ErrorIs(err, ErrIdleAuthentication)
	assertions.NotContains(err.Error(), "synthetic-private-provider-text")
	requirements.NoError(client.Close())
}

func TestWatchInboxTransientProtocolFailuresRemainRetryable(t *testing.T) {
	for _, mode := range []string{"login-unavailable", "capability-disconnect", "capability-unavailable", "examine-unavailable", "idle-serverbug", "done-serverbug"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			w := startIdleWire(t, "IDLE", mode)
			client := w.client(t)
			client.idleRenewInterval = 20 * time.Millisecond // Real wire peer exercises the renewal/DONE transition.
			err := client.WatchInbox(t.Context(), func() {})
			requirements.ErrorIs(err, ErrIdleDisconnected)
			assertions.NotContains(err.Error(), "synthetic temporary failure")
			requirements.NoError(client.Close())
		})
	}
}

func TestWatchInboxGreetingRetryExhaustionRemainsRetryable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	addr, accepted := startRetryMemServer(t, "plaintext", func(conn net.Conn, _ int64) net.Conn { return &retryDropConn{Conn: conn} }, nil)
	client := newRetryClient(t, addr, "plaintext")
	client.connectRetryInitialInterval = time.Nanosecond
	err := client.WatchInbox(t.Context(), func() { requirements.FailNow("EOF before greeting cannot establish IDLE") })
	requirements.ErrorIs(err, ErrIdleDisconnected)
	assertions.Equal(int64(4), accepted.accepted.Load())
}

func TestWatchInboxTokenEndpointFailureClass(t *testing.T) {
	for _, status := range []int{400, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			w := startIdleWire(t, "IDLE", "")
			client := w.client(t)
			client.config.AuthMethod = AuthXOAuth2
			client.tokenSource = func(context.Context) (string, error) {
				return "", &oauth2.RetrieveError{Response: &http.Response{StatusCode: status}, Body: []byte("synthetic-private-provider-text")}
			}
			err := client.WatchInbox(t.Context(), func() {})
			want := ErrIdleDisconnected
			if status == 400 {
				want = ErrIdleAuthentication
			}
			requirements.ErrorIs(err, want)
			assertions.NotContains(err.Error(), "synthetic-private-provider-text")
		})
	}
}

func TestWatchInboxCancellationPhasesProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		requirements := require.New(ht)
		phase := hegel.Draw(ht, hegel.Integers(0, 2))
		hints := min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 16)
		mode := ""
		if phase == 1 {
			mode = "stall-continuation"
		}
		wire := startIdleWire(t, "IDLE", mode)
		client := wire.client(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer func() { cancel(); wire.stop() }()
		if phase == 0 {
			cancel()
			err := client.WatchInbox(ctx, func() { requirements.FailNow("canceled watcher emitted hint") })
			requirements.ErrorIs(err, context.Canceled)
			return
		}
		wake, result := make(chan struct{}, 2), make(chan error, 1)
		go func() { result <- client.WatchInbox(ctx, func() { wake <- struct{}{} }) }()
		<-wire.idling
		if phase == 2 {
			<-wake
			for range hints {
				wire.updates <- "* 2 EXISTS"
				<-wake
			}
		}
		cancel()
		requirements.ErrorIs(<-result, context.Canceled)
		requirements.NoError(client.Close())
	}, hegel.WithTestCases(1000))
}
