package msmail

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// Only native provider HTTP and callback DNS/dial destinations are synthetic.
// Real Graph/MIME parsing, importer, SQL, SDK wire, signing and receipt readers
// establish this lifecycle; no journal rows or memberships are inserted by tests.
func TestNativeMicrosoftFolderSDKCaptureMembershipAndRemoval(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	const owner = "synthetic-source-events-owner"
	const sourceFamily = "msgvault.source_message_archived"
	const inboxFamily = "msgvault.inbox_message_archived"
	st := testutil.NewTestStore(t)
	graph := newFakeGraph(t)
	graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" {
			graph.writeJSON(w, map[string]any{"id": "synthetic-native-account", "mail": "me@example.com", "userPrincipalName": "me@example.com"})
			return
		}
		graph.serve(w, r)
	})
	var grants atomic.Value
	grants.Store([]string{"Mail.Read"})
	native := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000, WithGrantedScopesReader(func() ([]string, error) { values, _ := grants.Load().([]string); return values, nil }))
	run := func() {
		_, err := Import(t.Context(), st, native, Options{Email: "me@example.com"}, slog.Default())
		require.NoError(err)
	}
	graph.put("history", "inbox")
	run()
	source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
	require.NoError(err)
	require.NotNil(source)
	var conversationID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE source_id=? AND source_message_id='history'`), source.ID).Scan(&conversationID))
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(daemon.Shutdown(context.Background())) })
	secret := bytes.Repeat([]byte("s"), 32)
	received := make(chan mcpevents.Envelope, 8)
	var challenges atomic.Int64
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			http.Error(w, "read failed", http.StatusBadRequest)
			return
		}
		id, timestamp := r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp")
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(id + "." + timestamp + "."))
		_, _ = mac.Write(raw)
		valid := false
		for signature := range strings.FieldsSeq(r.Header.Get("Webhook-Signature")) {
			encoded, ok := strings.CutPrefix(signature, "v1,")
			if !ok {
				continue
			}
			sig, decodeErr := base64.StdEncoding.DecodeString(encoded)
			valid = valid || decodeErr == nil && hmac.Equal(sig, mac.Sum(nil))
		}
		if !assert.True(valid, "native callback must pass independent signature verification") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var challenge struct {
			Challenge string `json:"challenge"`
		}
		if json.Unmarshal(raw, &challenge) == nil && challenge.Challenge != "" {
			challenges.Add(1)
			if !assert.Equal("msg_verification_"+challenge.Challenge, id) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if encodeErr := json.NewEncoder(w).Encode(challenge); encodeErr != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}
		var envelope mcpevents.Envelope
		if !assert.NoError(json.Unmarshal(raw, &envelope)) || !assert.Equal(envelope.EventID, id) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.NotContains(string(raw), "Live synthetic body")
		select {
		case received <- envelope:
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	service, err := mcpevents.New(t.Context(), st, mcpevents.Options{Enabled: true, Sources: []string{SourceType}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, WithDeliveryOperation: daemon.MCPEventsDeliveryOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}})
	require.NoError(err)
	daemon.SetMCPEvents(service)
	runCtx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case runErr := <-done:
			assert.NoError(runErr)
		case <-time.After(30 * time.Second):
			assert.Fail("native Events workers did not stop")
		}
	})
	endpoint := httptest.NewServer(daemon.Router())
	t.Cleanup(endpoint.Close)
	bridge, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: owner, AllowInsecure: true})
	require.NoError(err)
	directory := t.TempDir()
	mcpCtx, stopMCP := context.WithCancel(t.Context())
	mcpDone := make(chan error, 1)
	go func() {
		mcpDone <- mcp.ServeHTTPWithOptions(mcpCtx, mcp.ServeOptions{Engine: daemonclient.NewEngineAdapter(bridge), Events: bridge}, mcp.HTTPOptions{Addr: "127.0.0.1:0", APIKey: owner, DiscoveryDirectory: directory})
	}()
	t.Cleanup(func() {
		stopMCP()
		select {
		case serveErr := <-mcpDone:
			assert.ErrorIs(serveErr, context.Canceled)
		case <-time.After(15 * time.Second):
			assert.Fail("native SDK HTTP listener did not stop")
		}
	})
	var sdkURL string
	require.Eventually(func() bool {
		listeners, listErr := mcpdiscovery.List(directory)
		if listErr != nil || len(listeners) != 1 {
			return false
		}
		sdkURL = listeners[0].URL
		return true
	}, 30*time.Second, 20*time.Millisecond, "synthetic listener must publish its actual bound endpoint")
	httpClient := &http.Client{Timeout: 30 * time.Second}
	call := func(method string, params map[string]any) map[string]any {
		t.Helper()
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		raw, marshalErr := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		require.NoError(marshalErr)
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, sdkURL, bytes.NewReader(raw))
		require.NoError(requestErr)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", method)
		request.Header.Set("Authorization", "Bearer "+owner)
		if method == "tools/call" {
			name, ok := params["name"].(string)
			require.True(ok)
			request.Header.Set("Mcp-Name", name)
		}
		response, requestErr := httpClient.Do(request)
		require.NoError(requestErr)
		defer func() { require.NoError(response.Body.Close()) }()
		var wire map[string]any
		require.NoError(json.NewDecoder(response.Body).Decode(&wire))
		if wire["error"] == nil {
			require.Equal(http.StatusOK, response.StatusCode)
		}
		return wire
	}
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	inboxPage, pageErr := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: owner, Kind: "inbox", SourceID: source.ID})
	require.NoError(pageErr)
	var inbox store.MCPResource
	for _, candidate := range inboxPage.Resources {
		if candidate.ProviderID == "inbox" {
			inbox = candidate
		}
	}
	require.Positive(inbox.ID)
	for _, family := range []string{"msgvault.message_archived", sourceFamily, inboxFamily} {
		args := map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10)}
		if family == sourceFamily {
			args = map[string]any{"source_id": strconv.FormatInt(source.ID, 10), "source_ref": resource.Reference}
		}
		if family == inboxFamily {
			args = map[string]any{"inbox_id": strconv.FormatInt(inbox.ID, 10), "inbox_ref": inbox.Reference}
		}
		wire := call("events/subscribe", map[string]any{"name": family, "arguments": args, "delivery": map[string]any{"mode": "webhook", "url": "https://example.com/native-source", "secret": "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
		require.Nil(wire["error"], "native SDK subscription must reach actual service: %v", wire["error"])
	}
	var sourceReceipt, inboxReceipt mcpevents.Envelope
	for n := int64(1); n <= 2; n++ {
		id, folder := "live-a", "inbox"
		if n == 2 {
			id, folder = "live-b", "archive"
		}
		if n == 1 {
			graph.rawOverride = map[string]string{id: strings.Replace(raw(id, 0), "Subject:", "References: <history@example.com>\r\nSubject:", 1)}
		}
		graph.put(id, folder)
		run()
		count := 1
		if n == 1 {
			count = 3
		}
		names := make([]string, 0, count)
		for range count {
			select {
			case envelope := <-received:
				names = append(names, envelope.Name)
				if envelope.Name == inboxFamily {
					inboxReceipt = envelope
				}
				if envelope.Name == sourceFamily {
					sourceReceipt = envelope
				}
			case <-time.After(30 * time.Second):
				require.FailNow("native arrival did not reach signed SDK callback")
			}
		}
		if n == 1 {
			assert.ElementsMatch([]string{"msgvault.message_archived", sourceFamily, inboxFamily}, names)
		} else {
			assert.Equal([]string{sourceFamily}, names, "a new conversation belongs only to the source subscription")
		}
	}
	require.NotEmpty(sourceReceipt.EventID)
	var data struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(json.Unmarshal(sourceReceipt.Data, &data))
	require.NotEmpty(data.MessageID)
	wire := call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": data.MessageID, "event_id": sourceReceipt.EventID, "offset": 0, "max_chars": 4}})
	require.Nil(wire["error"])
	result, ok := wire["result"].(map[string]any)
	require.True(ok)
	require.NotEqual(true, result["isError"])
	content, ok := result["structuredContent"].(map[string]any)
	require.True(ok)
	assert.Equal("body", content["body_text"])
	require.NotEmpty(inboxReceipt.EventID)
	var inboxData struct {
		InboxID   string `json:"inbox_id"`
		MessageID string `json:"message_id"`
	}
	require.NoError(json.Unmarshal(inboxReceipt.Data, &inboxData))
	assert.Equal(strconv.FormatInt(inbox.ID, 10), inboxData.InboxID)
	inboxWire := call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": inboxData.MessageID, "event_id": inboxReceipt.EventID, "offset": 0, "max_chars": 4}})
	require.Nil(inboxWire["error"])
	inboxResult, ok := inboxWire["result"].(map[string]any)
	require.True(ok)
	assert.NotEqual(true, inboxResult["isError"])
	graph.folders = []string{"archive"}
	run()
	_, err = bridge.GetMCPEvent(t.Context(), inboxReceipt.EventID)
	require.Error(err, "removed native folder retires its receipt")
	_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
	require.NoError(err, "source receipt retains independent authority")
	before := challenges.Load()
	removed := call("events/subscribe", map[string]any{"name": inboxFamily, "arguments": map[string]any{"inbox_id": strconv.FormatInt(inbox.ID, 10), "inbox_ref": inbox.Reference}, "delivery": map[string]any{"mode": "webhook", "url": "https://example.com/native-source", "secret": "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
	assert.NotNil(removed["error"])
	assert.Equal(before, challenges.Load(), "removed scope denies before callback IO")
	grants.Store([]string(nil))
	run()
	_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
	require.Error(err, "unknown current grant pauses receipt access")
	same, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(resource.Reference, same.Reference)
	resourcesWire := call("tools/call", map[string]any{"name": "list_mcp_event_resources", "arguments": map[string]any{"kind": "source", "source_id": strconv.FormatInt(source.ID, 10)}})
	require.Nil(resourcesWire["error"])
	resourcesResult, ok := resourcesWire["result"].(map[string]any)
	require.True(ok)
	require.NotEqual(true, resourcesResult["isError"], "resource discovery remains available during unknown native authority")
	structured, ok := resourcesResult["structuredContent"].(map[string]any)
	require.True(ok)
	rows, ok := structured["resources"].([]any)
	require.True(ok)
	require.Len(rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(ok)
	currentCaps, ok := row["capabilities"].(map[string]any)
	require.True(ok)
	assert.Empty(currentCaps["events"])
	assert.Equal("native_authority_unavailable", currentCaps["unavailable_reason"])
}
