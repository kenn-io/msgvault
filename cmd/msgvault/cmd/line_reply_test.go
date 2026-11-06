package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLineReplyOwnerAdmissionGuards(t *testing.T) {
	for _, kind := range []string{"disabled", "omitted", "revoked on disk", "missing secret", "invalid text", "cwd", "revision", "missing revision"} {
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
				_, err := a.store.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "")
				require.NoError(err)
				req.Args[2] = "--revision=2"
			case "cwd":
				req.Cwd = t.TempDir()
			case "revision":
				req.Args[2] = "--revision=2"
			case "missing revision":
				req.Args = []string{"send-line-reply", draft.DraftID}
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
			switch kind {
			case "disabled", "omitted", "revoked on disk":
				assert.Equal("send_disabled", err.Error())
			case "missing secret":
				assert.Equal("credentials_unavailable", err.Error())
			case "revision":
				assert.Equal("revision_mismatch", err.Error())
			case "missing revision", "cwd":
				assert.Equal("invalid_args", err.Error())
			}
			if kind == "invalid text" {
				assert.Equal("invalid_args", err.Error())
			}
			assert.Len(fake.Requests(), before)
		})
	}
}

func TestLineReplyCacheAfterOutputFailureAndRecovery(t *testing.T) {
	for _, mode := range []string{"output failure", "cancelled caller"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a, draft, _ := lineReplyAdapterFixture(t)
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
		})
	}
}

func TestLineReplyEmitsPendingArchiveReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, _ := lineReplyAdapterFixture(t)
	trigger := `CREATE TRIGGER fail_line_reply_archive BEFORE INSERT ON messages WHEN NEW.is_from_me=1 BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`
	_, err := a.store.DB().Exec(trigger)
	require.NoError(err)
	req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
	var out strings.Builder
	err = a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { out.WriteString(event.Data); return nil })
	require.NoError(err)
	var pending line.ReplyReceipt
	require.NoError(json.Unmarshal([]byte(out.String()), &pending))
	assert.True(pending.Accepted)
	assert.Equal("synthetic-request-1", pending.RequestID)
	assert.Equal("900000000000000001", pending.RemoteMessageID)
	assert.Equal("sent_archive_pending", pending.Status)
}

func TestLineReplyCredentialFailureCode(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusUnauthorized}, lineapi.Response{Status: http.StatusUnauthorized})
	err := a.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1"}}, func(api.CLIRunEvent) error { return nil })
	require.Error(err)
	assert.Equal("credentials_unavailable", err.Error())
}

func TestLineReplyInvalidTextExplainsLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	_, err := a.store.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, strings.Repeat("a", 5001))
	require.NoError(err)
	var events []api.CLIRunEvent
	err = a.RunCLICommand(t.Context(), api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=2"}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	require.Error(err)
	assert.Equal("invalid_args", err.Error())
	assert.Equal([]api.CLIRunEvent{{Type: cliStreamStderr, Data: "line: invalid reply text: exceeds 5000 UTF-16 code units\n"}}, events)
	assert.Zero(fake.Count(http.MethodPost, "/v2/bot/message/push"))
}

func TestLineReplyJSONFlagForwarding(t *testing.T) {
	for _, flag := range []string{"", "--json=false", "--json=true"} {
		t.Run("json flag "+flag, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			a, draft, _ := lineReplyAdapterFixture(t)
			command := newSendLineReplyCommand()
			args := []string{draft.DraftID, "--revision", "1"}
			if flag != "" {
				args = append(args, flag)
			}
			require.NoError(command.ParseFlags(args))
			forwarded, err := daemonCLIArgsFromCobra(command, command.Flags().Args())
			require.NoError(err)
			var out strings.Builder
			err = a.RunCLICommand(t.Context(), api.CLIRunRequest{Args: forwarded}, func(event api.CLIRunEvent) error {
				assert.Equal(cliStreamStdout, event.Type)
				out.WriteString(event.Data)
				return nil
			})
			require.NoError(err)
			if flag == "--json=true" {
				var receipt line.ReplyReceipt
				require.NoError(json.Unmarshal([]byte(out.String()), &receipt))
				assert.Equal(store.LineReplySent, receipt.Status)
			} else {
				assert.Contains(out.String(), "status=sent accepted=true")
			}
		})
	}
}
