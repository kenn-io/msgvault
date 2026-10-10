package imap

import (
	"bytes"
	"net"
	"sync/atomic"
	"testing"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Keep the real IMAP server and parser; inject only the selected access mode.
type inboxAccessListener struct {
	net.Listener

	mode string
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (l inboxAccessListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return inboxAccessConn{Conn: conn, mode: l.mode}, nil
}

type inboxAccessConn struct {
	net.Conn

	mode string
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func (c inboxAccessConn) Write(data []byte) (int, error) {
	replacement := []byte(" OK [" + c.mode + "]")
	if c.mode == "missing" {
		replacement = []byte(" OK")
	}
	transformed := bytes.ReplaceAll(data, []byte(" OK [READ-WRITE]"), replacement)
	n, err := c.Conn.Write(transformed)
	if n != len(transformed) || err != nil {
		return 0, err
	}
	return len(data), nil
}

//nolint:wrapcheck // This test adapter preserves native errors while injecting protocol evidence.
func TestInboxIMAPWritesRequireExplicitWritableSelection(t *testing.T) {
	for _, mode := range []string{"READ-ONLY", "missing", "READ-WRITE"} {
		for _, op := range []inboxcontrol.Operation{inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpTags} {
			t.Run(mode+"/"+string(op), func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				var stores atomic.Int64
				client, id := newKeywordTestClientFor(t, keywordTestSession{permanent: []imapapi.Flag{imapapi.FlagSeen, "Next"}, selectMode: mode, stores: &stores})
				source, target := inboxIMAPBinding(client, id)
				provider := NewInboxProvider(client, source)
				request := inboxcontrol.Request{Operation: op, Target: &target, DryRun: true}
				if op == inboxcontrol.OpTags {
					request.Tags = &emailtags.Change{Add: []string{"Next"}}
				}
				if op == inboxcontrol.OpSetUnread {
					// Seed via the real fixture server before exercising the controlled lane.
					requirements.NoError(client.withConn(t.Context(), func(conn *imapclient.Client) error {
						_, err := conn.Store(imapapi.UIDSetNum(1), &imapapi.StoreFlags{Op: imapapi.StoreFlagsAdd, Flags: []imapapi.Flag{imapapi.FlagSeen}}, nil).Collect()
						return err
					}))
					stores.Store(0)
				}
				before, err := provider.Observe(t.Context(), request)
				requirements.NoError(err)
				_, err = provider.Preview(t.Context(), request, before)
				requirements.NoError(err)
				_, err = provider.Dispatch(t.Context(), request, before)
				if mode == "READ-WRITE" {
					requirements.NoError(err)
					assertions.Equal(int64(1), stores.Load())
				} else {
					require.ErrorIs(t, err, inboxcontrol.ErrNoWrite)
					assertions.Zero(stores.Load())
					after, err := provider.Observe(t.Context(), request)
					requirements.NoError(err)
					assertions.ElementsMatch(before.Flags, after.Flags)
				}
			})
		}
	}
}
