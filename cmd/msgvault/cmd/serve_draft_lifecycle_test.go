package cmd

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestDraftLifecycleDelegatedIMAPGetEditDelete(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	var created draftReplyOutput
	err := adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{Args: []string{
		api.CLIRunDraftReplyCommand, strconv.FormatInt(fixture.parentID, 10),
		"--from", testutil.IMAPTestUsername, "--body", "initial body", "--json",
	}}, func(event api.CLIRunEvent) error {
		return json.Unmarshal([]byte(event.Data), &created)
	})
	requirements.NoError(err)

	providerCalls := 0
	clientFactory := adapter.draftClientFactory
	adapter.draftClientFactory = func(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return clientFactory(ctx, source)
	}
	_, senderKey, err := parseDraftSender(testutil.IMAPTestUsername)
	requirements.NoError(err)
	sourceRef := agentgrant.SourceRef{ID: fixture.source.ID, Type: fixture.source.SourceType, Identifier: fixture.source.Identifier}
	grantFor := func(ref agentgrant.SourceRef, permissions ...agentgrant.Permission) *agentgrant.Grant {
		return &agentgrant.Grant{ID: "imap-lifecycle-grant", Permissions: permissions, Sources: []agentgrant.SourceRef{ref}}
	}
	run := func(grant *agentgrant.Grant, args ...string) (string, error) {
		var out strings.Builder
		err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args, Grant: grant}, func(event api.CLIRunEvent) error {
			out.WriteString(event.Data)
			return nil
		})
		return out.String(), err
	}
	revision := strconv.FormatInt(created.Revision, 10)

	otherSource := sourceRef
	otherSource.Identifier = "other@example.test"
	createWithoutSender := grantFor(sourceRef, agentgrant.PermissionDraftCreate)
	otherSender := sourceRef
	otherSender.SenderKeys = []string{"other@example.test"}
	for _, tc := range []struct {
		name  string
		grant *agentgrant.Grant
		args  []string
	}{
		{"edit on another source", grantFor(otherSource, agentgrant.PermissionDraftEdit), []string{api.CLIRunDraftEditCommand, created.DraftID, "--revision", revision, "--body", "delegated"}},
		{"delete without draft.delete", grantFor(sourceRef, agentgrant.PermissionDraftEdit), []string{api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", revision}},
		{"get with draft.create and no sender", createWithoutSender, []string{api.CLIRunDraftGetCommand, created.DraftID, "--json"}},
		{"get with edit and another sender", grantFor(otherSender, agentgrant.PermissionDraftEdit), []string{api.CLIRunDraftGetCommand, created.DraftID, "--json"}},
		{"get with delete and another sender", grantFor(otherSender, agentgrant.PermissionDraftDelete), []string{api.CLIRunDraftGetCommand, created.DraftID, "--json"}},
		{"edit with another sender", grantFor(otherSender, agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit), []string{api.CLIRunDraftEditCommand, created.DraftID, "--revision", "99", "--body", "delegated"}},
		{"delete with another sender", grantFor(otherSender, agentgrant.PermissionDraftDelete), []string{api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "99"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			out, err := run(tc.grant, tc.args...)
			requirements.Error(err)
			assertions.Equal("not_permitted", err.Error())
			assertions.Empty(out)
			assertions.Equal(0, providerCalls)
		})
	}

	senderRef := sourceRef
	senderRef.SenderKeys = []string{senderKey}
	for _, permission := range []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete} {
		got, err := run(grantFor(senderRef, permission), api.CLIRunDraftGetCommand, created.DraftID, "--json")
		requirements.NoError(err)
		var output draftLifecycleOutput
		requirements.NoError(json.Unmarshal([]byte(got), &output))
		assertions.Equal(created.Revision, output.Revision)
		if permission == agentgrant.PermissionDraftDelete {
			assertions.Empty(output.Content)
			assertions.Empty(output.RawMIME)
		} else {
			assertions.Contains(output.Content, "initial body")
		}
	}

	_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), created.DraftID, created.Revision, store.IMAPDraftOperationEdit, []byte("pending candidate"))
	requirements.NoError(err)
	for _, asJSON := range []bool{false, true} {
		args := []string{api.CLIRunDraftGetCommand, created.DraftID}
		if asJSON {
			args = append(args, "--json")
		}
		got, err := run(grantFor(senderRef, agentgrant.PermissionDraftDelete), args...)
		requirements.NoError(err)
		assertions.NotContains(got, "initial body")
		assertions.NotContains(got, "pending candidate")
		assertions.NotContains(got, `"raw_mime"`)
		assertions.Contains(got, "edit")
	}
	_, err = fixture.store.AbortIMAPDraftContext(t.Context(), created.DraftID, created.Revision, "cancelled")
	requirements.NoError(err)

	edited, err := run(grantFor(senderRef, agentgrant.PermissionDraftEdit), api.CLIRunDraftEditCommand, created.DraftID, "--revision", revision, "--body", "delegated body", "--json")
	requirements.NoError(err)
	assertions.Contains(edited, "\"revision\":2")

	deleted, err := run(grantFor(senderRef, agentgrant.PermissionDraftDelete), api.CLIRunDraftDeleteCommand, created.DraftID, "--revision", "2", "--json")
	requirements.NoError(err)
	assertions.Contains(deleted, "\"lifecycle\":\"discarded\"")
	assertions.NotContains(deleted, "delegated body")
	assertions.NotContains(deleted, `"raw_mime"`)
	assertions.Equal(2, providerCalls)
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

func TestManagedDraftSenderKeyAcceptsQuotedLocalPart(t *testing.T) {
	raw := []byte("From: <\"first last\"@example.com>\r\nTo: to@example.com\r\nSubject: x\r\n\r\nbody\r\n")
	key, err := managedDraftSenderKey(raw)
	require.NoError(t, err)
	assert.Equal(t, store.NormalizeIdentifierForCompare("first last@example.com"), key)
}

func TestParseStoredMailboxRejectsUnquotableAddress(t *testing.T) {
	for _, value := range []string{"ali\x00ce@example.com", "first\tlast@example.com", "ali\u0085ce@example.com", "ali\xffce@example.com"} {
		_, _, err := parseStoredMailbox(value)
		require.Error(t, err, value)
	}
}
