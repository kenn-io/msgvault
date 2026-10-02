package cmd

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestGmailReplyAddressedAlias(t *testing.T) {
	for _, tc := range []struct {
		name, headers, explicit, want, code string
		unconfirmed, pending, delegated     bool
	}{
		{name: "workspace To", headers: "To: Person <WORK@workspace.example>\r\nDelivered-To: owner@example.test\r\n", want: "work@workspace.example"},
		{name: "fastmail Cc", headers: "To: recipient@example.test\r\nCc: masked@fastmail.example\r\n", want: "masked@fastmail.example"},
		{name: "delivered fallback", headers: "To: recipient@example.test\r\nDelivered-To: masked@fastmail.example\r\n", want: "masked@fastmail.example"},
		{name: "original fallback", headers: "To: recipient@example.test\r\nX-Original-To: work@workspace.example\r\n", want: "work@workspace.example"},
		{name: "malformed delivery trace with valid original", headers: "To: recipient@example.test\r\nDelivered-To: invalid trace address\r\nX-Original-To: work@workspace.example\r\n", want: "work@workspace.example"},
		{name: "malformed delivery trace fallback", headers: "To: recipient@example.test\r\nDelivered-To: invalid trace address\r\nX-Original-To: another invalid trace\r\n", code: "from_ambiguous"},
		{name: "explicit wins", headers: "To: work@workspace.example\r\n", explicit: "owner@example.test", want: "owner@example.test"},
		{name: "multiple matches", headers: "To: work@workspace.example\r\nCc: masked@fastmail.example\r\n", code: "from_ambiguous"},
		{name: "deduplicated", headers: "To: work@workspace.example\r\nCc: WORK@workspace.example\r\n", want: "work@workspace.example"},
		{name: "no match fallback", headers: "To: recipient@example.test\r\n", code: "from_ambiguous"},
		{name: "pending rejected", headers: "To: work@workspace.example\r\n", explicit: "work@workspace.example", pending: true, code: "invalid_from"},
		{name: "unconfirmed fallback", headers: "To: work@workspace.example\r\n", unconfirmed: true, want: "owner@example.test"},
		{name: "grant excludes addressed alias", headers: "To: work@workspace.example\r\n", delegated: true, code: "not_permitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newGmailDraftTestFixture(t)
			if !tc.unconfirmed {
				for _, address := range []string{"work@workspace.example", "masked@fastmail.example"} {
					require.NoError(f.store.AddAccountIdentity(f.source.ID, address, "manual"))
				}
			}
			status := "accepted"
			if tc.pending {
				status = "pending"
			}
			f.client.sendAs = append(f.client.sendAs, gmail.SendAs{Email: "work@workspace.example", VerificationStatus: status}, gmail.SendAs{Email: "masked@fastmail.example", VerificationStatus: "accepted"})
			raw := []byte("From: sender@example.test\r\n" + tc.headers + "Subject: Question\r\nMessage-ID: <parent@example.test>\r\n\r\nBody")
			require.NoError(f.store.UpsertMessageRaw(f.parentID, raw))
			args := []string{"draft-reply", strconv.FormatInt(f.parentID, 10), "--body", "reply", "--json"}
			if tc.explicit != "" {
				args = append(args, "--from", tc.explicit)
			}
			req := api.CLIRunRequest{Args: args}
			if tc.delegated {
				req.Grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: "gmail", Identifier: f.source.Identifier, SenderKeys: []string{f.source.Identifier}}}}
			}
			var events []api.CLIRunEvent
			err := f.adapter.runCLIReplyDraft(t.Context(), req, func(e api.CLIRunEvent) error { events = append(events, e); return nil })
			if tc.code != "" {
				require.EqualError(err, tc.code)
				assert.Zero(f.client.createCalls)
				return
			}
			require.NoError(err)
			assert.Equal(1, f.client.listCalls, "one send-as inventory read per draft command")
			require.Len(events, 1)
			var out gmailDraftReplyOutput
			require.NoError(json.Unmarshal([]byte(events[0].Data), &out))
			m, err := f.store.GetMessage(out.MessageID)
			require.NoError(err)
			assert.Equal(tc.want, m.From)
			assert.Equal("gmail-thread-1", out.ThreadID)
		})
	}
}

