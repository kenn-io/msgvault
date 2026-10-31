package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

func lineReplyAdapterFixture(t *testing.T) (*storeAPIAdapter, store.ChatDraft, *lineapi.Server) {
	t.Helper()
	cfg, st, fake := lineWiringFixture(t)
	client, err := configuredLineClient(config.LineSource{ChannelID: "2000000001", SecretEnv: "EXAMPLE_LINE_SECRET"})
	require.NoError(t, err)
	source, err := line.Register(t.Context(), st, client, "2000000001")
	require.NoError(t, err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "Example")
	require.NoError(t, err)
	parent, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line"}, BodyText: sql.NullString{String: "Incoming", Valid: true}})
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), chat, parent, "Thanks!", func(string, string) error { return nil })
	require.NoError(t, err)
	cfg.Line = []config.LineSource{{ChannelID: "2000000001", SecretEnv: "EXAMPLE_LINE_SECRET", Enabled: true, AllowSend: true}}
	require.NoError(t, cfg.Save())
	return &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}, draft, fake
}

func TestSendLineReplyThroughOwnerCLI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	refreshed := 0
	a.draftCacheRefresh = func(ctx context.Context, source string) error {
		refreshed++
		require.NoError(ctx.Err())
		assert.Equal("2000000001", source)
		execution, err := a.store.AcquireSyncExecutionContext(ctx, draft.SourceID)
		require.NoError(err)
		require.NoError(execution.Release())
		return errors.New("synthetic cache failure")
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: a.logger}).Router())
	defer server.Close()
	ctx := configureRemoteDaemonForTest(t, server.URL)
	run := func(args ...string) (string, error) {
		root := &cobra.Command{Use: "msgvault"}
		root.SetContext(ctx)
		root.AddCommand(newSendLineReplyCommand(), newDraftGetCommand(), newDraftEditCommand(), newDraftComposeCommand(), newDraftDeleteCommand())
		silenceUsageInRunE(root)
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return output.String(), err
	}
	parent, err := a.store.LineMessageContext(t.Context(), draft.SourceID, "100")
	require.NoError(err)
	createdJSON, err := run("draft-compose", "--conversation", strconv.FormatInt(draft.ConversationID, 10), "--reply-to", strconv.FormatInt(parent.ID, 10), "--body", "Other reviewed draft", "--json")
	require.NoError(err)
	var created chatDraftOutput
	require.NoError(json.Unmarshal([]byte(createdJSON), &created))
	assert.Equal("line", created.SourceType)
	_, err = run("draft-edit", created.DraftID, "--revision=1", "--body", "Edited")
	require.NoError(err)
	_, err = run("draft-delete", created.DraftID, "--revision=2")
	require.NoError(err)
	out, err := run("draft-get", draft.DraftID, "--json")
	require.NoError(err)
	assert.Contains(out, `"body":"Thanks!"`)
	out, err = run("send-line-reply", draft.DraftID, "--revision", "1", "--json")
	require.NoError(err)
	var sent line.ReplyReceipt
	require.NoError(json.Unmarshal([]byte(out), &sent))
	assert.Equal("sent", sent.Status)
	assert.True(sent.Accepted)
	// Remove policy completely: completed receipt still returns and repairs cache.
	a.config.Line = nil
	require.NoError(a.config.Save())
	out, err = run("send-line-reply", draft.DraftID, "--revision", "1", "--json")
	require.NoError(err)
	var replay line.ReplyReceipt
	require.NoError(json.Unmarshal([]byte(out), &replay))
	assert.Equal(sent, replay)
	assert.Equal(2, refreshed)
	assert.Equal(1, fake.Count("POST", "/v2/bot/message/push"))
}

