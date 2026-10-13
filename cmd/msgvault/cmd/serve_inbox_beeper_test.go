package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func beeperInboxFixture(t *testing.T) (*storeAPIAdapter, inboxcontrol.Target) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(source.ID, "!chat:example.invalid", "Synthetic chat")
	require.NoError(t, err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: conversation, ProviderID: "!chat:example.invalid"}
	return &storeAPIAdapter{store: st, config: &config.Config{}, inboxKey: []byte(strings.Repeat("k", 32))}, target
}

func TestBeeperInboxBindingAcceptsExactNativeConversation(t *testing.T) {
	a, target := beeperInboxFixture(t)
	source, binding, err := a.inboxBinding(context.Background(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, target.SourceID, source.ID)
	assert.Equal(t, target.AccountID, binding.AccountID)
}

func TestBeeperInboxBindingRejectsForeignConversation(t *testing.T) {
	for _, field := range []string{"source", "identifier", "account", "conversation", "provider"} {
		t.Run(field, func(t *testing.T) {
			a, target := beeperInboxFixture(t)
			switch field {
			case "source":
				target.SourceID++
			case "identifier":
				target.SourceIdentifier = "foreign-account"
			case "account":
				target.AccountID = "foreign-account"
			case "conversation":
				target.ItemID += 10000
			case "provider":
				target.ProviderID = "!foreign:example.invalid"
			}
			_, err := a.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, nil)
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		})
	}
}

const beeperControlInfo = `{"app":{"version":"4.2.808","name":"Beeper","bundle_id":"example.beeper"},"endpoints":{"spec":"https://example.invalid/spec"}}`
const beeperControlChat = `{"id":"!chat:example.invalid","accountID":"synthetic-account","type":"single","isArchived":false,"unreadCount":0,"isMarkedUnread":false,"lastReadMessageSortKey":"opaque:9","capabilities":{"archive":true,"markAsUnread":true,"readReceipts":true}}`
const beeperControlPage = `{"items":[],"hasMore":false,"oldestCursor":"old","newestCursor":"new"}`
const beeperControlOldMessage = `{"id":"old-A","chatID":"!chat:example.invalid","accountID":"synthetic-account","sortKey":"opaque:A","timestamp":"2026-01-01T00:00:00Z","isSender":false}`

type beeperControlPeer struct {
	mu           sync.Mutex
	chat, page   string
	calls        []string
	onInfo       func()
	factoryCalls int
	client       *beeper.Client
}

func newBeeperControlPeer(t *testing.T, a *storeAPIAdapter) *beeperControlPeer {
	t.Helper()
	p := &beeperControlPeer{chat: beeperControlChat, page: beeperControlPage}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.calls = append(p.calls, r.Method+" "+r.URL.RequestURI())
		chat, page, onInfo := p.chat, p.page, p.onInfo
		p.mu.Unlock()
		assert.Equal(t, http.MethodGet, r.Method, "observation cannot write to the provider")
		assert.Equal(t, "Bearer synthetic-beeper-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/info":
			if onInfo != nil {
				onInfo()
			}
			_, _ = io.WriteString(w, beeperControlInfo)
		case "/v1/chats/!chat:example.invalid":
			_, _ = io.WriteString(w, chat)
		case "/v1/chats/!chat:example.invalid/messages":
			_, _ = io.WriteString(w, page)
		case "/v1/chats/!chat:example.invalid/messages/old-A":
			_, _ = io.WriteString(w, beeperControlOldMessage)
		default:
			assert.Fail(t, "unexpected Beeper request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	p.client = beeper.NewClient(server.URL, func(context.Context) (string, error) { return "synthetic-beeper-token", nil }, 10000)
	a.beeperInboxClientFactory = func(context.Context, *store.Source) (*beeper.Client, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.factoryCalls++
		return p.client, nil
	}
	return p
}

func (p *beeperControlPeer) snapshot() ([]string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...), p.factoryCalls
}

func controlBeeper(t *testing.T, a *storeAPIAdapter, request inboxcontrol.Request) (*inboxcontrol.Result, error) {
	t.Helper()
	return a.ControlInbox(t.Context(), request, inboxcontrol.Principal{ID: "owner", Owner: true}, func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil }, func(context.Context) (func(), error) { return func() {}, nil })
}