func TestGmailSendAsConfirmation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newGmailDraftTestFixture(t)
	var events []api.CLIRunEvent
	emit := func(e api.CLIRunEvent) error { events = append(events, e); return nil }
	require.NoError(f.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{Args: []string{"draft-send-as", f.source.Identifier, "--json"}}, emit))
	ids, err := f.store.ListAccountIdentities(f.source.ID)
	require.NoError(err)
	assert.Len(ids, 1)
	require.NoError(f.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{Args: []string{"draft-send-as", f.source.Identifier, "--confirm", "alias@example.test", "--json"}}, emit))
	ids, err = f.store.ListAccountIdentities(f.source.ID)
	require.NoError(err)
	require.Len(ids, 2)
	var alias store.AccountIdentity
	for _, id := range ids {
		if id.Address == "alias@example.test" {
			alias = id
		}
	}
	assert.False(alias.ConfirmedAt.IsZero())
	assert.Contains(alias.SourceSignal, "gmail-send-as")
	var confirmation gmailSendAsOutput
	require.NoError(json.Unmarshal([]byte(events[len(events)-1].Data), &confirmation))
	require.Len(confirmation.Applied, 1)
	assert.Equal("alias@example.test", confirmation.Applied[0].Identifier)
	f.client.sendAs = append(f.client.sendAs, gmail.SendAs{Email: "pending@example.test", VerificationStatus: "pending"})
	require.EqualError(f.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{Args: []string{"draft-send-as", f.source.Identifier, "--confirm=pending@example.test"}}, emit), "invalid_from")
	f.client.sendAs = append(f.client.sendAs, gmail.SendAs{Email: "new@example.test", VerificationStatus: "accepted"})
	require.EqualError(f.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{Args: []string{"draft-send-as", f.source.Identifier, "--confirm=new@example.test", "--confirm=pending@example.test"}}, emit), "invalid_from")
	ids, err = f.store.ListAccountIdentities(f.source.ID)
	require.NoError(err)
	assert.Len(ids, 2)
}

func TestGmailSendAsConfirmationRefreshOutlivesRequestCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newGmailDraftTestFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var refreshContextErr error
	var refreshCalls int
	f.adapter.draftCacheRefresh = func(refreshCtx context.Context, _ string) error {
		// The identity transaction has committed before this callback runs.
		cancel()
		refreshCalls++
		refreshContextErr = refreshCtx.Err()
		return refreshContextErr
	}
	err := f.adapter.runCLIDraftSendAs(ctx, api.CLIRunRequest{
		Args: []string{"draft-send-as", f.source.Identifier, "--confirm=alias@example.test", "--json"},
	}, func(api.CLIRunEvent) error { return nil })
	require.ErrorIs(err, context.Canceled, "the disconnected request cannot finish its response")
	assert.Equal(1, refreshCalls)
	require.NoError(refreshContextErr, "committed identities still reach the cache")
	identities, err := f.store.ListAccountIdentitiesContext(t.Context(), f.source.ID)
	require.NoError(err)
	assert.Len(identities, 2, "confirmation committed before cancellation")
}

func TestGmailComposeAndForwardDrafts(t *testing.T) {
	for _, tc := range []struct {
		command                string
		crossSource, delegated bool
	}{
		{command: "draft-compose"},
		{command: "draft-compose", delegated: true},
		{command: "draft-forward"},
		{command: "draft-forward", crossSource: true},
	} {
		t.Run(tc.command+"/cross-source="+strconv.FormatBool(tc.crossSource)+"/delegated="+strconv.FormatBool(tc.delegated), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newGmailDraftTestFixture(t)
			if tc.crossSource {
				other, err := f.store.GetOrCreateSource("gmail", "other@example.test")
				require.NoError(err)
				f.source = other
				f.adapter.gmailDraftPolicy = append(f.adapter.gmailDraftPolicy, config.GmailDraftSource{SourceID: other.ID, Enabled: true})
			}
			command := tc.command
			require.NoError(f.store.AddAccountIdentity(f.source.ID, "alias@example.test", "manual"))
			args := []string{command}
			if command == "draft-forward" {
				args = append(args, strconv.FormatInt(f.parentID, 10))
			}
			args = append(args, "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", "alias@example.test", "--to", "to@example.test", "--cc", "copy@example.test", "--bcc", "hidden@example.test", "--body", "note", "--json")
			if command == "draft-compose" {
				args = append(args, "--subject", "New topic")
			}
			var events []api.CLIRunEvent
			emit := func(e api.CLIRunEvent) error { events = append(events, e); return nil }
			var err error
			req := api.CLIRunRequest{Args: args}
			if tc.delegated {
				req.Grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: "gmail", Identifier: f.source.Identifier, SenderKeys: []string{"alias@example.test"}}}}
			}
			if command == "draft-compose" {
				err = f.adapter.runCLIComposeDraft(t.Context(), req, emit)
			} else {
				err = f.adapter.runCLIForwardDraft(t.Context(), req, emit)
			}
			require.NoError(err)
			require.Len(events, 1)
			var out gmailDraftReplyOutput
			require.NoError(json.Unmarshal([]byte(events[0].Data), &out))
			assert.Equal([]string{""}, f.client.createThreads)
			assert.Equal("gmail-new-thread", out.ThreadID)
			replyTo, err := f.store.GetMessageReplyToMessageIDContext(t.Context(), out.MessageID)
			require.NoError(err)
			assert.False(replyTo.Valid)
			m, err := f.store.GetMessage(out.MessageID)
			require.NoError(err)
			assert.Equal("alias@example.test", m.From)
			assert.Equal([]string{"copy@example.test"}, m.Cc)
			assert.Equal([]string{"hidden@example.test"}, m.Bcc)
			raw, err := f.store.GetMessageRaw(out.MessageID)
			require.NoError(err)
			assert.NotContains(string(raw), "In-Reply-To:")
			assert.NotContains(string(raw), "References:")
			draft, err := f.store.GetGmailDraft(out.DraftID)
			require.NoError(err)
			f.client.getDraft = &gmail.Draft{ID: draft.CurrentReceipt.GmailDraftID, Message: gmail.RawMessage{ID: draft.CurrentReceipt.GmailMessageID, ThreadID: draft.CurrentReceipt.ThreadID, Raw: raw}}
			f.client.updateDraft = &gmail.Draft{ID: draft.CurrentReceipt.GmailDraftID, Message: gmail.RawMessage{ID: "updated-message", ThreadID: draft.CurrentReceipt.ThreadID}}
			_, err = f.lifecycle(t, "draft-edit", draft, "revised note")
			require.NoError(err)
			updated, err := f.store.GetGmailDraft(out.DraftID)
			require.NoError(err)
			updatedRaw, err := f.store.GetMessageRaw(updated.CurrentMessageID)
			require.NoError(err)
			assert.Contains(string(updatedRaw), "revised note")
			if command == "draft-forward" {
				assert.Contains(string(updatedRaw), "Parent body")
				assert.True(strings.HasPrefix(m.Subject, "Fwd:"))
			} else {
				assert.Equal("New topic", m.Subject)
			}
		})
	}
}

