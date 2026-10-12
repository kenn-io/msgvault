package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	archivesync "go.kenn.io/msgvault/internal/sync"
	"golang.org/x/oauth2"
)

// Every provider interaction crosses the real Gmail HTTP client. Full sync
// receives and parses MIME; the production daemon resolver owns control.
func TestDaemonInboxNativeReceiveAuthorizedReceiptRestart(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	const account = "inbox-owner@example.test"
	const ownerKey = "synthetic-inbox-owner-key"
	var mu sync.Mutex
	labels := []string{"INBOX", "UNREAD"}
	posts, rawGets, readbacks := 0, 0, 0
	raw := []byte("From: Sender <sender@example.test>\r\nTo: inbox-owner@example.test\r\nSubject: Native Inbox fixture\r\nMessage-ID: <native-inbox@example.test>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSynthetic native body.\r\n")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		assertions.Equal("Bearer synthetic-native-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/gmail/v1/users/me/profile":
			_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": account, "historyId": "42", "messagesTotal": 1})
		case "/gmail/v1/users/me/labels":
			_, _ = w.Write([]byte(`{"labels":[{"id":"INBOX","name":"INBOX","type":"system"},{"id":"UNREAD","name":"UNREAD","type":"system"},{"id":"STARRED","name":"STARRED","type":"system"}]}`))
		case "/gmail/v1/users/me/messages":
			_, _ = w.Write([]byte(`{"messages":[{"id":"native-message","threadId":"native-thread"}],"resultSizeEstimate":1}`))
		case "/gmail/v1/users/me/history":
			_, _ = w.Write([]byte(`{"historyId":"42","history":[]}`))
		case "/gmail/v1/users/me/messages/native-message":
			response := map[string]any{"id": "native-message", "threadId": "native-thread", "labelIds": labels, "historyId": "42"}
			if r.URL.Query().Get("format") == "raw" {
				rawGets++
				response["raw"] = base64.RawURLEncoding.EncodeToString(raw)
			} else if posts > 0 {
				readbacks++
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/gmail/v1/users/me/messages/native-message/modify":
			posts++
			assertions.Equal(http.MethodPost, r.Method)
			var delta struct {
				Add    []string `json:"addLabelIds"`
				Remove []string `json:"removeLabelIds"`
			}
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&delta)) {
				return
			}
			assertions.Empty(delta.Add)
			assertions.Equal([]string{"INBOX"}, delta.Remove)
			labels = slices.DeleteFunc(labels, func(id string) bool { return id == "INBOX" })
			// A mutation response alone cannot prove success.
			_, _ = w.Write([]byte(`{"id":"native-message"}`))
		default:
			assertions.Fail("unexpected native Gmail request", r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	endpoint, err := url.Parse(provider.URL)
	requirements.NoError(err)
	var factoryCalls atomic.Int64
	factory := func(context.Context, *store.Source) (gmail.API, error) {
		factoryCalls.Add(1)
		transport := tagGmailTransport(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			u := *r.URL
			clone.URL = &u
			clone.URL.Scheme, clone.URL.Host = endpoint.Scheme, endpoint.Host
			return http.DefaultTransport.RoundTrip(clone)
		})
		return gmail.NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "synthetic-native-token"}), gmail.WithTransport(transport), gmail.WithRateLimiter(gmail.NewRateLimiter(1000))), nil
	}
	dbPath := filepath.Join(t.TempDir(), "native.db")
	st, err := store.OpenForTest(dbPath)
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	client, err := factory(t.Context(), nil)
	requirements.NoError(err)
	_, err = archivesync.New(client, st, nil).Full(t.Context(), account)
	requirements.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("gmail", account)
	requirements.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"native-message"})
	requirements.NoError(err)
	requirements.Contains(ids, "native-message")
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: account, AccountID: account, Scope: inboxcontrol.ScopeMessage, ItemID: ids["native-message"], ProviderID: "native-message"}
	var body string
	requirements.NoError(st.DB().QueryRow("SELECT body_text FROM message_bodies WHERE message_id = ?", target.ItemID).Scan(&body))
	assertions.Contains(body, "Synthetic native body.")
	observed, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	assertions.Equal("42", observed.Revision)

	cfg := &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: ownerKey, AgentAccess: true}, OAuth: config.OAuthConfig{ServiceAccountKey: "synthetic-service-account"}}
	key, err := prepareInboxRuntime(t.Context(), cfg, st)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg, inboxKey: key, emailTagClientFactory: factory}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.Default(), OperationGate: api.NewSerialOperationGate()})
	call := func(method, path string, payload any, secret string) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(payload)
		requirements.NoError(err)
		r := httptest.NewRequest(method, path, bytes.NewReader(encoded))
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
	issue := func(expires *time.Time) (string, string) {
		t.Helper()
		payload := map[string]any{"label": "native-inbox", "permissions": []string{"inbox.read", "inbox.archive"}, "source_ids": []int64{source.ID}}
		if expires != nil {
			payload["expires_at"] = expires
		}
		response := call(http.MethodPost, "/api/v1/agent-tokens", payload, "")
		requirements.Equal(http.StatusCreated, response.Code, response.Body.String())
		var grant struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &grant))
		return grant.ID, grant.Secret
	}
	_, secret := issue(nil)
	networkDaemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { server.Router().ServeHTTP(w, r) }))
	t.Cleanup(networkDaemon.Close)
	daemon, err := daemonclient.New(daemonclient.Config{URL: networkDaemon.URL, AgentToken: secret, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = daemon.Close() })
	nativeInboxMCPSession(t, daemon)

	request := inboxcontrol.Request{Operation: inboxcontrol.OpArchive, Target: &target, DryRun: true}
	previewResponse := call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusOK, previewResponse.Code, previewResponse.Body.String())
	var preview inboxcontrol.Result
	requirements.NoError(json.Unmarshal(previewResponse.Body.Bytes(), &preview))
	requirements.NotNil(preview.Before)
	// A competing successful removal after preview wins before claim/dispatch.
	requirements.NoError(st.RemoveSource(source.ID))
	removedRequest := request
	removedRequest.DryRun, removedRequest.Expected, removedRequest.PreviewToken, removedRequest.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-removed-before-claim"
	removed := call(http.MethodPost, "/api/v1/inbox/control", removedRequest, secret)
	requirements.Equal(http.StatusForbidden, removed.Code, removed.Body.String())
	mu.Lock()
	assertions.Equal(0, posts)
	mu.Unlock()
	_, err = archivesync.New(client, st, nil).Full(t.Context(), account)
	requirements.NoError(err)
	oldSourceID, oldSecret := source.ID, secret
	source, err = st.GetSourceByTypeAndIdentifier("gmail", account)
	requirements.NoError(err)
	assertions.Greater(source.ID, oldSourceID, "ordinary recreation retains a new source lifetime")
	restored, err := st.MessageExistsBatch(source.ID, []string{"native-message"})
	requirements.NoError(err)
	target.SourceID, target.ItemID = source.ID, restored["native-message"]
	requirements.Positive(target.ItemID)
	beforeDeniedFactory := factoryCalls.Load()
	response := call(http.MethodPost, "/api/v1/inbox/control", request, oldSecret)
	requirements.Equal(http.StatusForbidden, response.Code, response.Body.String(), "the original issued HTTP grant must not bind a new source lifetime")
	assertions.Equal(beforeDeniedFactory, factoryCalls.Load(), "old grant denial precedes native provider construction")
	_, secret = issue(nil)
	requirements.NoError(daemon.Close())
	daemon, err = daemonclient.New(daemonclient.Config{URL: networkDaemon.URL, AgentToken: secret, AllowInsecure: true})
	requirements.NoError(err)
	session := nativeInboxMCPSession(t, daemon)
	// Refresh a current-source preview before changing its native labels.
	previewResponse = call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusOK, previewResponse.Code, previewResponse.Body.String())
	requirements.NoError(json.Unmarshal(previewResponse.Body.Bytes(), &preview))
	mu.Lock()
	assertions.Equal(0, posts)
	assertions.Equal(2, rawGets)
	labels = append(labels, "STARRED")
	mu.Unlock()
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-archive"
	stale := call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusConflict, stale.Code, stale.Body.String())
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = true, nil, "", ""
	previewResponse = call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusOK, previewResponse.Code, previewResponse.Body.String())
	requirements.NoError(json.Unmarshal(previewResponse.Body.Bytes(), &preview))
	// An actual MCP preview crosses the daemon HTTP client and native resolver.
	targetJSON, err := json.Marshal(target)
	requirements.NoError(err)
	var targetArgs map[string]any
	requirements.NoError(json.Unmarshal(targetJSON, &targetArgs))
	mcpPreview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: map[string]any{"target": targetArgs}})
	requirements.NoError(err)
	requirements.False(mcpPreview.IsError)
	mcpJSON, err := json.Marshal(mcpPreview.StructuredContent)
	requirements.NoError(err)
	var envelope struct {
		Result *inboxcontrol.Result `json:"result"`
	}
	requirements.NoError(json.Unmarshal(mcpJSON, &envelope))
	requirements.NotNil(envelope.Result)
	preview = *envelope.Result
	request.DryRun, request.Expected, request.PreviewToken, request.IdempotencyKey = false, preview.Before, preview.PreviewToken, "native-archive"
	// The real CLI loses stdout after the native write. The original key still
	// recovers the verified receipt and must never dispatch a second POST.
	input, err := json.Marshal(request)
	requirements.NoError(err)
	requestFile := filepath.Join(t.TempDir(), "request.json")
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(requestFile, input, 0o600))
	requirements.NoError(os.WriteFile(tokenFile, []byte(secret), 0o600))
	closedReader, closedWriter, err := os.Pipe()
	requirements.NoError(err)
	requirements.NoError(closedReader.Close())
	defer func() { assertions.NoError(closedWriter.Close()) }()
	childContext, cancelChild := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelChild()
	child := exec.CommandContext(childContext, os.Args[0], "-test.run=^TestInboxCLINativeClosedStdoutHelper$") //nolint:gosec // Re-executes this test binary with a fixed test selector.
	child.Env = append(os.Environ(), "MSGVAULT_NATIVE_CLOSED_STDOUT=1", "MSGVAULT_NATIVE_DAEMON_URL="+networkDaemon.URL, "MSGVAULT_NATIVE_REQUEST="+requestFile, "MSGVAULT_NATIVE_AGENT_TOKEN_FILE="+tokenFile)
	child.Stdout = closedWriter
	var childError bytes.Buffer
	child.Stderr = &childError
	requirements.NoError(child.Run(), childError.String())
	response = call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var result inboxcontrol.Result
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	requirements.NotNil(result.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, result.Receipt.Status)
	requirements.NotNil(result.After)
	assertions.False(*result.After.Inbox)
	assertions.Contains(result.After.Tags, "STARRED")
	mu.Lock()
	assertions.Equal(1, posts)
	assertions.Equal(1, readbacks, "success requires independent native metadata GET")
	mu.Unlock()
	response = call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	argsJSON, err := json.Marshal(request)
	requirements.NoError(err)
	var executeArgs map[string]any
	requirements.NoError(json.Unmarshal(argsJSON, &executeArgs))
	delete(executeArgs, "operation")
	executeArgs["dry_run"] = false
	pending, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: executeArgs})
	requirements.NoError(err)
	requirements.True(pending.NeedsInput())
	confirmed, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "inbox_archive", Arguments: executeArgs, RequestState: pending.RequestState, InputResponses: sdkmcp.InputResponseMap{"confirm": &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}})
	requirements.NoError(err)
	requirements.False(confirmed.IsError)
	confirmedJSON, err := json.Marshal(confirmed.StructuredContent)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(confirmedJSON, &envelope))
	requirements.NotNil(envelope.Result)
	requirements.NotNil(envelope.Result.Receipt)
	assertions.Equal(result.Receipt.ID, envelope.Result.Receipt.ID)
	revokedID, revokedSecret := issue(nil)
	revoked := call(http.MethodDelete, "/api/v1/agent-tokens/"+revokedID, nil, "")
	requirements.Less(revoked.Code, 300, revoked.Body.String())
	response = call(http.MethodPost, "/api/v1/inbox/control", request, revokedSecret)
	assertions.Contains([]int{http.StatusUnauthorized, http.StatusForbidden}, response.Code)

	expires := time.Now().Add(2 * time.Second)
	_, expiringSecret := issue(&expires)
	expiry := time.NewTimer(time.Until(expires))
	defer expiry.Stop()
	<-expiry.C
	response = call(http.MethodPost, "/api/v1/inbox/control", request, expiringSecret)
	assertions.Contains([]int{http.StatusUnauthorized, http.StatusForbidden}, response.Code)

	requirements.NoError(st.Close())
	st, err = store.OpenForTest(dbPath)
	requirements.NoError(err)
	adapter.store = st
	adapter.inboxKey, err = prepareInboxRuntime(t.Context(), cfg, st)
	requirements.NoError(err)
	assertions.Equal(key, adapter.inboxKey)
	server = api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.Default()})
	receiptRequest := inboxcontrol.Request{Operation: inboxcontrol.OpReceiptGet, ReceiptID: result.Receipt.ID}
	response = call(http.MethodPost, "/api/v1/inbox/control", receiptRequest, "")
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	var recovered inboxcontrol.Result
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &recovered))
	requirements.NotNil(recovered.Receipt)
	assertions.Equal(result.Receipt.ID, recovered.Receipt.ID)
	assertions.Equal(inboxcontrol.StatusVerified, recovered.Receipt.Status)

	activeID, activeSecret := issue(nil)
	secret = activeSecret
	makeUnknown := func(id string) inboxcontrol.Receipt {
		t.Helper()
		pending := *result.Receipt
		pending.ID, pending.IdempotencyKey, pending.PrincipalID = id, id, activeID
		pending.Status, pending.After, pending.DispatchedAt, pending.FinishedAt = inboxcontrol.StatusPrepared, nil, nil, nil
		_, claimed, err := st.PrepareInboxReceipt(t.Context(), pending)
		requirements.NoError(err)
		requirements.True(claimed)
		requirements.NoError(st.MarkInboxDispatching(t.Context(), id))
		requirements.NoError(st.FinishInboxReceipt(t.Context(), id, inboxcontrol.StatusUnknown, nil, "interrupted-after-native-dispatch"))
		return pending
	}
	controlPending := makeUnknown("native-reconcile-control")
	reconcileRequest := inboxcontrol.Request{Operation: inboxcontrol.OpReconcile, ReceiptID: controlPending.ID}
	response = call(http.MethodPost, "/api/v1/inbox/control", reconcileRequest, secret)
	requirements.Equal(http.StatusOK, response.Code, response.Body.String())
	requirements.NoError(json.Unmarshal(response.Body.Bytes(), &recovered))
	requirements.NotNil(recovered.Receipt)
	assertions.Equal(inboxcontrol.StatusVerified, recovered.Receipt.Status, "an authorized unknown receipt actually reconciles before removal")
	retiredPending := makeUnknown("native-retired-unknown")
	requirements.NoError(st.RemoveSource(source.ID))
	response = call(http.MethodPost, "/api/v1/inbox/control", receiptRequest, "")
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	// Deliberately bypass the ordinary allocator in this synthetic adversary:
	// retired receipts must remain denied even if both old IDs are forced back.
	_, err = st.DB().Exec(`INSERT INTO sources(id,source_type,identifier) VALUES(?,'gmail',?)`, source.ID, account)
	requirements.NoError(err)
	recreated, err := st.GetSourceByID(source.ID)
	requirements.NoError(err)
	assertions.Equal(source.ID, recreated.ID, "the adversary explicitly restores the retired source ID")
	client, err = factory(t.Context(), nil)
	requirements.NoError(err)
	_, err = archivesync.New(client, st, nil).Full(t.Context(), account)
	requirements.NoError(err)

	reimported, err := st.MessageExistsBatch(recreated.ID, []string{"native-message"})
	requirements.NoError(err)
	requirements.Equal(target.ItemID, reimported["native-message"], "the same source and message IDs were both reused")
	mu.Lock()
	priorFactories := factoryCalls.Load()
	priorReadbacks := readbacks
	mu.Unlock()
	for _, id := range []string{result.Receipt.ID, retiredPending.ID} {
		for _, operation := range []inboxcontrol.Operation{inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
			response = call(http.MethodPost, "/api/v1/inbox/control", inboxcontrol.Request{Operation: operation, ReceiptID: id}, secret)
			assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
			response = call(http.MethodPost, "/api/v1/inbox/control", inboxcontrol.Request{Operation: operation, ReceiptID: id}, "")
			assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
		}
	}
	request.IdempotencyKey = retiredPending.IdempotencyKey
	response = call(http.MethodPost, "/api/v1/inbox/control", request, secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String())
	changedIntent := request
	changedIntent.Operation = inboxcontrol.OpSetUnread
	response = call(http.MethodPost, "/api/v1/inbox/control", changedIntent, secret)
	assertions.Equal(http.StatusForbidden, response.Code, response.Body.String(), "a retired key cannot become a changed-intent replay")
	audit, err := st.GetInboxReceipt(t.Context(), retiredPending.ID)
	requirements.NoError(err)
	requirements.NotNil(audit)
	assertions.Equal(inboxcontrol.StatusUnknown, audit.Status, "denied reconciliation preserves trusted audit")
	mu.Lock()
	assertions.Equal(priorFactories, factoryCalls.Load(), "retired receipt denial precedes native provider construction")
	assertions.Equal(priorReadbacks, readbacks, "retired control authority must not reach native reconciliation reads")
	assertions.Equal(1, posts, "receipt replay, restart and revoked authority must never redispatch")
	mu.Unlock()
}