func beeperUnrelatedState(t *testing.T, a *storeAPIAdapter) map[string]int64 {
	t.Helper()
	result := map[string]int64{}
	tables := []string{"person_sweep_changes", "embedding_changes", "activity_events", "beeper_drafts", "chat_drafts", "gmail_drafts", "imap_drafts", "inbox_operation_receipts"}
	if !a.store.IsPostgreSQL() {
		tables = append(tables, "cache_related_change_journal")
	}
	for _, table := range tables {
		var count int64
		require.NoError(t, a.store.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count))
		result[table] = count
	}
	return result
}

func TestBeeperInboxControlPersistsMetadataUnderSourceFence(t *testing.T) {
	a, target := beeperInboxFixture(t)
	peer := newBeeperControlPeer(t, a)
	id, err := a.store.UpsertMessage(&store.Message{SourceID: target.SourceID, SourceMessageID: "old-A", ConversationID: target.ItemID, MessageType: "chat"})
	require.NoError(t, err)
	require.NoError(t, a.store.UpsertMessageBody(id, sql.NullString{String: "Synthetic archived body.", Valid: true}, sql.NullString{}))
	_, err = a.store.DB().Exec(a.store.Rebind("UPDATE messages SET is_read = ? WHERE id = ?"), false, id)
	require.NoError(t, err)
	before := beeperUnrelatedState(t, a)
	peer.onInfo = func() {
		require.ErrorIs(t, a.store.RemoveSource(target.SourceID), store.ErrSyncAlreadyActive)
		execution, err := a.store.AcquireSyncExecutionContext(t.Context(), target.SourceID)
		require.ErrorIs(t, err, store.ErrSyncAlreadyActive)
		if execution != nil {
			assert.NoError(t, execution.Release())
		}
	}
	request := inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}
	result, err := controlBeeper(t, a, request)
	require.NoError(t, err)
	require.NotNil(t, result.Before)
	assert.Equal(t, target, result.Before.Target)
	assert.Equal(t, new(true), result.Before.Inbox)
	assert.Nil(t, result.Before.Read)
	assert.Empty(t, result.Before.LastMessageID)
	assert.Contains(t, result.Before.Flags, "beeper:last-read-message-sort-key:opaque:9")
	stored, err := a.store.GetInboxProviderState(t.Context(), target)
	require.NoError(t, err)
	assert.Equal(t, result.Before, stored)
	var body string
	var read bool
	require.NoError(t, a.store.DB().QueryRow(a.store.Rebind("SELECT is_read FROM messages WHERE id = ?"), id).Scan(&read))
	require.NoError(t, a.store.DB().QueryRow(a.store.Rebind("SELECT body_text FROM message_bodies WHERE message_id = ?"), id).Scan(&body))
	assert.False(t, read)
	assert.Equal(t, "Synthetic archived body.", body)
	assert.Equal(t, before, beeperUnrelatedState(t, a))
	calls, factories := peer.snapshot()
	assert.Equal(t, 1, factories)
	assert.Len(t, calls, 3)
	// A second observation must release the first lease and preserve the digest.
	second, err := controlBeeper(t, a, request)
	require.NoError(t, err)
	assert.Equal(t, result.Before.Revision, second.Before.Revision)
	require.NoError(t, a.store.RemoveSource(target.SourceID))
}

