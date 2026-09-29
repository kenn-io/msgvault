package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChatDraftCLILifecycle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "slack-account")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "channel-1", "channel", "General",
	)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	run := func(args ...string) (chatDraftOutput, error) {
		var output chatDraftOutput
		err := adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
			return json.Unmarshal([]byte(event.Data), &output)
		})
		return output, err
	}

	created, err := run(api.CLIRunChatDraftCreateCommand,
		strconv.FormatInt(conversationID, 10), "--source=slack-account", "--body=hello", "--json")
	requirements.NoError(err)
	assertions.Equal("msgvault", created.Location)
	assertions.Equal("hello", created.Body)
	assertions.Equal(int64(1), created.Revision)

	loaded, err := run(api.CLIRunChatDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	assertions.Equal("ok", loaded.Status)
	created.Status = loaded.Status
	assertions.Equal(created, loaded)

	edited, err := run(api.CLIRunChatDraftEditCommand, created.DraftID, "--revision=1", "--body=updated", "--json")
	requirements.NoError(err)
	assertions.Equal("updated", edited.Body)
	assertions.Equal(int64(2), edited.Revision)

	_, err = run(api.CLIRunChatDraftEditCommand, created.DraftID, "--revision=1", "--body=stale", "--json")
	requirements.ErrorIs(err, store.ErrChatDraftRevisionConflict)

	deleted, err := run(api.CLIRunChatDraftDeleteCommand, created.DraftID, "--revision=2", "--json")
	requirements.NoError(err)
	assertions.Equal("deleted", deleted.Status)
	_, err = run(api.CLIRunChatDraftGetCommand, created.DraftID, "--json")
	requirements.ErrorIs(err, store.ErrChatDraftNotFound)
}

func TestChatDraftListByConversation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "list-account")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "channel-a", "channel", "Channel A")
	requirements.NoError(err)
	otherID, err := st.EnsureConversationWithType(source.ID, "channel-b", "channel", "Channel B")
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	list := func() ([]chatDraftOutput, error) {
		var drafts []chatDraftOutput
		err := adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{
			Args: []string{"chat-draft-list", strconv.FormatInt(conversationID, 10), "--json"},
		}, func(event api.CLIRunEvent) error {
			return json.Unmarshal([]byte(event.Data), &drafts)
		})
		return drafts, err
	}
	empty, err := list()
	requirements.NoError(err)
	assertions.NotNil(empty)
	assertions.Empty(empty)
	var wantIDs []string
	for _, id := range []int64{conversationID, otherID, conversationID} {
		draft, err := st.CreateChatDraftContext(t.Context(), store.ChatDraftCreate{
			SourceID: source.ID, SourceType: source.SourceType, SourceIdentifier: source.Identifier,
			ConversationID: id, Body: "saved text",
		})
		requirements.NoError(err)
		if id == conversationID {
			wantIDs = append(wantIDs, draft.DraftID)
		}
	}
	drafts, err := list()
	requirements.NoError(err)
	var gotIDs []string
	for _, draft := range drafts {
		gotIDs = append(gotIDs, draft.DraftID)
		assertions.Equal("saved text", draft.Body)
		assertions.Equal(int64(1), draft.Revision)
	}
	assertions.ElementsMatch(wantIDs, gotIDs)

	err = adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{
		Args:  []string{"chat-draft-list", strconv.FormatInt(conversationID, 10)},
		Grant: &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}},
	}, nil)
	requirements.EqualError(err, "not_permitted")
}

