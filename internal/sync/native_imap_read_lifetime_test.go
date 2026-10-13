package sync

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type nativeLifetimeReadSession struct {
	imapserver.Session

	mailbox string
	armed   *atomic.Bool
	entered chan<- struct{}
	release <-chan struct{}
}

func (s *nativeLifetimeReadSession) Select(mailbox string, opts *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	s.mailbox = mailbox
	selected, err := s.Session.Select(mailbox, opts)
	if err != nil {
		return nil, fmt.Errorf("select synthetic mailbox: %w", err)
	}
	return selected, nil
}
func (s *nativeLifetimeReadSession) Fetch(w *imapserver.FetchWriter, set imaplib.NumSet, opts *imaplib.FetchOptions) error {
	if s.mailbox == "Work" && len(opts.BodySection) > 0 && opts.BodySection[0].Specifier == "" && s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		<-s.release
	}
	if err := s.Session.Fetch(w, set, opts); err != nil {
		return fmt.Errorf("fetch synthetic message: %w", err)
	}
	return nil
}

// UIDVALIDITY alone cannot authorize a response after an independent positive
// removal/restoration rotated the native mailbox's permission lifetime.
func TestNativeIMAPReadCannotBorrowReplacementMailbox(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(strconv.FormatBool(replace), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
				{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}},
				{Family: "msgvault.source_message_archived", SourceType: "imap", Kinds: []string{"message"}},
				{Family: "msgvault.inbox_message_archived", SourceType: "imap", Kinds: []string{"message"}},
			}})
			require.NoError(err)
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			require.NoError(user.Create("INBOX", nil))
			require.NoError(user.Create("Work", nil))
			mem := imapmemserver.New()
			mem.AddUser(user)
			var armed atomic.Bool
			release, entered := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(err)
			server := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &nativeLifetimeReadSession{Session: mem.NewSession(), armed: &armed, entered: entered, release: release}, nil, nil
			}})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(func() { assert.NoError(server.Close()) })
			host, portText, err := net.SplitHostPort(listener.Addr().String())
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			cfg := &imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}
			opts := DefaultOptions()
			opts.SourceType = sourceTypeIMAP
			native := imapclient.NewClient(cfg, testutil.IMAPTestPassword)
			_, err = New(native, st, opts).Full(t.Context(), cfg.Identifier())
			require.NoError(err)
			states := native.ObservedFolderStates()
			require.NoError(native.Close())
			source, err := st.GetSourceByTypeAndIdentifier("imap", cfg.Identifier())
			require.NoError(err)
			require.NotNil(source)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			before, found, err := st.LookupNativeMessageScope(t.Context(), source.ID, resource.Reference, "imap_mailbox", "Work")
			require.NoError(err)
			require.True(found)
			testutil.AppendIMAPRawMessage(t, user, "Work", testemail.NewMessage().From("sender@example.net").Header("Message-ID", "<synthetic-stale-mailbox@example.net>").Body("Synthetic stale mailbox response").CRLF().Bytes())
			native = imapclient.NewClient(cfg, testutil.IMAPTestPassword, imapclient.WithFolderStates(states))
			t.Cleanup(func() { assert.NoError(native.Close()) })
			armed.Store(true)
			done := make(chan error, 1)
			go func() { _, err := New(native, st, opts).Full(t.Context(), cfg.Identifier()); done <- err }()
			select {
			case <-entered:
			case <-time.After(15 * time.Second):
				unblock()
				require.FailNow("native body request did not start")
			}
			observation := store.NativeMessageScopeInventory{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "imap", NativeIdentity: source.Identifier, Kind: "imap_mailbox", RetiredProviderIDs: []string{"Work"}}
			retireErr := st.ObserveNativeMessageScopes(t.Context(), observation)
			var replaceErr error
			if replace && retireErr == nil {
				observation.RetiredProviderIDs = nil
				observation.Scopes = []store.NativeMessageScope{{ProviderID: "Work", DisplayName: "Synthetic replacement mailbox", Epoch: before.Identity.Epoch}}
				replaceErr = st.ObserveNativeMessageScopes(t.Context(), observation)
			}
			unblock()
			require.NoError(retireErr)
			require.NoError(replaceErr)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				require.FailNow("native mailbox sync did not finish")
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Zero(count)
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Zero(count, "stale native lifetime rejects the complete message snapshot")
		})
	}
}
