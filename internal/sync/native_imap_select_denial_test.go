package sync

import (
	"fmt"
	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/testutil"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
)

type nativeScopeSelectSession struct {
	imapserver.Session

	code *atomic.Value
}

func (s *nativeScopeSelectSession) Select(mailbox string, options *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	if code, _ := s.code.Load().(string); mailbox == "Work" && code != "" {
		return nil, &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCode(code), Text: "synthetic native selection failure"}
	}
	selected, err := s.Session.Select(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("select synthetic mailbox: %w", err)
	}
	return selected, nil
}

// The actual native server sends typed failures through its IMAP wire parser.
// In particular, a temporary failure cannot be substituted for permission loss.
func TestNativeIMAPMailboxDenialRetiresOnlyObservedScope(t *testing.T) {
	for _, mode := range []struct {
		code           string
		filtered, deny bool
	}{{"NOPERM", false, true}, {"NONEXISTENT", false, true}, {"SERVERBUG", false, false}, {"NOPERM", true, true}} {
		name := mode.code
		if mode.filtered {
			name += "/partial"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			env := newTestEnv(t)
			configureNativeIMAPEventCapture(t, env.Store)
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			for _, mailbox := range []string{"INBOX", "Work", "Quiet"} {
				require.NoError(user.Create(mailbox, nil))
			}
			mem := imapmemserver.New()
			mem.AddUser(user)
			var code atomic.Value
			code.Store("")
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(err)
			server := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &nativeScopeSelectSession{Session: mem.NewSession(), code: &code}, nil, nil
			}})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { Assert.NoError(t, server.Close()) })
			host, portText, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			cfg := &imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
			opts := DefaultOptions()
			opts.SourceType = sourceTypeIMAP
			run := func(filtered bool) {
				var nativeOpts []imapclient.Option
				if filtered {
					nativeOpts = append(nativeOpts, imapclient.WithFolderFilter([]string{"Work"}, nil))
				}
				native := imapclient.NewClient(cfg, testutil.IMAPTestPassword, nativeOpts...)
				defer func() { Assert.NoError(t, native.Close()) }()
				_, runErr := New(native, env.Store, opts).Full(t.Context(), cfg.Identifier())
				if observed, _ := code.Load().(string); observed == "" {
					Require.NoError(t, runErr)
				}
			}
			run(false)
			var workID, quietID int64
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Work'`).Scan(&workID))
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Quiet'`).Scan(&quietID))
			before, err := env.Store.GetMCPResource(t.Context(), "inbox", workID)
			require.NoError(err)
			quiet, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
			require.NoError(err)
			code.Store(mode.code)
			run(mode.filtered)
			_, err = env.Store.GetMCPResource(t.Context(), "inbox", workID)
			if mode.deny {
				require.Error(err, "positive native denial must retire this exact mailbox even in partial scans")
			} else {
				require.NoError(err, "transient server failure cannot prove removal")
			}
			currentQuiet, err := env.Store.GetMCPResource(t.Context(), "inbox", quietID)
			require.NoError(err)
			assert.Equal(quiet.Reference, currentQuiet.Reference, "one denied mailbox does not retire quiet unrelated mailboxes")
			code.Store("")
			run(false)
			after, err := env.Store.GetMCPResource(t.Context(), "inbox", workID)
			require.NoError(err)
			if mode.deny {
				assert.NotEqual(before.Reference, after.Reference)
			} else {
				assert.Equal(before.Reference, after.Reference)
			}
		})
	}
}
