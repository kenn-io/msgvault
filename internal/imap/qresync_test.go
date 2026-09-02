package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

type qresyncServerConfig struct {
	capabilities            []string
	mailboxes               []string
	uidValidity             uint32
	uidNext                 uint32
	highestModSeq           uint64
	selectHighestModSeq     uint64
	selectFailureConnection int
	selectFailureAt         int
	numMessages             *uint32
	// selectExists is the EXISTS count SELECT reports. It defaults to the size
	// of searchUIDs, which is right whenever nothing vanishes; a test whose
	// scenario expunges messages must state the post-expunge count, because a
	// server cannot report a message gone and still count it.
	selectExists *uint32
	searchUIDs   []imapv2.UID
	// omitFromFetch names UIDs the server leaves out of a plain UID FETCH
	// response while still reporting them from SEARCH, which is how a server
	// that drops a live message from a response behaves.
	omitFromFetch []imapv2.UID
	fetchChanged  []imapv2.UID
	// fetchModSeq overrides the mod-sequence returned by a plain UID FETCH.
	// UIDs without an override are unchanged baseline messages.
	fetchModSeq              map[imapv2.UID]uint64
	fetchVanished            []imapv2.UID
	filterVanishedByFetchSet bool
	// dropOnSearchConnection closes the connection with that ID instead of
	// answering its first UID SEARCH, which is how a coverage search meets a
	// connection the server has given up on.
	dropOnSearchConnection int
	// uidValidityAfterConnection is the UIDVALIDITY reported to every
	// connection after that ID, which is how a mailbox recreated between two
	// connections presents itself.
	uidValidityAfterConnection int
	laterUIDValidity           uint32
}

type qresyncTestServer struct {
	mu       sync.Mutex
	commands map[int][]string
}

func (s *qresyncTestServer) record(connID int, command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands[connID] = append(s.commands[connID], command)
}

func (s *qresyncTestServer) commandsFor(connID int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands[connID]...)
}

func startQresyncTestServer(t *testing.T, cfg qresyncServerConfig) (string, *qresyncTestServer) {
	t.Helper()
	require.NotEmpty(t, cfg.capabilities)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	server := &qresyncTestServer{commands: make(map[int][]string)}
	if len(cfg.mailboxes) == 0 {
		cfg.mailboxes = []string{"INBOX"}
	}
	go func() {
		connID := 0
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			connID++
			caps := cfg.capabilities[min(connID-1, len(cfg.capabilities)-1)]
			go serveQresyncTestConn(conn, connID, caps, cfg, server)
		}
	}()
	return ln.Addr().String(), server
}

func serveQresyncTestConn(
	conn net.Conn,
	connID int,
	capabilities string,
	cfg qresyncServerConfig,
	server *qresyncTestServer,
) {
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "* OK synthetic IMAP ready\r\n")
	reader := bufio.NewReader(conn)
	selectCount := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		server.record(connID, line)
		tag, command, _ := strings.Cut(line, " ")
		upper := strings.ToUpper(command)
		switch {
		case strings.HasPrefix(upper, "CAPABILITY"):
			_, _ = fmt.Fprintf(conn, "* CAPABILITY %s\r\n%s OK CAPABILITY completed\r\n", capabilities, tag)
		case strings.HasPrefix(upper, "LOGIN"):
			_, _ = fmt.Fprintf(conn, "%s OK LOGIN completed\r\n", tag)
		case strings.HasPrefix(upper, "LIST"):
			for _, mailbox := range cfg.mailboxes {
				_, _ = fmt.Fprintf(conn, "* LIST () \"/\" %q\r\n", mailbox)
			}
			_, _ = fmt.Fprintf(conn, "%s OK LIST completed\r\n", tag)
		case strings.HasPrefix(upper, "STATUS"):
			mailbox := scriptedCommandMailbox(command, "STATUS")
			messages := ""
			if cfg.numMessages != nil {
				messages = fmt.Sprintf("MESSAGES %d ", *cfg.numMessages)
			}
			_, _ = fmt.Fprintf(conn,
				"* STATUS %q (%sUIDNEXT %d UIDVALIDITY %d HIGHESTMODSEQ %d)\r\n%s OK STATUS completed\r\n",
				mailbox, messages, cfg.uidNext, connectionUIDValidity(cfg, connID),
				cfg.highestModSeq, tag)
		case strings.HasPrefix(upper, "ENABLE"):
			_, _ = fmt.Fprintf(conn, "* ENABLED QRESYNC\r\n%s OK ENABLE completed\r\n", tag)
		case strings.HasPrefix(upper, "SELECT"):
			selectCount++
			if connID == cfg.selectFailureConnection && selectCount == cfg.selectFailureAt {
				_, _ = fmt.Fprintf(conn, "%s NO synthetic SELECT failure\r\n", tag)
				continue
			}
			selectModSeq := cfg.selectHighestModSeq
			if selectModSeq == 0 {
				selectModSeq = cfg.highestModSeq
			}
			exists := uint32(len(cfg.searchUIDs))
			if cfg.selectExists != nil {
				exists = *cfg.selectExists
			}
			_, _ = fmt.Fprintf(conn,
				"* FLAGS (\\Seen)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d]\r\n* OK [UIDNEXT %d]\r\n* OK [HIGHESTMODSEQ %d]\r\n",
				exists, connectionUIDValidity(cfg, connID), cfg.uidNext, selectModSeq)
			_, _ = fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
		case strings.HasPrefix(upper, "UID FETCH"):
			// Only a CHANGEDSINCE fetch is entitled to answer with a subset:
			// that is what "changed since" means. A plain UID FETCH returns
			// every message it was asked for, so it answers with the whole
			// mailbox, which is what the callers of this fixture request.
			if strings.Contains(upper, "CHANGEDSINCE") {
				writeVanished(conn, qresyncVanishedForFetch(command, cfg))
				writeFetchResponses(conn, cfg.fetchChanged, cfg.highestModSeq)
			} else {
				for _, uid := range withoutUIDs(cfg.searchUIDs, cfg.omitFromFetch) {
					modSeq := cfg.fetchModSeq[uid]
					if modSeq == 0 {
						modSeq = 1
					}
					writeFetchResponses(conn, []imapv2.UID{uid}, modSeq)
				}
			}
			_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
		case strings.HasPrefix(upper, "UID SEARCH"):
			if connID == cfg.dropOnSearchConnection {
				return
			}
			_, _ = fmt.Fprintf(conn, "* SEARCH%s\r\n%s OK UID SEARCH completed\r\n",
				formatSearchUIDs(cfg.searchUIDs), tag)
		case strings.HasPrefix(upper, "LOGOUT"):
			_, _ = fmt.Fprintf(conn, "* BYE closing\r\n%s OK LOGOUT completed\r\n", tag)
			return
		default:
			_, _ = fmt.Fprintf(conn, "%s BAD unsupported synthetic command\r\n", tag)
		}
	}
}

