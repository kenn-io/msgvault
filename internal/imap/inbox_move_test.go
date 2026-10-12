package imap

import (
	"bytes"
	"errors"
	"net"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil"
)

type inboxMoveSession struct {
	imapserver.Session
	moves, copies, expunges, creates *atomic.Int64
	omitCreatedEpoch                 bool
	moveFault                        string
	selected                         string
	archives                         []string
}

// The memory server has no public API for assigning SPECIAL-USE roles. This
// session supplies the extension's native LIST response while STATUS, SELECT
// and all mutations still use its real mailboxes.
func (s *inboxMoveSession) List(w *imapserver.ListWriter, ref string, patterns []string, opts *imapapi.ListOptions) error {
	if !opts.SelectSpecialUse {
		return s.Session.List(w, ref, patterns, opts)
	}
	for _, name := range s.archives {
		if err := w.WriteList(&imapapi.ListData{Mailbox: name, Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrArchive}}); err != nil {
			return err
		}
	}
	return nil
}

func (s *inboxMoveSession) Move(w *imapserver.MoveWriter, set imapapi.NumSet, dest string) error {
	s.moves.Add(1)
	if s.moveFault == "source remains" {
		// A broken server supplies genuine COPYUID without removing the source.
		// Native COPY is used only inside the server to construct this wire fault;
		// the production client still sends one MOVE and no fallback commands.
		data, err := s.Session.Copy(set, dest)
		if err != nil {
			return err
		}
		return w.WriteCopyData(data)
	}
	mover, ok := s.Session.(imapserver.SessionMove)
	if !ok {
		return errors.New("native fixture missing MOVE")
	}
	return mover.Move(w, set, dest)
}
func (s *inboxMoveSession) Select(name string, opts *imapapi.SelectOptions) (*imapapi.SelectData, error) {
	s.selected = name
	data, err := s.Session.Select(name, opts)
	if err == nil && name == "INBOX" && s.moves.Load() > 0 && s.moveFault == "changed origin epoch" {
		data.UIDValidity++
	}
	return data, err
}

func (s *inboxMoveSession) Fetch(w *imapserver.FetchWriter, set imapapi.NumSet, opts *imapapi.FetchOptions) error {
	if s.selected == "INBOX" && s.moves.Load() > 0 && s.moveFault == "source FETCH unavailable" {
		return &imapapi.Error{Type: imapapi.StatusResponseTypeNo, Text: "synthetic source readback failure"}
	}
	return s.Session.Fetch(w, set, opts)
}

func (s *inboxMoveSession) Copy(set imapapi.NumSet, dest string) (*imapapi.CopyData, error) {
	s.copies.Add(1)
	return s.Session.Copy(set, dest)
}
func (s *inboxMoveSession) Expunge(w *imapserver.ExpungeWriter, set *imapapi.UIDSet) error {
	s.expunges.Add(1)
	return s.Session.Expunge(w, set)
}
func (s *inboxMoveSession) Create(name string, opts *imapapi.CreateOptions) error {
	s.creates.Add(1)
	return s.Session.Create(name, opts)
}

func (s *inboxMoveSession) Status(name string, opts *imapapi.StatusOptions) (*imapapi.StatusData, error) {
	data, err := s.Session.Status(name, opts)
	if err == nil && s.omitCreatedEpoch && name == "Triage" {
		data.UIDValidity = 0
	}
	return data, err
}

type inboxFaultListener struct {
	net.Listener
	fault string
	moves *atomic.Int64
}

func (l inboxFaultListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return inboxFaultConn{Conn: conn, fault: l.fault, moves: l.moves}, nil
}

type inboxFaultConn struct {
	net.Conn
	fault string
	moves *atomic.Int64
}

var inboxCopyUIDCode = regexp.MustCompile(`\[COPYUID [^\]]+\]`)

