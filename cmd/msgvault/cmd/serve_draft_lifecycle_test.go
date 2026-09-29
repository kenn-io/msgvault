package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftLifecycleEndToEnd(t *testing.T) {
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir()}, Store: adapter, Logger: slog.New(slog.DiscardHandler)}).Router())
	t.Cleanup(server.Close)
	testCtx := configureRemoteDaemonForTest(t, server.URL)
	_ = testCtx
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.SetContext(testCtx)
		root.AddCommand(newDraftReplyCommand(), newDraftGetCommand(), newDraftEditCommand(), newDraftDeleteCommand())
		silenceUsageInRunE(root)
		var stdout, stderr bytes.Buffer
		root.SetOut(&stdout)
		root.SetErr(&stderr)
		root.SetArgs(args)
		err := root.ExecuteContext(testCtx)
		requirements.Empty(stderr.String())
		return stdout.String(), err
	}
	createdJSON, err := run("draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", "initial body", "--json")
	requirements.NoError(err)
	var created struct {
		DraftID  string `json:"draft_id"`
		Revision int64  `json:"revision"`
	}
	requirements.NoError(json.Unmarshal([]byte(createdJSON), &created))
	requirements.NotEmpty(created.DraftID)
	requirements.Equal(int64(1), created.Revision)

	getEvent, err := run(api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
	requirements.Contains(getEvent, "initial body")
	humanGet, err := run(api.CLIRunDraftGetCommand, created.DraftID)
	requirements.NoError(err)
	requirements.Contains(humanGet, "content:\ninitial body")
	requirements.Contains(humanGet, "receipt (revision 1): Drafts")

	editEvent, err := run(api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "edited body", "--json")
	requirements.NoError(err)
	requirements.Contains(editEvent, "\"revision\":2")

	deleteEvent, err := run(api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	requirements.Contains(deleteEvent, "\"lifecycle\":\"discarded\"")
	_, err = run(api.CLIRunDraftGetCommand, created.DraftID, "--json")
	requirements.NoError(err)
}

func TestDraftLifecycleDelegatedIMAPGetEditDeleteRemainOwnerOnly(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	var created draftReplyOutput
	err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: []string{
		api.CLIRunDraftReplyCommand, strconv.FormatInt(fixture.parentID, 10),
		"--from", testutil.IMAPTestUsername, "--body", "initial body", "--json",
	}}, func(event api.CLIRunEvent) error {
		return json.Unmarshal([]byte(event.Data), &created)
	})
	require.NoError(err)

	providerCalls := 0
	clientFactory := adapter.draftClientFactory
	adapter.draftClientFactory = func(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return clientFactory(ctx, source)
	}
	grant := &agentgrant.Grant{
		ID: "imap-lifecycle-grant",
		Permissions: []agentgrant.Permission{
			agentgrant.PermissionDraftCreate,
			agentgrant.PermissionDraftEdit,
			agentgrant.PermissionDraftDelete,
		},
		Sources: []agentgrant.SourceRef{{ID: fixture.source.ID, Type: fixture.source.SourceType, Identifier: fixture.source.Identifier}},
	}
	for _, operation := range []string{
		api.CLIRunDraftGetCommand,
		api.CLIRunDraftEditCommand,
		api.CLIRunDraftDeleteCommand,
	} {
		t.Run(operation, func(t *testing.T) {
			args := []string{operation, created.DraftID}
			if operation != api.CLIRunDraftGetCommand {
				args = append(args, "--revision", strconv.FormatInt(created.Revision, 10))
			}
			if operation == api.CLIRunDraftEditCommand {
				args = append(args, "--body", "delegated")
			}
			err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args, Grant: grant}, func(api.CLIRunEvent) error {
				assert.Fail("delegated IMAP lifecycle must not emit events")
				return nil
			})
			require.Error(err)
			assert.Equal("not_permitted", err.Error())
			assert.Equal(0, providerCalls)
		})
	}
	latest, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	require.NoError(err)
	assert.Equal(created.Revision, latest.Revision)
}

func TestDraftLifecycleReplacementIndexesCc(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	parentRaw, err := fixture.store.GetMessageRaw(fixture.parentID)
	requirements.NoError(err)
	parentRaw = bytes.Replace(parentRaw, []byte("Subject: Question\r\n"), []byte("Cc: copy@example.test\r\nSubject: Question\r\n"), 1)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, parentRaw))
	adapter := fixture.grantedAdapter()

	var created draftReplyOutput
	err = adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-reply", strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername,
		"--all", "--body", "initial body", "--json",
	}}, func(event api.CLIRunEvent) error {
		return json.Unmarshal([]byte(event.Data), &created)
	})
	requirements.NoError(err)
	requirements.Equal(draftReplyStatusCreated, created.Status)
	requirements.Equal(int64(1), created.Revision)

	var events []api.CLIRunEvent
	err = adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: []string{
		api.CLIRunDraftEditCommand, created.DraftID, "--revision", "1", "--body", "edited body", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)

	draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
	requirements.NoError(err)
	requirements.Equal(int64(2), draft.Revision)
	requirements.NotEqual(created.MessageID, draft.CurrentMessageID)
	// The backends tokenize punctuation in full email queries differently.
	matches, total, err := fixture.store.SearchMessages("copy", 0, 10)
	requirements.NoError(err)
	requirements.Equal(int64(1), total)
	requirements.Len(matches, 1)
	assertions.Equal(draft.CurrentMessageID, matches[0].ID)
}
