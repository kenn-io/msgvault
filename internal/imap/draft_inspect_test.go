package imap

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

const testDraftRaw = "From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft-test@example.com>\r\n\r\nTest draft body\r\n"

func newDraftTestClient(t *testing.T, addr string) *Client {
	requirements := require.New(t)

	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	requirements.NoError(err)
	port, err := strconv.Atoi(portText)
	requirements.NoError(err)
	return NewClient(&Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}, testutil.IMAPTestPassword)
}

func startStalledDraftIMAPServer(t *testing.T, phase string) (string, <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	reached := make(chan struct{})
	release := make(chan struct{})
	var reachedOnce sync.Once
	signalReached := func() { reachedOnce.Do(func() { close(reached) }) }
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		capabilityCount := 0
		if _, err := fmt.Fprint(writer, "* OK IMAP4rev1 ready\r\n"); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			tag := fields[0]
			upper := strings.ToUpper(line)
			if strings.Contains(upper, " LOGIN ") {
				_, _ = fmt.Fprintf(writer, "%s OK LOGIN completed\r\n", tag)
			} else if strings.Contains(upper, " CAPABILITY") {
				capabilityCount++
				if phase == "capability" && capabilityCount == 2 {
					signalReached()
					<-release
					return
				}
				_, _ = fmt.Fprintf(writer, "* CAPABILITY IMAP4rev1 UIDPLUS\r\n%s OK CAPABILITY completed\r\n", tag)
			} else if strings.Contains(upper, " SELECT ") {
				if phase == "select" {
					signalReached()
					<-release
					return
				}
				_, _ = fmt.Fprintf(writer, "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)\r\n* 0 EXISTS\r\n* 0 RECENT\r\n* OK [UIDVALIDITY 123] UIDs valid\r\n* OK [UIDNEXT 1] Predicted next UID\r\n%s OK [READ-WRITE] SELECT completed\r\n", tag)
			} else if strings.Contains(upper, " UID FETCH ") {
				if phase == "fetch" {
					signalReached()
					<-release
					return
				}
				_, _ = fmt.Fprintf(writer, "%s OK FETCH completed\r\n", tag)
			} else if strings.Contains(upper, " LOGOUT") {
				if phase == "logout" {
					signalReached()
					<-release
					return
				}
				_, _ = fmt.Fprintf(writer, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
			}
			if err := writer.Flush(); err != nil {
				return
			}
		}
	}()
	return listener.Addr().String(), reached
}

func TestInspectDraftCancellationInterruptsBlockingProtocolPhases(t *testing.T) {
	for _, phase := range []string{"capability", "select", "fetch"} {
		t.Run(phase, func(t *testing.T) {
			addr, reached := startStalledDraftIMAPServer(t, phase)
			client := newDraftTestClient(t, addr)
			ctx, cancel := context.WithCancel(t.Context())
			result := make(chan error, 1)
			go func() {
				_, err := client.InspectDraft(ctx, DraftTarget{Mailbox: "Drafts", UIDValidity: 123, UID: 1})
				result <- err
			}()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatal("IMAP server did not reach stalled phase")
			}
			cancel()
			select {
			case err := <-result:
				require.Error(t, err)
				var appendErr *DraftAppendError
				require.ErrorAs(t, err, &appendErr)
				require.Equal(t, DraftStateCancelled, appendErr.State)
			case <-time.After(2 * time.Second):
				t.Fatal("draft inspection did not stop after cancellation")
			}
		})
	}
}

func TestCloseContextCancellationInterruptsBlockingLogout(t *testing.T) {
	addr, reached := startStalledDraftIMAPServer(t, "logout")
	client := newDraftTestClient(t, addr)
	_, err := client.InspectDraft(t.Context(), DraftTarget{Mailbox: "Drafts", UIDValidity: 123, UID: 1})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- client.CloseContext(ctx) }()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("IMAP server did not reach stalled logout")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("context-aware logout did not stop after cancellation")
	}
}

func appendWithFlags(t *testing.T, addr, mailbox string, raw []byte, flags []emersionimap.Flag) uint32 {
	requirements := require.New(t)

	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	requirements.NoError(client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	command := client.Append(mailbox, int64(len(raw)), &emersionimap.AppendOptions{Flags: flags})
	_, err = io.Copy(command, bytes.NewReader(raw))
	requirements.NoError(err)
	requirements.NoError(command.Close())
	data, err := command.Wait()
	requirements.NoError(err)
	requirements.NotNil(data)
	return uint32(data.UID)
}

func TestInspectDraftClassifiesRemoteStateInIsolation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps: emersionimap.CapSet{
			emersionimap.CapIMAP4rev1: {},
			emersionimap.CapUIDPlus:   {},
		},
	})
	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)
	client := newDraftTestClient(t, addr)
	appendResult, err := client.AppendDraft(context.Background(), "Drafts", raw)
	requirements.NoError(err)
	target := DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: appendResult.UID, RawSHA256: digest,
	}

	t.Run("present", func(t *testing.T) {
		result, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), target)
		requirements.NoError(err)
		assertions.Equal(DraftRemotePresent, result.State)
		assertions.Equal(appendResult.UIDValidity, result.UIDValidity)
	})
	t.Run("absent", func(t *testing.T) {
		absent := target
		absent.UID++
		result, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), absent)
		requirements.NoError(err)
		assertions.Equal(DraftRemoteAbsent, result.State)
	})
	t.Run("epoch guard", func(t *testing.T) {
		wrongEpoch := target
		wrongEpoch.UIDValidity++
		_, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), wrongEpoch)
		requirements.Error(err)
		var appendErr *DraftAppendError
		requirements.ErrorAs(err, &appendErr)
		assertions.Equal("uidvalidity_changed", appendErr.Code)
	})
}

func TestInspectDraftReportsFlagLossAndContentChange(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps: emersionimap.CapSet{
			emersionimap.CapIMAP4rev1: {},
			emersionimap.CapUIDPlus:   {},
		},
	})
	raw := []byte(testDraftRaw)
	digest := sha256.Sum256(raw)
	flagMissingUID := appendWithFlags(t, addr, "Drafts", raw, []emersionimap.Flag{emersionimap.FlagSeen})
	client := newDraftTestClient(t, addr)
	appendResult, err := client.AppendDraft(context.Background(), "Drafts", raw)
	requirements.NoError(err)

	flagMissing, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: flagMissingUID, RawSHA256: digest,
	})
	requirements.NoError(err)
	assertions.Equal(DraftRemoteFlagMissing, flagMissing.State)

	different := sha256.Sum256([]byte("different content"))
	changed, err := newDraftTestClient(t, addr).InspectDraft(context.Background(), DraftTarget{
		Mailbox: "Drafts", UIDValidity: appendResult.UIDValidity,
		UID: appendResult.UID, RawSHA256: different,
	})
	requirements.NoError(err)
	assertions.Equal(DraftRemoteChanged, changed.State)
	assertions.Equal(digest, changed.RawSHA256)
}

func TestInspectDraftDoesNotRequireUIDPlus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	addr, _ := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}},
	})
	result, err := newDraftTestClient(t, addr).InspectDraft(t.Context(), DraftTarget{
		Mailbox: "Drafts", UIDValidity: 1, UID: 1,
	})
	requirements.NoError(err)
	assertions.Equal(DraftRemoteAbsent, result.State)
}