func TestLineReplyOwnerAdmissionGuards(t *testing.T) {
	for _, kind := range []string{"disabled", "omitted", "revoked on disk", "missing secret", "invalid text", "grant", "env", "cwd", "revision", "missing revision", "duplicate revision", "unknown flag", "extra positional"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a, draft, fake := lineReplyAdapterFixture(t)
			req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
			switch kind {
			case "disabled":
				a.config.Line[0].Enabled = false
			case "omitted":
				a.config.Line[0].AllowSend = false
			case "missing secret":
				t.Setenv("EXAMPLE_LINE_SECRET", "")
			case "invalid text":
				edited, err := a.store.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "")
				require.NoError(err)
				draft = edited
				req.Args[2] = "--revision=2"
			case "grant":
				req.Grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit, agentgrant.PermissionDraftDelete}}
				req.Args[1] = "missing-draft"
			case "env":
				req.Env = map[string]string{"EXAMPLE_LINE_SECRET": "spoofed"}
			case "cwd":
				req.Cwd = t.TempDir()
			case "revision":
				req.Args[2] = "--revision=2"
			case "missing revision":
				req.Args = []string{"send-line-reply", draft.DraftID}
			case "duplicate revision":
				req.Args = append(req.Args, "--revision=1")
			case "unknown flag":
				req.Args = append(req.Args, "--allow-send")
			case "extra positional":
				req.Args = append(req.Args, "other")
			}
			require.NoError(a.config.Save())
			if kind == "revoked on disk" {
				fresh, err := config.Load(a.config.ConfigFilePath(), a.config.HomeDir)
				require.NoError(err)
				fresh.Line[0].AllowSend = false
				require.NoError(fresh.Save())
				assert.True(a.config.Line[0].AllowSend)
			}
			before := len(fake.Requests())
			err := a.RunCLICommand(t.Context(), req, func(api.CLIRunEvent) error { return nil })
			require.Error(err)
			if kind == "grant" {
				assert.Equal("not_permitted", err.Error())
			}
			if kind == "invalid text" {
				assert.Equal("invalid_args", err.Error())
			}
			assert.Len(fake.Requests(), before)
			_, err = a.store.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
			require.ErrorIs(err, sql.ErrNoRows)
			_, err = a.store.UpdateChatDraftContext(t.Context(), draft.DraftID, draft.Revision, "still editable")
			require.NoError(err)
		})
	}
}

func TestLineReplyCacheAfterOutputFailureAndRecovery(t *testing.T) {
	for _, mode := range []string{"output failure", "cancelled caller"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a, draft, fake := lineReplyAdapterFixture(t)
			refreshed := 0
			a.draftCacheRefresh = func(ctx context.Context, _ string) error { refreshed++; require.NoError(ctx.Err()); return nil }
			req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
			outputErr := errors.New("synthetic output failure")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			err := a.RunCLICommand(ctx, req, func(api.CLIRunEvent) error {
				if mode == "cancelled caller" {
					cancel()
					return nil
				}
				return outputErr
			})
			if mode == "output failure" {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Equal(1, refreshed)
			var out strings.Builder
			err = a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil })
			require.NoError(err)
			assert.Contains(out.String(), `"status":"sent"`)
			assert.Equal(2, refreshed)
			assert.Equal(1, fake.Count("POST", "/v2/bot/message/push"))
		})
	}
}

func TestLineReplyKnownAcceptanceEmitsRecoveryReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	_, err := a.store.DB().Exec(`CREATE TRIGGER fail_line_reply_archive BEFORE INSERT ON messages WHEN NEW.is_from_me=1 BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`)
	require.NoError(err)
	req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
	var out strings.Builder
	err = a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil })
	require.NoError(err)
	var pending line.ReplyReceipt
	require.NoError(json.Unmarshal([]byte(out.String()), &pending))
	assert.Equal("sent_archive_pending", pending.Status)
	assert.True(pending.Accepted)
	assert.NotEmpty(pending.RemoteMessageID)
	_, err = a.store.DB().Exec(`DROP TRIGGER fail_line_reply_archive`)
	require.NoError(err)
	a.config.Line = nil
	require.NoError(a.config.Save())
	out.Reset()
	err = a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil })
	require.NoError(err)
	assert.Contains(out.String(), `"status":"sent"`)
	assert.Equal(1, fake.Count("POST", "/v2/bot/message/push"))
}

func TestLineReplyUnpersistedAcceptanceEmitsBoundedRecoveryReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	_, err := a.store.DB().Exec(`CREATE TRIGGER fail_line_reply_receipt BEFORE UPDATE ON line_reply_attempts WHEN NEW.state='remote_accepted' BEGIN SELECT RAISE(ABORT,'synthetic receipt failure'); END`)
	require.NoError(err)
	req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
	var out strings.Builder
	err = a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil })
	require.NoError(err)
	var pending line.ReplyReceipt
	require.NoError(json.Unmarshal([]byte(out.String()), &pending))
	assert.Equal("accepted_receipt_pending", pending.Status)
	assert.True(pending.Accepted)
	assert.Equal("synthetic-request-1", pending.RequestID)
	assert.Equal("900000000000000001", pending.RemoteMessageID)
	durable, err := a.store.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(store.LineReplyRemoteUnknown, durable.State)
	assert.Empty(durable.RemoteMessageID)
	assert.Equal(1, fake.AcceptedPushes())
}
