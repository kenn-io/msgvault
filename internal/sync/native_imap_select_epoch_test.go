package sync

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"sync/atomic"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type nativeEpochLiteral struct{ *bytes.Reader }

func (l nativeEpochLiteral) Size() int64 { return int64(l.Len()) }

type nativeEpochSelectSession struct {
	imapserver.Session

	resetAt      int
	user         *imapmemserver.User
	armed, reset *atomic.Bool
	workSelects  int
	replacement  []byte
}

func (s *nativeEpochSelectSession) Select(mailbox string, options *imaplib.SelectOptions) (*imaplib.SelectData, error) {
	if mailbox == "Work" {
		s.workSelects++
		// The first EXAMINE is the complete native inventory. The second SELECT
		// happens while the native client enumerates this run's changed UIDs.
		if s.workSelects == s.resetAt && s.armed.CompareAndSwap(true, false) {
			if err := s.user.Delete("Work"); err != nil {
				return nil, fmt.Errorf("reset synthetic mailbox: %w", err)
			}
			if err := s.user.Create("Work", nil); err != nil {
				return nil, fmt.Errorf("recreate synthetic mailbox: %w", err)
			}
			if len(s.replacement) > 0 {
				if _, err := s.user.Append("Work", nativeEpochLiteral{bytes.NewReader(s.replacement)}, &imaplib.AppendOptions{}); err != nil {
					return nil, fmt.Errorf("append synthetic replacement: %w", err)
				}
			}
			s.reset.Store(true)
		}
	}
	selected, err := s.Session.Select(mailbox, options)
	if err != nil {
		return nil, fmt.Errorf("select synthetic mailbox: %w", err)
	}
	return selected, nil
}

// Keeping only the pre-listing inventory leaves old receipts/resources valid
// after native SELECT has already proven that the mailbox epoch was retired.
func TestNativeIMAPSelectEpochResetRetiresResourceInSameSync(t *testing.T) {
	for _, mode := range []struct {
		name        string
		replacement bool
		resetAt     int
	}{{"listing_empty", false, 2}, {"listing_replacement", true, 2}, {"fetch_empty", false, 3}, {"fetch_replacement", true, 3}} {
		name, replacement := mode.name, mode.replacement
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			env := newTestEnv(t)
			_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{
				{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}},
				{Family: "msgvault.source_message_archived", SourceType: "imap", Kinds: []string{"message"}},
				{Family: "msgvault.inbox_message_archived", SourceType: "imap", Kinds: []string{"message"}},
			}})
			require.NoError(err)
			user := imapmemserver.NewUser(testutil.IMAPTestUsername, testutil.IMAPTestPassword)
			for _, mailbox := range []string{"INBOX", "Work", "Z-last"} {
				require.NoError(user.Create(mailbox, nil))
			}
			mem := imapmemserver.New()
			mem.AddUser(user)
			var armed, reset atomic.Bool
			var raw []byte
			if replacement {
				raw = testemail.NewMessage().Header("Message-ID", "<synthetic-reset-history@example.net>").Body("synthetic replacement").CRLF().Bytes()
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(err)
			server := imapserver.New(&imapserver.Options{InsecureAuth: true, NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return &nativeEpochSelectSession{Session: mem.NewSession(), user: user, armed: &armed, reset: &reset, replacement: raw, resetAt: mode.resetAt}, nil, nil
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
			var states map[string]imapclient.FolderState
			run := func(allowFailure bool) {
				native := imapclient.NewClient(cfg, testutil.IMAPTestPassword, imapclient.WithFolderStates(states))
				_, err := New(native, env.Store, opts).Full(t.Context(), cfg.Identifier())
				if !allowFailure {
					Require.NoError(t, err)
				}
				states = native.ObservedFolderStates()
				Require.NoError(t, native.Close())
			}
			testutil.AppendIMAPRawMessage(t, user, "Work", testemail.NewMessage().Header("Message-ID", "<synthetic-baseline@example.net>").Body("synthetic baseline").CRLF().Bytes())
			run(false)
			var id int64
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Work' AND present`).Scan(&id))
			before, err := env.Store.GetMCPResource(t.Context(), "inbox", id)
			require.NoError(err)
			testutil.AppendIMAPRawMessage(t, user, "Work", testemail.NewMessage().Header("Message-ID", "<synthetic-before-reset@example.net>").Body("synthetic removed arrival").CRLF().Bytes())
			if mode.resetAt == 3 {
				// Force a native selection of a later mailbox after Work's listing.
				// The later copy deduplicates against the baseline and has no new message to persist.
				testutil.AppendIMAPRawMessage(t, user, "Z-last", testemail.NewMessage().Header("Message-ID", "<synthetic-baseline@example.net>").Body("synthetic baseline").CRLF().Bytes())
			}
			armed.Store(true)
			run(true)
			require.True(reset.Load(), "the real server must reset after inventory and before native SELECT response")
			afterSelect, scopeErr := env.Store.GetMCPResource(t.Context(), "inbox", id)
			if scopeErr != nil {
				require.ErrorContains(scopeErr, "resource_unavailable")
			} else {
				assert.NotEqual(before.Reference, afterSelect.Reference, "an observed UIDVALIDITY reset must immediately retire the old mailbox reference")
			}
			var count int
			require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			expected := 0
			assert.Equal(expected, count, "reset history and an unrelated deduplicated copy must remain silent")
			var workEvents int
			require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log e JOIN messages m ON m.id=e.message_id WHERE m.source_message_id LIKE 'Work|%'`).Scan(&workEvents))
			assert.Equal(0, workEvents, "the reset mailbox cannot publish live arrivals")
			run(false)
			var currentID int64
			require.NoError(env.Store.DB().QueryRow(`SELECT id FROM native_message_scopes WHERE provider_id='Work' AND present`).Scan(&currentID))
			current, err := env.Store.GetMCPResource(t.Context(), "inbox", currentID)
			require.NoError(err)
			assert.NotEqual(before.Reference, current.Reference)
			require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
			assert.Equal(expected, count, "the next baseline cannot replay reset content")
		})
	}
}
