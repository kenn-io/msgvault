package testutil

import (
	"bytes"
	"net"
	"testing"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/require"
)

// IMAPTestUsername and IMAPTestPassword are the credentials accepted by
// the server returned from StartIMAPMemServer.
const (
	IMAPTestUsername = "alice@example.com"
	IMAPTestPassword = "secret"
)

type imapLiteral struct {
	*bytes.Reader
}

func (l imapLiteral) Size() int64 { return int64(l.Len()) }

// AppendIMAPMessage appends one synthetic RFC822 message to a mailbox
// of an in-memory IMAP test user.
func AppendIMAPMessage(t *testing.T, user *imapmemserver.User, mailbox string) {
	t.Helper()
	body := []byte("From: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n")
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{})
	require.NoError(t, err)
}

// StartIMAPMemServer runs an in-memory IMAP server with the given
// mailboxes and per-mailbox message counts, returning its listen
// address and the user handle for later mutation. The server is shut
// down via t.Cleanup.
func StartIMAPMemServer(t *testing.T, messagesPerMailbox map[string]int) (string, *imapmemserver.User) {
	t.Helper()

	user := imapmemserver.NewUser(IMAPTestUsername, IMAPTestPassword)
	for mailbox, count := range messagesPerMailbox {
		require.NoError(t, user.Create(mailbox, nil))
		for range count {
			AppendIMAPMessage(t, user, mailbox)
		}
	}
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })

	return ln.Addr().String(), user
}
