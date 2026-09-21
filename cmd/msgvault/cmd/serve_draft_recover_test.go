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
	"strconv"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
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

func newDraftRecoveryHTTPServer(t *testing.T, fixture reviewManagedLifecycleFixture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-key", AgentAccess: true},
		},
		Store:  fixture.adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
}

func issueDraftRecoveryToken(t *testing.T, server *httptest.Server, sourceID int64, permission string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"label":       "recovery-agent",
		"permissions": []string{permission},
		"source_ids":  []int64{sourceID},
	})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agent-tokens", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "owner-key")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var issued struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&issued))
	return issued.Secret
}

func runDraftRecoveryHTTP(t *testing.T, server *httptest.Server, secret, draftID string, revision int64) []api.CLIRunEvent {
	t.Helper()
	body, err := json.Marshal(api.CLIRunRequest{Args: []string{
		draftRecoverCommand, draftID, "--revision", strconv.FormatInt(revision, 10), "--json",
	}})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Msgvault-Agent-Token", secret)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var events []api.CLIRunEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		var event api.CLIRunEvent
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	return events
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
		defer func() { _ = resp.Body.Close() }()
		requirements.Equal(http.StatusCreated, resp.StatusCode)
		var issued struct {
			Secret string `json:"secret"`
		}
		requirements.NoError(json.NewDecoder(resp.Body).Decode(&issued))
		return issued.Secret
	}

	run := func(secret, draftID string, revision int64) []api.CLIRunEvent {
		body, err := json.Marshal(api.CLIRunRequest{Args: []string{draftRecoverCommand, draftID, "--revision", strconv.FormatInt(revision, 10), "--json"}})
		requirements.NoError(err)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
		requirements.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Msgvault-Agent-Token", secret)
		resp, err := http.DefaultClient.Do(req)
		requirements.NoError(err)
		defer func() { _ = resp.Body.Close() }()
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

	allowed := run(issue("draft.edit"), fixture.draft.DraftID, 1)
	requirements.Len(allowed, 2)
	assertions.Equal(cliStreamStdout, allowed[0].Type)
	assertions.Contains(allowed[0].Data, `"status":"active"`)
	var delegatedOutput draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(allowed[0].Data), &delegatedOutput))
	assertions.Empty(delegatedOutput.Content)
	assertions.Empty(delegatedOutput.RawMIME)
	assertions.Empty(delegatedOutput.CandidateContent)
	assertions.Equal("complete", allowed[1].Type)

	denied := run(issue("draft.delete"), fixture.draft.DraftID, 1)
	requirements.Len(denied, 1)
	assertions.Empty(denied[0].Data)
	assertions.Equal("not_permitted", denied[0].Error)

	otherSource, err := fixture.store.GetOrCreateSource("imap", "imap://other@example.com:143")
	requirements.NoError(err)
	conversationID, err := fixture.store.EnsureConversation(otherSource.ID, "other-draft", "Other draft")
	requirements.NoError(err)
	otherReceipt := store.IMAPDraftReceipt{SourceID: otherSource.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	otherDraft, err := fixture.store.PersistIMAPDraftContext(t.Context(), otherReceipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: otherSource.ID, SourceMessageID: store.IMAPDraftSourceMessageID(otherReceipt),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			RawMIME: []byte("From: alice@example.com\r\n\r\nother\r\n"),
		}
	})
	requirements.NoError(err)
	for _, tc := range []struct {
		name     string
		draftID  string
		revision int64
	}{
		{name: "missing", draftID: "missing-draft", revision: 1},
		{name: "out of scope current", draftID: otherDraft.DraftID, revision: 1},
		{name: "out of scope stale", draftID: otherDraft.DraftID, revision: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Helper()
			events := run(issue("draft.edit"), tc.draftID, tc.revision)
			requirements.Len(events, 1)
			assertions.Empty(events[0].Data)
			assertions.Equal("not_permitted", events[0].Error)
		})
	}
}

