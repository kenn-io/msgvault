package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	imapv2 "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

// Schedule a real sync at the next query after the first MIME read completes.
// Only the timing is controlled; storage, sync, and authorization run normally.
type draftContentReadHandler struct {
	slog.Handler

	afterRead func()
	read      atomic.Bool
}

func (h *draftContentReadHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *draftContentReadHandler) Handle(_ context.Context, record slog.Record) error {
	var statement, requestID string
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "stmt":
			statement = attr.Value.String()
		case "request_id":
			requestID = attr.Value.String()
		}
		return true
	})
	if requestID == "draft-sync-race" {
		if h.read.Load() {
			h.afterRead()
		}
		if strings.Contains(statement, "FROM message_raw") {
			h.read.Store(true)
		}
	}
	return nil
}

func TestDelegatedDraftLifecycleConcurrentIMAPSync(t *testing.T) {
	for _, tc := range []struct {
		name, operation, sender string
		allowed, pending        bool
	}{
		{"get excluded sender", api.CLIRunDraftGetCommand, "other@example.test", false, false},
		{"get excluded candidate sender", api.CLIRunDraftGetCommand, "other@example.test", false, true},
		{"get allowed candidate sender", api.CLIRunDraftGetCommand, "alice@example.com", true, true},
		{"get allowed sender", api.CLIRunDraftGetCommand, "alice@example.com", true, false},
		{"edit excluded sender", api.CLIRunDraftEditCommand, "other@example.test", false, false},
		{"delete excluded sender", api.CLIRunDraftDeleteCommand, "other@example.test", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			addr, user := testutil.StartIMAPMemServerWithSpecialUse(t,
				map[string]int{"INBOX": 0, "Drafts": 0, "Sent": 0},
				map[string][]imapv2.MailboxAttr{"Drafts": {imapv2.MailboxAttrDrafts}, "Sent": {imapv2.MailboxAttrSent}},
			)
			const original = "From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <draft-race@example.test>\r\nSubject: Draft\r\n\r\noriginal body"
			fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
				testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte(original))
			})
			// Match the fixture to this server's mailbox epoch and original MIME.
			control, err := imapclient.DialInsecure(addr, nil)
			require.NoError(err)
			t.Cleanup(func() { _ = control.Close() })
			require.NoError(control.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
			selected, err := control.Select("Drafts", nil).Wait()
			require.NoError(err)
			_, err = fixture.store.DB().Exec(fixture.store.Rebind("UPDATE imap_drafts SET current_uidvalidity = ? WHERE draft_id = ?"), selected.UIDValidity, fixture.draft.DraftID)
			require.NoError(err)
			_, err = fixture.store.DB().Exec(fixture.store.Rebind("UPDATE imap_message_memberships SET uidvalidity = ? WHERE message_id = ?"), selected.UIDValidity, fixture.draft.CurrentMessageID)
			require.NoError(err)
			message, err := fixture.store.GetMessage(fixture.draft.CurrentMessageID)
			require.NoError(err)
			archivedID, err := fixture.store.PersistMessage(&store.MessagePersistData{
				Message: &store.Message{
					SourceID: fixture.source.ID, SourceMessageID: "Drafts|1", ConversationID: message.ConversationID,
					MessageType: store.MessageTypeEmail, RFC822MessageID: sql.NullString{String: "<draft-race@example.test>", Valid: true},
				},
				BodyText: sql.NullString{String: "original body", Valid: true}, RawMIME: []byte(original),
			})
			require.NoError(err)
			require.Equal(fixture.draft.CurrentMessageID, archivedID)
			testutil.AppendIMAPRawMessage(t, user, "Sent", []byte(strings.ReplaceAll(strings.ReplaceAll(original,
				"alice@example.com", tc.sender), "original body", "synced body")))

			client := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
			t.Cleanup(func() { _ = client.Close() })
			opts := msgsync.DefaultOptions()
			opts.SourceType = "imap"
			syncer := msgsync.New(client, fixture.store, opts)
			var synced atomic.Bool
			if tc.pending {
				_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, []byte(original))
				require.NoError(err)
				_, err = syncer.Full(t.Context(), fixture.source.Identifier)
				require.NoError(err)
				synced.Store(true)
			}
			previous := slog.Default()
			slog.SetDefault(slog.New(&draftContentReadHandler{
				Handler: slog.DiscardHandler,
				afterRead: func() {
					if synced.CompareAndSwap(false, true) {
						_, syncErr := syncer.Full(t.Context(), fixture.source.Identifier)
						require.NoError(syncErr)
					}
				},
			}))
			t.Cleanup(func() { slog.SetDefault(previous) })

			grant := &agentgrant.Grant{
				ID:          "sender-scoped",
				Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete},
				Sources:     []agentgrant.SourceRef{{Type: "imap", Identifier: fixture.source.Identifier, SenderKeys: []string{"alice@example.com"}}},
			}
			if tc.pending {
				grant.Sources[0].SenderKeys = []string{tc.sender}
			}
			args := []string{tc.operation, fixture.draft.DraftID, "--json"}
			if tc.operation != api.CLIRunDraftGetCommand {
				args = append(args, "--revision", "1")
			}
			if tc.operation == api.CLIRunDraftEditCommand {
				args = append(args, "--body", "delegated body")
			}
			providerCalls := 0
			factory := fixture.adapter.draftClientFactory
			fixture.adapter.draftClientFactory = func(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
				providerCalls++
				return factory(ctx, source)
			}
			var events []api.CLIRunEvent
			err = fixture.adapter.runCLIDraftLifecycle(store.WithRequestID(t.Context(), "draft-sync-race"), api.CLIRunRequest{Args: args, Grant: grant}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			})
			assert.True(synced.Load())
			if tc.allowed {
				require.NoError(err)
				require.Len(events, 1)
				var output draftLifecycleOutput
				require.NoError(json.Unmarshal([]byte(events[0].Data), &output))
				assert.Equal("synced body", output.Content)
				assert.Contains(output.RawMIME, "From: "+tc.sender+"\r\n")
				assert.Contains(output.RawMIME, "synced body")
				if tc.pending {
					assert.Equal(original, output.CandidateContent)
				}
			} else {
				require.Error(err)
				assert.Equal("not_permitted", err.Error())
				assert.Empty(events)
			}
			assert.Zero(providerCalls)
			latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			require.NoError(err)
			assert.Equal(int64(1), latest.Revision)
			archived, err := fixture.store.GetMessage(latest.CurrentMessageID)
			require.NoError(err)
			assert.Equal(tc.sender, archived.FromEmail)
			assert.Equal("synced body", archived.BodyText)
			assert.Equal(fixture.draft.CurrentMessageID, latest.CurrentMessageID)
		})
	}
}
