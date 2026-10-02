package cmd

import (
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
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

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