// connectionUIDValidity reports the UIDVALIDITY this connection sees. A
// mailbox recreated between two connections reports the new value from every
// command, so STATUS and SELECT must agree.
func connectionUIDValidity(cfg qresyncServerConfig, connID int) uint32 {
	if cfg.uidValidityAfterConnection != 0 && connID > cfg.uidValidityAfterConnection {
		return cfg.laterUIDValidity
	}
	return cfg.uidValidity
}

func qresyncVanishedForFetch(command string, cfg qresyncServerConfig) []imapv2.UID {
	if !cfg.filterVanishedByFetchSet {
		return cfg.fetchVanished
	}
	fields := strings.Fields(command)
	if len(fields) < 3 {
		return nil
	}
	startText, stopText, ok := strings.Cut(fields[2], ":")
	if !ok {
		return nil
	}
	start, err := strconv.ParseUint(startText, 10, 32)
	if err != nil {
		return nil
	}
	var stop uint64
	if stopText == "*" {
		for _, uid := range cfg.searchUIDs {
			stop = max(stop, uint64(uid))
		}
	} else {
		stop, err = strconv.ParseUint(stopText, 10, 32)
		if err != nil {
			return nil
		}
	}
	var vanished []imapv2.UID
	for _, uid := range cfg.fetchVanished {
		if uint64(uid) >= start && uint64(uid) <= stop {
			vanished = append(vanished, uid)
		}
	}
	return vanished
}

func scriptedCommandMailbox(command, commandName string) string {
	rest := strings.TrimSpace(command[len(commandName):])
	if strings.HasPrefix(rest, "\"") {
		if end := strings.Index(rest[1:], "\""); end >= 0 {
			return rest[1 : end+1]
		}
	}
	mailbox, _, _ := strings.Cut(rest, " ")
	return mailbox
}

func writeVanished(w io.Writer, uids []imapv2.UID) {
	if len(uids) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "* VANISHED (EARLIER) %s\r\n", imapv2.UIDSetNum(uids...).String())
}

func writeFetchResponses(w io.Writer, uids []imapv2.UID, modSeq uint64) {
	for i, uid := range uids {
		_, _ = fmt.Fprintf(w, "* %d FETCH (UID %d FLAGS (\\Seen) MODSEQ (%d))\r\n", i+1, uid, modSeq)
	}
}

func withoutUIDs(uids, omit []imapv2.UID) []imapv2.UID {
	if len(omit) == 0 {
		return uids
	}
	kept := make([]imapv2.UID, 0, len(uids))
	for _, uid := range uids {
		if !slices.Contains(omit, uid) {
			kept = append(kept, uid)
		}
	}
	return kept
}

func formatSearchUIDs(uids []imapv2.UID) string {
	var b strings.Builder
	for _, uid := range uids {
		b.WriteByte(' ')
		b.WriteString(strconv.FormatUint(uint64(uid), 10))
	}
	return b.String()
}

func newQresyncTestClient(
	t *testing.T, addr string, states map[string]FolderState, opts ...Option,
) *Client {
	t.Helper()
	host, portString, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portString)
	require.NoError(t, err)
	clientOpts := append([]Option{WithFolderStates(states)}, opts...)
	client := NewClient(&Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, clientOpts...)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func listQresyncMessages(t *testing.T, client *Client) []string {
	t.Helper()
	return listAllMessages(t, client)
}

func joinedCommands(commands []string) string {
	return strings.ToUpper(strings.Join(commands, "\n"))
}

func TestListMessagesQresyncUsesHighestModSeqAndCollectsDelta(t *testing.T) {
	assert := assert.New(t)
	// Two of the three messages are expunged by this run, so the mailbox holds
	// one. A server cannot report a message gone and still count it.
	postExpungeExists := uint32(1)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
		selectExists:  &postExpungeExists,
		fetchChanged:  []imapv2.UID{2},
		fetchVanished: []imapv2.UID{1, 3},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity:   77,
			UIDNext:       4,
			HighestModSeq: 10,
			KnownUIDs:     []uint32{1, 2, 3},
		},
	})

	assert.Equal([]string{"INBOX|2"}, listQresyncMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.Equal("INBOX", deltas[0].Mailbox)
	assert.Equal(uint64(20), deltas[0].State.HighestModSeq)
	assert.Equal([]imapv2.UID{2}, deltas[0].ChangedUIDs)
	assert.Equal([]imapv2.UID{1, 3}, deltas[0].VanishedUIDs)
	assert.False(deltas[0].Reset)
	assert.True(deltas[0].Incremental)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "ENABLE QRESYNC")
	assert.Contains(commands, "SELECT INBOX (CONDSTORE)")
	assert.Contains(commands, "UID FETCH 1:3")
	assert.Contains(commands, "(CHANGEDSINCE 10 VANISHED)")
	assert.NotContains(commands, "SELECT INBOX (QRESYNC")
	assert.NotContains(commands, "UID SEARCH")
}

