package cmd

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inline"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

func TestRunConfiguredInlineSyncRotatesAccountsAfterYield(t *testing.T) {
	for _, cooperative := range []bool{false, true} {
		name := "cancelled history request"
		if cooperative {
			name = "cooperative preemption"
		}
		t.Run(name, func(t *testing.T) {
			checks := assert.New(t)
			requires := require.New(t)
			previousRotation := scheduledInlineRotation
			scheduledInlineRotation = &inlineAccountRotation{}
			t.Cleanup(func() { scheduledInlineRotation = previousRotation })
			st := testutil.NewTestStore(t)
			cfg := config.NewDefaultConfig()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Analytics.AutoBuildCache = false
			fixture := &scheduledInlineFixture{cooperative: cooperative}
			var sources []*store.Source
			for _, userID := range []int64{1, 2, 3} {
				account := fixture.account(t, cfg, userID)
				cfg.Inline.Accounts = append(cfg.Inline.Accounts, account)
				source, err := st.GetOrCreateSource(sourceTypeInline, account.Identifier)
				requires.NoError(err)
				sources = append(sources, source)
			}
			// Exercise the scheduler entrypoint, OAuth connection, MCP parser and
			// importer checkpoints. Each pass yields during its first account;
			// starting at config account one on every pass would starve two and three.
			for range sources {
				ctx, cancel := context.WithCancelCause(t.Context())
				ctx, requestPreemption := jobctx.WithPreemption(ctx)
				fixture.mu.Lock()
				fixture.stop = func() { cancel(jobctx.ErrYieldedToWaiter) }
				if cooperative {
					fixture.stop = requestPreemption
				}
				fixture.mu.Unlock()
				err := runConfiguredInlineSync(testInvocationContext(ctx, cfg, invocationOptions{}), st)
				if cooperative {
					requires.NoError(err)
				} else {
					requires.ErrorIs(err, context.Canceled)
				}
				cancel(nil)
			}
			fixture.mu.Lock()
			attempted := append([]int64(nil), fixture.attempted...)
			fixture.stop = nil
			fixture.mu.Unlock()
			checks.Equal([]int64{1, 2, 3}, attempted)
			for _, source := range sources {
				count, err := st.CountMessagesForSource(source.ID)
				requires.NoError(err)
				checks.Equal(int64(1), count, "every configured account gets a scheduled pass")
				if !cooperative {
					run, err := st.GetLatestCheckpointedSync(source.ID)
					requires.NoError(err)
					checks.Equal(store.SyncStatusCancelled, run.Status, "scheduler handoff must not report a provider failure")
					checks.Empty(run.ErrorMessage.String)
					checks.Equal(int64(0), run.ErrorsCount)
					checks.Equal(int64(1), run.MessagesProcessed)
					checks.Equal(int64(1), run.MessagesAdded)
					state, err := inline.LoadSyncState(run.CursorBefore.String, source.Identifier)
					requires.NoError(err)
					requires.NotNil(state.Chats["chat:7"])
					checks.Equal(int64(9), state.Chats["chat:7"].HistoryBefore)
				}
			}
			// Account rotation must not discard the interrupted account's older
			// history. The next unpreempted production pass pays all retained debt.
			requires.NoError(runConfiguredInlineSync(testInvocationContext(t.Context(), cfg, invocationOptions{}), st))
			wantCount := int64(1)
			if !cooperative {
				wantCount = 2
			}
			for _, source := range sources {
				count, err := st.CountMessagesForSource(source.ID)
				requires.NoError(err)
				checks.Equal(wantCount, count)
				_, err = st.GetLastSuccessfulSync(source.ID)
				requires.NoError(err)
			}
		})
	}
}

type scheduledInlineFixture struct {
	mu          sync.Mutex
	cooperative bool
	stop        func()
	attempted   []int64
}

func (f *scheduledInlineFixture) account(t *testing.T, cfg *config.Config, userID int64) config.InlineAccount {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "scheduled-inline-fixture", Version: "1.0.0"}, nil)
	for _, name := range []string{"account.me", "conversations.get", "messages.list"} {
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var body string
			switch req.Params.Name {
			case "account.me":
				body = fmt.Sprintf(`{"user":{"id":"%d"},"session":{"scopes":["messages:read","offline_access"]},"allowed":{"spaceIds":[],"allowDms":true,"allowHomeThreads":true}}`, userID)
			case "conversations.get":
				f.mu.Lock()
				f.attempted = append(f.attempted, userID)
				f.mu.Unlock()
				body = `{"chat":{"chatId":"7","title":"Example chat","kind":"space_chat"},"details":{},"participants":[]}`
			case "messages.list":
				var args struct {
					OffsetID string `json:"offsetId"`
				}
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
				f.mu.Lock()
				stop := f.stop
				f.mu.Unlock()
				id, next := "9", `"9"`
				if f.cooperative {
					next = "null"
					if stop != nil {
						stop()
					}
				} else if args.OffsetID != "" {
					if stop != nil {
						stop()
						return nil, context.Canceled
					}
					id, next = "8", "null"
				}
				body = fmt.Sprintf(`{"chat":{"chatId":"7"},"nextOffsetId":%s,"messages":[{"id":"%s","chatId":"7","fromId":"9","text":"Example message","out":false,"date":"1788220800"}]}`, next, id)
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: body}}}, nil
		})
	}
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true}))
	t.Cleanup(httpServer.Close)
	account := config.InlineAccount{Identifier: fmt.Sprintf("api.inline.chat:user:%d", userID), Endpoint: httpServer.URL + "/mcp/v2", ChatIDs: []string{"7"}}
	credentials := inline.OAuthCredentials{
		Version: 1, Endpoint: account.Endpoint,
		ClientID: "synthetic-scheduled-client", Resource: inline.MCPResource,
		AuthEndpoint: httpServer.URL + "/oauth/authorize", TokenEndpoint: httpServer.URL + "/oauth/token",
		Scopes: []string{"messages:read", "offline_access"},
		Token:  oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour).UTC()},
	}
	payload, err := json.Marshal(credentials)
	require.NoError(t, err)
	manager := inline.NewOAuthManager(account.Endpoint, cfg.TokensDir(), nil)
	require.NoError(t, manager.ImportCredentials(account.Identifier, payload))
	return account
}
