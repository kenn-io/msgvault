package imap

import (
	"bufio"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// A narrow native server supplies CONDSTORE, conflict and dropped-response
// behavior the go-imap memory server cannot implement. State changes obey the
// wire precondition; production client parsing and provider calls remain real.
type inboxWireServer struct {
	mu         sync.Mutex
	flags      []string
	modSeq     uint64
	concurrent bool
	lostReply  bool
	access     string
	writeEpoch uint32
	omitModSeq bool
	zeroModSeq bool
	stores     []string
	writes     int
}

func newInboxWireServer(t *testing.T, state *inboxWireServer) (*Client, *InboxProvider, inboxcontrol.Target) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var connections sync.Map
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			go func() { defer connections.Delete(conn); state.serve(conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); return true })
	})
	client := newTestClient(t, listener.Addr().String())
	source, target := inboxIMAPBinding(client, DraftReceipt{Mailbox: "INBOX", UIDValidity: 77, UID: 1})
	return client, NewInboxProvider(client, source), target
}

var inboxStorePattern = regexp.MustCompile(`^UID STORE 1 (?:\(UNCHANGEDSINCE ([0-9]+)\) )?([+-]?FLAGS(?:\.SILENT)?) \(([^)]*)\)$`)

func (s *inboxWireServer) serve(conn net.Conn) {
	defer conn.Close()
	fmt.Fprint(conn, "* OK synthetic native server\r\n")
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tag, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			return
		}
		switch {
		case command == "CAPABILITY":
			fmt.Fprintf(conn, "* CAPABILITY IMAP4rev1 CONDSTORE\r\n%s OK capabilities\r\n", tag)
		case strings.HasPrefix(command, "LOGIN "):
			fmt.Fprintf(conn, "%s OK authenticated\r\n", tag)
		case strings.HasPrefix(command, "SELECT ") || strings.HasPrefix(command, "EXAMINE "):
			s.mu.Lock()
			modSeq, mode := s.modSeq, s.access
			s.mu.Unlock()
			if strings.HasPrefix(command, "EXAMINE ") {
				mode = "READ-ONLY"
			} else if mode == "" {
				mode = "READ-WRITE"
			}
			status, extra, epoch := "OK", "", uint32(77)
			if !strings.HasPrefix(command, "EXAMINE ") {
				if s.writeEpoch != 0 {
					epoch = s.writeEpoch
				}
				switch mode {
				case "NO", "BAD":
					status, mode = mode, "READ-WRITE"
				case "malformed":
					mode = "READ-WRITE extra"
				case "untagged":
					extra = "* OK [READ-WRITE] unrelated\r\n"
					mode = "missing"
				}
			}
			code := ""
			if mode != "missing" {
				code = " [" + mode + "]"
			}
			fmt.Fprintf(conn, "* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Recent Old Next Unrelated Concurrent Recent)\r\n* 1 EXISTS\r\n* OK [PERMANENTFLAGS (\\Seen \\Flagged Old Next Unrelated Concurrent)] flags\r\n* OK [UIDVALIDITY %d] epoch\r\n* OK [UIDNEXT 2] next\r\n* OK [HIGHESTMODSEQ %d] revision\r\n%s%s %s%s selected\r\n", epoch, modSeq, extra, tag, status, code)
		case strings.HasPrefix(command, "UID FETCH "):
			s.mu.Lock()
			flags, modSeq := slices.Clone(s.flags), s.modSeq
			omit, zero := s.omitModSeq, s.zeroModSeq
			s.mu.Unlock()
			revision := ""
			if strings.Contains(command, "MODSEQ") && !omit {
				if zero {
					modSeq = 0
				}
				revision = fmt.Sprintf(" MODSEQ (%d)", modSeq)
			}
			fmt.Fprintf(conn, "* 1 FETCH (UID 1 FLAGS (%s)%s)\r\n%s OK fetched\r\n", strings.Join(flags, " "), revision, tag)
		case strings.HasPrefix(command, "UID STORE "):
			matched := inboxStorePattern.FindStringSubmatch(command)
			if matched == nil {
				fmt.Fprintf(conn, "%s BAD invalid store\r\n", tag)
				continue
			}
			s.mu.Lock()
			s.stores = append(s.stores, command)
			if s.concurrent {
				s.flags = append(s.flags, "Concurrent")
				s.modSeq++
				s.concurrent = false
			}
			unchanged, _ := strconv.ParseUint(matched[1], 10, 64)
			if matched[1] != "" && s.modSeq > unchanged {
				s.mu.Unlock()
				fmt.Fprintf(conn, "%s OK [MODIFIED 1] conflict\r\n", tag)
				continue
			}
			values := strings.Fields(matched[3])
			switch matched[2][0] {
			case '+':
				for _, flag := range values {
					if !slices.Contains(s.flags, flag) {
						s.flags = append(s.flags, flag)
					}
				}
			case '-':
				s.flags = slices.DeleteFunc(s.flags, func(flag string) bool { return slices.Contains(values, flag) })
			default:
				s.flags = values
			}
			s.modSeq++
			s.writes++
			lost := s.lostReply
			s.mu.Unlock()
			if lost {
				return
			}
			fmt.Fprintf(conn, "%s OK stored\r\n", tag)
		case command == "LOGOUT":
			fmt.Fprintf(conn, "* BYE done\r\n%s OK logout\r\n", tag)
			return
		default:
			fmt.Fprintf(conn, "%s BAD unsupported native fixture command\r\n", tag)
		}
	}
}

