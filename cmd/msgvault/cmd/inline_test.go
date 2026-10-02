package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inline"
)

type inlineProbeClient struct {
	account      inline.Account
	chatID       int64
	page         inline.Page
	messageCalls int
	discovered   []inline.Conversation
}

func (c *inlineProbeClient) Me(context.Context) (inline.Account, error) { return c.account, nil }
func (c *inlineProbeClient) Conversation(_ context.Context, id int64) (inline.Conversation, error) {
	if c.chatID != 0 {
		id = c.chatID
	}
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

func TestInlineSetupRejectsWrongOriginOrChat(t *testing.T) {
	client := &inlineProbeClient{account: inline.Account{UserID: 42, Origin: "example.com"}}
	_, err := verifyInlineSelection(context.Background(), client, []int64{123})
	require.ErrorContains(t, err, "production account")
	client.account.Origin = inline.ProductionOrigin
	client.chatID = 456
	_, err = verifyInlineSelection(context.Background(), client, []int64{123})
	require.ErrorContains(t, err, "different chat")
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
		t.Run(map[bool]string{false: "sync", true: "media"}[backfill], func(t *testing.T) {
			name := "sync-inline"
			if backfill {
				name = "backfill-inline-media"
			}
			server, requests := newDaemonCLIRunnerTestServer(t, func(req daemonCLIRunTestRequest) {
				assert.Equal(t, []string{name, "api.inline.chat:user:42"}, req.Args)
			}, `{"type":"complete"}`)
			cmd := newInlineReadCommand(backfill)
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			cmd.SetArgs([]string{"api.inline.chat:user:42"})
			require.NoError(t, cmd.Execute())
			assert.Equal(t, 1, int(requests.Load()))
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
