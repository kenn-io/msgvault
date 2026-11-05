package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
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

func TestSendLineReplyThroughLocalDaemonCLI(t *testing.T) {
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

	cmd := &cobra.Command{Use: "send-line-reply"}
	cmd.SetContext(withStoreResolverConfig(t, a.config))
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	require.NoError(runDaemonCLICommandHTTPWithEnv(cmd, []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}, nil, false, false))
	var sent line.ReplyReceipt
	require.NoError(json.Unmarshal(stdout.Bytes(), &sent))
	assert.Equal("sent", sent.Status)
	assert.True(sent.Accepted)
	assert.Empty(stderr.String())
	assert.Equal(1, fake.Count("POST", "/v2/bot/message/push"))
}

func TestLineReplyOwnerAdmissionGuards(t *testing.T) {
	for _, kind := range []string{"disabled", "omitted", "revoked on disk", "missing secret", "invalid text", "grant", "env", "cwd", "revision", "missing revision"} {
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

func TestLineReplyArchiveRecoveryAfterConfigRemoval(t *testing.T) {
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

func TestLineReplyKnownAcceptanceEmitsRecoveryReceipt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, _ := lineReplyAdapterFixture(t)
	_, err := a.store.DB().Exec(`CREATE TRIGGER fail_line_reply_receipt BEFORE UPDATE ON line_reply_attempts WHEN NEW.state='remote_accepted' BEGIN SELECT RAISE(ABORT,'synthetic receipt failure'); END`)
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

func TestLineReplyRevokedSendCanDeleteDraftAndRecover(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	a, draft, fake := lineReplyAdapterFixture(t)
	req := api.CLIRunRequest{Args: []string{"send-line-reply", draft.DraftID, "--revision=1", "--json"}}
	fake.Enqueue("POST", "/v2/bot/message/push", lineapi.Response{Status: http.StatusServiceUnavailable})
	run := func() (string, error) {
		var output strings.Builder
		err := a.RunCLICommand(t.Context(), req, func(event api.CLIRunEvent) error { output.WriteString(event.Data); return nil })
		return output.String(), err
	}
	out, err := run()
	require.NoError(err)
	assert.Contains(out, `"status":"remote_unknown"`)
	attempt, err := a.store.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
	require.NoError(err)
	a.config.Line[0].AllowSend = false
	require.NoError(a.config.Save())
	_, err = a.store.DB().Exec(`UPDATE line_reply_attempts SET next_retry_at=admitted_at`)
	require.NoError(err)
	_, err = run()
	require.EqualError(err, "send_disabled")
	require.NoError(a.store.DeleteChatDraftContext(t.Context(), draft.DraftID, 1))
	_, err = a.store.GetChatDraftContext(t.Context(), draft.DraftID)
	require.ErrorIs(err, store.ErrChatDraftNotFound)
	a.config.Line[0].AllowSend = true
	require.NoError(a.config.Save())
	_, err = a.store.DB().Exec(`UPDATE line_reply_attempts SET next_retry_at=admitted_at`)
	require.NoError(err)
	out, err = run()
	require.NoError(err)
	assert.Contains(out, `"status":"sent"`)
	replayed, err := run()
	require.NoError(err)
	assert.Equal(out, replayed)
	assert.Equal(2, fake.Count("POST", "/v2/bot/message/push"))
	for _, request := range fake.Requests() {
		if request.Path == "/v2/bot/message/push" {
			assert.Equal(attempt.RetryKey, request.Header.Get("X-Line-Retry-Key"))
			assert.JSONEq(`{"to":"U00000000000000000000000000000002","messages":[{"type":"text","text":"Thanks!"}]}`, string(request.Body))
		}
	}
}
