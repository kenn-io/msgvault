package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
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
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.PersistentFlags().Bool("verbose", false, "")
		root.PersistentFlags().Bool("log-sql", false, "")
		root.SetContext(testCtx)
		root.AddCommand(
			newChatDraftCreateCommand(), newChatDraftGetCommand(),
			newChatDraftEditCommand(), newChatDraftDeleteCommand(),
		)
		silenceUsageInRunE(root)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(testCtx)
		requirements.Empty(stderr.String())
		return stdout.String(), err
	}
	createdJSON, err := run(
		api.CLIRunChatDraftCreateCommand, strconv.FormatInt(conversationID, 10),
		"--source", source.Identifier, "--body", "route body", "--json",
	)
	requirements.NoError(err)
	var created chatDraftOutput
	requirements.NoError(json.Unmarshal([]byte(createdJSON), &created))
	requirements.NotEmpty(created.DraftID)
	requirements.Equal(int64(1), created.Revision)
	textOutput, err := run(api.CLIRunChatDraftGetCommand, created.DraftID,
		"--json=false", "--verbose=false", "--log-sql=false")
	requirements.NoError(err)
	requirements.Contains(textOutput, "location=msgvault")

	release, acquired := gate.BeginLabeledWorkContext(t.Context(), "hold chat draft mutation")
	requirements.True(acquired)
	defer func() {
		if release != nil {
			release()
		}
	}()
	getDone := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		output, getErr := run(api.CLIRunChatDraftGetCommand, created.DraftID, "--json")
		getDone <- struct {
			output string
			err    error
		}{output, getErr}
	}()
	select {
	case result := <-getDone:
		requirements.NoError(result.err)
		assertions.Contains(result.output, "\"location\":\"msgvault\"")
	case <-time.After(time.Second):
		requirements.FailNow("owner chat-draft-get waited on the mutation gate")
	}

	editDone := make(chan error, 1)
	go func() {
		_, editErr := run(
			api.CLIRunChatDraftEditCommand, created.DraftID,
			"--revision", "1", "--body", "edited through route", "--json",
		)
		editDone <- editErr
	}()
	waiterTimer := time.NewTimer(time.Second)
	waiterTicker := time.NewTicker(10 * time.Millisecond)
	waiting := false
	for !waiting {
		select {
		case editErr := <-editDone:
			requirements.FailNow("chat-draft-edit completed while the mutation gate was held", fmt.Sprint(editErr))
		case <-waiterTimer.C:
			requirements.FailNow("chat-draft-edit did not reach the held mutation gate")
		case <-waiterTicker.C:
			waiting = gate.HasRequestWaiters()
		}
	}
	waiterTicker.Stop()
	waiterTimer.Stop()
	assertions.True(waiting)
	release()
	release = nil
	requirements.NoError(<-editDone)
	editedJSON, err := run(api.CLIRunChatDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	var edited chatDraftOutput
	requirements.NoError(json.Unmarshal([]byte(editedJSON), &edited))
	assertions.Equal("edited through route", edited.Body)
	assertions.Equal(int64(2), edited.Revision)
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
