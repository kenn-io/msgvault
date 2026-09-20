package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type scriptedGmailDraftClient struct {
	createDraft *gmail.Draft
	createErr   error
	getDraft    *gmail.Draft
	getErr      error
	updateDraft *gmail.Draft
	updateErr   error
	updateHook  func()
	deleteErr   error
	deleteHook  func()
	sendAs      []gmail.SendAs
	sendAsErr   error

	createCalls int
	getCalls    int
	updateCalls int
	deleteCalls int
	listCalls   int
}

func (c *scriptedGmailDraftClient) CreateDraft(_ context.Context, raw []byte, threadID string) (*gmail.Draft, error) {
	c.createCalls++
	if c.createErr != nil {
		return nil, c.createErr
	}
	if c.createDraft != nil {
		return c.createDraft, nil
	}
	return &gmail.Draft{
		ID: "gmail-draft-created",
		Message: gmail.RawMessage{
			ID: "gmail-message-created", ThreadID: threadID, Raw: append([]byte(nil), raw...),
		},
	}, nil
}

func (c *scriptedGmailDraftClient) GetDraft(context.Context, string) (*gmail.Draft, error) {
	c.getCalls++
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.getDraft == nil {
		return nil, errors.New("test Gmail draft was not configured")
	}
	return c.getDraft, nil
}

func (c *scriptedGmailDraftClient) UpdateDraft(context.Context, string, []byte, string) (*gmail.Draft, error) {
	c.updateCalls++
	if c.updateHook != nil {
		c.updateHook()
	}
	if c.updateErr != nil {
		return nil, c.updateErr
	}
	if c.updateDraft == nil {
		return nil, errors.New("test Gmail update was not configured")
	}
	return c.updateDraft, nil
}

func (c *scriptedGmailDraftClient) DeleteDraft(context.Context, string) error {
	c.deleteCalls++
	if c.deleteHook != nil {
		c.deleteHook()
	}
	return c.deleteErr
}

func (c *scriptedGmailDraftClient) ListSendAs(context.Context) ([]gmail.SendAs, error) {
	c.listCalls++
	if c.sendAsErr != nil {
		return nil, c.sendAsErr
	}
	return c.sendAs, nil
}

func (c *scriptedGmailDraftClient) Close() error { return nil }

type gmailDraftTestFixture struct {
	store          *store.Store
	source         *store.Source
	conversationID int64
	parentID       int64
	client         *scriptedGmailDraftClient
	adapter        *storeAPIAdapter
}

func newGmailDraftTestFixture(t *testing.T) gmailDraftTestFixture {
	t.Helper()
	return newGmailDraftTestFixtureWithStore(t, testutil.NewTestStore)
}

func newSQLiteGmailDraftTestFixture(t *testing.T) gmailDraftTestFixture {
	t.Helper()
	return newGmailDraftTestFixtureWithStore(t, testutil.NewSQLiteTestStore)
}