func TestListMessagesQresyncFetchIncludesExpungedFormerHighestUID(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:             []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:              77,
		uidNext:                  4,
		highestModSeq:            20,
		searchUIDs:               []imapv2.UID{1, 2},
		fetchVanished:            []imapv2.UID{3},
		filterVanishedByFetchSet: true,
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3},
		},
	})

	assert.Empty(listQresyncMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.Equal([]imapv2.UID{3}, deltas[0].VanishedUIDs)
	assert.Equal([]uint32{1, 2}, deltas[0].State.KnownUIDs)
	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID FETCH 1:3")
	assert.NotContains(commands, "UID FETCH 1:*")
}

func TestCaptureQresyncVanishedExpandsStaticUIDRanges(t *testing.T) {
	client := &Client{}
	client.beginQresyncCapture()
	var vanished imapv2.UIDSet
	vanished.AddRange(4, 6)

	client.captureQresyncVanished(vanished, false)

	assert.Equal(t, []imapv2.UID{4, 5, 6}, client.snapshotQresyncCapture().vanished)
}

func TestListMessagesQresyncUIDValidityMismatchResetsWithFullEnumeration(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   88,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
	})

	assert.Equal([]string{"INBOX|1", "INBOX|2", "INBOX|3"}, listQresyncMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.True(deltas[0].Reset)
	assert.False(deltas[0].Incremental)
	assert.Equal([]imapv2.UID{1, 2, 3}, deltas[0].ChangedUIDs)
	// Ineligible before a single command went out, so the full enumeration
	// reuses the connection instead of redialing to discard nothing.
	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "QRESYNC (")
	assert.Empty(server.commandsFor(2))
}

func TestListMessagesQresyncFallsBackWithoutCompleteEligibility(t *testing.T) {
	tests := []struct {
		name          string
		capabilities  string
		priorModSeq   uint64
		currentModSeq uint64
	}{
		{name: "missing QRESYNC", capabilities: "IMAP4rev1 ENABLE CONDSTORE", priorModSeq: 10, currentModSeq: 20},
		{name: "missing ENABLE", capabilities: "IMAP4rev1 QRESYNC CONDSTORE", priorModSeq: 10, currentModSeq: 20},
		{name: "zero saved modseq", capabilities: "IMAP4rev1 ENABLE QRESYNC CONDSTORE", currentModSeq: 20},
		{name: "zero current modseq", capabilities: "IMAP4rev1 ENABLE QRESYNC CONDSTORE", priorModSeq: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			addr, server := startQresyncTestServer(t, qresyncServerConfig{
				capabilities:  []string{tt.capabilities},
				uidValidity:   77,
				uidNext:       4,
				highestModSeq: tt.currentModSeq,
				searchUIDs:    []imapv2.UID{1, 2, 3},
			})
			client := newQresyncTestClient(t, addr, map[string]FolderState{
				"INBOX": {UIDValidity: 77, UIDNext: 4, HighestModSeq: tt.priorModSeq, KnownUIDs: []uint32{1, 2, 3}},
			})

			assert.Equal([]string{"INBOX|1", "INBOX|2", "INBOX|3"}, listQresyncMessages(t, client))
			deltas := client.ObservedMailboxDeltas()
			require.Len(t, deltas, 1)
			assert.True(deltas[0].Reset)
			assert.False(deltas[0].Incremental)
			commands := joinedCommands(server.commandsFor(1))
			assert.Contains(commands, "UID SEARCH")
			assert.NotContains(commands, "QRESYNC (")
			assert.Empty(server.commandsFor(2),
				"an ineligible baseline issues no command, so nothing needs discarding")
		})
	}
}

func TestListMessagesQresyncReconnectDoesNotReuseEnabledState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{
			"IMAP4rev1 ENABLE QRESYNC CONDSTORE",
			"IMAP4rev1 ENABLE CONDSTORE",
		},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
		fetchChanged:  []imapv2.UID{2},
		fetchVanished: []imapv2.UID{1},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
	})
	require.Equal([]string{"INBOX|2"}, listQresyncMessages(t, client))

	client.mu.Lock()
	require.NoError(client.reconnect(context.Background()))
	client.messageListCache = nil
	client.mu.Unlock()

	assert.Equal([]string{"INBOX|1", "INBOX|2", "INBOX|3"}, listQresyncMessages(t, client))
	commands := joinedCommands(server.commandsFor(2))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "ENABLE QRESYNC")
	assert.NotContains(commands, "QRESYNC (")
	assert.Empty(server.commandsFor(3))
}