func TestBeeperInboxControlIdentityAndLifetimeNegatives(t *testing.T) {
	for _, scenario := range []string{"removed", "reused ID", "removed during factory", "foreign source", "foreign account", "foreign conversation", "foreign provider"} {
		t.Run(scenario, func(t *testing.T) {
			a, target := beeperInboxFixture(t)
			peer := newBeeperControlPeer(t, a)
			switch scenario {
			case "removed", "reused ID":
				require.NoError(t, a.store.RemoveSource(target.SourceID))
				if scenario == "reused ID" {
					insert := "INSERT INTO sources (id,source_type,identifier) VALUES (?, ?, ?)"
					if a.store.IsPostgreSQL() {
						// Deliberately reuse a retired ID without changing the allocator.
						insert = "INSERT INTO sources (id,source_type,identifier) OVERRIDING SYSTEM VALUE VALUES (?, ?, ?)"
					}
					_, err := a.store.DB().Exec(a.store.Rebind(insert), target.SourceID, "beeper", "replacement-account")
					require.NoError(t, err)
					_, err = a.store.EnsureConversation(target.SourceID, target.ProviderID, "Replacement")
					require.NoError(t, err)
				}
			case "removed during factory":
				a.beeperInboxClientFactory = func(context.Context, *store.Source) (*beeper.Client, error) {
					require.NoError(t, a.store.RemoveSource(target.SourceID))
					return peer.client, nil
				}
			case "foreign source":
				target.SourceID++
			case "foreign account":
				target.AccountID = "foreign-account"
			case "foreign conversation":
				target.ItemID += 10000
			case "foreign provider":
				target.ProviderID = "!foreign:example.invalid"
			}
			_, err := controlBeeper(t, a, inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
			want := inboxcontrol.ErrDenied
			if scenario == "removed during factory" {
				// The existing execution lease cannot recover an absent source.
				want = inboxcontrol.ErrUnavailable
			}
			require.ErrorIs(t, err, want)
			calls, factories := peer.snapshot()
			assert.Empty(t, calls)
			assert.Zero(t, factories)
		})
	}
}

func TestBeeperInboxControlRejectsMergedAndMemberAliases(t *testing.T) {
	for _, change := range []string{
		`,"merge":{"chatIDs":["!member:example.invalid"],"defaultChatID":"!chat:example.invalid"}`,
		`,"mergedIntoChatID":"!parent:example.invalid"`,
	} {
		a, target := beeperInboxFixture(t)
		peer := newBeeperControlPeer(t, a)
		peer.chat = strings.TrimSuffix(beeperControlChat, "}") + change + "}"
		_, err := controlBeeper(t, a, inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target})
		require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		stored, err := a.store.GetInboxProviderState(t.Context(), target)
		require.NoError(t, err)
		assert.Nil(t, stored)
		calls, _ := peer.snapshot()
		assert.Len(t, calls, 2)
		for _, call := range calls {
			assert.NotContains(t, call, "/messages")
		}
	}
}

func TestBeeperInboxControlPrimaryEvidenceNeverAdmitsMutations(t *testing.T) {
	for _, scenario := range []string{"chat markers and latest prose", "singleton terminal repeated with retrieved A", "outgoing preview with retrieved old A", "omitted preview with retrieved old A"} {
		t.Run(scenario, func(t *testing.T) {
			a, target := beeperInboxFixture(t)
			peer := newBeeperControlPeer(t, a)
			switch scenario {
			case "chat markers and latest prose":
				peer.chat = strings.TrimSuffix(beeperControlChat, "}") + `,"lastMessage":{"text":"Latest synthetic prose"}}`
			case "singleton terminal repeated with retrieved A":
				peer.page = `{"items":[` + beeperControlOldMessage + `],"hasMore":false,"oldestCursor":"old","newestCursor":"new"}`
			case "outgoing preview with retrieved old A":
				peer.chat = strings.TrimSuffix(beeperControlChat, "}") + `,"lastMessage":{"id":"outgoing","isSender":true}}`
			}
			if scenario != "chat markers and latest prose" {
				message, err := peer.client.GetMessage(t.Context(), target.ProviderID, "old-A")
				require.NoError(t, err)
				assert.Equal(t, "old-A", message.ID)
			}
			request := inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}
			for range 2 {
				observed, err := controlBeeper(t, a, request)
				require.NoError(t, err)
				assert.Nil(t, observed.Before.Read)
				assert.Empty(t, observed.Before.LastMessageID)
			}
			callsBefore, factoriesBefore := peer.snapshot()
			before := beeperUnrelatedState(t, a)
			for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
				request.Operation, request.DryRun = op, true
				result, err := controlBeeper(t, a, request)
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
				assert.Nil(t, result)
			}
			callsAfter, factoriesAfter := peer.snapshot()
			assert.Equal(t, callsBefore, callsAfter)
			assert.Equal(t, factoriesBefore, factoriesAfter, "mutations are refused before client construction")
			assert.Equal(t, before, beeperUnrelatedState(t, a))
		})
	}
}

