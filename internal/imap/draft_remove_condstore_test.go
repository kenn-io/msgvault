package imap

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const conditionalDraftRaw = "From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <conditional@example.com>\r\n\r\nConditional draft\r\n"

type conditionalIMAPServer struct {
	addr          string
	storeCommands chan string
	fetchStarted  chan struct{}
	expunged      chan struct{}
}

func startConditionalIMAPServer(t *testing.T, blockFetch bool) *conditionalIMAPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &conditionalIMAPServer{
		addr:          listener.Addr().String(),
		storeCommands: make(chan string, 1),
		fetchStarted:  make(chan struct{}),
		expunged:      make(chan struct{}, 1),
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		write := func(format string, args ...any) bool {
			if _, err := fmt.Fprintf(writer, format, args...); err != nil {
				return false
			}
			return writer.Flush() == nil
		}
		if !write("* OK [CAPABILITY IMAP4rev1 UIDPLUS CONDSTORE] ready\r\n") {
			return
		}
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			line = strings.TrimSpace(line)
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			tag := fields[0]
			upper := strings.ToUpper(line)
			switch {
			case strings.Contains(upper, " LOGIN "):
				if !write("%s OK LOGIN completed\r\n", tag) {
					return
				}
			case strings.Contains(upper, " CAPABILITY"):
				if !write("* CAPABILITY IMAP4rev1 UIDPLUS CONDSTORE\r\n%s OK CAPABILITY completed\r\n", tag) {
					return
				}
			case strings.Contains(upper, " SELECT "):
				if !write("* 1 EXISTS\r\n* OK [UIDVALIDITY 7]\r\n%s OK [READ-WRITE] SELECT completed\r\n", tag) {
					return
				}
			case strings.Contains(upper, "UID FETCH"):
				select {
				case <-server.fetchStarted:
				default:
					close(server.fetchStarted)
				}
				if blockFetch {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				if !write("* 1 FETCH (UID 1 FLAGS (\\Draft) MODSEQ (7) BODY[] {%d}\r\n%s)\r\n%s OK FETCH completed\r\n", len(conditionalDraftRaw), conditionalDraftRaw, tag) {
					return
				}
			case strings.Contains(upper, "UID STORE"):
				server.storeCommands <- line
				if !write("* 1 FETCH (UID 1 FLAGS (\\Draft \\Deleted))\r\n%s OK STORE completed\r\n", tag) {
					return
				}
			case strings.Contains(upper, "UID EXPUNGE"):
				server.expunged <- struct{}{}
				if !write("* 1 EXPUNGE\r\n%s OK EXPUNGE completed\r\n", tag) {
					return
				}
			case strings.Contains(upper, " LOGOUT"):
				_ = write("* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
				return
			}
		}
	}()
	return server
}

func TestRemoveDraftUsesConditionalStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := startConditionalIMAPServer(t, false)
	client := newDraftTestClient(t, server.addr)
	t.Cleanup(func() { _ = client.Close() })
	target := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: 7,
		UID:         1,
		RawSHA256:   sha256.Sum256([]byte(conditionalDraftRaw)),
	}

	result, err := client.RemoveDraft(context.Background(), target)
	require.NoError(err)
	assert.Equal(DraftRemotePresent, result.State)
	assert.Contains(<-server.storeCommands, "UNCHANGEDSINCE 7")
	<-server.expunged
}

func TestRemoveDraftCancellationClosesFetchBeforeMutation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server := startConditionalIMAPServer(t, true)
	client := newDraftTestClient(t, server.addr)
	t.Cleanup(func() { _ = client.Close() })
	target := DraftTarget{
		Mailbox:     "Drafts",
		UIDValidity: 7,
		UID:         1,
		RawSHA256:   sha256.Sum256([]byte(conditionalDraftRaw)),
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.RemoveDraft(ctx, target)
		done <- err
	}()
	<-server.fetchStarted
	cancel()
	err := <-done
	require.Error(err)
	var appendErr *DraftAppendError
	require.ErrorAs(err, &appendErr)
	assert.Equal("cancelled", appendErr.Code)
	select {
	case <-server.storeCommands:
		assert.Fail("cancellation reached STORE")
	default:
	}
	select {
	case <-server.expunged:
		assert.Fail("cancellation reached EXPUNGE")
	default:
	}
	assert.ErrorIs(err, context.Canceled)
}