func TestListMessagesQresyncErrorDiscardsPartialDeltaStateBeforeFallback(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:            []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		mailboxes:               []string{"First", "Second"},
		uidValidity:             77,
		uidNext:                 4,
		highestModSeq:           20,
		selectFailureConnection: 1,
		selectFailureAt:         2,
		searchUIDs:              []imapv2.UID{1, 2, 3},
		fetchChanged:            []imapv2.UID{2},
	})
	states := map[string]FolderState{
		"First":  {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
		"Second": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
	}
	client := newQresyncTestClient(t, addr, states, WithFolderStateSave(func(string, FolderState) {}))
	client.mailboxCache = []string{"First", "Second"}

	assert.Len(listQresyncMessages(t, client), 6)
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 2)
	assert.Equal([]string{"First", "Second"}, []string{deltas[0].Mailbox, deltas[1].Mailbox})
	assert.True(deltas[0].Reset)
	assert.True(deltas[1].Reset)
	assert.False(deltas[0].Incremental)
	assert.False(deltas[1].Incremental)
	assert.Equal(map[string]int{"First": 3, "Second": 3}, client.pendingFolderCounts)
	assert.Len(client.observedFolderStates, 2)
	assert.Contains(joinedCommands(server.commandsFor(2)), "UID SEARCH")
}

func TestListMessagesQresyncRegressedStatusModSeqForcesFullFallback(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 5,
		searchUIDs:    []imapv2.UID{1, 2, 3},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
	})

	assert.Equal([]string{"INBOX|1", "INBOX|2", "INBOX|3"}, listQresyncMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.True(deltas[0].Reset)
	assert.False(deltas[0].Incremental)
	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "ENABLE QRESYNC")
	assert.NotContains(commands, "CHANGEDSINCE")
	assert.Empty(server.commandsFor(2))
}

func TestListMessagesQresyncRegressedSelectModSeqForcesFreshFullFallback(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:        []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:         77,
		uidNext:             4,
		highestModSeq:       20,
		selectHighestModSeq: 5,
		searchUIDs:          []imapv2.UID{1, 2, 3},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: []uint32{1, 2, 3}},
	})

	assert.Equal([]string{"INBOX|1", "INBOX|2", "INBOX|3"}, listQresyncMessages(t, client))
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.True(deltas[0].Reset)
	assert.False(deltas[0].Incremental)
	assert.NotContains(joinedCommands(server.commandsFor(1)), "UID FETCH")
	assert.Contains(joinedCommands(server.commandsFor(2)), "UID SEARCH")
}

func TestListMessagesAllMailboxNoopPreservesKnownUIDBaseline(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		mailboxes:     []string{"All Mail", "Projects"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 10,
		searchUIDs:    []imapv2.UID{1, 2, 3},
	})
	wantUIDs := map[string][]uint32{
		"All Mail": {1, 2, 3},
		"Projects": {2},
	}
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"All Mail": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: wantUIDs["All Mail"]},
		"Projects": {UIDValidity: 77, UIDNext: 4, HighestModSeq: 10, KnownUIDs: wantUIDs["Projects"]},
	})
	client.mailboxCache = []string{"All Mail", "Projects"}
	client.allMailFolder = "All Mail"

	assert.Empty(listQresyncMessages(t, client))
	states := client.ObservedFolderStates()
	assert.Equal(wantUIDs["All Mail"], states["All Mail"].KnownUIDs)
	assert.Equal(wantUIDs["Projects"], states["Projects"].KnownUIDs)

	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 2)
	assert.Equal([]string{"All Mail", "Projects"}, []string{deltas[0].Mailbox, deltas[1].Mailbox})
	for _, delta := range deltas {
		assert.True(delta.Incremental)
		assert.False(delta.Reset)
		assert.Empty(delta.ChangedUIDs)
		assert.Empty(delta.VanishedUIDs)
		assert.Equal(wantUIDs[delta.Mailbox], delta.State.KnownUIDs)
	}
	deltas[0].State.KnownUIDs[0] = 99
	assert.Equal(wantUIDs["All Mail"], client.ObservedMailboxDeltas()[0].State.KnownUIDs)
	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "ENABLE QRESYNC")
	assert.Contains(commands, "CHANGEDSINCE 10 VANISHED")
}

func TestListMessagesFallbackRebuildsKnownUIDBaseline(t *testing.T) {
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1"},
		uidValidity:  77,
		uidNext:      5,
		searchUIDs:   []imapv2.UID{1, 2, 3, 4},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 77, UIDNext: 4, KnownUIDs: []uint32{1, 2, 3}},
	})

	// The server reports no MESSAGES count, so the baseline cannot be verified
	// arithmetically and a full UID SEARCH settles it. The search is ground
	// truth, so only the UID missing from the baseline needs listing.
	assert.Equal(t, []string{"INBOX|4"}, listQresyncMessages(t, client))
	assert.Equal(t, []uint32{1, 2, 3, 4}, client.ObservedFolderStates()["INBOX"].KnownUIDs)
}

func TestObservedMailboxDeltasReturnsDefensiveCopy(t *testing.T) {
	client := &Client{observedMailboxDeltas: []MailboxDelta{{
		Mailbox:      "INBOX",
		State:        FolderState{KnownUIDs: []uint32{1, 2}},
		ChangedUIDs:  []imapv2.UID{2},
		VanishedUIDs: []imapv2.UID{1},
		Incremental:  true,
	}}}

	first := client.ObservedMailboxDeltas()
	first[0].Mailbox = "mutated"
	first[0].State.KnownUIDs[0] = 99
	first[0].ChangedUIDs[0] = 99
	first[0].VanishedUIDs[0] = 99

	assert.Equal(t, []MailboxDelta{{
		Mailbox:      "INBOX",
		State:        FolderState{KnownUIDs: []uint32{1, 2}},
		ChangedUIDs:  []imapv2.UID{2},
		VanishedUIDs: []imapv2.UID{1},
		Incremental:  true,
	}}, client.ObservedMailboxDeltas())
}