func newGmailDraftTestFixtureWithStore(t *testing.T, newStore func(*testing.T) *store.Store) gmailDraftTestFixture {
	t.Helper()
	previousCfg := cfg
	cfg = &config.Config{
		Data:  config.DataConfig{DataDir: t.TempDir()},
		OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"},
	}
	t.Cleanup(func() { cfg = previousCfg })

	st := newStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	require.NoError(t, err)
	require.NoError(t, st.AddAccountIdentity(source.ID, source.Identifier, "manual"))
	conversationID, err := st.EnsureConversation(source.ID, "gmail-thread-1", "Question")
	require.NoError(t, err)
	senderID, err := st.EnsureParticipant("sender@example.test", "Sender", "example.test")
	require.NoError(t, err)
	ownerID, err := st.EnsureParticipant(source.Identifier, "", "example.test")
	require.NoError(t, err)
	parentRaw := []byte("From: Sender <sender@example.test>\r\n" +
		"To: " + source.Identifier + "\r\n" +
		"Subject: Question\r\n" +
		"Message-ID: <gmail-parent@example.test>\r\n\r\n" +
		"Parent body\r\n")
	parentID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "gmail-parent-1",
			ConversationID:  conversationID,
			RFC822MessageID: sql.NullString{String: "gmail-parent@example.test", Valid: true},
			MessageType:     store.MessageTypeEmail,
			SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		},
		Conversation: &store.ConversationPersistData{
			SourceConversationID: "gmail-thread-1", ConversationType: "email_thread", Title: "Question",
		},
		BodyText: sql.NullString{String: "Parent body", Valid: true}, RawMIME: parentRaw,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{senderID}, EmailAddresses: []string{"sender@example.test"}},
			{Type: "to", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{source.Identifier}},
		},
	})
	require.NoError(t, err)

	client := &scriptedGmailDraftClient{
		sendAs: []gmail.SendAs{
			{Email: source.Identifier, Primary: true, Default: true, VerificationStatus: "accepted"},
			{Email: "alias@example.test", VerificationStatus: "accepted"},
		},
	}
	adapter := &storeAPIAdapter{
		store:            st,
		gmailDraftPolicy: []config.GmailDraftSource{{SourceID: source.ID, Enabled: true}},
		gmailDraftClientFactory: func(context.Context, *store.Source) (gmail.DraftAPI, error) {
			return client, nil
		},
	}
	return gmailDraftTestFixture{
		store: st, source: source, conversationID: conversationID,
		parentID: parentID, client: client, adapter: adapter,
	}
}

func gmailDraftTestRaw(body, messageID string) []byte {
	return []byte(fmt.Sprintf(
		"From: owner@example.test\r\nTo: sender@example.test\r\nSubject: Re: Question\r\nMessage-ID: <%s>\r\n\r\n%s\r\n",
		messageID, body,
	))
}

func gmailDraftTestRawWithAttachment(messageID string) []byte {
	return testemail.NewMessage().
		From("owner@example.test").
		To("sender@example.test").
		Subject("Re: Question").
		Header("Message-ID", "<"+messageID+">").
		Body("external with attachment").
		WithAttachment("notes.txt", "text/plain", []byte("attachment bytes")).
		CRLF().
		Bytes()
}

func (f gmailDraftTestFixture) create(t *testing.T, body string, asJSON bool) ([]api.CLIRunEvent, error) {
	t.Helper()
	return f.createContext(t.Context(), t, body, asJSON)
}

