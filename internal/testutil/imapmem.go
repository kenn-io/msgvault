package testutil

import (
	"bytes"
	"fmt"
	"net"
	"sort"
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

type specialUseSession struct {
	imapserver.Session

	mailboxes  []string
	specialUse map[string][]imap.MailboxAttr
}

type selectErrorSession struct {
	imapserver.Session

	mailbox   string
	remaining int
}

func (s *selectErrorSession) Select(
	mailbox string,
	options *imap.SelectOptions,
) (*imap.SelectData, error) {
	if mailbox == s.mailbox && s.remaining != 0 {
		if s.remaining > 0 {
			s.remaining--
		}
		return nil, fmt.Errorf("synthetic SELECT failure for %q", mailbox)
	}
	data, err := s.Session.Select(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("select %q: %w", mailbox, err)
	}
	return data, nil
}

func (s *specialUseSession) List(
	w *imapserver.ListWriter,
	ref string,
	patterns []string,
	_ *imap.ListOptions,
) error {
	for _, mailbox := range s.mailboxes {
		matches := false
		for _, pattern := range patterns {
			if imapserver.MatchList(mailbox, '/', ref, pattern) {
				matches = true
				break
			}
		}
		if !matches {
			continue
		}
		if err := w.WriteList(&imap.ListData{
			Mailbox: mailbox,
			Delim:   '/',
			Attrs:   s.specialUse[mailbox],
		}); err != nil {
			return fmt.Errorf("write LIST response for %q: %w", mailbox, err)
		}
	}
	return nil
}

// AppendIMAPMessage appends one synthetic RFC822 message to a mailbox
// of an in-memory IMAP test user.
func AppendIMAPMessage(t *testing.T, user *imapmemserver.User, mailbox string) {
	t.Helper()
	AppendIMAPMessageWithoutMessageID(t, user, mailbox, "body")
}

// AppendIMAPMessageWithoutMessageID appends one synthetic RFC822 message with
// the supplied body and no Message-ID header.
func AppendIMAPMessageWithoutMessageID(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	messageBody string,
) {
	t.Helper()
	body := fmt.Appendf(nil,
		"From: alice@example.com\r\nTo: bob@example.com\r\n\r\n%s\r\n",
		messageBody,
	)
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{})
	require.NoError(t, err)
}

// AppendIMAPMessageWithMessageID appends one synthetic RFC822 message with
// the supplied Message-ID to a mailbox of an in-memory IMAP test user.
func AppendIMAPMessageWithMessageID(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	messageID string,
) {
	t.Helper()
	body := fmt.Appendf(nil,
		"Message-ID: <%s>\r\nFrom: alice@example.com\r\nTo: bob@example.com\r\n\r\nbody\r\n",
		messageID,
	)
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{})
	require.NoError(t, err)
}

// StartIMAPMemServer runs an in-memory IMAP server with the given
// mailboxes and per-mailbox message counts, returning its listen
// address and the user handle for later mutation. The server is shut
// down via t.Cleanup.
func StartIMAPMemServer(t *testing.T, messagesPerMailbox map[string]int) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0)
}

// StartIMAPMemServerWithSpecialUse runs an in-memory IMAP server whose LIST
// responses advertise the supplied special-use attributes.
func StartIMAPMemServerWithSpecialUse(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, specialUse, "", 0)
}

// StartIMAPMemServerWithSelectError runs an in-memory IMAP server that rejects
// SELECT for one mailbox while serving all other mailboxes normally.
func StartIMAPMemServerWithSelectError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(
		t, messagesPerMailbox, specialUse, selectErrorMailbox, -1)
}

// StartIMAPMemServerWithOneShotSelectError runs an in-memory IMAP server that
// rejects the first SELECT for one mailbox, then serves it normally.
func StartIMAPMemServerWithOneShotSelectError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(
		t, messagesPerMailbox, specialUse, selectErrorMailbox, 1)
}

func startIMAPMemServer(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
	selectErrorCount int,
) (string, *imapmemserver.User) {
	t.Helper()
	user := imapmemserver.NewUser(IMAPTestUsername, IMAPTestPassword)
	mailboxes := make([]string, 0, len(messagesPerMailbox))
	for mailbox, count := range messagesPerMailbox {
		require.NoError(t, user.Create(mailbox, nil))
		mailboxes = append(mailboxes, mailbox)
		for range count {
			AppendIMAPMessage(t, user, mailbox)
		}
	}
	sort.Strings(mailboxes)
	memServer := imapmemserver.New()
	memServer.AddUser(user)

	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			var session imapserver.Session
			session = memServer.NewSession()
			if len(specialUse) > 0 {
				session = &specialUseSession{
					Session:    session,
					mailboxes:  mailboxes,
					specialUse: specialUse,
				}
			}
			if selectErrorMailbox != "" {
				session = &selectErrorSession{
					Session:   session,
					mailbox:   selectErrorMailbox,
					remaining: selectErrorCount,
				}
			}
			return session, nil, nil
		},
		InsecureAuth: true,
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })

	return ln.Addr().String(), user
}
