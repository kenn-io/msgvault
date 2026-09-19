package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const draftRecoverCommand = "draft-recover"

func newDraftRecoveryFixture(t *testing.T) (reviewManagedLifecycleFixture, *imapmemserver.User) {
	t.Helper()
	addr, user := testutil.StartIMAPMemServerForDrafts(t, testutil.IMAPDraftServerOptions{
		MessagesPerMailbox: map[string]int{"Drafts": 0},
		Caps:               emersionimap.CapSet{emersionimap.CapIMAP4rev1: {}, emersionimap.CapUIDPlus: {}},
	})
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\nContent-Type: text/plain\r\n\r\noriginal\r\n"))
	})
	return fixture, user
}

func removeRecoveryOriginal(t *testing.T, fixture reviewManagedLifecycleFixture) {
	t.Helper()
	client := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
	observation, err := client.RemoveDraft(t.Context(), recoveryTestReceipt(fixture.draft.CurrentReceipt))
	require.NoError(t, err)
	require.True(t, observation.Complete)
	require.NoError(t, client.Close())
}

func recoveryTestReceipt(receipt store.IMAPDraftReceipt) imaplib.DraftReceipt {
	return imaplib.DraftReceipt{Mailbox: receipt.Mailbox, UIDValidity: receipt.UIDValidity, UID: receipt.UID}
}

func recoveryUIDNext(t *testing.T, fixture reviewManagedLifecycleFixture) uint32 {
	t.Helper()
	client, err := imapclient.DialInsecure(fixture.config.Addr(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	status, err := client.Status("Drafts", &emersionimap.StatusOptions{UIDNext: true}).Wait()
	require.NoError(t, err)
	return uint32(status.UIDNext)
}

func TestDraftRecoverReconcilesManualCleanup(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	removeRecoveryOriginal(t, fixture)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("deleted", output.Status)
	assertions.Equal(int64(2), output.Revision)
	assertions.Equal("discarded", output.Lifecycle)
	assertions.Equal(uint32(0), reviewDraftMailboxCount(t, fixture.config.Addr()))
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.Nil(latest.Pending)
	requirements.NotNil(latest.DiscardedAt)
}

func TestDraftRecoverThroughHTTP(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-key", AgentAccess: true},
		},
		Store:  fixture.adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)

	issue := func(permission string) string {
		body, err := json.Marshal(map[string]any{
			"label":       "recovery-agent",
			"permissions": []string{permission},
			"source_ids":  []int64{fixture.source.ID},
		})
		requirements.NoError(err)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agent-tokens", bytes.NewReader(body))
		requirements.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "owner-key")
		resp, err := http.DefaultClient.Do(req)
		requirements.NoError(err)
		defer resp.Body.Close()
		requirements.Equal(http.StatusCreated, resp.StatusCode)
		var issued struct {
			Secret string `json:"secret"`
		}
		requirements.NoError(json.NewDecoder(resp.Body).Decode(&issued))
		return issued.Secret
	}

	run := func(secret string) []api.CLIRunEvent {
		body, err := json.Marshal(api.CLIRunRequest{Args: []string{draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json"}})
		requirements.NoError(err)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
		requirements.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Msgvault-Agent-Token", secret)
		resp, err := http.DefaultClient.Do(req)
		requirements.NoError(err)
		defer resp.Body.Close()
		requirements.Equal(http.StatusOK, resp.StatusCode)
		var events []api.CLIRunEvent
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var event api.CLIRunEvent
			requirements.NoError(json.Unmarshal(scanner.Bytes(), &event))
			events = append(events, event)
		}
		requirements.NoError(scanner.Err())
		return events
	}

	allowed := run(issue("draft.edit"))
	requirements.Len(allowed, 2)
	assertions.Equal(cliStreamStdout, allowed[0].Type)
	assertions.Contains(allowed[0].Data, `"status":"active"`)
	assertions.Equal("complete", allowed[1].Type)

	denied := run(issue("draft.delete"))
	requirements.Len(denied, 1)
	assertions.Empty(denied[0].Data)
	assertions.Equal("not_permitted", denied[0].Error)
}

func TestDraftRecoverPublishesKnownReplacement(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, user := newDraftRecoveryFixture(t)
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Replacement\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
	testutil.AppendIMAPRawMessage(t, user, "Drafts", candidate)
	reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
	uidNextBefore := recoveryUIDNext(t, fixture)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)
	replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &replacement))

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("edited", output.Status)
	assertions.Equal(int64(2), output.Revision)
	assertions.Equal(uint32(2), output.Receipt.UID)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, fixture.config.Addr()))
	assertions.Equal(uidNextBefore, recoveryUIDNext(t, fixture))
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.Nil(latest.Pending)
	requirements.Equal(replacement, latest.CurrentReceipt)
}

