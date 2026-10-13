package sync

import (
	"net"
	"strconv"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// The native All Mail path fetches one canonical MIME and observes exact
// secondary UID memberships through native headers. Both mailboxes must be
// bound before the first archive occurrence, rather than after completion.
func TestNativeIMAPInboxCapturesObservedAliases(t *testing.T) {
	for _, tc := range []struct {
		name, primary string
		allMail       bool
		events        int
	}{{"mailbox_delta", "Work", false, 4}, {"all_mail_fallback_is_history", "All Mail", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			env := newTestEnv(t)
			_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "imap", Kinds: []string{"message"}}, {Family: "msgvault.source_message_archived", SourceType: "imap", Kinds: []string{"message"}}, {Family: "msgvault.inbox_message_archived", SourceType: "imap", Kinds: []string{"message"}}}})
			require.NoError(err)
			attrs := map[string][]imapv2.MailboxAttr{}
			if tc.allMail {
				attrs[tc.primary] = []imapv2.MailboxAttr{imapv2.MailboxAttrAll}
			}
			addr, user := testutil.StartIMAPMemServerWithSpecialUse(t, map[string]int{tc.primary: 0, "INBOX": 0}, attrs)
			host, portText, err := net.SplitHostPort(addr)
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			route := (&imapclient.Config{Host: host, Port: port, Username: testutil.IMAPTestUsername}).Identifier()
			var saved map[string]imapclient.FolderState
			run := func() {
				client := newSyncTestIMAPClient(t, addr, imapclient.WithFolderStates(saved))
				opts := DefaultOptions()
				opts.SourceType = sourceTypeIMAP
				summary, err := New(client.Client, env.Store, opts).Full(t.Context(), route)
				Require.NoError(t, err)
				Require.Zero(t, summary.Errors)
				saved = client.ObservedFolderStates()
				Require.NoError(t, client.Close())
			}
			appendCopies := func(id string) {
				raw := testemail.NewMessage().From("sender@example.net").Header("Message-ID", "<"+id+"@example.net>").Body("synthetic " + id).CRLF().Bytes()
				for _, mailbox := range []string{tc.primary, "INBOX"} {
					testutil.AppendIMAPRawMessage(t, user, mailbox, raw)
				}
			}
			appendCopies("history")
			run()
			appendCopies("arrival")
			run()
			var events int
			require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&events))
			assert.Equal(tc.events, events, "only proven mailbox deltas produce live events; full fallback stays historical")
			var messages int
			require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
			assert.Equal(2, messages, "native copies retain one canonical archive identity")
			rows, err := env.Store.DB().Query(`SELECT provider_id FROM native_message_scopes n WHERE EXISTS(SELECT 1 FROM native_message_scope_memberships m WHERE m.scope_id=n.id AND EXISTS(SELECT 1 FROM messages a WHERE a.id=m.message_id AND a.id=(SELECT MAX(id) FROM messages)))`)
			require.NoError(err)
			defer func() { require.NoError(rows.Close()) }()
			var mailboxes []string
			for rows.Next() {
				var mailbox string
				require.NoError(rows.Scan(&mailbox))
				mailboxes = append(mailboxes, mailbox)
			}
			require.NoError(rows.Err())
			assert.ElementsMatch([]string{tc.primary, "INBOX"}, mailboxes)
		})
	}
}
