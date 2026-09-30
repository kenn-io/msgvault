package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestChatDraftThroughDraftCommands(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "slack-account")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	require.NoError(err)
	conversation := strconv.FormatInt(conversationID, 10)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{HomeDir: t.TempDir()}, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	testCtx := configureRemoteDaemonForTest(t, server.URL)
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.SetContext(testCtx)
		root.AddCommand(newDraftComposeCommand(), newDraftGetCommand(), newDraftEditCommand(), newDraftDeleteCommand(), newDraftRecoverCommand())
		silenceUsageInRunE(root)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		if err := root.ExecuteContext(testCtx); err != nil {
			return stdout.String() + stderr.String(), fmt.Errorf("%v: %w", args, err)
		}
		return stdout.String() + stderr.String(), nil
	}

	createdJSON, err := run("draft-compose", "--conversation", conversation, "--body", "hello", "--json")
	require.NoError(err)
	var created chatDraftOutput
	require.NoError(json.Unmarshal([]byte(createdJSON), &created))
	assert.Equal("msgvault", created.Location)
	assert.Equal("C1", created.SourceConversationID)
	listJSON, err := run("draft-get", "--conversation", conversation, "--json")
	require.NoError(err)
	var listed []chatDraftOutput
	require.NoError(json.Unmarshal([]byte(listJSON), &listed))
	created.Status = "ok"
	assert.Equal([]chatDraftOutput{created}, listed)
	text, err := run("draft-get", created.DraftID)
	require.NoError(err)
	assert.Contains(text, "location=msgvault draft="+created.DraftID+" revision=1")

	_, err = run("draft-edit", created.DraftID, "--revision", "1", "--body", "edited")
	require.NoError(err)
	stale, err := run("draft-delete", created.DraftID, "--revision", "1")
	require.Error(err)
	assert.Contains(stale, "revision_mismatch")
	recovered, err := run("draft-recover", created.DraftID, "--revision", "2")
	require.Error(err)
	assert.Contains(recovered, "not_supported")
	_, err = run("draft-delete", created.DraftID, "--revision", "2")
	require.NoError(err)
	listJSON, err = run("draft-get", "--conversation", conversation, "--json")
	require.NoError(err)
	assert.JSONEq("[]", listJSON)
}

func TestChatDraftDelegatedGrantIsSourceScoped(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	granted, err := st.GetOrCreateSource("slack", "granted")
	require.NoError(err)
	other, err := st.GetOrCreateSource("discord", "other")
	require.NoError(err)
	grantedConversation, err := st.EnsureConversationWithType(granted.ID, "C1", "channel", "granted")
	require.NoError(err)
	otherConversation, err := st.EnsureConversationWithType(other.ID, "D1", "channel", "other")
	require.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	grant := &agentgrant.Grant{
		ID: "chat-grant", Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources: []agentgrant.SourceRef{{Type: granted.SourceType, Identifier: granted.Identifier}},
	}
	run := func(args ...string) (string, error) {
		var out strings.Builder
		emit := func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil }
		req := api.CLIRunRequest{Args: args, Grant: grant}
		runner := adapter.runCLIDraftLifecycle
		if args[0] == api.CLIRunDraftComposeCommand {
			runner = adapter.runCLIComposeDraft
		}
		err := runner(t.Context(), req, emit)
		return out.String(), err
	}

	createdJSON, err := run(api.CLIRunDraftComposeCommand, "--conversation", strconv.FormatInt(grantedConversation, 10), "--body", "ok", "--json")
	require.NoError(err)
	var created chatDraftOutput
	require.NoError(json.Unmarshal([]byte(createdJSON), &created))

	owned, err := st.CreateChatDraftContext(t.Context(), otherConversation, 0, "private", func(string, string) error { return nil })
	require.NoError(err)
	for _, args := range [][]string{
		{api.CLIRunDraftComposeCommand, "--conversation", strconv.FormatInt(otherConversation, 10), "--body", "x"},
		{api.CLIRunDraftGetCommand, "--conversation", strconv.FormatInt(otherConversation, 10)},
		{api.CLIRunDraftGetCommand, owned.DraftID},
		{api.CLIRunDraftGetCommand, "chat-draft-missing"},
		{api.CLIRunDraftGetCommand, created.DraftID},
		{api.CLIRunDraftGetCommand, "--conversation", strconv.FormatInt(grantedConversation, 10)},
		{api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "needs draft.edit"},
	} {
		out, err := run(args...)
		require.Error(err, args)
		assert.Equal("not_permitted", err.Error(), args)
		assert.Empty(out, args)
	}
	unchanged, err := st.ListChatDraftsContext(t.Context(), otherConversation, func(string, string) error { return nil })
	require.NoError(err)
	assert.Len(unchanged, 1)
}