func TestObservedSnapshotsPreserveEmptyKnownUIDBaseline(t *testing.T) {
	client := &Client{
		observedFolderStates: map[string]FolderState{
			"Empty": {KnownUIDs: []uint32{}},
		},
		observedMailboxDeltas: []MailboxDelta{{
			Mailbox: "Empty",
			State:   FolderState{KnownUIDs: []uint32{}},
		}},
	}

	assert.NotNil(t, client.ObservedFolderStates()["Empty"].KnownUIDs)
	assert.NotNil(t, client.ObservedMailboxDeltas()[0].State.KnownUIDs)
}

// TestListMessagesToleratesBoundaryUIDAboveHighWaterMark pins the union-based
// deletion check. RFC 3501 6.4.8 makes "UID SEARCH UID n:*" return the last
// message in the mailbox even when n is past it, so a search above the saved
// high water mark can re-report a UID the baseline already holds. Adding the
// two counts would read that as a deletion; merging them does not. The
// in-memory server resolves "*" to UIDNEXT-1 instead, so only a scripted
// server can exercise this.
func TestListMessagesToleratesBoundaryUIDAboveHighWaterMark(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	numMessages := uint32(2)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1"},
		mailboxes:    []string{"INBOX"},
		uidValidity:  1,
		uidNext:      4, // a message arrived and was expunged
		numMessages:  &numMessages,
		searchUIDs:   []imapv2.UID{2}, // "3:*" re-reports the last message
	})

	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {UIDValidity: 1, UIDNext: 3, KnownUIDs: []uint32{1, 2}},
	})
	// Re-listing the boundary UID is harmless: it is already archived, so the
	// syncer skips it. Re-enumerating the whole mailbox would not be.
	assert.Equal([]string{"INBOX|2"}, listQresyncMessages(t, client))

	deltas := client.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.False(deltas[0].Reset,
		"a re-reported boundary UID is not evidence of a deletion")
	assert.Equal([]uint32{1, 2}, deltas[0].State.KnownUIDs)
	assert.Contains(joinedCommands(server.commandsFor(1)), "UID SEARCH UID 3:*")
}

// TestListMessagesCondStoreOnlyFlagChangeForcesFullEnumeration covers a server
// that advertises CONDSTORE but not QRESYNC. A flags-only change advances
// HIGHESTMODSEQ while UIDNEXT and the message count both stay put, so the
// UIDNEXT high water mark alone would never look at the modified message. The
// mod-sequence is the signal that something below the mark moved.
func TestListMessagesCondStoreOnlyFlagChangeForcesFullEnumeration(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	numMessages := uint32(2)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE CONDSTORE"},
		mailboxes:     []string{"INBOX"},
		uidValidity:   1,
		uidNext:       3, // unchanged
		numMessages:   &numMessages,
		highestModSeq: 20, // a flag changed on an existing message
		searchUIDs:    []imapv2.UID{1, 2},
	})

	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity:   1,
			UIDNext:       3,
			HighestModSeq: 10,
			KnownUIDs:     []uint32{1, 2},
		},
	})

	assert.Equal([]string{"INBOX|1", "INBOX|2"}, listQresyncMessages(t, client),
		"an advanced mod-sequence must re-read the mailbox, not just its tail")

	deltas := client.ObservedMailboxDeltas()
	require.Len(deltas, 1)
	assert.True(deltas[0].Reset,
		"a flags-only change is invisible to UIDNEXT and the message count")
	assert.Equal(uint64(20), deltas[0].State.HighestModSeq)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID SEARCH UID 1:*")
	assert.NotContains(commands, "UID SEARCH UID 3:*")
}

// TestListMessagesIneligibleQresyncReusesConnection covers a server that never
// reports a mod-sequence, which is the common case for Office 365 IMAP. No
// mailbox can be QRESYNC-eligible, so tryBuildQresyncMessageList returns before
// issuing a single command and leaves the connection, the mailbox plan and the
// STATUS results intact. Reconnecting there would redial and re-STATUS every
// mailbox on every scheduled sync to discard nothing.
func TestListMessagesIneligibleQresyncReusesConnection(t *testing.T) {
	assert := assert.New(t)

	numMessages := uint32(2)
	mailboxes := []string{"Archive", "INBOX", "Projects"}
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1"},
		mailboxes:    mailboxes,
		uidValidity:  1,
		uidNext:      3,
		numMessages:  &numMessages,
	})

	states := make(map[string]FolderState, len(mailboxes))
	for _, mailbox := range mailboxes {
		states[mailbox] = FolderState{UIDValidity: 1, UIDNext: 3, KnownUIDs: []uint32{1, 2}}
	}
	client := newQresyncTestClient(t, addr, states)

	assert.Empty(listQresyncMessages(t, client),
		"every mailbox is proven unchanged by UIDNEXT and message count")

	assert.Empty(server.commandsFor(2), "the fallback must not redial")
	commands := joinedCommands(server.commandsFor(1))
	assert.Equal(1, strings.Count(commands, "LIST "), "one LIST, not one per attempt")
	assert.Equal(len(mailboxes), strings.Count(commands, "STATUS "),
		"one STATUS per mailbox, not two")
	assert.NotContains(commands, "QRESYNC (")
}