func TestInboxIMAPMixedKeywordsConditionalSingleWrite(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("conflict=%t", conflict), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			wire := &inboxWireServer{flags: []string{`\Seen`, `\Flagged`, "Old", "Unrelated"}, modSeq: 10, concurrent: conflict}
			_, provider, target := newInboxWireServer(t, wire)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Tags: &emailtags.MessageTagChange{Add: []string{"Next"}, Remove: []string{"old"}}, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			wire.mu.Lock()
			assertions.Empty(wire.stores)
			wire.mu.Unlock()
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			if conflict {
				requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
			} else {
				requirements.NoError(err)
			}
			after, readErr := provider.Observe(t.Context(), request)
			requirements.NoError(readErr)
			wire.mu.Lock()
			defer wire.mu.Unlock()
			requirements.Len(wire.stores, 1)
			assertions.Equal(`UID STORE 1 (UNCHANGEDSINCE 10) FLAGS.SILENT (Next Unrelated \Flagged \Seen)`, wire.stores[0])
			if conflict {
				assertions.Zero(wire.writes)
				assertions.ElementsMatch([]string{`\Seen`, `\Flagged`, "Old", "Unrelated", "Concurrent"}, after.Flags)
				requirements.ErrorIs(provider.Verify(request, before, projected, after), inboxcontrol.ErrOutcomeUnknown)
			} else {
				assertions.Equal(1, wire.writes)
				assertions.ElementsMatch([]string{`\Seen`, `\Flagged`, "Next", "Unrelated"}, after.Flags)
				requirements.NoError(provider.Verify(request, before, projected, after))
			}
		})
	}
}

func TestInboxIMAPWriteRequiresExplicitWritableSelection(t *testing.T) {
	for _, mode := range []string{"READ-ONLY", "missing", "NO", "BAD", "malformed", "untagged", "changed epoch"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			wire := &inboxWireServer{flags: []string{}, modSeq: 10, access: mode}
			client, provider, target := newInboxWireServer(t, wire)
			if mode == "changed epoch" {
				wire.access = "READ-WRITE"
				wire.writeEpoch = 78
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			_, err = provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			request.DryRun = false
			client.selectedMailbox = "previous writable mailbox"
			_, err = provider.Dispatch(t.Context(), request, before)
			assertions.Empty(client.selectedMailbox)
			requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
			wire.mu.Lock()
			defer wire.mu.Unlock()
			assertions.Empty(wire.stores)
			assertions.Zero(wire.writes)
		})
	}
}

func TestInboxIMAPLostStoreResponseNeverRetries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	wire := &inboxWireServer{flags: []string{}, modSeq: 10, lostReply: true}
	_, provider, target := newInboxWireServer(t, wire)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	request.DryRun = false
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NoError(provider.Verify(request, before, projected, after))
	wire.mu.Lock()
	defer wire.mu.Unlock()
	assertions.Len(wire.stores, 1)
	assertions.Equal(1, wire.writes)
}

func TestInboxIMAPLostStoreReplyRecoversWithoutSessionRecent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	wire := &inboxWireServer{flags: []string{`\Recent`}, modSeq: 10, lostReply: true}
	_, provider, target := newInboxWireServer(t, wire)
	request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
	before, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	projected, err := provider.Preview(t.Context(), request, before)
	requirements.NoError(err)
	request.DryRun = false
	_, err = provider.Dispatch(t.Context(), request, before)
	requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
	// A writable session claimed Recent before its connection was lost.
	// The reconnect sees the permanent flags but not that session-only marker.
	wire.mu.Lock()
	wire.flags = slices.DeleteFunc(wire.flags, func(flag string) bool { return strings.EqualFold(flag, `\Recent`) })
	wire.mu.Unlock()
	after, err := provider.Observe(t.Context(), request)
	requirements.NoError(err)
	requirements.NotNil(after.Read)
	requirements.True(*after.Read)
	assertions.Equal([]string{`\Seen`}, after.Flags)
	wire.mu.Lock()
	assertions.Len(wire.stores, 1)
	assertions.Equal(1, wire.writes)
	wire.mu.Unlock()
	assertions.NoError(provider.Verify(request, before, projected, after), "session-only Recent must not prevent recovery of the permanent read-state change")
	assertions.Equal([]string{`\Recent`}, before.Flags, "raw signed observation is retained")
	assertions.ElementsMatch([]string{`\Recent`, `\Seen`}, projected.Flags, "verification never mutates the signed projection")
}

func TestInboxIMAPVerifyOnlyIgnoresSessionRecent(t *testing.T) {
	for _, flag := range []string{`\Recent`, `\rEcEnT`, `\Flagged`, `\Answered`, `\Deleted`, "Unrelated", "Recent"} {
		t.Run(flag, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			wire := &inboxWireServer{flags: []string{}, modSeq: 10}
			_, provider, target := newInboxWireServer(t, wire)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpSetRead, Target: &target, DryRun: true}
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			request.DryRun = false
			_, err = provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			wire.mu.Lock()
			wire.flags = append(wire.flags, flag)
			wire.mu.Unlock()
			after, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			err = provider.Verify(request, before, projected, after)
			if strings.EqualFold(flag, `\Recent`) {
				requirements.NoError(err)
			} else {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
			}
			assertions.Equal([]string{}, before.Flags)
			assertions.Equal([]string{`\Seen`}, projected.Flags)
			assertions.Contains(after.Flags, flag)
			wire.mu.Lock()
			assertions.Len(wire.stores, 1)
			assertions.Equal(1, wire.writes)
			wire.mu.Unlock()
		})
	}
}