func (c inboxFaultConn) Write(data []byte) (int, error) {
	if c.fault == "lost CREATE reply" && bytes.Contains(data, []byte("CREATE completed")) {
		_ = c.Conn.Close()
		return len(data), nil
	}
	changed := data
	// The memory server omits the session-only marker. Supply its native
	// destination FETCH effect while leaving MOVE, COPYUID and UID lookup real.
	if c.fault == "destination Recent" && c.moves.Load() > 0 && bytes.Contains(changed, []byte(" FETCH (")) {
		changed = bytes.ReplaceAll(changed, []byte("FLAGS ("), []byte("FLAGS (\\Recent "))
		changed = bytes.ReplaceAll(changed, []byte("FLAGS (\\Recent )"), []byte("FLAGS (\\Recent)"))
	}
	if c.fault == "read-only" {
		changed = bytes.ReplaceAll(changed, []byte(" OK [READ-WRITE]"), []byte(" OK [READ-ONLY]"))
	}
	if c.fault == "missing mode" {
		changed = bytes.ReplaceAll(changed, []byte(" OK [READ-WRITE]"), []byte(" OK"))
	}
	changed = inboxCopyUIDCode.ReplaceAllFunc(changed, func(code []byte) []byte {
		fields := strings.Fields(strings.Trim(string(code), "[]"))
		switch c.fault {
		case "missing COPYUID":
			return []byte("[ALERT missing mapping]")
		case "wrong source":
			fields[2] = "99"
		case "wrong epoch":
			fields[1] = "4294967295"
		case "multiple destinations":
			fields[3] = "3:4"
		case "huge destination range":
			fields[3] = "1:4294967295"
		case "lost MOVE reply":
			_ = c.Conn.Close()
			return nil
		}
		return []byte("[" + strings.Join(fields, " ") + "]")
	})
	if _, err := c.Conn.Write(changed); err != nil {
		return 0, err
	}
	return len(data), nil
}

type inboxNativeFixture struct {
	provider                         *InboxProvider
	source                           inboxcontrol.SourceIdentity
	target                           inboxcontrol.Target
	destination                      inboxcontrol.Folder
	moves, copies, expunges, creates atomic.Int64
}

func newInboxNativeFixture(t *testing.T, fault string) *inboxNativeFixture {
	t.Helper()
	fixture := &inboxNativeFixture{}
	user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
	require.NoError(t, user.Create("INBOX", nil))
	require.NoError(t, user.Create("Archive", nil))
	require.NoError(t, user.Create("Other", nil))
	testutil.AppendIMAPMessage(t, user, "INBOX")
	testutil.AppendIMAPMessage(t, user, "Archive")
	mem := imapmemserver.New()
	mem.AddUser(user)
	caps := imapapi.CapSet{imapapi.CapIMAP4rev1: {}, imapapi.CapMove: {}, imapapi.CapUIDPlus: {}}
	var archives []string
	if strings.HasPrefix(fault, "special") {
		caps[imapapi.CapSpecialUse] = struct{}{}
		archives = []string{"Archive"}
		if fault == "special ambiguous" {
			archives = append(archives, "Other")
		}
		if fault == "special empty" {
			archives = nil
		}
	}
	if fault == "no MOVE" {
		delete(caps, imapapi.CapMove)
	}
	if fault == "no UIDPLUS" {
		delete(caps, imapapi.CapUIDPlus)
	}
	server := imapserver.New(&imapserver.Options{Caps: caps, InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		return &inboxMoveSession{Session: mem.NewSession(), moves: &fixture.moves, copies: &fixture.copies, expunges: &fixture.expunges, creates: &fixture.creates, omitCreatedEpoch: fault == "missing created epoch", moveFault: fault, archives: archives}, nil, nil
	}})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = server.Serve(inboxFaultListener{Listener: listener, fault: fault, moves: &fixture.moves}) }()
	t.Cleanup(func() { _ = server.Close() })
	client := newTestClient(t, listener.Addr().String())
	var origin, dest *imapapi.StatusData
	require.NoError(t, client.withConn(t.Context(), func(conn *imapclient.Client) error {
		var err error
		origin, err = conn.Status("INBOX", &imapapi.StatusOptions{UIDValidity: true}).Wait()
		if err != nil {
			return err
		}
		dest, err = conn.Status("Archive", &imapapi.StatusOptions{UIDValidity: true}).Wait()
		return err
	}))
	fixture.source, fixture.target = inboxIMAPBinding(client, DraftReceipt{Mailbox: "INBOX", UIDValidity: origin.UIDValidity, UID: 1})
	fixture.destination = inboxcontrol.Folder{ID: "Archive", Name: "Archive", UIDValidity: dest.UIDValidity}
	fixture.provider = NewInboxProvider(client, fixture.source)
	return fixture
}