// TestListMessagesQresyncErrorStillCoversSkippedMailboxes pins the safety
// property behind letting the post-error fallback take scan shortcuts. A failed
// QRESYNC attempt invalidates nothing the plan depends on: the stored baseline
// is untouched and the reconnect re-runs STATUS, so a mailbox proven unchanged
// by UIDVALIDITY, UIDNEXT and message count is still safe to skip. What must
// not happen is a skipped mailbox dropping out of the published topology --
// applyIMAPMailboxDeltas retires every mailbox missing from the delta set,
// deleting its memberships and tombstoning the messages that lived only there.
func TestListMessagesQresyncErrorStillCoversSkippedMailboxes(t *testing.T) {
	assert := assert.New(t)

	numMessages := uint32(3)
	mailboxes := []string{"Archive", "INBOX"}
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:            []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		mailboxes:               mailboxes,
		uidValidity:             77,
		uidNext:                 4,
		highestModSeq:           20,
		numMessages:             &numMessages,
		selectFailureConnection: 1,
		selectFailureAt:         1,
		searchUIDs:              []imapv2.UID{1, 2, 3},
	})

	states := make(map[string]FolderState, len(mailboxes))
	for _, mailbox := range mailboxes {
		states[mailbox] = FolderState{
			UIDValidity:   77,
			UIDNext:       4,
			HighestModSeq: 20,
			KnownUIDs:     []uint32{1, 2, 3},
		}
	}
	client := newQresyncTestClient(t, addr, states)
	client.mailboxCache = mailboxes

	// The QRESYNC SELECT fails, so the run redials and re-STATUSes. Every
	// mailbox then matches its baseline and is skipped.
	assert.Empty(listQresyncMessages(t, client))
	assert.Contains(joinedCommands(server.commandsFor(2)), "STATUS",
		"a genuine QRESYNC failure still redials and re-reads STATUS")

	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, len(mailboxes),
		"a skipped mailbox must still appear in the topology or the store retires it")
	for _, delta := range deltas {
		assert.True(delta.Incremental, "%s", delta.Mailbox)
		assert.False(delta.Reset, "%s", delta.Mailbox)
		assert.Empty(delta.VanishedUIDs, "%s", delta.Mailbox)
		assert.Equal([]uint32{1, 2, 3}, delta.State.KnownUIDs, "%s", delta.Mailbox)
	}
	assert.Len(client.ObservedFolderStates(), len(mailboxes))
}

// TestLabelMapOmittedUIDSuppressesAuthoritativeSnapshot covers the label map's
// half of reading a server's silence as fact. SEARCH reports the UID, so the
// run knows the message is there, and then every FETCH leaves it out. Nothing
// downstream can see that: the message is absent from the map with no error
// against it.
//
// Publishing that map as authoritative is what loses the message. A republish
// deletes every membership row the run did not observe, and for a mailbox
// whose membership comes only from this map there is no later fetch to put it
// back. The map must therefore declare itself incomplete, which suppresses the
// snapshot and leaves the stored rows alone.
func TestLabelMapOmittedUIDSuppressesAuthoritativeSnapshot(t *testing.T) {
	assert := assert.New(t)
	addr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1"},
		uidValidity:   77,
		uidNext:       4,
		searchUIDs:    []imapv2.UID{1, 2, 3},
		omitFromFetch: []imapv2.UID{2},
	})
	client := newQresyncTestClient(t, addr, nil)

	listQresyncMessages(t, client)

	assert.False(client.LabelsSnapshotComplete(),
		"a label map missing a UID the server reported is not authoritative")
	assert.Empty(client.ObservedMailboxDeltas(),
		"an incomplete label map must not publish a mailbox topology")
	assert.Empty(client.ObservedFolderStates(),
		"nor the folder states derived from it")
}

// TestQresyncOmittedNewUIDFallsBackToFullEnumeration covers the QRESYNC half
// of reading a server's silence as fact.
//
// A message that arrived since the last run has a UID at or above the previous
// UIDNEXT, so its mod-sequence is above the one this fetch asks about and the
// server is obliged to report it. When it does not, nothing downstream can
// tell: the UID never reaches KnownUIDs, it is never listed or fetched, no
// error is recorded, and HIGHESTMODSEQ still advances past it, so no later run
// has a reason to ask about it again.
//
// A UID SEARCH is independent of the FETCH that misbehaved. Here the mailbox
// reports three messages while the response accounts for two, so the search
// runs, finds UID 3 above the high water mark and unaccounted for, and the run
// abandons QRESYNC for a full enumeration rather than saving a cursor past a
// message it never saw.
func TestQresyncOmittedNewUIDFallsBackToFullEnumeration(t *testing.T) {
	assert := assert.New(t)
	mailboxExists := uint32(3)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
		selectExists:  &mailboxExists,
		// UID 3 arrived since the last run and the server leaves it out.
		fetchChanged: []imapv2.UID{},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity:   77,
			UIDNext:       3,
			HighestModSeq: 10,
			KnownUIDs:     []uint32{1, 2},
		},
	})

	listed := listQresyncMessages(t, client)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "CHANGEDSINCE 10 VANISHED",
		"the run must try QRESYNC first")
	assert.Contains(commands, "UID SEARCH UID 3:*",
		"a mailbox short of its message count must be checked independently")

	// The fallback enumerates the mailbox, so the message the CHANGEDSINCE
	// response omitted is listed after all. The cursor does advance here, and
	// that is correct: this run really did read the whole mailbox.
	assert.Contains(listed, "INBOX|3",
		"the message the response omitted must be recovered by the fallback")
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.Contains(deltas[0].State.KnownUIDs, uint32(3),
		"and must reach the saved baseline")
}