func TestDraftRecoverCleansUpPublishedEdit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, user := newDraftRecoveryFixture(t)
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Replacement\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
	testutil.AppendIMAPRawMessage(t, user, "Drafts", candidate)
	reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)
	replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &replacement))
	published := publishRecoveryReplacement(t, fixture, candidate)
	assertions.Equal(int64(2), published.Revision)
	removeRecoveryOriginal(t, fixture)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("edited", output.Status)
	assertions.Equal(int64(2), output.Revision)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.Nil(latest.Pending)
}

func TestDraftRecoverRefusesDifferingRecordedGenerations(t *testing.T) {
	requirements := require.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)
	replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 2, UID: 2}
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &replacement))

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assert.Equal(t, "uidvalidity_mismatch", err.Error())
	requirements.Len(events, 1)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(latest.Pending)
	assert.Equal(t, int64(1), latest.Revision)
}

func TestDraftRecoverRefusesUnknownReplacement(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, []byte("candidate"))
	requirements.NoError(err)
	providerCalls := 0
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return nil, errors.New("unknown replacement must not connect")
	}

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("unknown_replacement", err.Error())
	assertions.Equal(0, providerCalls)
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(latest.Pending)
	assertions.Equal(int64(1), latest.Revision)
}

func TestDraftRecoverPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, reviewManagedLifecycleFixture)
		code  string
	}{
		{name: "draft disabled", setup: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
			requirements := require.New(t)
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
			requirements.NoError(err)
			fixture.adapter.draftPolicy = nil
		}, code: "draft_disabled"},
		{name: "invalid mailbox", setup: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
			requirements := require.New(t)
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
			requirements.NoError(err)
			fixture.adapter.draftPolicy[0].Mailbox = "\n"
		}, code: "invalid_mailbox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture, _ := newDraftRecoveryFixture(t)
			tc.setup(t, fixture)
			events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
			requirements.Error(err)
			assertions.Equal(tc.code, err.Error())
			assertions.Empty(events)
			assertions.Equal(uint32(1), reviewDraftMailboxCount(t, fixture.config.Addr()))
		})
	}
}

func TestDraftRecoverParser(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, args := range [][]string{
		{draftRecoverCommand, "draft-1"},
		{draftRecoverCommand, "draft-1", "--revision", "1", "--body", "blocked"},
		{draftRecoverCommand, "draft-1", "--revision", "0"},
	} {
		_, err := parseDraftLifecycleArgs(args)
		requirements.Error(err)
		assertions.Equal("invalid_args", err.Error())
	}
	intent, err := parseDraftLifecycleArgs([]string{draftRecoverCommand, "draft-1", "--revision", "1", "--json"})
	requirements.NoError(err)
	assertions.Equal(draftRecoverCommand, intent.Operation)
}

func TestDraftRecoverSavedRemovedDeleteFinishesWithoutProvider(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftCodeRemoved, nil))
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		return nil, errors.New("saved removal must finish locally")
	}
	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"deleted"`)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, fixture.config.Addr()))
}

func TestDraftRecoverSettledRepeatNoProvider(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	normalFactory := fixture.adapter.draftClientFactory
	providerCalls := 0
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return nil, errors.New("settled recovery must not connect")
	}
	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"active"`)
	assertions.Equal(0, providerCalls)

	fixture.adapter.draftClientFactory = normalFactory
	_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	removeRecoveryOriginal(t, fixture)
	_, err = runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)

	providerCalls = 0
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return nil, errors.New("discarded recovery must not connect")
	}
	events, err = runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"already_discarded"`)
	assertions.Equal(0, providerCalls)
}

func TestDraftRecoverRejectsEnvAndCwd(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	adapter := &storeAPIAdapter{}
	for _, request := range []api.CLIRunRequest{
		{Args: []string{draftRecoverCommand, "draft-1", "--revision", "1"}, Env: map[string]string{"HOME": "blocked"}},
		{Args: []string{draftRecoverCommand, "draft-1", "--revision", "1"}, Cwd: `C:\blocked`},
	} {
		err := adapter.runCLIDraftLifecycle(t.Context(), request, nil)
		requirements.Error(err)
		assertions.Equal("invalid_args", err.Error())
	}
}

func publishRecoveryReplacement(t *testing.T, fixture reviewManagedLifecycleFixture, candidate []byte) store.IMAPDraft {
	t.Helper()
	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	require.NoError(t, err)
	parsed, err := msgmime.Parse(candidate)
	require.NoError(t, err)
	message, err := fixture.store.GetMessageContext(t.Context(), draft.CurrentMessageID)
	require.NoError(t, err)
	replyTo, err := fixture.store.GetMessageReplyToMessageIDContext(t.Context(), draft.CurrentMessageID)
	require.NoError(t, err)
	replacement := *draft.Pending.ReplacementReceipt
	participants, build := draftLifecyclePersistData(message.ConversationID, replyTo, imaplib.ReplyDraft{Raw: candidate, Parsed: parsed}, replacement)
	published, err := fixture.store.PublishIMAPDraftReplacementContext(t.Context(), draft.DraftID, draft.Revision, participants, build)
	require.NoError(t, err)
	return published
}