func (f gmailDraftTestFixture) createContext(ctx context.Context, t *testing.T, body string, asJSON bool) ([]api.CLIRunEvent, error) {
	t.Helper()
	args := []string{
		api.CLIRunDraftReplyCommand, strconv.FormatInt(f.parentID, 10),
		"--from", f.source.Identifier, "--body", body,
	}
	if asJSON {
		args = append(args, "--json")
	}
	var events []api.CLIRunEvent
	err := f.adapter.runCLIReplyDraft(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func (f gmailDraftTestFixture) seedDraft(t *testing.T) store.GmailDraft {
	t.Helper()
	raw := gmailDraftTestRaw("original", "gmail-original@example.test")
	parsed, err := msgmime.Parse(raw)
	require.NoError(t, err)
	receipt := store.GmailDraftReceipt{
		SourceID: f.source.ID, GmailDraftID: "gmail-draft-managed",
		GmailMessageID: "gmail-message-original", ThreadID: "gmail-thread-1",
	}
	draft, err := f.store.PersistGmailDraftContext(
		t.Context(), receipt, gmailDraftParticipants(parsed),
		gmailDraftMessagePersistData(f.source.ID, f.parentID, parsed, raw, receipt, messageRFC822ID(parsed)),
	)
	require.NoError(t, err)
	f.client.getDraft = &gmail.Draft{
		ID:      receipt.GmailDraftID,
		Message: gmail.RawMessage{ID: receipt.GmailMessageID, ThreadID: receipt.ThreadID, Raw: raw},
	}
	return draft
}

func (f gmailDraftTestFixture) lifecycle(t *testing.T, operation string, draft store.GmailDraft, body string) ([]api.CLIRunEvent, error) {
	t.Helper()
	args := []string{
		operation, draft.DraftID, "--revision", strconv.FormatInt(draft.Revision, 10),
	}
	if operation == api.CLIRunDraftEditCommand {
		args = append(args, "--body", body)
	}
	args = append(args, "--json")
	var events []api.CLIRunEvent
	err := f.adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func TestGmailDraftCreateAndSendAsUseLocalBehavior(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)

	events, err := fixture.create(t, "created body", true)
	require.NoError(err)
	require.Len(events, 1)
	var created gmailDraftReplyOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &created))
	assert.Equal(gmailDraftStatusCreated, created.Status)
	assert.Equal("gmail-draft-created", created.GmailDraftID)
	assert.Equal(int64(1), created.Revision)
	assert.Equal(1, fixture.client.createCalls)
	draft, err := fixture.store.GetGmailDraftContext(t.Context(), created.DraftID)
	require.NoError(err)
	assert.Equal(created.GmailMessageID, draft.CurrentReceipt.GmailMessageID)

	var labelCount int
	require.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ? AND l.source_label_id = 'DRAFT'
	`), draft.CurrentMessageID).Scan(&labelCount))
	assert.Equal(1, labelCount)

	var sendAsEvents []api.CLIRunEvent
	err = fixture.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{
		Args: []string{api.CLIRunDraftSendAsCommand, fixture.source.Identifier, "--json"},
	}, func(event api.CLIRunEvent) error {
		sendAsEvents = append(sendAsEvents, event)
		return nil
	})
	require.NoError(err)
	require.Len(sendAsEvents, 1)
	var sendAs gmailSendAsOutput
	require.NoError(json.Unmarshal([]byte(sendAsEvents[0].Data), &sendAs))
	require.Len(sendAs.Entries, 2)
	assert.True(sendAs.Entries[0].ConfirmedIdentity)
}

func TestGmailDraftSendAsFailureIsReportedLocally(t *testing.T) {
	fixture := newGmailDraftTestFixture(t)
	fixture.client.sendAsErr = errors.New("send-as unavailable")
	err := fixture.adapter.runCLIDraftSendAs(t.Context(), api.CLIRunRequest{
		Args: []string{api.CLIRunDraftSendAsCommand, fixture.source.Identifier},
	}, nil)
	require.Error(t, err)
	assert.Equal(t, "provider_refused", err.Error())
}

func TestGmailDraftLifecyclePublishesEditAndDelete(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	fixture.client.updateDraft = &gmail.Draft{
		ID:      "gmail-draft-managed",
		Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
	}
	events, err := fixture.lifecycle(t, api.CLIRunDraftEditCommand, draft, "edited")
	require.NoError(err)
	require.Len(events, 1)
	assert.Contains(events[0].Data, `"status":"edited"`)
	updated, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(int64(2), updated.Revision)
	assert.Equal("gmail-message-edited", updated.CurrentReceipt.GmailMessageID)

	fixture.client.getDraft = &gmail.Draft{
		ID:      "gmail-draft-managed",
		Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
	}
	events, err = fixture.lifecycle(t, api.CLIRunDraftDeleteCommand, updated, "")
	require.NoError(err)
	require.Len(events, 1)
	assert.Contains(events[0].Data, `"status":"deleted"`)
	deleted, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.NotNil(deleted.DiscardedAt)
}

func TestGmailDraftExternalAdoptionReturnsFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	fixture.client.getDraft = &gmail.Draft{
		ID: "gmail-draft-managed",
		Message: gmail.RawMessage{
			ID: "gmail-message-external", ThreadID: "gmail-thread-1",
			Raw: gmailDraftTestRaw("external", "gmail-external@example.test"),
		},
	}
	events, err := fixture.lifecycle(t, api.CLIRunDraftEditCommand, draft, "candidate")
	require.Error(err)
	assert.Equal("changed_externally", err.Error())
	require.Len(events, 1)
	assert.Equal(cliStreamStderr, events[0].Type)
	var output gmailDraftLifecycleOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assert.Equal("changed_externally", output.Status)
	require.NotNil(output.ProviderObservation)
	assert.Equal("gmail-message-external", output.ProviderObservation.GmailMessageID)
	assert.Equal(0, fixture.client.updateCalls)
	adopted, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(int64(2), adopted.Revision)
	assert.Equal("gmail-message-external", adopted.CurrentReceipt.GmailMessageID)
}

func TestGmailDraftExternalAdoptionPersistsAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	fixture.client.getDraft = &gmail.Draft{
		ID: "gmail-draft-managed",
		Message: gmail.RawMessage{
			ID: "gmail-message-external-attachment", ThreadID: "gmail-thread-1",
			Raw: gmailDraftTestRawWithAttachment("gmail-external-attachment@example.test"),
		},
	}

	events, err := fixture.lifecycle(t, api.CLIRunDraftEditCommand, draft, "candidate")
	require.Error(err)
	assert.Equal("changed_externally", err.Error())
	require.Len(events, 1)

	adopted, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	message, err := fixture.store.GetMessageContext(t.Context(), adopted.CurrentMessageID)
	require.NoError(err)
	require.Len(message.Attachments, 1)
	assert.True(message.HasAttachments)
	assert.Equal("notes.txt", message.Attachments[0].Filename)
	assert.Equal("text/plain", message.Attachments[0].MimeType)
	assert.Equal(int64(len("attachment bytes")), message.Attachments[0].Size)
	assert.NotEmpty(message.Attachments[0].ContentHash)
}

func TestGmailDraftCancellationClearsClaim(t *testing.T) {
	for _, operation := range []string{api.CLIRunDraftEditCommand, api.CLIRunDraftDeleteCommand} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := newGmailDraftTestFixture(t)
			draft := fixture.seedDraft(t)
			ctx, cancel := context.WithCancel(t.Context())
			fixture.client.updateDraft = &gmail.Draft{
				ID:      "gmail-draft-managed",
				Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
			}
			fixture.client.updateErr = &gmail.DraftWriteError{
				State: gmail.DraftStateCancelled, Code: "cancelled", Err: context.Canceled,
			}
			if operation == api.CLIRunDraftDeleteCommand {
				fixture.client.updateErr = nil
				fixture.client.deleteErr = &gmail.DraftWriteError{
					State: gmail.DraftStateCancelled, Code: "cancelled", Err: context.Canceled,
				}
			}
			fixture.client.updateHook = cancel
			if operation == api.CLIRunDraftDeleteCommand {
				fixture.client.updateHook = nil
				fixture.client.deleteErr = &gmail.DraftWriteError{
					State: gmail.DraftStateCancelled, Code: "cancelled", Err: context.Canceled,
				}
			}
			fixture.client.getDraft = &gmail.Draft{
				ID:      "gmail-draft-managed",
				Message: gmail.RawMessage{ID: "gmail-message-original", ThreadID: "gmail-thread-1"},
			}
			if operation == api.CLIRunDraftDeleteCommand {
				fixture.client.deleteErr = &gmail.DraftWriteError{
					State: gmail.DraftStateCancelled, Code: "cancelled", Err: context.Canceled,
				}
				fixture.client.deleteHook = cancel
			}
			events, err := fixture.lifecycleContext(ctx, operation, draft, "candidate")
			require.Error(err)
			assert.Equal("cancelled", err.Error())
			assert.Len(events, 1)
			latest, loadErr := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
			require.NoError(loadErr)
			assert.Nil(latest.Pending)
		})
	}
}

func TestGmailDraftAcceptedResponseSurvivesCancellation(t *testing.T) {
	for _, operation := range []string{api.CLIRunDraftEditCommand, api.CLIRunDraftDeleteCommand} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			fixture := newGmailDraftTestFixture(t)
			draft := fixture.seedDraft(t)
			ctx, cancel := context.WithCancel(t.Context())
			if operation == api.CLIRunDraftEditCommand {
				fixture.client.updateDraft = &gmail.Draft{
					ID:      "gmail-draft-managed",
					Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
				}
				fixture.client.updateHook = cancel
			} else {
				fixture.client.deleteHook = cancel
			}
			events, err := fixture.lifecycleContext(ctx, operation, draft, "edited")
			require.NoError(err)
			require.Len(events, 1)
			latest, loadErr := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
			require.NoError(loadErr)
			if operation == api.CLIRunDraftEditCommand {
				require.Equal(int64(2), latest.Revision)
				require.Equal("gmail-message-edited", latest.CurrentReceipt.GmailMessageID)
			} else {
				require.NotNil(latest.DiscardedAt)
			}
			require.Nil(latest.Pending)
		})
	}
}

func TestGmailDraftLifecycleRefreshRunsAfterOutput(t *testing.T) {
	for _, operation := range []string{
		api.CLIRunDraftEditCommand,
		api.CLIRunDraftDeleteCommand,
		api.CLIRunDraftEditCommand + " external adoption",
	} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			fixture := newGmailDraftTestFixture(t)
			draft := fixture.seedDraft(t)
			switch operation {
			case api.CLIRunDraftEditCommand:
				fixture.client.updateDraft = &gmail.Draft{
					ID:      "gmail-draft-managed",
					Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
				}
			case api.CLIRunDraftEditCommand + " external adoption":
				fixture.client.getDraft = &gmail.Draft{
					ID: "gmail-draft-managed",
					Message: gmail.RawMessage{
						ID: "gmail-message-external", ThreadID: "gmail-thread-1",
						Raw: gmailDraftTestRaw("external", "gmail-external@example.test"),
					},
				}
			}

			var events []api.CLIRunEvent
			refreshSawOutput := false
			fixture.adapter.draftCacheRefresh = func(ctx context.Context, _ string) error {
				refreshSawOutput = len(events) == 1
				require.NoError(ctx.Err())
				return nil
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			args := []string{api.CLIRunDraftDeleteCommand, draft.DraftID, "--revision", strconv.FormatInt(draft.Revision, 10), "--json"}
			if operation == api.CLIRunDraftEditCommand || strings.HasSuffix(operation, "external adoption") {
				args[0] = api.CLIRunDraftEditCommand
				args = append(args, "--body", "candidate")
			}
			err := fixture.adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				cancel()
				return nil
			})
			if strings.HasSuffix(operation, "external adoption") {
				require.ErrorContains(err, "changed_externally")
			} else {
				require.NoError(err)
			}
			require.Len(events, 1)
			assert.True(refreshSawOutput)
		})
	}
}

func TestGmailDraftCreateOutputsReceiptBeforeCacheRefreshAfterCancellation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	var events []api.CLIRunEvent
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.adapter.draftCacheRefresh = func(refreshCtx context.Context, _ string) error {
		assert.Len(events, 1)
		require.ErrorIs(ctx.Err(), context.Canceled)
		assert.NoError(refreshCtx.Err())
		return nil
	}
	args := []string{
		api.CLIRunDraftReplyCommand, strconv.FormatInt(fixture.parentID, 10),
		"--from", fixture.source.Identifier, "--body", "created after cancellation", "--json",
	}
	err := fixture.adapter.runCLIReplyDraft(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		cancel()
		return nil
	})
	require.NoError(err)
	require.Len(events, 1)
	var output gmailDraftReplyOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assert.Equal(gmailDraftStatusCreated, output.Status)
	assert.Equal("gmail-draft-created", output.GmailDraftID)
}

func (f gmailDraftTestFixture) lifecycleContext(ctx context.Context, operation string, draft store.GmailDraft, body string) ([]api.CLIRunEvent, error) {
	args := []string{operation, draft.DraftID, "--revision", strconv.FormatInt(draft.Revision, 10)}
	if operation == api.CLIRunDraftEditCommand {
		args = append(args, "--body", body)
	}
	args = append(args, "--json")
	var events []api.CLIRunEvent
	err := f.adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	return events, err
}

func TestGmailDraftAcceptedReplacementReceiptIsOutputWhenPublicationFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	conflictRaw := gmailDraftTestRaw("conflict", "gmail-conflict@example.test")
	conflictSender, err := fixture.store.EnsureParticipant("owner@example.test", "", "example.test")
	require.NoError(err)
	conflictTo, err := fixture.store.EnsureParticipant("sender@example.test", "", "example.test")
	require.NoError(err)
	_, err = fixture.store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: fixture.source.ID, SourceMessageID: "gmail-message-edited",
			ConversationID: fixture.conversationID, MessageType: store.MessageTypeEmail,
			SenderID: sql.NullInt64{Int64: conflictSender, Valid: true},
		},
		BodyText: sql.NullString{String: "conflict", Valid: true}, RawMIME: conflictRaw,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{conflictSender}, EmailAddresses: []string{"owner@example.test"}},
			{Type: "to", ParticipantIDs: []int64{conflictTo}, EmailAddresses: []string{"sender@example.test"}},
		},
	})
	require.NoError(err)
	fixture.client.updateDraft = &gmail.Draft{
		ID:      "gmail-draft-managed",
		Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
	}

	events, err := fixture.lifecycle(t, api.CLIRunDraftEditCommand, draft, "candidate")
	require.Error(err)
	assert.Equal("accepted_local_failed", err.Error())
	require.Len(events, 1)
	var output gmailDraftLifecycleOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assert.Equal("gmail-message-edited", output.PendingReplacementGmailMessageID)
	require.NotNil(output.ProviderObservation)
	assert.Equal("gmail-message-edited", output.ProviderObservation.GmailMessageID)
	latest, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	require.NotNil(latest.Pending)
	assert.Equal("gmail-message-edited", latest.Pending.ReplacementGmailMessageID)

	var human api.CLIRunEvent
	require.NoError(emitGmailDraftLifecycleOutput(func(event api.CLIRunEvent) error {
		human = event
		return nil
	}, cliStreamStderr, false, output))
	assert.Contains(human.Data, "pending replacement Gmail message ID: gmail-message-edited")
	assert.Contains(human.Data, "acknowledged replacement receipt:")
}

func TestGmailDraftAcceptedReplacementReceiptIsOutputWhenOutcomeRecordFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newSQLiteGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	_, err := fixture.store.DB().Exec(`
		CREATE TRIGGER fail_gmail_draft_outcome
		BEFORE UPDATE OF pending_code ON gmail_drafts
		WHEN NEW.pending_code = 'accepted_local_failed'
		BEGIN
			SELECT RAISE(FAIL, 'injected Gmail outcome failure');
		END
	`)
	require.NoError(err)
	fixture.client.updateDraft = &gmail.Draft{
		ID:      "gmail-draft-managed",
		Message: gmail.RawMessage{ID: "gmail-message-edited", ThreadID: "gmail-thread-1"},
	}

	events, err := fixture.lifecycle(t, api.CLIRunDraftEditCommand, draft, "candidate")
	require.Error(err)
	assert.Equal("accepted_local_failed", err.Error())
	require.Len(events, 1)
	var output gmailDraftLifecycleOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assert.Equal("accepted_local_failed", output.Status)
	assert.Equal("gmail-message-original", output.Receipt.GmailMessageID)
	assert.Equal("gmail-message-edited", output.PendingReplacementGmailMessageID)
	require.NotNil(output.ProviderObservation)
	assert.Equal("gmail-message-edited", output.ProviderObservation.GmailMessageID)
	assert.True(output.ManualReconciliation)

	latest, err := fixture.store.GetGmailDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	require.NotNil(latest.Pending)
	assert.Empty(latest.Pending.ReplacementGmailMessageID)
}

func TestGmailDraftRemoteUnknownCreateHumanOutputIncludesRFC822ID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	fixture.client.createErr = &gmail.DraftWriteError{
		State: gmail.DraftStateRemoteUnknown, Code: "remote_unknown", Err: errors.New("response lost"),
	}

	events, err := fixture.create(t, "unknown create", false)
	require.Error(err)
	assert.Equal("remote_unknown", err.Error())
	require.Len(events, 1)
	assert.Equal(cliStreamStderr, events[0].Type)
	assert.Contains(events[0].Data, "RFC822 Message-ID: <")
	assert.Contains(events[0].Data, "inspect operation")
	var count int
	require.NoError(fixture.store.DB().QueryRow("SELECT COUNT(*) FROM gmail_drafts").Scan(&count))
	assert.Zero(count)
}