// TestQresyncOmittedExistingChangeIsRecoveredIndependently covers the part of
// CHANGEDSINCE coverage that UIDNEXT cannot measure. UID 2 existed before the
// saved high-water mark, but its flags changed afterward. The server omits it
// from CHANGEDSINCE while still returning it from a plain FETCH with a newer
// mod-sequence. The independent refresh must put it back into the delta before
// HIGHESTMODSEQ advances past the change.
func TestQresyncOmittedExistingChangeIsRecoveredIndependently(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
		fetchChanged:  []imapv2.UID{},
		fetchModSeq:   map[imapv2.UID]uint64{2: 20},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 4, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3},
		},
	})

	listed := listQresyncMessages(t, client)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "CHANGEDSINCE 10 VANISHED",
		"the server omission must occur on the incremental request")
	plainRefresh := false
	for command := range strings.SplitSeq(commands, "\n") {
		if strings.Contains(command, "UID FETCH 1:3") &&
			strings.Contains(command, "MODSEQ") &&
			!strings.Contains(command, "CHANGEDSINCE") {
			plainRefresh = true
			break
		}
	}
	assert.True(plainRefresh, "existing flags need an independent complete response")
	assert.Contains(listed, "INBOX|2",
		"the independently observed change must be refreshed")
	deltas := client.ObservedMailboxDeltas()
	require.Len(t, deltas, 1)
	assert.Equal([]imapv2.UID{2}, deltas[0].ChangedUIDs)
	assert.Equal(uint64(20), deltas[0].State.HighestModSeq)
}

// TestQresyncOmittedNewUIDAfterExpungeFallsBack pins that a mailbox losing
// messages and gaining one in the same cycle is still checked. The vanished
// UIDs are already out of knownUIDs by the time coverage is verified, so an
// expunge cannot offset an addition and hide it.
func TestQresyncOmittedNewUIDAfterExpungeFallsBack(t *testing.T) {
	assert := assert.New(t)
	// Started with 1 and 2. UID 1 is expunged, UID 3 arrives -> 2 messages.
	mailboxExists := uint32(2)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{2, 3},
		selectExists:  &mailboxExists,
		fetchVanished: []imapv2.UID{1},
		// UID 3 arrived since the last run and the server leaves it out.
		fetchChanged: []imapv2.UID{},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 3, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2},
		},
	})

	listed := listQresyncMessages(t, client)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID SEARCH UID 3:*",
		"an expunge in the same cycle must not excuse the check")
	assert.Contains(listed, "INBOX|3",
		"the omitted message must be recovered by the fallback")
}

// TestQresyncStaleBaselineDoesNotMaskOmittedNewUID is why the check looks at
// UIDNEXT and not at the message count.
//
// KnownUIDs is read back from stored memberships, so it drifts from the server
// in both directions. Here it drifts long: three messages left the mailbox
// without the server reporting VANISHED, so the baseline holds four UIDs while
// the mailbox reports two. A count comparison reads that as "we already know
// about more than exist" and asks nothing further, which is precisely when a
// newly arrived UID the server omitted would be lost.
//
// UIDNEXT does not drift. It is the server's own statement that something was
// appended, and it is what decides whether to look.
func TestQresyncStaleBaselineDoesNotMaskOmittedNewUID(t *testing.T) {
	assert := assert.New(t)
	// UIDs 1-3 are gone but the server never said VANISHED. UID 5 is new and
	// is left out of the response. The mailbox reports 2 messages.
	mailboxExists := uint32(2)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       6,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{4, 5},
		selectExists:  &mailboxExists,
		fetchVanished: []imapv2.UID{},
		fetchChanged:  []imapv2.UID{},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3, 4},
		},
	})

	listed := listQresyncMessages(t, client)

	commands := joinedCommands(server.commandsFor(1))
	assert.Contains(commands, "UID SEARCH UID 5:*",
		"a baseline larger than the mailbox must not skip the check")
	assert.Contains(listed, "INBOX|5",
		"the omitted message must be recovered by the fallback")
}

// TestQresyncOutOfRangeReportCannotCoverAnOmission pins what the coverage
// arithmetic counts. The span it checks is the UIDs assigned since the last
// run, so only a report about a UID inside that span is evidence about it. A
// report naming a UID at or above the current UIDNEXT names a message the
// server has not assigned yet, and counting it would let one such report stand
// in for a real message the response left out.
func TestQresyncOutOfRangeReportCannotCoverAnOmission(t *testing.T) {
	assert := assert.New(t)
	// UIDs 5 and 6 were assigned since the last run. The response reports 5 as
	// changed, omits 6 entirely, and reports a VANISHED UID the mailbox cannot
	// have assigned yet.
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       7,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3, 4, 5, 6},
		fetchChanged:  []imapv2.UID{5},
		fetchVanished: []imapv2.UID{99},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3, 4},
		},
	})

	listed := listQresyncMessages(t, client)

	assert.Contains(joinedCommands(server.commandsFor(1)), "UID SEARCH UID 5:*",
		"a report from outside the assigned span must not satisfy the count")
	assert.Contains(listed, "INBOX|6",
		"the omitted message must be recovered by the fallback")
}