func TestBeeperInboxHTTPGeneratedSDKMCPCLIAndCurrentGrants(t *testing.T) {
	// Each scenario has its own real daemon and request budget. Rate limiting
	// remains enabled; runner throughput is not a correctness requirement.
	for _, scenario := range []string{"get-state", "get-capabilities", "archive/preview", "archive/apply", "unarchive/preview", "unarchive/apply", "set-read/preview", "set-read/apply", "set-unread/preview", "set-unread/apply", "grants", "source-lifetime", "discovery"} {
		t.Run(scenario, func(t *testing.T) { testBeeperInboxWireScenario(t, scenario) })
	}
}

func testBeeperInboxWireScenario(t *testing.T, scenario string) {
	t.Helper()
	a, target := beeperInboxFixture(t)
	peer := newBeeperControlPeer(t, a)
	const ownerKey = "synthetic-beeper-owner-key"
	a.config = &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: ownerKey, AgentAccess: true}}
	key, err := prepareInboxRuntime(t.Context(), a.config, a.store)
	require.NoError(t, err)
	a.inboxKey = key
	server := api.NewServerWithOptions(api.ServerOptions{Config: a.config, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
	call := func(method, path string, payload any, secret string) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if secret == "" {
			r.Header.Set("Authorization", "Bearer "+ownerKey)
		} else {
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		}
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, r)
		return response
	}
	issue := func(permissions []string, sources []int64) (string, string) {
		t.Helper()
		response := call(http.MethodPost, "/api/v1/agent-tokens", map[string]any{"label": "synthetic-beeper", "permissions": permissions, "source_ids": sources}, "")
		require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		var grant struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &grant))
		return grant.ID, grant.Secret
	}
	grantID, secret := issue([]string{"inbox.read", "inbox.archive", "inbox.read-state"}, []int64{target.SourceID})
	network := httptest.NewServer(server.Router())
	t.Cleanup(network.Close)
	daemon, err := daemonclient.New(daemonclient.Config{URL: network.URL, AgentToken: secret, AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = daemon.Close() })
	source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
	stateRequest := inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &target}
	capabilityRequest := inboxcontrol.Request{Operation: inboxcontrol.OpGetCapabilities, Source: &source}
	if scenario == "discovery" {
		before, factories := peer.snapshot()
		response := call(http.MethodGet, "/api/v1/mcp/capabilities", nil, secret)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var descriptor apiprotocol.MCPCapabilities
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &descriptor))
		require.True(t, descriptor.HasInboxContract(), "Beeper grants must discover the registered control contract")
		assert.Contains(t, descriptor.InboxOperations, "get-state")
		assert.Contains(t, descriptor.InboxOperations, "get-capabilities")
		assert.Contains(t, descriptor.InboxOperations, "archive", "transport admission still requires native capability checks per call")
		assert.NotContains(t, descriptor.InboxOperations, "tags")
		_, readOnlySecret := issue([]string{"inbox.read"}, []int64{target.SourceID})
		response = call(http.MethodGet, "/api/v1/mcp/capabilities", nil, readOnlySecret)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &descriptor))
		require.True(t, descriptor.HasInboxContract())
		assert.Contains(t, descriptor.InboxOperations, "get-state")
		assert.NotContains(t, descriptor.InboxOperations, "archive")
		assert.NotContains(t, descriptor.InboxOperations, "set-read")
		after, factoriesAfter := peer.snapshot()
		assert.Equal(t, before, after, "discovery must not observe the provider")
		assert.Equal(t, factories, factoriesAfter)
		return
	}
	if scenario == "source-lifetime" {
		_, err := daemon.ControlInbox(t.Context(), stateRequest)
		require.NoError(t, err)
		retired := target
		require.NoError(t, a.store.RemoveSource(retired.SourceID))
		recreated, err := a.store.GetOrCreateSource("beeper", retired.SourceIdentifier)
		require.NoError(t, err)
		require.NotEqual(t, retired.SourceID, recreated.ID, "ordinary reimport must allocate a new lifetime")
		conversation, err := a.store.EnsureConversation(recreated.ID, retired.ProviderID, "Synthetic reimported chat")
		require.NoError(t, err)
		target.SourceID = recreated.ID
		target.ItemID = conversation
		before, factories := peer.snapshot()
		for _, deniedTarget := range []inboxcontrol.Target{retired, target} {
			_, err := daemon.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpGetState, Target: &deniedTarget})
			require.ErrorIs(t, err, inboxcontrol.ErrDenied)
		}
		after, factoriesAfter := peer.snapshot()
		assert.Equal(t, before, after)
		assert.Equal(t, factories, factoriesAfter, "old grant denial must precede provider construction")
		response := call(http.MethodPost, "/api/v1/inbox/control", stateRequest, "")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		_, freshSecret := issue([]string{"inbox.read", "inbox.archive"}, []int64{recreated.ID})
		fresh, err := daemonclient.New(daemonclient.Config{URL: network.URL, AgentToken: freshSecret, AllowInsecure: true})
		require.NoError(t, err)
		t.Cleanup(func() { _ = fresh.Close() })
		result, err := fresh.ControlInbox(t.Context(), stateRequest)
		require.NoError(t, err)
		require.NotNil(t, result.Before)
		assert.Equal(t, target, result.Before.Target)
		assert.Nil(t, result.Before.Read)
		assert.Empty(t, result.Before.LastMessageID)
		before, factories = peer.snapshot()
		_, err = fresh.ControlInbox(t.Context(), inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true})
		require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
		after, factoriesAfter = peer.snapshot()
		assert.Equal(t, before, after)
		assert.Equal(t, factories, factoriesAfter)
		return
	}
	assertResult := func(t *testing.T, request inboxcontrol.Request, result *inboxcontrol.Result) {
		t.Helper()
		require.NotNil(t, result)
		if request.Operation == inboxcontrol.OpGetState {
			require.NotNil(t, result.Before)
			assert.Equal(t, target, result.Before.Target)
			assert.Nil(t, result.Before.Read)
			assert.Empty(t, result.Before.LastMessageID)
			assert.Contains(t, result.Before.Flags, "beeper:last-read-message-sort-key:opaque:9")
		} else {
			require.NotNil(t, result.Capabilities)
			assert.Equal(t, source, result.Capabilities.Source)
			assert.False(t, result.Capabilities.ConditionalWrite)
			for _, op := range result.Capabilities.Operations {
				if op.Operation.IsMutation() {
					assert.NotEqual(t, inboxcontrol.CapabilitySupported, op.Status)
				}
			}
		}
		assert.Empty(t, result.PreviewToken)
		assert.Nil(t, result.Receipt)
	}
	for _, request := range []inboxcontrol.Request{stateRequest, capabilityRequest} {
		if scenario != string(request.Operation) {
			continue
		}
		t.Run(string(request.Operation), func(t *testing.T) {
			response := call(http.MethodPost, "/api/v1/inbox/control", request, secret)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var wire inboxcontrol.Result
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &wire))
			assertResult(t, request, &wire)
			// ControlInbox uses the actual generated Go request/response transport.
			result, err := daemon.ControlInbox(t.Context(), request)
			require.NoError(t, err)
			assertResult(t, request, result)
			session := nativeInboxMCPSession(t, daemon)
			input, err := json.Marshal(request)
			require.NoError(t, err)
			var args map[string]any
			require.NoError(t, json.Unmarshal(input, &args))
			delete(args, "operation")
			tool, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_" + strings.ReplaceAll(string(request.Operation), "-", "_"), Arguments: args})
			require.NoError(t, err)
			require.False(t, tool.IsError)
			content, err := json.Marshal(tool.StructuredContent)
			require.NoError(t, err)
			var envelope struct {
				Result *inboxcontrol.Result `json:"result"`
			}
			require.NoError(t, json.Unmarshal(content, &envelope))
			assertResult(t, request, envelope.Result)
			tokenFile := filepath.Join(t.TempDir(), "agent.token")
			require.NoError(t, os.WriteFile(tokenFile, []byte(secret), 0o600))
			ctx := testInvocationContext(t.Context(), a.config, invocationOptions{agentURL: network.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			command := newInboxCmd()
			command.SetContext(ctx)
			command.SetArgs([]string{string(request.Operation), "--request", "-"})
			command.SetIn(bytes.NewReader(input))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			require.NoError(t, command.Execute())
			var cli inboxcontrol.Result
			require.NoError(t, json.Unmarshal(output.Bytes(), &cli))
			assertResult(t, request, &cli)
		})
	}
	if scenario == "get-state" || scenario == "get-capabilities" {
		return
	}
	if scenario != "grants" {
		session := nativeInboxMCPSession(t, daemon)
		_, err = daemon.Health(t.Context())
		require.NoError(t, err)
		observed, err := daemon.ControlInbox(t.Context(), stateRequest)
		require.NoError(t, err)
		tokenFile := filepath.Join(t.TempDir(), "agent.token")
		require.NoError(t, os.WriteFile(tokenFile, []byte(secret), 0o600))
		for _, op := range []inboxcontrol.Operation{inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread} {
			for _, dryRun := range []bool{true, false} {
				suffix := "/apply"
				if dryRun {
					suffix = "/preview"
				}
				if scenario != string(op)+suffix {
					continue
				}
				request := inboxcontrol.Request{Operation: op, Target: &target, DryRun: dryRun}
				if !dryRun {
					// Client-supplied execution data cannot establish provider admission.
					request.Expected = observed.Before
					request.PreviewToken = "synthetic-nonadmitted-preview"
					request.IdempotencyKey = "refused-" + string(op)
				}
				before, factories := peer.snapshot()
				response := call(http.MethodPost, "/api/v1/inbox/control", request, secret)
				assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
				_, err := daemon.ControlInbox(t.Context(), request)
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
				input, err := json.Marshal(request)
				require.NoError(t, err)
				if dryRun {
					var args map[string]any
					require.NoError(t, json.Unmarshal(input, &args))
					delete(args, "operation")
					tool, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_" + strings.ReplaceAll(string(op), "-", "_"), Arguments: args})
					require.NoError(t, err)
					assert.True(t, tool.IsError)
				}
				ctx := testInvocationContext(t.Context(), a.config, invocationOptions{agentURL: network.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
				command := newInboxCmd()
				command.SetContext(ctx)
				args := []string{string(op), "--request", "-"}
				if !dryRun {
					args = append(args, "--apply")
				}
				command.SetArgs(args)
				command.SetIn(bytes.NewReader(input))
				command.SetOut(io.Discard)
				command.SetErr(io.Discard)
				require.ErrorIs(t, command.Execute(), inboxcontrol.ErrUnavailable)
				after, factoriesAfter := peer.snapshot()
				assert.Equal(t, before, after)
				assert.Equal(t, factories, factoriesAfter)
			}
		}

		return
	}
	// Current permissions, source scope and revocation are checked before native access.
	foreign, err := a.store.GetOrCreateSource("beeper", "foreign-account")
	require.NoError(t, err)
	_, foreignSecret := issue([]string{"inbox.read"}, []int64{foreign.ID})
	_, readOnlySecret := issue([]string{"inbox.read"}, []int64{target.SourceID})
	before, factories := peer.snapshot()
	response := call(http.MethodPost, "/api/v1/inbox/control", stateRequest, foreignSecret)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	response = call(http.MethodPost, "/api/v1/inbox/control", inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}, readOnlySecret)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	revoked := call(http.MethodDelete, "/api/v1/agent-tokens/"+grantID, nil, "")
	require.Less(t, revoked.Code, 300, revoked.Body.String())
	response = call(http.MethodPost, "/api/v1/inbox/control", stateRequest, secret)
	assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, response.Code, response.Body.String())
	after, factoriesAfter := peer.snapshot()
	assert.Equal(t, before, after)
	assert.Equal(t, factories, factoriesAfter)
	require.NoError(t, a.store.RemoveSource(target.SourceID))
	response = call(http.MethodPost, "/api/v1/inbox/control", stateRequest, readOnlySecret)
	assert.Equal(t, http.StatusForbidden, response.Code, response.Body.String())
	after, _ = peer.snapshot()
	assert.Equal(t, before, after)
}