func TestInboxCLINativeClosedStdoutHelper(t *testing.T) {
	if os.Getenv("MSGVAULT_NATIVE_CLOSED_STDOUT") != "1" {
		return
	}
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{agentURL: os.Getenv("MSGVAULT_NATIVE_DAEMON_URL"), agentTokenFile: os.Getenv("MSGVAULT_NATIVE_AGENT_TOKEN_FILE"), agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
	command := newInboxCmd()
	ensureSilenceUsageWrapped(command)
	command.SetContext(ctx)
	command.SetArgs([]string{"archive", "--request", os.Getenv("MSGVAULT_NATIVE_REQUEST"), "--apply"})
	command.SetOut(os.Stdout)
	command.SetErr(io.Discard)
	if !errors.Is(command.Execute(), inboxcontrol.ErrOutcomeUnknown) {
		os.Exit(23)
	}
	// The Go test runner would also write its summary to the broken descriptor.
	os.Exit(0)
}

func nativeInboxMCPSession(t *testing.T, client *daemonclient.Client) *sdkmcp.ClientSession {
	t.Helper()
	opts, err := delegatedMCPServeOptions(t.Context(), client)
	require.NoError(t, err)
	require.NotNil(t, opts.Inbox)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- mcpserver.ServeTransport(ctx, opts, serverTransport) }()
	mcpClient := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "native-inbox", Version: "1"}, &sdkmcp.ClientOptions{MultiRoundTrip: &sdkmcp.MultiRoundTripOptions{Disabled: true}})
	session, err := mcpClient.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				require.ErrorIs(t, err, context.Canceled)
			}
		case <-time.After(5 * time.Second):
			require.Fail(t, "native MCP transport did not stop")
		}
	})
	return session
}
