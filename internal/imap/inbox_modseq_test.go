package imap

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This wire fixture exercises the real IMAP client against two exact UIDs and
// a CONDSTORE server, whose mailbox MODSEQ advances when only one message changes.
type inboxModSeqWire struct {
	mu      sync.Mutex
	highest uint64
	modSeq  map[uint32]uint64
	seen    map[uint32]bool
}

func startInboxModSeqWireServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	state := &inboxModSeqWire{highest: 10, modSeq: map[uint32]uint64{1: 10, 2: 10}, seen: map[uint32]bool{}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveInboxModSeqWireConn(conn, state)
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

func serveInboxModSeqWireConn(conn net.Conn, state *inboxModSeqWire) {
	defer func() { _ = conn.Close() }()
	_, _ = fmt.Fprint(conn, "* OK synthetic CONDSTORE server ready\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		tag, command, ok := strings.Cut(line, " ")
		if !ok {
			return
		}
		upper := strings.ToUpper(command)
		switch {
		case upper == "CAPABILITY":
			_, _ = fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 CONDSTORE\r\n%s OK CAPABILITY completed\r\n", tag)
		case strings.HasPrefix(upper, "LOGIN "):
			_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
		case strings.HasPrefix(upper, "SELECT ") || strings.HasPrefix(upper, "EXAMINE "):
			state.mu.Lock()
			highest := state.highest
			state.mu.Unlock()
			mode := "READ-ONLY"
			if strings.HasPrefix(upper, "SELECT ") && !strings.Contains(upper, "READ-ONLY") {
				mode = "READ-WRITE"
			}
			_, _ = fmt.Fprintf(conn, "* FLAGS (\\Seen \\Flagged)\r\n* 2 EXISTS\r\n* OK [PERMANENTFLAGS (\\Seen \\*)]\r\n* OK [UIDVALIDITY 77]\r\n* OK [UIDNEXT 3]\r\n* OK [HIGHESTMODSEQ %d]\r\n%s OK [%s] SELECT completed\r\n", highest, tag, mode)
		case strings.HasPrefix(upper, "UID FETCH "):
			fields := strings.Fields(command)
			uid, err := strconv.ParseUint(fields[2], 10, 32)
			if err != nil {
				_, _ = fmt.Fprintf(conn, "%s BAD invalid UID\r\n", tag)
				continue
			}
			state.mu.Lock()
			seen, modSeq := state.seen[uint32(uid)], state.modSeq[uint32(uid)]
			state.mu.Unlock()
			flags, modSeqItem := "", ""
			if seen {
				flags = "\\Seen"
			}
			if strings.Contains(upper, "MODSEQ") {
				modSeqItem = fmt.Sprintf(" MODSEQ (%d)", modSeq)
			}
			seq := uid
			_, _ = fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (%s)%s)\r\n%s OK UID FETCH completed\r\n", seq, uid, flags, modSeqItem, tag)
		case strings.HasPrefix(upper, "UID STORE "):
			fields := strings.Fields(command)
			uid, err := strconv.ParseUint(fields[2], 10, 32)
			if err != nil {
				_, _ = fmt.Fprintf(conn, "%s BAD invalid UID\r\n", tag)
				continue
			}
			state.mu.Lock()
			if strings.Contains(upper, "+FLAGS") && !state.seen[uint32(uid)] {
				state.seen[uint32(uid)] = true
				state.highest++
				state.modSeq[uint32(uid)] = state.highest
			}
			state.mu.Unlock()
			_, _ = fmt.Fprintf(conn, "%s OK UID STORE completed\r\n", tag)
		case upper == "LOGOUT":
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unsupported synthetic command\r\n", tag)
		}
	}
}

func TestInboxIMAPRevisionUsesExactUIDModSeq(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	client := newTestClient(t, startInboxModSeqWireServer(t))
	source, targetOne := inboxIMAPBinding(client, KeywordIdentity{Mailbox: "INBOX", UIDValidity: 77, UID: 1})
	targetTwo := targetOne
	targetTwo.ItemID, targetTwo.ProviderID, targetTwo.UID = 2, "synthetic-archive-message-2", 2
	provider := NewInboxProvider(client, source)
	requestOne := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &targetOne, DryRun: true}
	requestTwo := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &targetTwo, DryRun: true}

	beforeTwo, err := provider.Observe(t.Context(), requestTwo)
	requirements.NoError(err)
	beforeOne, err := provider.Observe(t.Context(), requestOne)
	requirements.NoError(err)
	projectedOne, err := provider.Preview(t.Context(), requestOne, beforeOne)
	requirements.NoError(err)
	_, err = provider.Dispatch(t.Context(), requestOne, beforeOne)
	requirements.NoError(err)
	afterOne, err := provider.Observe(t.Context(), requestOne)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(requestOne, beforeOne, projectedOne, afterOne))
	afterTwo, err := provider.Observe(t.Context(), requestTwo)
	requirements.NoError(err)

	assertions.Equal(beforeTwo.Revision, afterTwo.Revision, "a flag change to another UID must not change this UID's revision")
	beforeHash, err := inboxcontrol.SemanticFingerprint(beforeTwo)
	requirements.NoError(err)
	afterHash, err := inboxcontrol.SemanticFingerprint(afterTwo)
	requirements.NoError(err)
	assertions.Equal(beforeHash, afterHash, "the saved triage observation for the unchanged UID remains current")
}