func TestDraftRecoverDelegatedUnknownReplacementRedactsCandidate(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	server := newDraftRecoveryHTTPServer(t, fixture)
	defer server.Close()
	secret := issueDraftRecoveryToken(t, server, fixture.source.ID, string(agentgrant.PermissionDraftEdit))
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)

	events := runDraftRecoveryHTTP(t, server, secret, fixture.draft.DraftID, 1)
	requirements.Len(events, 2)
	assertions.Equal(cliStreamStderr, events[0].Type)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Empty(output.PendingCode)
	assertions.Equal("unknown_replacement", output.Status)
	assertions.Equal("unknown_replacement", output.RefusalCode)
	assertions.Empty(output.Content)
	assertions.Empty(output.RawMIME)
	assertions.Empty(output.CandidateContent)
	assertions.Equal("unknown_replacement", events[1].Error)
}

func TestDraftRecoverPolicyPrecedesSettledOutputThroughHTTP(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		prepare    func(*testing.T, reviewManagedLifecycleFixture) (string, int64)
	}{
		{
			name:       "active",
			permission: string(agentgrant.PermissionDraftEdit),
			prepare: func(_ *testing.T, fixture reviewManagedLifecycleFixture) (string, int64) {
				return fixture.draft.DraftID, fixture.draft.Revision
			},
		},
		{
			name:       "discarded",
			permission: string(agentgrant.PermissionDraftDelete),
			prepare: func(t *testing.T, fixture reviewManagedLifecycleFixture) (string, int64) {
				t.Helper()
				requirements := require.New(t)
				_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
				requirements.NoError(err)
				removeRecoveryOriginal(t, fixture)
				_, err = runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
				requirements.NoError(err)
				latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
				requirements.NoError(err)
				return latest.DraftID, latest.Revision
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture, _ := newDraftRecoveryFixture(t)
			draftID, revision := tc.prepare(t, fixture)
			providerCalls := 0
			fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
				providerCalls++
				return nil, errors.New("policy refusal must not connect")
			}
			fixture.adapter.draftPolicy = nil
			execution, err := fixture.store.AcquireSyncExecutionContext(t.Context(), fixture.source.ID)
			requirements.NoError(err)
			t.Cleanup(func() { _ = execution.Release() })
			server := newDraftRecoveryHTTPServer(t, fixture)
			t.Cleanup(server.Close)
			secret := issueDraftRecoveryToken(t, server, fixture.source.ID, tc.permission)

			events := runDraftRecoveryHTTP(t, server, secret, draftID, revision)
			requirements.Len(events, 1)
			assertions.Empty(events[0].Data)
			assertions.Equal("draft_disabled", events[0].Error)
			assertions.Zero(providerCalls)
		})
	}
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
	for _, replacementState := range []string{"live", "absent", "deleted", "not draft"} {
		t.Run(replacementState, func(t *testing.T) {
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
			requirements.Equal(int64(2), published.Revision)
			client := imaplib.NewClient(fixture.config, testutil.IMAPTestPassword)
			defer func() { _ = client.Close() }()
			switch replacementState {
			case "absent":
				_, err = client.RemoveDraft(t.Context(), recoveryTestReceipt(replacement))
				requirements.NoError(err)
			case "deleted":
				reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDeleted)
			case "not draft":
				reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsDel, emersionimap.FlagDraft)
			}

			events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "2", "--json")
			requirements.NoError(err)
			requirements.Len(events, 1)
			var output draftLifecycleOutput
			requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
			assertions.Equal("edited", output.Status)
			assertions.Equal(int64(2), output.Revision)
			latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			requirements.NoError(err)
			assertions.Nil(latest.Pending)
			assertions.Equal(replacement, latest.CurrentReceipt)
			original, err := client.InspectDraft(t.Context(), recoveryTestReceipt(fixture.draft.CurrentReceipt))
			requirements.NoError(err)
			assertions.False(original.Present)
		})
	}
}

func TestDraftRecoverRefusesUnavailableReplacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{name: "absent", code: "absent"},
		{name: "deleted", code: "already_deleted"},
		{name: "not draft", code: "not_draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture, user := newDraftRecoveryFixture(t)
			candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Replacement\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
			if tc.name == "deleted" || tc.name == "not draft" {
				testutil.AppendIMAPRawMessage(t, user, "Drafts", candidate)
				if tc.name == "deleted" {
					reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
					reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDeleted)
				}
			}
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
			requirements.NoError(err)
			replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
			requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &replacement))

			uidNextBefore := recoveryUIDNext(t, fixture)
			events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
			requirements.Error(err)
			assertions.Equal(tc.code, err.Error())
			requirements.Len(events, 1)
			assertions.Equal(cliStreamStderr, events[0].Type)
			assertions.Contains(events[0].Data, `"status":"refused"`)
			assertions.Equal(uidNextBefore, recoveryUIDNext(t, fixture))

			latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
			requirements.NoError(err)
			assertions.Equal(int64(1), latest.Revision)
			assertions.Equal(fixture.draft.CurrentReceipt, latest.CurrentReceipt)
			requirements.NotNil(latest.Pending)
			assertions.Equal(store.IMAPDraftOperationEdit, latest.Pending.Operation)
			assertions.Equal("append_uidplus", latest.Pending.Code)
			assertions.Equal(replacement, *latest.Pending.ReplacementReceipt)
		})
	}
}

func TestDraftRecoverRemovesPresentOriginal(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	uidNextBefore := recoveryUIDNext(t, fixture)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("deleted", output.Status)
	assertions.Equal(int64(2), output.Revision)
	assertions.Equal(uidNextBefore, recoveryUIDNext(t, fixture))
	assertions.Equal(uint32(0), reviewDraftMailboxCount(t, fixture.config.Addr()))
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	assertions.Nil(latest.Pending)
	assertions.NotNil(latest.DiscardedAt)
}

func TestDraftRecoverArchivedReplacementReportsAcceptedLocalFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, user := newDraftRecoveryFixture(t)
	candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Replacement\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
	testutil.AppendIMAPRawMessage(t, user, "Drafts", candidate)
	reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 2, emersionimap.StoreFlagsAdd, emersionimap.FlagDraft)
	message, err := fixture.store.GetMessageContext(t.Context(), fixture.draft.CurrentMessageID)
	requirements.NoError(err)
	archivedReceipt := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
	_, err = fixture.store.PersistIMAPDraftContext(t.Context(), archivedReceipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: fixture.source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(archivedReceipt),
				MessageType: store.MessageTypeEmail, ConversationID: message.ConversationID,
			},
			RawMIME: candidate,
		}
	})
	requirements.NoError(err)
	_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
	requirements.NoError(err)
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &archivedReceipt))

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("accepted_local_failed", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, `"status":"accepted_local_failed"`)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	assertions.Equal(int64(1), latest.Revision)
	assertions.Equal(fixture.draft.CurrentReceipt, latest.CurrentReceipt)
	requirements.NotNil(latest.Pending)
	assertions.Equal("append_uidplus", latest.Pending.Code)
	assertions.Equal(archivedReceipt, *latest.Pending.ReplacementReceipt)
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

func TestDraftRecoverKeepsRowUnchangedOnRemovalGenerationRefusal(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	control := &reviewUIDValidityChangeControl{}
	addr, user := startReviewUIDValidityChangeServer(t, control)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\nContent-Type: text/plain\r\n\r\noriginal\r\n"))
	})
	control.selects.Store(0)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	before, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	var beforeUpdatedAt string
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`
		SELECT CAST(updated_at AS TEXT) FROM imap_drafts WHERE draft_id = ?
	`), fixture.draft.DraftID).Scan(&beforeUpdatedAt))

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("uidvalidity_mismatch", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, `"code":"uidvalidity_mismatch"`)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal(before.Pending.Code, output.PendingCode)
	assertions.Equal("uidvalidity_mismatch", output.RefusalCode)
	assertions.Equal(int32(2), control.selects.Load())
	after, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	var afterUpdatedAt string
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(`
		SELECT CAST(updated_at AS TEXT) FROM imap_drafts WHERE draft_id = ?
	`), fixture.draft.DraftID).Scan(&afterUpdatedAt))
	assertions.Equal(before.Revision, after.Revision)
	assertions.Equal(before.CurrentReceipt, after.CurrentReceipt)
	requirements.NotNil(after.Pending)
	assertions.Equal(before.Pending.Code, after.Pending.Code)
	assertions.Equal(before.Pending.OriginalReceipt, after.Pending.OriginalReceipt)
	assertions.Equal(before.Pending.ReplacementReceipt, after.Pending.ReplacementReceipt)
	assertions.Equal(beforeUpdatedAt, afterUpdatedAt)
}

