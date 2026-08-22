package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
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
	capabilities             []string
	mailboxes                []string
	uidValidity              uint32
	uidNext                  uint32
	highestModSeq            uint64
	selectHighestModSeq      uint64
	selectFailureConnection  int
	selectFailureAt          int
	searchUIDs               []imapv2.UID
	fetchChanged             []imapv2.UID
	fetchVanished            []imapv2.UID
	filterVanishedByFetchSet bool
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
			_, _ = fmt.Fprintf(conn,
				"* STATUS %q (UIDNEXT %d UIDVALIDITY %d HIGHESTMODSEQ %d)\r\n%s OK STATUS completed\r\n",
				mailbox, cfg.uidNext, cfg.uidValidity, cfg.highestModSeq, tag)
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
			_, _ = fmt.Fprintf(conn,
				"* FLAGS (\\Seen)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d]\r\n* OK [UIDNEXT %d]\r\n* OK [HIGHESTMODSEQ %d]\r\n",
				len(cfg.searchUIDs), cfg.uidValidity, cfg.uidNext, selectModSeq)
			_, _ = fmt.Fprintf(conn, "%s OK [READ-WRITE] SELECT completed\r\n", tag)
		case strings.HasPrefix(upper, "UID FETCH"):
			writeVanished(conn, qresyncVanishedForFetch(command, cfg))
			writeFetchResponses(conn, cfg.fetchChanged, cfg.highestModSeq)
			_, _ = fmt.Fprintf(conn, "%s OK UID FETCH completed\r\n", tag)
		case strings.HasPrefix(upper, "UID SEARCH"):
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
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities:  []string{"IMAP4rev1 ENABLE QRESYNC CONDSTORE"},
		uidValidity:   77,
		uidNext:       4,
		highestModSeq: 20,
		searchUIDs:    []imapv2.UID{1, 2, 3},
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
	assert.NotContains(joinedCommands(server.commandsFor(1)), "UID SEARCH")
	commands := joinedCommands(server.commandsFor(2))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "QRESYNC (")
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
			assert.NotContains(joinedCommands(server.commandsFor(1)), "UID SEARCH")
			commands := joinedCommands(server.commandsFor(2))
			assert.Contains(commands, "UID SEARCH")
			assert.NotContains(commands, "QRESYNC (")
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
	assert.NotContains(joinedCommands(server.commandsFor(2)), "UID SEARCH")
	commands := joinedCommands(server.commandsFor(3))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "ENABLE QRESYNC")
	assert.NotContains(commands, "QRESYNC (")
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
	assert.NotContains(joinedCommands(server.commandsFor(1)), "UID SEARCH")
	commands := joinedCommands(server.commandsFor(2))
	assert.Contains(commands, "UID SEARCH")
	assert.NotContains(commands, "ENABLE QRESYNC")
	assert.NotContains(commands, "CHANGEDSINCE")
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

	assert.Equal(t, []string{"INBOX|1", "INBOX|2", "INBOX|3", "INBOX|4"},
		listQresyncMessages(t, client))
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
