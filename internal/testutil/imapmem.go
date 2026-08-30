package testutil

import (
	"bytes"
	"fmt"
	"net"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
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

type statusErrorSession struct {
	imapserver.Session

	mailbox string
}

func (s *statusErrorSession) Status(
	mailbox string,
	options *imap.StatusOptions,
) (*imap.StatusData, error) {
	if mailbox == s.mailbox {
		return nil, fmt.Errorf("synthetic STATUS failure for %q", mailbox)
	}
	data, err := s.Session.Status(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("status %q: %w", mailbox, err)
	}
	return data, nil
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

// missingUIDSession leaves one UID out of every FETCH response for one
// mailbox, the way a real server answers a FETCH for a message another session
// expunged. The in-memory server cannot produce that shape on its own: it
// still returns the UID, with an empty body, which is a live message whose
// headers are missing rather than a message that left the mailbox.
type missingUIDSession struct {
	imapserver.Session

	config   *missingUIDConfig
	selected string
}

// missingUIDConfig is shared by every session the server opens, so the UID can
// be hidden after one run has already fetched it.
type missingUIDConfig struct {
	mailbox string
	uid     imap.UID
	hidden  atomic.Bool
}

func (s *missingUIDSession) Select(
	mailbox string,
	options *imap.SelectOptions,
) (*imap.SelectData, error) {
	data, err := s.Session.Select(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("select %q: %w", mailbox, err)
	}
	s.selected = mailbox
	return data, nil
}

func (s *missingUIDSession) Fetch(
	w *imapserver.FetchWriter,
	numSet imap.NumSet,
	options *imap.FetchOptions,
) error {
	fetch := func(set imap.NumSet) error {
		if err := s.Session.Fetch(w, set, options); err != nil {
			return fmt.Errorf("fetch %q: %w", s.selected, err)
		}
		return nil
	}
	uidSet, ok := numSet.(imap.UIDSet)
	if !ok || !s.config.hidden.Load() ||
		s.selected != s.config.mailbox || !uidSet.Contains(s.config.uid) {
		return fetch(numSet)
	}
	uids, static := uidSet.Nums()
	if !static {
		return fetch(numSet)
	}
	kept := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		if uid != s.config.uid {
			kept = append(kept, uid)
		}
	}
	if len(kept) == 0 {
		// Every requested UID is gone, so the response carries no messages.
		return nil
	}
	return fetch(imap.UIDSetNum(kept...))
}

// StartIMAPMemServerWithMissingUID runs an in-memory IMAP server that can
// leave one UID out of every FETCH response for one mailbox. LIST, STATUS and
// SEARCH still report it, so a run enumerates the UID and then cannot fetch
// it. That is the expunge race as a real server presents it.
//
// The returned function starts hiding the UID. Call it between two runs to
// make a message disappear after it was already archived.
func StartIMAPMemServerWithMissingUID(
	t *testing.T,
	messagesPerMailbox map[string]int,
	mailbox string,
	uid imap.UID,
) (string, *imapmemserver.User, func()) {
	t.Helper()
	config := &missingUIDConfig{mailbox: mailbox, uid: uid}
	addr, user := startIMAPMemServer(
		t, messagesPerMailbox, nil, "", 0, "", config)
	return addr, user, func() { config.hidden.Store(true) }
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
	AppendIMAPMessageAt(t, user, mailbox, messageBody, time.Time{})
}

// AppendIMAPMessageAt appends one synthetic RFC822 message with the supplied
// body and internal date to a mailbox of an in-memory IMAP test user.
func AppendIMAPMessageAt(
	t *testing.T,
	user *imapmemserver.User,
	mailbox string,
	messageBody string,
	internalDate time.Time,
) {
	t.Helper()
	body := fmt.Appendf(nil,
		"From: alice@example.com\r\nTo: bob@example.com\r\n\r\n%s\r\n",
		messageBody,
	)
	_, err := user.Append(mailbox, imapLiteral{bytes.NewReader(body)}, &imap.AppendOptions{
		Time: internalDate,
	})
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
	return startIMAPMemServer(t, messagesPerMailbox, nil, "", 0, "", nil)
}

// StartIMAPMemServerWithSpecialUse runs an in-memory IMAP server whose LIST
// responses advertise the supplied special-use attributes.
func StartIMAPMemServerWithSpecialUse(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(t, messagesPerMailbox, specialUse, "", 0, "", nil)
}

// StartIMAPMemServerWithStatusError runs an in-memory IMAP server that rejects
// STATUS for one mailbox while serving LIST, SELECT, SEARCH, and FETCH normally.
func StartIMAPMemServerWithStatusError(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	statusErrorMailbox string,
) (string, *imapmemserver.User) {
	t.Helper()
	return startIMAPMemServer(
		t, messagesPerMailbox, specialUse, "", 0, statusErrorMailbox, nil)
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
		t, messagesPerMailbox, specialUse, selectErrorMailbox, -1, "", nil)
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
		t, messagesPerMailbox, specialUse, selectErrorMailbox, 1, "", nil)
}

func startIMAPMemServer(
	t *testing.T,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
	selectErrorCount int,
	statusErrorMailbox string,
	missingUID *missingUIDConfig,
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
			if statusErrorMailbox != "" {
				session = &statusErrorSession{
					Session: session,
					mailbox: statusErrorMailbox,
				}
			}
			if missingUID != nil {
				session = &missingUIDSession{Session: session, config: missingUID}
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

// ExpungeIMAPMessage permanently removes one UID from a mailbox on a server
// returned by StartIMAPMemServer. The mailbox's STATUS MESSAGES count drops by
// one while UIDNEXT stays put — the shape of a change that a cursor without
// CONDSTORE cannot detect from UIDNEXT alone.
func ExpungeIMAPMessage(t *testing.T, addr, mailbox string, uid imap.UID) {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()

	require.NoError(t, client.Login(IMAPTestUsername, IMAPTestPassword).Wait())
	_, err = client.Select(mailbox, nil).Wait()
	require.NoError(t, err)

	var uidSet imap.UIDSet
	uidSet.AddNum(uid)
	require.NoError(t, client.Store(uidSet, &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagDeleted},
	}, nil).Close())
	require.NoError(t, client.Expunge().Close())
}