func TestInboxIMAPNativeMoveMapping(t *testing.T) {
	for _, fault := range []string{"native", "destination Recent", "no MOVE", "no UIDPLUS", "stale destination", "missing COPYUID", "wrong source", "wrong epoch", "multiple destinations", "huge destination range", "lost MOVE reply"} {
		t.Run(fault, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeFixture(t, fault)
			if fault == "stale destination" {
				f.destination.UIDValidity++
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &f.target, Destination: &f.destination, DryRun: true}
			before, err := f.provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := f.provider.Preview(t.Context(), request, before)
			if fault == "no MOVE" || fault == "no UIDPLUS" || fault == "stale destination" {
				requirements.Error(err)
				assertions.Zero(f.moves.Load())
				return
			}
			requirements.NoError(err)
			assertions.Zero(f.moves.Load())
			request.DryRun = false
			dispatch, err := f.provider.Dispatch(t.Context(), request, before)
			assertions.Equal(int64(1), f.moves.Load())
			assertions.Zero(f.copies.Load())
			assertions.Zero(f.expunges.Load())
			if fault != "native" && fault != "destination Recent" {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Nil(dispatch.Target)
				return
			}
			requirements.NoError(err)
			requirements.NotNil(dispatch.Target)
			want := f.target
			want.Mailbox = "Archive"
			want.UIDValidity = f.destination.UIDValidity
			want.UID = 2
			assertions.Equal(want, *dispatch.Target, "use returned destination UID, not Message-ID search")
			read := request
			read.Target = dispatch.Target
			after, err := f.provider.Observe(t.Context(), read)
			requirements.NoError(err)
			requirements.NoError(f.provider.Verify(read, before, projected, after))
			if fault == "destination Recent" {
				assertions.Contains(after.Flags, `\Recent`, "retain the native destination FETCH marker")
				assertions.NotContains(before.Flags, `\Recent`)
				assertions.NotContains(projected.Flags, `\Recent`)
			}
			requirements.NoError(f.provider.VerifyMoveSource(t.Context(), f.target))
			assertions.Equal(int64(1), f.moves.Load(), "independent verification never redispatches")
		})
	}
}

func TestInboxIMAPArchiveBindsNativeDestination(t *testing.T) {
	for _, tc := range []struct {
		name, fault, configured string
		wantError               bool
	}{
		{name: "name is not evidence", fault: "native", wantError: true},
		{name: "configured exact mailbox", fault: "native", configured: "Archive"},
		{name: "missing configured mailbox", fault: "native", configured: "Missing", wantError: true},
		{name: "cannot archive into inbox", fault: "native", configured: "INBOX", wantError: true},
		{name: "one special use archive", fault: "special"},
		{name: "ambiguous special use", fault: "special ambiguous", wantError: true},
		{name: "no special use archive", fault: "special empty", wantError: true},
		{name: "configuration overrides roles", fault: "special ambiguous", configured: "Archive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeFixture(t, tc.fault)
			if tc.configured != "" {
				data, err := f.provider.client.config.ToJSON()
				requirements.NoError(err)
				cfg, err := ConfigFromJSON(strings.TrimSuffix(data, "}") + `,"archive_mailbox":"` + tc.configured + `"}`)
				requirements.NoError(err)
				f.provider.client.config = cfg
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &f.target, DryRun: true}
			before, err := f.provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := f.provider.Preview(t.Context(), request, before)
			assertions.Zero(f.moves.Load())
			assertions.Zero(f.copies.Load())
			assertions.Zero(f.expunges.Load())
			assertions.Zero(f.creates.Load())
			if tc.wantError {
				requirements.Error(err)
				return
			}
			requirements.NoError(err)
			assertions.Equal([]inboxcontrol.Folder{f.destination}, projected.Folders)
			assertions.Equal("Archive", projected.Location)
			assertions.Equal(f.target, projected.Target, "origin identity remains signed")
			assertions.False(*projected.Inbox)
		})
	}
}

func TestInboxIMAPMoveRequiresIndependentSourceAbsence(t *testing.T) {
	for _, fault := range []string{"source remains", "changed origin epoch", "source FETCH unavailable"} {
		t.Run(fault, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newInboxNativeFixture(t, fault)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &f.target, Destination: &f.destination, DryRun: true}
			before, err := f.provider.Observe(t.Context(), request)
			requirements.NoError(err)
			projected, err := f.provider.Preview(t.Context(), request, before)
			requirements.NoError(err)
			dispatch, err := f.provider.Dispatch(t.Context(), request, before)
			requirements.NoError(err)
			requirements.NotNil(dispatch.Target)
			read := request
			read.Target = dispatch.Target
			after, err := f.provider.Observe(t.Context(), read)
			requirements.NoError(err)
			requirements.NoError(f.provider.Verify(read, before, projected, after), "destination alone can look complete")
			requirements.ErrorIs(f.provider.VerifyMoveSource(t.Context(), f.target), inboxcontrol.ErrOutcomeUnknown)
			assertions.Equal(int64(1), f.moves.Load())
			assertions.Zero(f.copies.Load())
			assertions.Zero(f.expunges.Load())
		})
	}
}