func TestDraftRecoverRecordsWriteAttemptedRemovalFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	addr, user := startReviewExpungeFailureServer(t)
	fixture := newReviewManagedLifecycleFixtureOnServer(t, addr, func() {
		testutil.AppendIMAPRawMessage(t, user, "Drafts", []byte("From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Original\r\nContent-Type: text/plain\r\n\r\noriginal\r\n"))
	})
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("expunge_failed", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, `"pending_code":"expunge_failed"`)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(latest.Pending)
	assertions.Equal("expunge_failed", latest.Pending.Code)
	assertions.Equal(int64(1), latest.Revision)
}

func TestDraftRecoverReportsProviderRefusal(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		return imaplib.NewClient(&imaplib.Config{
			Host: "127.0.0.1", Port: 1, Username: testutil.IMAPTestUsername,
		}, testutil.IMAPTestPassword), nil
	}

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("provider_refused", err.Error())
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, `"status":"refused"`)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NotNil(latest.Pending)
	assertions.Empty(latest.Pending.Code)
	assertions.Equal(int64(1), latest.Revision)
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
			t.Helper()
			requirements := require.New(t)
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
			requirements.NoError(err)
			fixture.adapter.draftPolicy = nil
		}, code: "draft_disabled"},
		{name: "invalid mailbox", setup: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
			t.Helper()
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

func TestDraftRecoverSavedRemovedEditFinishesWithoutProvider(t *testing.T) {
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
	requirements.Equal(int64(2), published.Revision)
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, published.Revision, store.IMAPDraftCodeRemoved, nil))
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		return nil, errors.New("saved removal must finish locally")
	}

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"edited"`)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	assertions.Nil(latest.Pending)
	assertions.Equal(int64(2), latest.Revision)
	assertions.Equal(replacement, latest.CurrentReceipt)
}

func TestDraftRecoverReloadsSettledStateAfterSourceLock(t *testing.T) {
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
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, published.Revision, store.IMAPDraftCodeRemoved, nil))
	providerCalls := 0
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return nil, errors.New("settled recovery must not connect")
	}
	fired := false
	previous := slog.Default()
	slog.SetDefault(slog.New(reviewDraftCommitHandler{
		Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
		onCommit: func() {
			if fired {
				return
			}
			fired = true
			_, finishErr := fixture.store.FinishIMAPDraftRemovalContext(t.Context(), fixture.draft.DraftID, published.Revision)
			requirements.NoError(finishErr)
		},
	}))
	defer slog.SetDefault(previous)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.True(fired)
	requirements.Len(events, 1)
	assertions.Contains(events[0].Data, `"status":"active"`)
	assertions.Zero(providerCalls)
}

func TestDraftRecoverReloadsActionAndSourceAfterSourceLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, reviewManagedLifecycleFixture)
	}{
		{
			name: "action",
			mutate: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
				t.Helper()
				_, err := fixture.store.DB().Exec(fixture.store.Rebind(`
					UPDATE imap_drafts
					SET pending_operation = 'delete', pending_raw = NULL,
					    pending_replacement_mailbox = NULL,
					    pending_replacement_uidvalidity = NULL,
					    pending_replacement_uid = NULL
					WHERE draft_id = ?
				`), fixture.draft.DraftID)
				require.NoError(t, err)
			},
		},
		{
			name: "draft removed",
			mutate: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
				t.Helper()
				_, err := fixture.store.DB().Exec(fixture.store.Rebind(`DELETE FROM imap_drafts WHERE draft_id = ?`), fixture.draft.DraftID)
				require.NoError(t, err)
			},
		},
		{
			name: "source type",
			mutate: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
				t.Helper()
				_, err := fixture.store.DB().Exec(fixture.store.Rebind(`
					UPDATE sources SET source_type = 'gmail' WHERE id = ?
				`), fixture.source.ID)
				require.NoError(t, err)
			},
		},
		{
			name: "source identifier",
			mutate: func(t *testing.T, fixture reviewManagedLifecycleFixture) {
				t.Helper()
				_, err := fixture.store.DB().Exec(fixture.store.Rebind(`
					UPDATE sources SET identifier = 'imap://changed@example.test:143' WHERE id = ?
				`), fixture.source.ID)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture, _ := newDraftRecoveryFixture(t)
			candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
			requirements.NoError(err)
			replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
			requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "append_uidplus", &replacement))
			grant := &agentgrant.Grant{
				ID:          "reload-grant",
				Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit},
				Sources:     []agentgrant.SourceRef{{ID: fixture.source.ID, Type: fixture.source.SourceType, Identifier: fixture.source.Identifier}},
			}
			providerCalls := 0
			fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
				providerCalls++
				return nil, errors.New("reload denial must not connect")
			}
			fired := false
			previous := slog.Default()
			slog.SetDefault(slog.New(reviewDraftCommitHandler{
				Handler: slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}),
				onCommit: func() {
					if fired {
						return
					}
					fired = true
					tc.mutate(t, fixture)
				},
			}))
			defer slog.SetDefault(previous)

			var events []api.CLIRunEvent
			err = fixture.adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args:  []string{draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json"},
				Grant: grant,
			}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			})
			requirements.Error(err)
			assertions.Equal("not_permitted", err.Error())
			assertions.True(fired)
			assertions.Empty(events)
			assertions.Zero(providerCalls)
		})
	}
}

func TestDraftRecoverRejectsInvalidPendingOperation(t *testing.T) {
	testutil.SkipIfPostgres(t, "invalid pending operation injection uses SQLite check-constraint bypass")
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	requirements.NoError(func() error {
		_, err := fixture.store.DB().Exec("PRAGMA ignore_check_constraints = ON")
		return err
	}())
	_, err = fixture.store.DB().Exec(fixture.store.Rebind(`
		UPDATE imap_drafts SET pending_operation = 'invalid' WHERE draft_id = ?
	`), fixture.draft.DraftID)
	requirements.NoError(err)
	requirements.NoError(func() error {
		_, err := fixture.store.DB().Exec("PRAGMA ignore_check_constraints = OFF")
		return err
	}())
	providerCalls := 0
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return nil, errors.New("invalid operation must not connect")
	}

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.Error(err)
	assertions.Equal("invalid_state", err.Error())
	assertions.Empty(events)
	assertions.Zero(providerCalls)
}

func TestDraftRecoverRejectsWrongSourceAndAction(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission agentgrant.Permission
		source     func(reviewManagedLifecycleFixture) agentgrant.SourceRef
	}{
		{
			name:       "wrong action",
			permission: agentgrant.PermissionDraftDelete,
			source: func(fixture reviewManagedLifecycleFixture) agentgrant.SourceRef {
				return agentgrant.SourceRef{ID: fixture.source.ID, Type: fixture.source.SourceType, Identifier: fixture.source.Identifier}
			},
		},
		{
			name:       "wrong source type",
			permission: agentgrant.PermissionDraftEdit,
			source: func(fixture reviewManagedLifecycleFixture) agentgrant.SourceRef {
				return agentgrant.SourceRef{ID: fixture.source.ID, Type: "gmail", Identifier: fixture.source.Identifier}
			},
		},
		{
			name:       "wrong source identifier",
			permission: agentgrant.PermissionDraftEdit,
			source: func(fixture reviewManagedLifecycleFixture) agentgrant.SourceRef {
				return agentgrant.SourceRef{ID: fixture.source.ID, Type: fixture.source.SourceType, Identifier: "imap://changed@example.test:143"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture, _ := newDraftRecoveryFixture(t)
			candidate := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nContent-Type: text/plain\r\n\r\ncandidate\r\n")
			_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationEdit, candidate)
			requirements.NoError(err)
			grant := &agentgrant.Grant{
				ID:          "denial-grant",
				Permissions: []agentgrant.Permission{tc.permission},
				Sources:     []agentgrant.SourceRef{tc.source(fixture)},
			}
			providerCalls := 0
			fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
				providerCalls++
				return nil, errors.New("denial must not connect")
			}
			var events []api.CLIRunEvent
			err = fixture.adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{
				Args:  []string{draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json"},
				Grant: grant,
			}, func(event api.CLIRunEvent) error {
				events = append(events, event)
				return nil
			})
			requirements.Error(err)
			assertions.Equal("not_permitted", err.Error())
			assertions.Empty(events)
			assertions.Zero(providerCalls)
		})
	}
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

func TestDraftRecoverRefusalAfterCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.adapter.draftClientFactory = func(context.Context, *store.Source) (*imaplib.Client, error) {
		cancel()
		return imaplib.NewClient(fixture.config, testutil.IMAPTestPassword), nil
	}
	var events []api.CLIRunEvent
	err = fixture.adapter.runCLIDraftLifecycle(ctx, api.CLIRunRequest{
		Args: []string{draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json"},
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.EqualError(err, "cancelled")
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("refused", output.Status)
	assertions.Equal("cancelled", output.RefusalCode)
	assertions.Empty(output.PendingCode)
	requirements.NotNil(output.ProviderObservation)
	assertions.Equal("cancelled", output.ProviderObservation.Code)
}

func TestDraftRecoverDelegatedCompletionWithoutContentReads(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	_, err = fixture.store.DB().Exec(fixture.store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?
	`), []byte("invalid compressed content"), fixture.draft.CurrentMessageID)
	requirements.NoError(err)
	server := newDraftRecoveryHTTPServer(t, fixture)
	defer server.Close()
	secret := issueDraftRecoveryToken(t, server, fixture.source.ID, string(agentgrant.PermissionDraftDelete))

	events := runDraftRecoveryHTTP(t, server, secret, fixture.draft.DraftID, 1)
	requirements.Len(events, 2)
	assertions.Equal(cliStreamStdout, events[0].Type)
	assertions.Equal("complete", events[1].Type)
	assertions.Empty(events[1].Error)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("deleted", output.Status)
	assertions.Empty(output.Content)
	assertions.Empty(output.RawMIME)
	assertions.Empty(output.CandidateContent)
}

func TestDraftRecoverAlreadyDeletedNeedsManualExpunge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	_, err := fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, 1, store.IMAPDraftOperationDelete, nil)
	requirements.NoError(err)
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, 1, "expunge_failed", nil))
	reviewStoreFlagsForLifecycle(t, fixture.config.Addr(), 1, emersionimap.StoreFlagsAdd, emersionimap.FlagDeleted)

	events, err := runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.EqualError(err, "already_deleted")
	requirements.Len(events, 1)
	var output draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("expunge_failed", output.PendingCode)
	assertions.Equal("already_deleted", output.RefusalCode)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, fixture.config.Addr()))

	client, err := imapclient.DialInsecure(fixture.config.Addr(), nil)
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	requirements.NoError(client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	_, err = client.Select("Drafts", nil).Wait()
	requirements.NoError(err)
	requirements.NoError(client.UIDExpunge(emersionimap.UIDSetNum(1)).Close())
	events, err = runReviewLifecycle(t, fixture.adapter, draftRecoverCommand, fixture.draft.DraftID, "--revision", "1", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	assertions.Equal("deleted", output.Status)
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	assertions.Nil(latest.Pending)
	assertions.NotNil(latest.DiscardedAt)
}