func TestGmailForwardRetainsAttachmentsThroughEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newGmailDraftTestFixture(t)
	raw := testemail.NewMessage().From("sender@example.test").To(f.source.Identifier).Subject("Source topic").Body("Original text").WithAttachment("notes.txt", "text/plain", []byte("retained bytes")).CRLF().Bytes()
	parsed, err := msgmime.Parse(raw)
	require.NoError(err)
	require.NoError(f.store.UpsertMessageRaw(f.parentID, raw))
	writes, err := gmailDraftAttachmentWrites(f.adapter.config, parsed.Attachments)
	require.NoError(err)
	require.Len(writes, 1)
	for _, write := range writes {
		require.NoError(f.store.UpsertAttachmentRecord(t.Context(), f.parentID, write))
	}
	maintenance, err := newAttachmentMaintenance(f.store, f.adapter.config.AttachmentsDir(), testLoggerValue(), true)
	require.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	f.adapter.attachmentMaintenance = maintenance
	var events []api.CLIRunEvent
	require.NoError(f.adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{"draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "to@example.test", "--body", "note", "--json"}}, func(e api.CLIRunEvent) error { events = append(events, e); return nil }))
	require.Len(events, 1)
	var out gmailDraftReplyOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &out))
	draft, err := f.store.GetGmailDraft(out.DraftID)
	require.NoError(err)
	currentRaw, err := f.store.GetMessageRaw(draft.CurrentMessageID)
	require.NoError(err)
	f.client.getDraft = &gmail.Draft{ID: draft.CurrentReceipt.GmailDraftID, Message: gmail.RawMessage{ID: draft.CurrentReceipt.GmailMessageID, ThreadID: draft.CurrentReceipt.ThreadID, Raw: currentRaw}}
	f.client.updateDraft = &gmail.Draft{ID: draft.CurrentReceipt.GmailDraftID, Message: gmail.RawMessage{ID: "edited-message", ThreadID: draft.CurrentReceipt.ThreadID}}
	_, err = f.lifecycle(t, "draft-edit", draft, "new note")
	require.NoError(err)
	latest, err := f.store.GetGmailDraft(out.DraftID)
	require.NoError(err)
	refs, err := f.store.MessageMIMEAttachmentsContext(t.Context(), latest.CurrentMessageID)
	require.NoError(err)
	require.Len(refs, 1)
	assert.Equal(writes[0].ContentHash, refs[0].ContentHash)
	editedRaw, err := f.store.GetMessageRaw(latest.CurrentMessageID)
	require.NoError(err)
	edited, err := msgmime.Parse(editedRaw)
	require.NoError(err)
	require.Len(edited.Attachments, 1)
	assert.Equal([]byte("retained bytes"), edited.Attachments[0].Content)
	assert.Contains(edited.BodyText, "Original text")
	assert.Contains(edited.BodyText, "new note")
}