func TestChatDraftCobraDaemonRouteAndGate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "cobra-slack")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "cobra-channel", "channel", "Cobra",
	)
	requirements.NoError(err)
	gate := api.NewSerialOperationGate()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config:        &config.Config{HomeDir: t.TempDir()},
		Store:         &storeAPIAdapter{store: st},
		Logger:        slog.New(slog.DiscardHandler),
		OperationGate: gate,
	}).Router())
	t.Cleanup(server.Close)
	testCtx := configureRemoteDaemonForTest(t, server.URL)
	type cliResult struct {
		stdout, stderr string
		err            error
	}
	run := func(args ...string) cliResult {
		root := &cobra.Command{Use: "msgvault"}
		root.PersistentFlags().Bool("verbose", false, "")
		root.PersistentFlags().Bool("log-sql", false, "")
		root.SetContext(testCtx)
		root.AddCommand(
			newChatDraftCreateCommand(), newChatDraftGetCommand(), newChatDraftListCommand(),
			newChatDraftEditCommand(), newChatDraftDeleteCommand(),
		)
		silenceUsageInRunE(root)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(testCtx)
		return cliResult{stdout.String(), stderr.String(), err}
	}
	createdResult := run(
		api.CLIRunChatDraftCreateCommand, strconv.FormatInt(conversationID, 10),
		"--source", source.Identifier, "--body", "route body", "--json",
	)
	requirements.NoError(createdResult.err)
	assertions.Empty(createdResult.stderr)
	var created chatDraftOutput
	requirements.NoError(json.Unmarshal([]byte(createdResult.stdout), &created))
	requirements.NotEmpty(created.DraftID)
	assertions.Equal(int64(1), created.Revision)
	textResult := run(api.CLIRunChatDraftGetCommand, created.DraftID,
		"--json=false", "--verbose=false", "--log-sql=false")
	requirements.NoError(textResult.err)
	assertions.Empty(textResult.stderr)
	assertions.Contains(textResult.stdout, "location=msgvault")

	release, acquired := gate.BeginLabeledWorkContext(t.Context(), "hold chat draft mutation")
	requirements.True(acquired)
	defer func() {
		if release != nil {
			release()
		}
	}()
	const gateWaitTimeout = 10 * time.Second
	for _, args := range [][]string{
		{api.CLIRunChatDraftGetCommand, created.DraftID, "--json"},
		{api.CLIRunChatDraftListCommand, strconv.FormatInt(conversationID, 10), "--json"},
	} {
		readDone := make(chan cliResult, 1)
		go func() { readDone <- run(args...) }()
		select {
		case result := <-readDone:
			requirements.NoError(result.err, result.stderr)
			assertions.Empty(result.stderr)
			assertions.Contains(result.stdout, created.DraftID)
		case <-time.After(gateWaitTimeout):
			requirements.FailNow(args[0] + " waited on the mutation gate")
		}
	}

	editDone := make(chan cliResult, 1)
	go func() {
		editDone <- run(
			api.CLIRunChatDraftEditCommand, created.DraftID,
			"--revision", "1", "--body", "edited through route", "--json",
		)
	}()
	waiterTimer := time.NewTimer(gateWaitTimeout)
	defer waiterTimer.Stop()
	waiterTicker := time.NewTicker(10 * time.Millisecond)
	defer waiterTicker.Stop()
	for !gate.HasRequestWaiters() {
		select {
		case result := <-editDone:
			requirements.FailNowf("chat-draft-edit completed while the mutation gate was held",
				"error: %v; stderr: %s", result.err, result.stderr)
		case <-waiterTimer.C:
			requirements.FailNow("chat-draft-edit did not reach the held mutation gate")
		case <-waiterTicker.C:
		}
	}
	release()
	release = nil
	editResult := <-editDone
	requirements.NoError(editResult.err)
	assertions.Empty(editResult.stderr)
	editedResult := run(api.CLIRunChatDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(editedResult.err)
	assertions.Empty(editedResult.stderr)
	var edited chatDraftOutput
	requirements.NoError(json.Unmarshal([]byte(editedResult.stdout), &edited))
	assertions.Equal("edited through route", edited.Body)
	assertions.Equal(int64(2), edited.Revision)

	conflict := run(api.CLIRunChatDraftEditCommand, created.DraftID,
		"--revision=1", "--body=stale", "--json")
	requirements.EqualError(conflict.err, "revision_conflict")
	assertions.Empty(conflict.stdout)
	assertions.Equal("Error: revision_conflict\n", conflict.stderr)

	// Run the real entry point to verify its process exit status as well.
	binaryName := "msgvault"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5 sqlite_vec", "-o", binary, "./cmd/msgvault")
	build.Dir = filepath.Join("..", "..", "..")
	buildOutput, err := build.CombinedOutput()
	requirements.NoError(err, "build CLI: %s", buildOutput)
	home := t.TempDir()
	configFile := filepath.Join(home, "config.toml")
	requirements.NoError(os.WriteFile(configFile, []byte(fmt.Sprintf(
		"[remote]\nurl = %q\nallow_insecure = true\n", server.URL,
	)), 0o600))
	child := exec.CommandContext(t.Context(), binary, "--home", home, "--config", configFile, "--log-level=error",
		"chat-draft-edit", created.DraftID, "--revision=1", "--body=stale", "--json")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	var exitErr *exec.ExitError
	requirements.ErrorAs(child.Run(), &exitErr)
	assertions.Equal(1, exitErr.ExitCode())
	assertions.Empty(stdout.String())
	assertions.Equal("Error: revision_conflict\n", stderr.String())
}

func TestChatDraftDelegatedGrantScopesSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	granted, err := st.GetOrCreateSource("slack", "granted-account")
	requirements.NoError(err)
	requested, err := st.GetOrCreateSource("teams", "requested-account")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		requested.ID, "team-chat", "channel", "Team chat",
	)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources: []agentgrant.SourceRef{{
			ID: granted.ID, Type: granted.SourceType, Identifier: granted.Identifier,
		}},
	}
	var events []api.CLIRunEvent
	err = adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{
		Args: []string{
			api.CLIRunChatDraftCreateCommand, strconv.FormatInt(conversationID, 10),
			"--source=" + requested.Identifier, "--body=secret", "--json",
		}, Grant: grant,
	}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.Error(err)
	assertions.Equal("not_permitted", err.Error())
	assertions.NotContains(err.Error(), "secret")
	assertions.Empty(events)
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT COUNT(*) FROM chat_drafts")).Scan(&count))
	assertions.Equal(0, count)
	err = adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{
		Args: []string{api.CLIRunChatDraftGetCommand, "chat-draft-unavailable"}, Grant: grant,
	}, nil)
	requirements.Error(err)
	assertions.Equal("not_permitted", err.Error())
}

func TestChatDraftCreateSelectsExactSourceID(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	teams, err := st.GetOrCreateSource("teams", "shared@example.com")
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("imap", "shared@example.com")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(teams.ID, "chat-1", "direct_chat", "Chat")
	requirements.NoError(err)
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources: []agentgrant.SourceRef{{
			ID: teams.ID, Type: teams.SourceType, Identifier: teams.Identifier,
		}},
	}
	adapter := &storeAPIAdapter{store: st}
	var output chatDraftOutput
	err = adapter.runCLIChatDraft(t.Context(), api.CLIRunRequest{
		Args: []string{
			api.CLIRunChatDraftCreateCommand, strconv.FormatInt(conversationID, 10),
			"--source-id=" + strconv.FormatInt(teams.ID, 10), "--body=hello", "--json",
		}, Grant: grant,
	}, func(event api.CLIRunEvent) error {
		return json.Unmarshal([]byte(event.Data), &output)
	})
	requirements.NoError(err)
	requirements.Equal(teams.ID, output.SourceID)
	requirements.Equal("hello", output.Body)
}
