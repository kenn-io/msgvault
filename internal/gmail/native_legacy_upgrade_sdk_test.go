package gmail_test

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
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Assertion objects are shared with the controlled receiver.
	Require "github.com/stretchr/testify/require" //nolint:importas // Assertion constructors keep setup failures fatal.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/oauth"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil/eventstest"
)

func TestNativeGmailLegacyUpgradePreservesReceiptsAndFreshRetries(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	const owner = "synthetic-events-owner"
	fixtures := filepath.Join("..", "store", "testdata", "mcp_events_legacy")
	st := eventstest.OpenLegacy(t, fixtures)
	rows, err := st.ListMCPSubscriptionsForStartup(t.Context())
	require.NoError(err)
	require.Len(rows, 1)
	before := rows[0]
	require.Equal("active", before.State)
	require.Positive(before.PendingSeq)
	require.NotEmpty(before.PendingEnvelope)
	backend := "sqlite"
	if st.IsPostgreSQL() {
		backend = "postgres"
	}
	key, err := os.ReadFile(filepath.Join(fixtures, backend, "events.key"))
	require.NoError(err)
	keyPath := filepath.Join(t.TempDir(), "events.key")
	require.NoError(os.WriteFile(keyPath, key, 0600))
	manifestBytes, err := os.ReadFile(filepath.Join(fixtures, backend, "manifest.json"))
	require.NoError(err)
	var manifest struct {
		DeliveredEventID   string `json:"delivered_event_id"`
		DeliveredMessageID int64  `json:"delivered_message_id"`
		PendingEventID     string `json:"pending_event_id"`
	}
	require.NoError(json.Unmarshal(manifestBytes, &manifest))
	require.NoError(st.InitSchemaContext(t.Context()))
	now := before.NextAttemptAt.Add(10 * time.Second)
	require.True(now.Before(before.ExpiresAt))
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	observe := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}, Store: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(daemon.Shutdown(context.Background())) })
	type attempt struct {
		raw     []byte
		headers http.Header
		valid   bool
	}
	received := make(chan attempt, 4)
	var sends atomic.Int32
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 262145))
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// Verify the exact bytes using an independent Standard Webhooks receiver.
		// The synthetic fixture's signing secret is intentionally public test data.
		mac := hmac.New(sha256.New, bytes.Repeat([]byte("s"), 32))
		_, _ = mac.Write([]byte(r.Header.Get("Webhook-Id") + "." + r.Header.Get("Webhook-Timestamp") + "."))
		_, _ = mac.Write(raw)
		valid := false
		for signature := range strings.FieldsSeq(r.Header.Get("Webhook-Signature")) {
			if !strings.HasPrefix(signature, "v1,") {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signature, "v1,"))
			valid = valid || err == nil && hmac.Equal(mac.Sum(nil), decoded)
		}
		select {
		case received <- attempt{raw: raw, headers: r.Header.Clone(), valid: valid}:
		default:
		}
		if sends.Add(1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	opts := mcpevents.Options{Enabled: true, Sources: []string{"gmail", "imap", "gcal"}, OwnerKey: owner, KeyPath: keyPath, Now: observe, WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}}
	_, err = mcpevents.New(t.Context(), st, opts)
	require.NoError(err)
	after, err := st.GetMCPSubscription(t.Context(), before.ID)
	require.NoError(err)
	require.NotNil(after)
	// Compare the complete state without printing ciphertext on a failure.
	unchanged := reflect.DeepEqual(before, *after)
	assert.True(unchanged, "startup must preserve every durable subscription field")
	var sourceGeneration, scopeGeneration string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_generation,scope_generation FROM mcp_subscription_resource_bindings WHERE subscription_id=?`), before.ID).Scan(&sourceGeneration, &scopeGeneration))
	bindingDigest := sha256.Sum256([]byte(sourceGeneration + scopeGeneration))
	require.NoError(st.InitSchemaContext(t.Context()))
	service, err := mcpevents.New(t.Context(), st, opts)
	require.NoError(err)
	after, err = st.GetMCPSubscription(t.Context(), before.ID)
	require.NoError(err)
	require.NotNil(after)
	unchanged = reflect.DeepEqual(before, *after)
	assert.True(unchanged, "repeat startup must not rotate secrets, replace cursors or clear pending bytes")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_generation,scope_generation FROM mcp_subscription_resource_bindings WHERE subscription_id=?`), before.ID).Scan(&sourceGeneration, &scopeGeneration))
	assert.Equal(bindingDigest, sha256.Sum256([]byte(sourceGeneration+scopeGeneration)))
	retainedKey, err := os.ReadFile(keyPath)
	require.NoError(err)
	assert.Equal(sha256.Sum256(key), sha256.Sum256(retainedKey))
	daemon.SetMCPEvents(service)
	endpoint := httptest.NewServer(daemon.Router())
	t.Cleanup(endpoint.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: owner, AllowInsecure: true})
	require.NoError(err)
	directory := t.TempDir()
	mcpContext, stopMCP := context.WithCancel(t.Context())
	mcpDone := make(chan error, 1)
	go func() {
		mcpDone <- mcp.ServeHTTPWithOptions(mcpContext, mcp.ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Events: client}, mcp.HTTPOptions{Addr: "127.0.0.1:0", APIKey: owner, DiscoveryDirectory: directory})
	}()
	t.Cleanup(func() {
		stopMCP()
		select {
		case serveErr := <-mcpDone:
			assert.ErrorIs(serveErr, context.Canceled)
		case <-time.After(30 * time.Second):
			assert.Fail("native SDK listener did not stop")
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
	}, 30*time.Second, 20*time.Millisecond, "native SDK listener must publish its actual bound endpoint")
	httpClient := &http.Client{Timeout: 30 * time.Second}
	call := func(method string, params any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(params)
		require.NoError(err)
		var fields map[string]any
		require.NoError(json.Unmarshal(encoded, &fields))
		fields["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		encoded, err = json.Marshal(fields)
		require.NoError(err)
		name := ""
		if method == "tools/call" {
			name = nativeLegacyWireString(t, fields["name"])
		}
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodPost, sdkURL, bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+string(encoded)+`}`)))
		require.NoError(requestErr)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", method)
		if name != "" {
			request.Header.Set("Mcp-Name", name)
		}
		request.Header.Set("Authorization", "Bearer "+owner)
		response, requestErr := httpClient.Do(request)
		require.NoError(requestErr)
		defer func() { require.NoError(response.Body.Close()) }()
		var wire map[string]any
		decoder := json.NewDecoder(response.Body)
		decoder.UseNumber()
		require.NoError(decoder.Decode(&wire))
		require.Nil(wire["error"])
		return nativeLegacyWireObject(t, wire["result"])
	}
	discovery := call("server/discover", map[string]any{})
	assert.Contains(discovery["capabilities"], "events")
	// Restored archive rows and profile identity alone cannot grant current
	// message read. Only the real native parser plus saved grant can resume it.
	_, err = st.GetMCPEvent(t.Context(), before.ID, before.CursorSeq, before.Principal, now)
	require.Error(err, "old receipt must pause until a current native read grant is observed")
	source, err := st.GetSourceByID(before.SourceID)
	require.NoError(err)
	require.Equal("gmail", source.SourceType)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			value = map[string]any{"emailAddress": source.Identifier, "historyId": "10", "messagesTotal": 0, "threadsTotal": 0}
		case strings.HasSuffix(r.URL.Path, "/labels"):
			value = map[string]any{"labels": []any{}}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value = map[string]any{"messages": []any{}, "resultSizeEstimate": 0}
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(json.NewEncoder(w).Encode(value))
	}))
	t.Cleanup(provider.Close)
	native := gmail.NewNativePermissionFixtureClient(provider.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
	t.Cleanup(func() { assert.NoError(native.Close()) })
	nativeOptions := msgsync.DefaultOptions()
	nativeOptions.NoResume = true
	_, err = msgsync.New(native, st, nativeOptions).Full(t.Context(), source.Identifier)
	require.NoError(err)
	after, err = st.GetMCPSubscription(t.Context(), before.ID)
	require.NoError(err)
	require.NotNil(after)
	assert.True(reflect.DeepEqual(before, *after), "native authority refresh preserves every old subscription field")
	receipt := call("tools/call", map[string]any{"name": "get_mcp_event", "arguments": map[string]any{"event_id": manifest.DeliveredEventID}})
	assert.Equal(manifest.DeliveredEventID, nativeLegacyWireObject(t, receipt["structuredContent"])["eventId"])
	const body = "Synthetic body split across receipt-bound pages."
	var joined strings.Builder
	for offset := 0; offset < len(body); offset += 8 {
		result := call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(manifest.DeliveredMessageID, 10), "event_id": manifest.DeliveredEventID, "offset": offset, "max_chars": 8}})
		require.NotEqual(true, result["isError"])
		content := nativeLegacyWireObject(t, result["structuredContent"])
		joined.WriteString(nativeLegacyWireString(t, content["body_text"]))
	}
	assert.Equal(body, joined.String())
	runContext, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- service.Run(runContext) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(err)
		case <-time.After(30 * time.Second):
			assert.Fail("upgraded Events workers did not stop")
		}
	})
	receive := func() attempt {
		t.Helper()
		select {
		case value := <-received:
			return value
		case <-time.After(30 * time.Second):
			require.FailNow("upgraded pending occurrence was not delivered")
			return attempt{}
		}
	}
	first := receive()
	assert.True(first.valid)
	sameBytes := bytes.Equal(before.PendingEnvelope, first.raw)
	assert.True(sameBytes, "first retry must sign the unchanged persisted envelope")
	assert.Equal(before.ID, first.headers.Get("X-Mcp-Subscription-Id"))
	assert.Equal(manifest.PendingEventID, first.headers.Get("Webhook-Id"))
	assert.Equal(strconv.FormatInt(now.Unix(), 10), first.headers.Get("Webhook-Timestamp"))
	require.Eventually(func() bool {
		row, err := st.GetMCPSubscription(t.Context(), before.ID)
		return err == nil && row != nil && row.LastOutcome == "retry_http_5xx" && row.AttemptCount == before.AttemptCount+1 && row.NextAttemptAt.Equal(now.Add(2*time.Second))
	}, 30*time.Second, 20*time.Millisecond, "retry outcome must be durable before advancing time")
	clock.Store(now.Add(3 * time.Second).UnixNano())
	service.Wake()
	second := receive()
	assert.True(second.valid)
	sameBytes = bytes.Equal(before.PendingEnvelope, second.raw)
	assert.True(sameBytes, "second retry must reuse the same persisted envelope")
	assert.Equal(first.headers.Get("Webhook-Id"), second.headers.Get("Webhook-Id"))
	assert.Equal(strconv.FormatInt(now.Add(3*time.Second).Unix(), 10), second.headers.Get("Webhook-Timestamp"))
	sameSignature := first.headers.Get("Webhook-Signature") == second.headers.Get("Webhook-Signature")
	assert.False(sameSignature, "each changed attempt timestamp requires a fresh signature")
	require.Eventually(func() bool {
		row, err := st.GetMCPSubscription(t.Context(), before.ID)
		return err == nil && row != nil && row.PendingSeq == 0 && row.CursorSeq == before.PendingSeq
	}, 30*time.Second, 20*time.Millisecond, "acknowledgement must settle the original pending occurrence")
}

func nativeLegacyWireObject(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	Require.True(t, ok, "expected SDK object")
	return out
}
func nativeLegacyWireString(t *testing.T, value any) string {
	t.Helper()
	out, ok := value.(string)
	Require.True(t, ok, "expected SDK string")
	return out
}
