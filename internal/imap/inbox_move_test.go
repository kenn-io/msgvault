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

// Counters detect client fallback commands while retaining real server behavior.
type inboxMoveSession struct {
	imapserver.Session

	moves, copies, expunges *atomic.Int64
	archiveMailboxes        []string
}

// The memory backend ignores CREATE SpecialUse and has no setter. Supply only
// that extension metadata; mailbox status, messages and MOVE remain native.
//
//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (s *inboxMoveSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *imapapi.ListOptions) error {
	if !options.SelectSpecialUse {
		return s.Session.List(w, ref, patterns, options)
	}
	for _, mailbox := range s.archiveMailboxes {
		if err := w.WriteList(&imapapi.ListData{Mailbox: mailbox, Delim: '/', Attrs: []imapapi.MailboxAttr{imapapi.MailboxAttrArchive}}); err != nil {
			return err
		}
	}
	return nil
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (s *inboxMoveSession) Move(w *imapserver.MoveWriter, set imapapi.NumSet, dest string) error {
	s.moves.Add(1)
	mover, ok := s.Session.(imapserver.SessionMove)
	if !ok {
		return errors.New("native test session does not implement MOVE")
	}
	return mover.Move(w, set, dest)
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (s *inboxMoveSession) Copy(set imapapi.NumSet, dest string) (*imapapi.CopyData, error) {
	s.copies.Add(1)
	return s.Session.Copy(set, dest)
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (s *inboxMoveSession) Expunge(w *imapserver.ExpungeWriter, set *imapapi.UIDSet) error {
	s.expunges.Add(1)
	return s.Session.Expunge(w, set)
}

// Alter only the native server COPYUID response. The actual MOVE still runs;
// this exercises the client parser after a successful write with bad evidence.
type inboxMoveFaultListener struct {
	net.Listener

	mapping string
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (l inboxMoveFaultListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return inboxMoveFaultConn{Conn: conn, mapping: l.mapping}, nil
}

type inboxMoveFaultConn struct {
	net.Conn

	mapping string
}

var inboxCopyUIDCode = regexp.MustCompile(`\[COPYUID [^\]]+\]`)

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (c inboxMoveFaultConn) Write(data []byte) (int, error) {
	if !bytes.Contains(data, []byte("[COPYUID ")) {
		return c.Conn.Write(data)
	}
	altered := inboxCopyUIDCode.ReplaceAllFunc(data, func(code []byte) []byte {
		fields := strings.Fields(strings.Trim(string(code), "[]"))
		switch c.mapping {
		case "wrong source UID":
			fields[2] = "99"
		case "multiple destination UIDs":
			fields[3] = "3:4"
		default:
			return []byte("[ALERT synthetic missing mapping]")
		}
		return []byte("[" + strings.Join(fields, " ") + "]")
	})
	_, err := c.Conn.Write(altered)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func TestInboxIMAPNativeMoveUsesExactDestinationUID(t *testing.T) {
	for _, name := range []string{"native", "no MOVE", "no UIDPLUS", "stale destination", "stale INBOX unarchive", "unarchive to Trash", "missing COPYUID", "wrong source UID", "multiple destination UIDs", "configured archive", "missing archive", "explicit unarchive", "special-use archive", "ambiguous archive", "bound archive", "read-only SELECT", "missing SELECT mode"} {
		fault := name == "missing COPYUID" || name == "wrong source UID" || name == "multiple destination UIDs"
		supported := name == "native" || name == "read-only SELECT" || name == "missing SELECT mode" || fault || name == "configured archive" || name == "explicit unarchive" || name == "special-use archive" || name == "bound archive"
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			for _, mailbox := range []string{"INBOX", "Archive", "Trash"} {
				requirements.NoError(user.Create(mailbox, nil))
			}
			testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", "duplicate@example.com")
			for range 2 {
				testutil.AppendIMAPMessageWithMessageID(t, user, "Archive", "duplicate@example.com")
			}
			mem := imapmemserver.New()
			mem.AddUser(user)
			var moves, copies, expunges atomic.Int64
			caps := imapapi.CapSet{imapapi.CapIMAP4rev1: {}}
			if name == "special-use archive" || name == "ambiguous archive" {
				caps[imapapi.CapSpecialUse] = struct{}{}
			}
			if name != "no UIDPLUS" {
				caps[imapapi.CapUIDPlus] = struct{}{}
			}
			if name != "no MOVE" {
				caps[imapapi.CapMove] = struct{}{}
			}
			var archives []string
			if name == "special-use archive" {
				archives = []string{"Archive"}
			}
			if name == "ambiguous archive" {
				archives = []string{"Archive", "INBOX"}
			}
			srv := imapserver.New(&imapserver.Options{InsecureAuth: true, Caps: caps, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &inboxMoveSession{Session: mem.NewSession(), moves: &moves, copies: &copies, expunges: &expunges, archiveMailboxes: archives}, nil, nil
			}})
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			requirements.NoError(err)
			var serverListener = listener
			if name == "read-only SELECT" {
				serverListener = inboxAccessListener{Listener: listener, mode: "READ-ONLY"}
			}
			if name == "missing SELECT mode" {
				serverListener = inboxAccessListener{Listener: listener, mode: "missing"}
			}
			if fault {
				serverListener = inboxMoveFaultListener{Listener: listener, mapping: name}
			}
			go func() { _ = srv.Serve(serverListener) }()
			t.Cleanup(func() { _ = srv.Close() })
			client := newTestClient(t, listener.Addr().String())
			var origin, dest, trash *imapapi.SelectData
			requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
				var err error
				origin, err = conn.Select("INBOX", &imapapi.SelectOptions{ReadOnly: true}).Wait()
				if err != nil {
					return err
				}
				dest, err = conn.Select("Archive", &imapapi.SelectOptions{ReadOnly: true}).Wait()
				if err != nil {
					return err
				}
				trash, err = conn.Select("Trash", &imapapi.SelectOptions{ReadOnly: true}).Wait()
				return err
			}))
			mailbox, epoch, uid := "INBOX", origin.UIDValidity, uint32(1)
			if name == "explicit unarchive" || name == "stale INBOX unarchive" || name == "unarchive to Trash" {
				mailbox, epoch, uid = "Archive", dest.UIDValidity, 1
			}
			source, target := inboxIMAPBinding(client, KeywordIdentity{Mailbox: mailbox, UIDValidity: epoch, UID: uid})
			provider := NewInboxProvider(client, source)
			request := inboxcontrol.Request{Operation: inboxcontrol.OpMove, Target: &target, Destination: &inboxcontrol.Folder{ID: "Archive", UIDValidity: dest.UIDValidity}, DryRun: true}
			if name == "configured archive" || name == "missing archive" || name == "special-use archive" || name == "ambiguous archive" || name == "bound archive" {
				request.Operation, request.Destination = inboxcontrol.OpArchive, nil
				if name == "configured archive" || name == "bound archive" {
					client.config.ArchiveMailbox = "Archive"
				}
			}
			if name == "explicit unarchive" {
				request.Operation = inboxcontrol.OpUnarchive
				request.Destination = &inboxcontrol.Folder{ID: "INBOX", UIDValidity: origin.UIDValidity}
			}
			if name == "stale INBOX unarchive" {
				request.Operation = inboxcontrol.OpUnarchive
				request.Destination = &inboxcontrol.Folder{ID: "INBOX", UIDValidity: origin.UIDValidity + 1}
			}
			if name == "unarchive to Trash" {
				request.Operation = inboxcontrol.OpUnarchive
				request.Destination = &inboxcontrol.Folder{ID: "Trash", UIDValidity: trash.UIDValidity}
			}
			if name == "stale destination" {
				request.Destination.UIDValidity++
			}
			observedCaps, err := provider.Capabilities(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source})
			requirements.NoError(err)
			for _, capability := range observedCaps.Operations {
				if capability.Operation == inboxcontrol.OpMove {
					want := inboxcontrol.CapabilitySupported
					if name == "no MOVE" || name == "no UIDPLUS" {
						want = inboxcontrol.CapabilityUnsupported
					}
					assertions.Equal(want, capability.Status)
				}
			}
			assertions.Zero(moves.Load())
			assertions.Zero(copies.Load())
			assertions.Zero(expunges.Load())
			before, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			requirements.ErrorIs(provider.VerifyMoveSource(t.Context(), target), inboxcontrol.ErrOutcomeUnknown)
			projected, err := provider.Preview(t.Context(), request, before)
			if !supported {
				if name == "stale destination" || name == "stale INBOX unarchive" {
					requirements.ErrorIs(err, inboxcontrol.ErrPlanChanged)
				} else {
					requirements.ErrorIs(err, inboxcontrol.ErrUnavailable)
				}
				_, err = provider.Dispatch(t.Context(), request, before)
				requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
				if name == "stale INBOX unarchive" {
					_, err = provider.dispatchMove(t.Context(), request, before)
					requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
				}
				assertions.Zero(moves.Load())
				assertions.Zero(copies.Load())
				assertions.Zero(expunges.Load())
				return
			}
			requirements.NoError(err)
			unchanged, err := provider.Observe(t.Context(), request)
			requirements.NoError(err)
			assertions.Equal(name != "explicit unarchive", *unchanged.Inbox)
			if name == "bound archive" {
				request.ResolvedFolder = &projected.Folders[0]
				client.config.ArchiveMailbox = "Other Archive"
			}
			mapping, err := provider.Dispatch(t.Context(), request, before)
			if name == "read-only SELECT" || name == "missing SELECT mode" {
				requirements.ErrorIs(err, inboxcontrol.ErrNoWrite)
				assertions.Nil(mapping.Target)
				assertions.Zero(moves.Load())
				assertions.Zero(copies.Load())
				assertions.Zero(expunges.Load())
				after, err := provider.Observe(t.Context(), request)
				requirements.NoError(err)
				assertions.Equal(before.Target, after.Target)
				return
			}
			if fault {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Nil(mapping.Target)
				_, err = provider.Observe(t.Context(), request)
				requirements.Error(err)
				actual := target
				actual.Mailbox = "Archive"
				actual.UIDValidity = dest.UIDValidity
				actual.UID = 3
				readback := request
				readback.Target = &actual
				_, err = provider.Observe(t.Context(), readback)
				requirements.NoError(err)
				assertions.Equal(int64(1), moves.Load())
				assertions.Zero(copies.Load())
				assertions.Zero(expunges.Load())
				return
			}
			requirements.NoError(err)
			requirements.NotNil(mapping.Target)
			wantMailbox, wantUID, wantEpoch := "Archive", uint32(3), dest.UIDValidity
			if name == "explicit unarchive" {
				wantMailbox, wantUID, wantEpoch = "INBOX", 2, origin.UIDValidity
			}
			assertions.Equal(wantMailbox, mapping.Target.Mailbox)
			assertions.Equal(wantUID, mapping.Target.UID)
			assertions.Equal(wantEpoch, mapping.Target.UIDValidity)
			readback := request
			readback.Target = mapping.Target
			after, err := provider.Observe(t.Context(), readback)
			requirements.NoError(err)
			requirements.NoError(provider.Verify(readback, before, projected, after))
			requirements.NoError(provider.VerifyMoveSource(t.Context(), target))
			assertions.Equal(name == "explicit unarchive", *after.Inbox)
			assertions.ElementsMatch(before.Flags, after.Flags)
			_, err = provider.Observe(t.Context(), request)
			requirements.Error(err)
			assertions.Equal(int64(1), moves.Load())
			assertions.Zero(copies.Load())
			assertions.Zero(expunges.Load())
		})
	}
}
