package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the production FETCH reader through the actual go-imap wire parser.
// The memory server cannot emit missing items or CONDSTORE observations.
func observationWireClient(t *testing.T, response string) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] synthetic server\r\n")
		reader := bufio.NewReader(serverConn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch {
			case strings.HasPrefix(command, "UID FETCH "):
				fmt.Fprintf(serverConn, "%s%s OK FETCH done\r\n", response, tag)
			case command == "CAPABILITY":
				fmt.Fprintf(serverConn, "* CAPABILITY IMAP4rev1 CONDSTORE\r\n%s OK done\r\n", tag)
			default:
				fmt.Fprintf(serverConn, "%s BAD unexpected command\r\n", tag)
			}
		}
	}()
	conn := imapclient.New(clientConn, nil)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.WaitGreeting())
	return conn
}

func TestInboxIMAPFetchEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		modSeq         bool
		wantFlags      []string
		wantRevision   uint64
		wantError      bool
	}{
		{name: "empty flags", response: "* 1 FETCH (UID 7 FLAGS ())\r\n", wantFlags: []string{}},
		{name: "complete revision", response: "* 1 FETCH (UID 7 FLAGS (Todo \\Seen) MODSEQ (9))\r\n", modSeq: true, wantFlags: []string{"Todo", `\Seen`}, wantRevision: 9},
		{name: "missing flags", response: "* 1 FETCH (UID 7)\r\n", wantError: true},
		{name: "missing UID", response: "* 1 FETCH (FLAGS ())\r\n", wantError: true},
		{name: "wrong UID", response: "* 1 FETCH (UID 8 FLAGS () MODSEQ (9))\r\n", modSeq: true, wantError: true},
		{name: "missing MODSEQ", response: "* 1 FETCH (UID 7 FLAGS ())\r\n", modSeq: true, wantError: true},
		{name: "duplicate UID", response: "* 1 FETCH (UID 7 UID 7 FLAGS ())\r\n", wantError: true},
		{name: "duplicate FLAGS", response: "* 1 FETCH (UID 7 FLAGS (Old) FLAGS (New))\r\n", wantError: true},
		{name: "duplicate MODSEQ", response: "* 1 FETCH (UID 7 FLAGS () MODSEQ (9) MODSEQ (10))\r\n", modSeq: true, wantError: true},
		{name: "zero MODSEQ", response: "* 1 FETCH (UID 7 FLAGS () MODSEQ (0))\r\n", modSeq: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			flags, revision, err := fetchKeywordObservation(observationWireClient(t, tc.response), 7, tc.modSeq)
			if tc.wantError {
				requirements.Error(err)
				return
			}
			requirements.NoError(err)
			assertions.Equal(tc.wantFlags, flags)
			assertions.Equal(tc.wantRevision, revision)
		})
	}
}

// An observation of any other UID must never prove the requested message's flags.
func FuzzInboxIMAPExactObservation(f *testing.F) {
	f.Add(uint32(7), uint32(7))
	f.Add(uint32(7), uint32(8))
	f.Add(uint32(1), uint32(4294967295))
	f.Fuzz(func(t *testing.T, wanted, actual uint32) {
		if wanted == 0 || actual == 0 {
			return
		} // IMAP UIDs are nonzero.
		response := fmt.Sprintf("* 1 FETCH (UID %d FLAGS (Todo))\r\n", actual)
		flags, _, err := fetchKeywordObservation(observationWireClient(t, response), wanted, false)
		if wanted != actual {
			require.Error(t, err)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, []string{"Todo"}, flags)
	})
}
