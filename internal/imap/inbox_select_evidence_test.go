package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A pipelined NOOP's tagged access code and an unsolicited status cannot
// supply writable evidence to SELECT. Exercise the adopted SDK's real parser.
func TestInboxIMAPSelectModeBindsExactCommand(t *testing.T) {
	for _, mode := range []string{"READ-ONLY", ""} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			clientConn, serverConn := net.Pipe()
			requirements.NoError(clientConn.SetDeadline(time.Now().Add(10 * time.Second)))
			requirements.NoError(serverConn.SetDeadline(time.Now().Add(10 * time.Second)))
			t.Cleanup(func() { _ = serverConn.Close() })
			done := make(chan error, 1)
			go func() {
				if _, err := fmt.Fprint(serverConn, "* PREAUTH [CAPABILITY IMAP4rev1] synthetic server\r\n"); err != nil {
					done <- err
					return
				}
				reader := bufio.NewReader(serverConn)
				selectLine, err := reader.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				noopLine, err := reader.ReadString('\n')
				if err != nil {
					done <- err
					return
				}
				selected, command, _ := strings.Cut(strings.TrimSpace(selectLine), " ")
				noop, other, _ := strings.Cut(strings.TrimSpace(noopLine), " ")
				if !strings.HasPrefix(command, "SELECT ") || other != "NOOP" {
					done <- fmt.Errorf("unexpected native commands")
					return
				}
				code := ""
				if mode != "" {
					code = " [" + mode + "]"
				}
				_, err = fmt.Fprintf(serverConn, "%s OK [READ-WRITE] unrelated command\r\n* OK [READ-WRITE] unsolicited\r\n* 1 EXISTS\r\n* FLAGS (\\Seen)\r\n* OK [UIDVALIDITY 77] epoch\r\n%s OK%s selected\r\n", noop, selected, code)
				done <- err
			}()
			client := imapclient.New(clientConn, nil)
			t.Cleanup(func() { _ = client.Close() })
			requirements.NoError(client.WaitGreeting())
			selection := client.Select("INBOX", &imapapi.SelectOptions{})
			noop := client.Noop()
			data, err := selection.Wait()
			requirements.NoError(err)
			requirements.NoError(noop.Wait())
			requirements.NoError(<-done)
			requirements.NotNil(data)
			if mode == "" {
				assertions.Nil(data.ReadOnly)
			} else {
				requirements.NotNil(data.ReadOnly)
				assertions.True(*data.ReadOnly)
			}
		})
	}
}
