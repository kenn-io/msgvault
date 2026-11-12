package imap

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A dual-revision server uses modified UTF-7 until the client explicitly
// enables rev2. LIST can therefore return a decoded name that a client sending
// UTF-8 too early cannot address. Exercise the real wire encoder and parser.
func TestUnicodeMailboxBaselineRequiresRev2Negotiation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const mailbox = "Projects/Café"
	const legacyMailbox = "Projects/Caf&AOk-"
	count := uint32(1)
	var mu sync.Mutex
	enabled := make(map[int]bool)
	addr, server := startQresyncTestServer(t, qresyncServerConfig{
		capabilities: []string{"IMAP4rev1 IMAP4rev2 ENABLE CONDSTORE QRESYNC"},
		mailboxes:    []string{"INBOX", mailbox},
		uidValidity:  77, uidNext: 2, highestModSeq: 10,
		numMessages: &count, searchUIDs: []imapv2.UID{1},
		commandHook: func(conn net.Conn, connID int, line string) bool {
			tag, command, _ := strings.Cut(line, " ")
			mu.Lock()
			defer mu.Unlock()
			if command == "ENABLE IMAP4rev2" {
				enabled[connID] = true
				_, _ = fmt.Fprintf(conn, "* ENABLED IMAP4rev2\r\n%s OK enabled\r\n", tag)
				return true
			}
			if strings.HasPrefix(command, "LIST ") {
				name := legacyMailbox
				if enabled[connID] {
					name = mailbox
				}
				_, _ = fmt.Fprintf(conn, "* LIST () \"/\" INBOX\r\n* LIST () \"/\" %q\r\n%s OK listed\r\n", name, tag)
				return true
			}
			if (strings.HasPrefix(command, "STATUS ") || strings.HasPrefix(command, "SELECT ")) &&
				strings.Contains(command, mailbox) && !enabled[connID] {
				_, _ = fmt.Fprintf(conn, "%s NO Mailbox does not exist\r\n", tag)
				return true
			}
			return false
		},
	})

	first := newTestClient(t, addr)
	require.ElementsMatch([]string{"INBOX|1", mailbox + "|1"}, listAllMessages(t, first))
	require.True(first.LabelsSnapshotComplete())
	saved := first.ObservedFolderStates()
	require.Len(saved, 2)
	assert.Equal([]uint32{1}, saved[mailbox].KnownUIDs)
	require.NoError(first.Close())

	second := newTestClient(t, addr, WithFolderStates(saved))
	assert.Empty(listAllMessages(t, second), "an unchanged account must not enumerate archived messages again")
	assert.True(second.LabelsSnapshotComplete())
	assert.Len(second.ObservedMailboxDeltas(), 2)

	second.mu.Lock()
	err := second.reconnect(t.Context())
	second.mu.Unlock()
	require.NoError(err)
	for _, connID := range []int{1, 2, 3} {
		commands := joinedCommands(server.commandsFor(connID))
		assert.Contains(commands, "ENABLE IMAP4REV2", "each connection negotiates its own encoding")
	}
}

// A saved Unicode mailbox baseline must still discover new mail and changed
// flags without returning unchanged messages or falling back to a full scan.
func TestUnicodeMailboxBaselineProcessesOnlyChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		uids    []imapv2.UID
		changed imapv2.UID
		uidNext uint32
	}{
		{name: "new message", uids: []imapv2.UID{1, 2, 3}, changed: 3, uidNext: 4},
		{name: "changed flags", uids: []imapv2.UID{1, 2}, changed: 2, uidNext: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			const mailbox = "Projects/Café"
			count := uint32(len(tc.uids))
			var mu sync.Mutex
			enabled := make(map[int]bool)
			addr, server := startQresyncTestServer(t, qresyncServerConfig{
				capabilities: []string{"IMAP4rev1 IMAP4rev2 ENABLE CONDSTORE QRESYNC"},
				mailboxes:    []string{mailbox},
				uidValidity:  77, uidNext: tc.uidNext, highestModSeq: 20,
				numMessages: &count, searchUIDs: tc.uids,
				fetchChanged: []imapv2.UID{tc.changed},
				fetchModSeq:  map[imapv2.UID]uint64{tc.changed: 20},
				commandHook: func(conn net.Conn, connID int, line string) bool {
					tag, command, _ := strings.Cut(line, " ")
					mu.Lock()
					defer mu.Unlock()
					if command == "ENABLE IMAP4rev2" {
						enabled[connID] = true
						_, _ = fmt.Fprintf(conn, "* ENABLED IMAP4rev2\r\n%s OK enabled\r\n", tag)
						return true
					}
					if strings.Contains(command, mailbox) && !enabled[connID] {
						_, _ = fmt.Fprintf(conn, "%s NO Mailbox does not exist\r\n", tag)
						return true
					}
					return false
				},
			})
			client := newQresyncTestClient(t, addr, map[string]FolderState{
				mailbox: {UIDValidity: 77, UIDNext: 3, HighestModSeq: 10, KnownUIDs: []uint32{1, 2}},
			})

			assert.Equal([]string{fmt.Sprintf("%s|%d", mailbox, tc.changed)}, listAllMessages(t, client))
			require.True(client.LabelsSnapshotComplete())
			deltas := client.ObservedMailboxDeltas()
			require.Len(deltas, 1)
			assert.False(deltas[0].Reset, "the existing baseline must be updated incrementally")
			assert.Equal([]imapv2.UID{tc.changed}, deltas[0].ChangedUIDs)
			assert.Equal(uint64(20), deltas[0].State.HighestModSeq)
			assert.Equal(tc.uidNext, deltas[0].State.UIDNext)
			assert.Len(deltas[0].State.KnownUIDs, len(tc.uids))
			commands := joinedCommands(server.commandsFor(1))
			assert.Contains(commands, "CHANGEDSINCE 10 VANISHED")
			assert.NotContains(commands, "UID SEARCH ALL")
		})
	}
}

func TestSingleRevisionServerNeedsNoRev2Negotiation(t *testing.T) {
	for _, capabilities := range []string{"IMAP4rev1", "IMAP4rev2"} {
		t.Run(capabilities, func(t *testing.T) {
			addr, server := startQresyncTestServer(t, qresyncServerConfig{
				capabilities: []string{capabilities},
			})
			client := newTestClient(t, addr)
			labels, err := client.ListLabels(t.Context())
			require.NoError(t, err)
			require.Len(t, labels, 1)
			assert.NotContains(t, joinedCommands(server.commandsFor(1)), "ENABLE")
		})
	}
}

func TestRev2EncodingNegotiationRejectsUnconfirmedMode(t *testing.T) {
	for _, response := range []string{"NO cannot enable", "OK ignored"} {
		t.Run(response, func(t *testing.T) {
			addr, _ := startQresyncTestServer(t, qresyncServerConfig{
				capabilities: []string{"IMAP4rev1 IMAP4rev2 ENABLE"},
				commandHook: func(conn net.Conn, _ int, line string) bool {
					tag, command, _ := strings.Cut(line, " ")
					if command != "ENABLE IMAP4rev2" {
						return false
					}
					_, _ = fmt.Fprintf(conn, "%s %s\r\n", tag, response)
					return true
				},
			})
			client := newTestClient(t, addr)
			_, err := client.ListLabels(t.Context())
			require.ErrorContains(t, err, "enable IMAP4rev2 mailbox encoding")
			assert.Nil(t, client.conn, "an unnegotiated connection must not be reused")
		})
	}
}