// TestQresyncCoverageSearchReconnectKeepsQresyncEnabled covers what the
// coverage search costs the rest of the run. It is the only command in the
// QRESYNC loop that reconnects on a network error, and a reconnect clears the
// ENABLE. Every mailbox after this one asks for VANISHED, which a connection
// that never enabled QRESYNC does not answer, so the run would read an
// expunged message as still present.
func TestQresyncCoverageSearchReconnectKeepsQresyncEnabled(t *testing.T) {
	assert := assert.New(t)
	// UIDs 5 and 6 were assigned since the last run, but the server skipped 6
	// rather than using it, so one changed UID accounts for the span and the
	// search confirms it. The first connection dies answering that search.
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:           []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		mailboxes:              []string{"First", "Second"},
		uidValidity:            77,
		uidNext:                7,
		highestModSeq:          20,
		searchUIDs:             []imapv2.UID{1, 2, 3, 4, 5},
		fetchChanged:           []imapv2.UID{5},
		dropOnSearchConnection: 1,
	})
	states := map[string]FolderState{
		"First": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3, 4},
		},
		"Second": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3, 4},
		},
	}
	client := newQresyncTestClient(t, addr, states)

	listQresyncMessages(t, client)

	reconnected := joinedCommands(server.commandsFor(2))
	assert.Contains(reconnected, "ENABLE QRESYNC",
		"the connection the search left behind must enable QRESYNC again")
	assert.Contains(reconnected, "CHANGEDSINCE",
		"the run continues on the new connection, so the ENABLE has to hold")
	assert.Less(
		strings.Index(reconnected, "ENABLE QRESYNC"),
		strings.Index(reconnected, "CHANGEDSINCE"),
		"QRESYNC must be enabled before the next mailbox asks for VANISHED")
}

// TestFullEnumerationSavesUIDNextAboveKnownUIDs covers the one race that can
// save a baseline holding a UID its own UIDNEXT does not cover. A full
// enumeration reads UIDNEXT from STATUS and the UIDs from a later SEARCH, so a
// message delivered between the two is enumerated while the saved UIDNEXT
// still sits at or below its UID.
//
// The next QRESYNC run reads that as a corrupt baseline, and a QRESYNC failure
// is not local to one mailbox: the caller discards every delta and enumerates
// the whole account in full.
func TestFullEnumerationSavesUIDNextAboveKnownUIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// STATUS reports UIDNEXT 4. The SEARCH that follows reports UID 4, which
	// arrived in between.
	firstAddr, _ := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3, 4},
	})
	first := newQresyncTestClient(t, firstAddr, map[string]FolderState{})

	listQresyncMessages(t, first)

	saved := first.ObservedFolderStates()
	require.Contains(saved, "INBOX")
	require.Contains(saved["INBOX"].KnownUIDs, uint32(4))
	assert.Greater(saved["INBOX"].UIDNext, uint32(4),
		"a saved UIDNEXT must cover every UID in the baseline saved with it")

	// The next run meets a server that offers QRESYNC and reports the UIDNEXT
	// the first enumeration implied.
	secondAddr, secondServer := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       5,
		highestModSeq: 25,
		searchUIDs:    []imapv2.UID{1, 2, 3, 4},
		fetchChanged:  []imapv2.UID{4},
	})
	second := newQresyncTestClient(t, secondAddr, saved)

	listQresyncMessages(t, second)

	assert.Contains(joinedCommands(secondServer.commandsFor(1)), "CHANGEDSINCE",
		"the second run must still be able to use QRESYNC")
	// A QRESYNC failure discards every delta, reconnects, and enumerates the
	// whole account, so a second connection is the signal that the baseline
	// was rejected.
	assert.Empty(secondServer.commandsFor(2),
		"a self-consistent baseline must not force a full enumeration")
}

// TestQresyncOverlappingReportCannotCoverAnOmission pins that coverage counts
// the union of the two reports. A UID may arrive as changed and as vanished in
// one response, and the coverage check runs before the caller removes vanished
// UIDs from the changed set. Counting that UID twice lets one message account
// for two of the UIDs assigned since the last run.
func TestQresyncOverlappingReportCannotCoverAnOmission(t *testing.T) {
	assert := assert.New(t)
	// UIDs 5, 6 and 7 were assigned since the last run. The response reports 5
	// and 6 as changed, reports 6 as vanished as well, and omits 7.
	mailboxExists := uint32(5)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       8,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3, 5, 7},
		selectExists:  &mailboxExists,
		fetchChanged:  []imapv2.UID{5, 6},
		fetchVanished: []imapv2.UID{6},
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3},
		},
	})

	listed := listQresyncMessages(t, client)

	assert.Contains(joinedCommands(server.commandsFor(1)), "UID SEARCH UID 5:*",
		"one UID reported twice must not account for two assigned UIDs")
	assert.Contains(listed, "INBOX|7",
		"the omitted message must be recovered by the fallback")
}

// TestQresyncCoverageSearchRejectsChangedEpoch covers the other thing the
// coverage search can bring back from a reconnect. The reconnect reselects the
// mailbox, and the mailbox that answers may be a different one wearing the
// same name. Every UID in the delta was collected under the old UIDVALIDITY,
// so the mailbox has to be enumerated in full rather than saved.
func TestQresyncCoverageSearchRejectsChangedEpoch(t *testing.T) {
	assert := assert.New(t)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:               []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:                77,
		uidNext:                    7,
		highestModSeq:              20,
		searchUIDs:                 []imapv2.UID{1, 2, 3, 4, 5},
		fetchChanged:               []imapv2.UID{5},
		dropOnSearchConnection:     1,
		uidValidityAfterConnection: 1,
		laterUIDValidity:           88,
	})
	client := newQresyncTestClient(t, addr, map[string]FolderState{
		"INBOX": {
			UIDValidity: 77, UIDNext: 5, HighestModSeq: 10,
			KnownUIDs: []uint32{1, 2, 3, 4},
		},
	})

	listQresyncMessages(t, client)

	// A QRESYNC failure reconnects once more and enumerates the account, so
	// the run reaches a third connection.
	assert.NotEmpty(server.commandsFor(3),
		"a mailbox that changed epoch mid-run must fall back to full enumeration")
	assert.NotContains(joinedCommands(server.commandsFor(3)), "CHANGEDSINCE",
		"the fallback enumerates rather than trusting the delta")
}
