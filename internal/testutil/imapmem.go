package testutil

import (
	"bytes"
	"crypto/tls"
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
	// hideRemaining is negative to hide the UID from every FETCH, zero to hide
	// it from none, and positive to hide it from that many more responses.
	hideRemaining atomic.Int64
	// expunged makes the message leave the mailbox for good the moment a FETCH
	// first leaves it out, so SEARCH stops reporting it too. Until then SEARCH
	// must still report the UID: a run has to enumerate a message before it can
	// watch it vanish from the fetch it then asks for.
	expunged     atomic.Bool
	searchHidden atomic.Bool
}

// takeHide reports whether this FETCH response must leave the UID out, and
// spends one of a limited budget when the budget is what allows it.
func (c *missingUIDConfig) takeHide() bool {
	for {
		remaining := c.hideRemaining.Load()
		if remaining == 0 {
			return false
		}
		if remaining < 0 {
			return true
		}
		if c.hideRemaining.CompareAndSwap(remaining, remaining-1) {
			return true
		}
	}
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
	if !ok || s.selected != s.config.mailbox || !uidSet.Contains(s.config.uid) {
		return fetch(numSet)
	}
	uids, static := uidSet.Nums()
	if !static {
		return fetch(numSet)
	}
	// Spend the budget only once the response is known to be one that would
	// have carried the UID, so a one-shot omission is not used up elsewhere.
	if !s.config.takeHide() {
		return fetch(numSet)
	}
	if s.config.expunged.Load() {
		s.config.searchHidden.Store(true)
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

// Search drops the UID once a FETCH has reported the message gone, which is
// what a real server does after an expunge. A server that answers a FETCH
// without the UID and still lists it in every SEARCH is describing a live
// message the fetch dropped, not one that left the mailbox --
// StartIMAPMemServerOmittingUIDFromEveryFetch is that server.
func (s *missingUIDSession) Search(
	kind imapserver.NumKind,
	criteria *imap.SearchCriteria,
	options *imap.SearchOptions,
) (*imap.SearchData, error) {
	data, err := s.Session.Search(kind, criteria, options)
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", s.selected, err)
	}
	if s.selected != s.config.mailbox || !s.config.searchHidden.Load() {
		return data, nil
	}
	uidSet, ok := data.All.(imap.UIDSet)
	if !ok || !uidSet.Contains(s.config.uid) {
		return data, nil
	}
	uids, static := uidSet.Nums()
	if !static {
		return data, nil
	}
	var kept imap.UIDSet
	filtered := imap.SearchData{ModSeq: data.ModSeq}
	for _, uid := range uids {
		if uid == s.config.uid {
			continue
		}
		kept.AddNum(uid)
		if filtered.Min == 0 || uint32(uid) < filtered.Min {
			filtered.Min = uint32(uid)
		}
		if uint32(uid) > filtered.Max {
			filtered.Max = uint32(uid)
		}
		filtered.Count++
	}
	filtered.All = kept
	return &filtered, nil
}

// StartIMAPMemServerWithMissingUID runs an in-memory IMAP server that can
// leave one UID out of every FETCH response for one mailbox. SEARCH reports
// the UID until the first FETCH that omits it and never again, so a run
// enumerates the message, cannot fetch it, and finds it gone when it asks.
// That is the expunge race as a real server presents it.
//
// The returned function sets whether the UID is hidden. Call it between two
// runs to make a message disappear, or to bring it back.
func StartIMAPMemServerWithMissingUID(
	t *testing.T,
	messagesPerMailbox map[string]int,
	mailbox string,
	uid imap.UID,
) (string, *imapmemserver.User, func(hidden bool)) {
	t.Helper()
	config := &missingUIDConfig{mailbox: mailbox, uid: uid}
	config.expunged.Store(true)
	addr, user := startIMAPMemServer(
		t, messagesPerMailbox, nil, "", 0, "", config)
	return addr, user, func(hidden bool) {
		if hidden {
			config.hideRemaining.Store(-1)
			return
		}
		config.hideRemaining.Store(0)
		config.searchHidden.Store(false)
	}
}

// StartIMAPMemServerOmittingUIDFromEveryFetch runs an in-memory IMAP server
// that never returns one UID from a FETCH of one mailbox and keeps reporting
// it in every SEARCH.
//
// The message is still in the mailbox: the server drops it from the fetch for
// its own reasons, and repeating the same fetch cannot tell the two apart.
// StartIMAPMemServerWithMissingUID is the other shape, where the message
// really left.
func StartIMAPMemServerOmittingUIDFromEveryFetch(
	t *testing.T,
	messagesPerMailbox map[string]int,
	mailbox string,
	uid imap.UID,
) (string, *imapmemserver.User) {
	t.Helper()
	config := &missingUIDConfig{mailbox: mailbox, uid: uid}
	config.hideRemaining.Store(-1)
	addr, user := startIMAPMemServer(
		t, messagesPerMailbox, nil, "", 0, "", config)
	return addr, user
}

// StartIMAPMemServerOmittingUIDOnce runs an in-memory IMAP server that leaves
// one UID out of the next FETCH response that would have carried it, and
// returns it on every later fetch.
//
// That is a live message the server dropped from one response, not an expunge.
// A run that believes the first omission loses it; a run that asks again finds
// it. StartIMAPMemServerWithMissingUID is the other shape, where the message
// really is gone and stays gone.
func StartIMAPMemServerOmittingUIDOnce(
	t *testing.T,
	messagesPerMailbox map[string]int,
	mailbox string,
	uid imap.UID,
) (string, *imapmemserver.User) {
	t.Helper()
	config := &missingUIDConfig{mailbox: mailbox, uid: uid}
	config.hideRemaining.Store(1)
	addr, user := startIMAPMemServer(
		t, messagesPerMailbox, nil, "", 0, "", config)
	return addr, user
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

// ServeIMAPMemServer serves an in-memory IMAP server on the caller-supplied
// listener and returns the user handle for later mutation.
func ServeIMAPMemServer(
	t *testing.T,
	ln net.Listener,
	messagesPerMailbox map[string]int,
	startTLSConfig *tls.Config,
) *imapmemserver.User {
	t.Helper()
	return serveIMAPMemServer(t, ln, messagesPerMailbox, nil, "", 0, "", nil, startTLSConfig)
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	user := serveIMAPMemServer(
		t, ln, messagesPerMailbox, specialUse, selectErrorMailbox,
		selectErrorCount, statusErrorMailbox, missingUID, nil)
	return ln.Addr().String(), user
}

func serveIMAPMemServer(
	t *testing.T,
	ln net.Listener,
	messagesPerMailbox map[string]int,
	specialUse map[string][]imap.MailboxAttr,
	selectErrorMailbox string,
	selectErrorCount int,
	statusErrorMailbox string,
	missingUID *missingUIDConfig,
	startTLSConfig *tls.Config,
) *imapmemserver.User {
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
		TLSConfig:    startTLSConfig,
	})

	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() { _ = server.Close() })

	return user
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
