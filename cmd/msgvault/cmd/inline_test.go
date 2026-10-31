package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inline"
	"go.kenn.io/msgvault/internal/store"
)

func TestAddInlineContinuesWhenDisplayNameConflictsWithSelector(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	// Only the external CLI read contract is synthetic. Registration, config,
	// identity setup, and retry all use their production paths.
	executable := filepath.Join(t.TempDir(), "inline")
	require.NoError(t, os.WriteFile(executable, []byte(`#!/bin/sh
case "$*" in
  "--version") printf '%s' 'inline 0.7.16' ;;
  "doctor --json --compact") printf '%s' '{"config":{"apiBaseUrl":"https://api.inline.chat/v1","realtimeUrl":"wss://api.inline.chat/realtime"}}' ;;
  "auth me --json --compact") printf '%s' '{"id":42,"first_name":"Example","last_name":"User"}' ;;
  "chats list --include-subthreads --json --compact") printf '%s' '{"chats":[],"dialogs":[],"spaces":[],"users":[],"messages":[],"folders":[]}' ;;
  *) exit 9 ;;
esac
`), 0700))
	for _, selectorKind := range []string{"identifier", "alias"} {
		t.Run(selectorKind, func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			requires.NoError(os.WriteFile(cfg.ConfigFilePath(), nil, 0600))
			st, err := store.Open(cfg.DatabaseDSN())
			requires.NoError(err)
			t.Cleanup(func() { assertions.NoError(st.Close()) })
			requires.NoError(st.InitSchema())
			identifier := "InLiNe"
			if selectorKind == "alias" {
				identifier = "reader@example.test"
			}
			other, err := st.GetOrCreateSource(sourceTypeGmail, identifier)
			requires.NoError(err)
			if selectorKind == "alias" {
				alias := "iNlInE"
				_, err = st.UpdateSourceSettingsContext(t.Context(), other.ID, store.SourceSettingsUpdate{Alias: &alias})
				requires.NoError(err)
			}

			for range 2 {
				cmd := newAddInlineCmd()
				cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&output)
				cmd.SetArgs([]string{"--transport", "cli", "--cli-path", executable})
				requires.NoError(cmd.Execute())
				assertions.Contains(output.String(), "Added Inline account api.inline.chat:user:42")
			}
			source, err := st.GetSourceByTypeAndIdentifier(sourceTypeInline, "api.inline.chat:user:42")
			requires.NoError(err)
			assertions.False(source.DisplayName.Valid)
			identities, err := st.ListAccountIdentities(source.ID)
			requires.NoError(err)
			requires.Len(identities, 1, "registration must complete identity setup and retry without duplicates")
			assertions.Equal("api.inline.chat:user:42", identities[0].Address)
		})
	}
}

type inlineProbeClient struct {
	account      inline.Account
	page         inline.Page
	messageCalls int
	discovered   []inline.Conversation
}

func (c *inlineProbeClient) Me(context.Context) (inline.Account, error) { return c.account, nil }
func (c *inlineProbeClient) Conversation(_ context.Context, id int64) (inline.Conversation, error) {
	return inline.Conversation{ID: id}, nil
}
func (c *inlineProbeClient) Messages(context.Context, int64, int64) (inline.Page, error) {
	c.messageCalls++
	return c.page, nil
}
func (*inlineProbeClient) Files(context.Context, int64, []int64) ([]inline.Media, error) {
	return nil, errors.New("probe must not fetch media")
}
func (*inlineProbeClient) Close() error { return nil }
func (c *inlineProbeClient) Discover(context.Context) ([]inline.Conversation, error) {
	return c.discovered, nil
}

