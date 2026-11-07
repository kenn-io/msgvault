package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftComposeArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	intent, err := parseDraftComposeArgs([]string{
		"draft-compose", "--source-id", "42", "--from", "owner@example.test",
		"--to", "to@example.test", "--cc=copy@example.test", "--bcc", "hidden@example.test",
		"--subject", "Subject", "--body=body", "--json",
	})
	requirements.NoError(err)
	assertions.Equal(int64(42), intent.SourceID)
	assertions.Equal([]string{"to@example.test"}, intent.To)
	assertions.Equal([]string{"copy@example.test"}, intent.Cc)
	assertions.Equal([]string{"hidden@example.test"}, intent.Bcc)
	assertions.True(intent.JSON)

	for _, args := range [][]string{
		{"draft-compose", "--source-id", "0", "--to", "to@example.test"},
		{"draft-compose", "--to", "to@example.test"},
		{"draft-compose", "--source-id", "42"},
		{"draft-compose", "--source-id", "42", "--account", "owner@example.test", "--to", "to@example.test"},
	} {
		_, err := parseDraftComposeArgs(args)
		assertions.Error(err)
	}
}

func TestDraftComposeEndToEnd(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	events := make([]api.CLIRunEvent, 0, 1)
	err := adapter.runCLIComposeDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-compose", "--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername,
		"--to", "to@example.test", "--cc", "copy@example.test", "--bcc", "hidden@example.test",
		"--subject", "Compose subject", "--body", "compose body", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal(draftReplyStatusCreated, result.Status)
	assertions.Equal(int64(1), result.Revision)

	message, err := fixture.store.GetMessage(result.MessageID)
	requirements.NoError(err)
	assertions.Equal([]string{"to@example.test"}, message.To)
	assertions.Equal([]string{"copy@example.test"}, message.Cc)
	assertions.Equal([]string{"hidden@example.test"}, message.Bcc)
	raw, err := fixture.store.GetMessageRaw(result.MessageID)
	requirements.NoError(err)
	assertions.Contains(string(raw), "Bcc:")
	assertions.Contains(string(raw), "hidden@example.test")

	// The backends tokenize punctuation in full email queries differently.
	matches, total, err := fixture.store.SearchMessages("copy", 0, 10)
	requirements.NoError(err)
	requirements.Equal(int64(1), total)
	requirements.Len(matches, 1)
	assertions.Equal(result.MessageID, matches[0].ID)
}

func TestLineReplyThroughOwnerCLI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	clearServerKeyEnvironment(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	a.config.Server.DaemonAutoStart = new(false)
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.Handle("/", api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: a.logger}).Router())
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(a.config.Server.AuthenticationKey()))
	_, err := daemonRuntimeStore(a.config.Data.DataDir).Write(rt.Record)
	require.NoError(err)

	cmd := newSendLineReplyCommand()
	cmd.SetContext(withStoreResolverConfig(t, a.config))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{draft.DraftID, "--revision=1", "--json"})
	require.NoError(cmd.ExecuteContext(cmd.Context()))
	var sent line.ReplyReceipt
	require.NoError(json.Unmarshal(stdout.Bytes(), &sent))
	assert.Equal("sent", sent.Status)
	assert.True(sent.Accepted)
	assert.Empty(stderr.String())
	assert.Equal(1, fake.Count("POST", "/v2/bot/message/push"))
}

func TestDraftComposeHTTPPublishesManagedDraft(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-test-key"},
		},
		Store:  adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)

	args := []string{
		"draft-compose", "--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "to@example.test",
		"--cc", "copy@example.test", "--bcc", "hidden@example.test",
		"--subject", "Compose subject", "--body", "compose body", "--json",
	}
	body, err := json.Marshal(map[string]any{"args": args})
	requirements.NoError(err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	requirements.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "owner-test-key")
	response, err := http.DefaultClient.Do(request)
	requirements.NoError(err)
	defer func() { _ = response.Body.Close() }()
	requirements.Equal(http.StatusOK, response.StatusCode)

	var events []api.CLIRunEvent
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var event api.CLIRunEvent
		requirements.NoError(json.Unmarshal(scanner.Bytes(), &event))
		events = append(events, event)
	}
	requirements.NoError(scanner.Err())
	requirements.Len(events, 2)
	requirements.Equal(cliStreamStdout, events[0].Type)
	requirements.Equal("complete", events[1].Type)

	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal(draftReplyStatusCreated, result.Status)
	assertions.Equal(fixture.source.ID, result.SourceID)
	assertions.Equal("Drafts", result.Mailbox)
	assertions.NotZero(result.UID)
	assertions.NotZero(result.UIDValidity)
	assertions.Equal(int64(1), result.Revision)

	draft, err := fixture.store.GetIMAPDraft(result.DraftID)
	requirements.NoError(err)
	assertions.Equal(result.MessageID, draft.CurrentMessageID)
	assertions.Equal(result.UID, draft.CurrentReceipt.UID)
	assertions.Equal(result.UIDValidity, draft.CurrentReceipt.UIDValidity)
	message, err := fixture.store.GetMessage(result.MessageID)
	requirements.NoError(err)
	assertions.Equal([]string{"to@example.test"}, message.To)
	assertions.Equal([]string{"copy@example.test"}, message.Cc)
	assertions.Equal([]string{"hidden@example.test"}, message.Bcc)
	storedRaw, err := fixture.store.GetMessageRaw(result.MessageID)
	requirements.NoError(err)
	assertions.Contains(string(storedRaw), "Bcc:")
	assertions.Contains(string(storedRaw), "hidden@example.test")

	flags, fetchedRaw := fetchDraftMailboxMessage(t, fixture.config, draft.CurrentReceipt)
	assertions.Contains(flags, emersionimap.FlagDraft)
	assertions.Equal(storedRaw, fetchedRaw)
}