func TestInlineProbeChecksPrincipalAndAvoidsPrivateText(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	account := inline.Account{UserID: 42, Origin: inline.ProductionOrigin}
	client := &inlineProbeClient{account: account, page: inline.Page{Messages: []inline.Message{{ID: 7, ChatID: 123, Text: "synthetic private message body"}}}}
	var out bytes.Buffer
	requires.NoError(runInlineProbe(context.Background(), &out, client, inline.ImportOptions{Account: account, ChatIDs: []int64{123}}))
	assertions.Contains(out.String(), "schema accepted, 1 first-page messages")
	assertions.NotContains(out.String(), "synthetic private")
	assertions.Equal(1, client.messageCalls)
	client.account.UserID = 43
	client.messageCalls = 0
	requires.ErrorContains(runInlineProbe(context.Background(), &out, client, inline.ImportOptions{Account: account, ChatIDs: []int64{123}}), "differs from configuration")
	assertions.Zero(client.messageCalls, "a different signed-in account must not reach message reads")
}

func TestInlineChatSelectionValidatesCanonicalIDs(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "+1", "01", "1.0", " 1", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) { _, err := parseInlineChatIDs([]string{value}); require.Error(t, err) })
	}
	ids, err := parseInlineChatIDs([]string{"123", "456", "123"})
	require.NoError(t, err)
	assert.Equal(t, []int64{123, 456}, ids)
	_, err = parseInlineChatIDs(nil)
	require.NoError(t, err, "an omitted filter means all chats")
}

func TestInlineCLISetupUsesDaemonExecutable(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
		assert.Equal(t, []string{"add-inline", "--chat-id=123", "--cli-path=/opt/example/inline", "--transport=cli"}, req.Args)
	}, `{"type":"stdout","data":"Inline configured\n"}`, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	cmd := newAddInlineCmd()
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--transport", "cli", "--cli-path", "/opt/example/inline", "--chat-id", "123"})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, 1, int(requests.Load()))
	assert.Equal(t, "Inline configured\n", out.String())
}

func TestInlineSyncCommandsRouteToDaemon(t *testing.T) {
	for _, backfill := range []bool{false, true} {
		name := "sync-inline"
		if backfill {
			name = "backfill-inline-media"
		}
		for _, flag := range []string{"", "--build-cache", "--no-build-cache", "--build-cache=false", "--no-build-cache=false"} {
			t.Run(name+flag, func(t *testing.T) {
				want := []string{name}
				args := []string{"api.inline.chat:user:42"}
				if flag != "" {
					want = append(want, flag)
					args = append(args, flag)
				}
				want = append(want, args[0])
				server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
					assert.Equal(t, want, req.Args)
				}, `{"type":"complete"}`)
				cmd := newInlineReadCommand(backfill)
				cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
				cmd.SetArgs(args)
				require.NoError(t, cmd.Execute())
				assert.Equal(t, 1, int(requests.Load()))
			})
		}
		t.Run(name+" conflicting cache flags", func(t *testing.T) {
			server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
			cmd := newInlineReadCommand(backfill)
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			cmd.SetArgs([]string{"--build-cache", "--no-build-cache"})
			require.ErrorContains(t, cmd.Execute(), "[build-cache no-build-cache]")
			assert.Zero(t, requests.Load())
		})
	}
}

func TestInlineOptionsResolveSelectedChatsAndPrincipal(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	cfg := config.NewDefaultConfig()
	account := config.InlineAccount{Identifier: "api.inline.chat:user:42", ChatIDs: []string{"123"}}
	opts, err := inlineImportOptions(cfg, account)
	requires.NoError(err)
	assertions.Equal(int64(42), opts.Account.UserID)
	assertions.Equal([]int64{123}, opts.ChatIDs)
	account.Identifier = "api.inline.chat:user:042"
	_, err = inlineImportOptions(cfg, account)
	requires.Error(err)
	assertions.True(attachmentProducingCommand([]string{"sync-inline"}))
	assertions.True(attachmentProducingCommand([]string{"backfill-inline-media"}))
	assertions.False(attachmentProducingCommand([]string{"add-inline"}))
}
