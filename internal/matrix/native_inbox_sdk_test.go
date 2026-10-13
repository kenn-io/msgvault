package matrix

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

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpdiscovery"
	"go.kenn.io/msgvault/internal/mcpevents"
)

type nativeMatrixSubscribeParams struct {
	sdkmcp.ParamsBase

	Name      string             `json:"name"`
	Arguments map[string]any     `json:"arguments"`
	Delivery  mcpevents.Delivery `json:"delivery"`
}

type nativeMatrixSubscribeResult struct {
	sdkmcp.ResultBase

	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
}

type nativeMatrixOwnerTransport struct{ owner string }

func (rt nativeMatrixOwnerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	request.Header.Set("Authorization", "Bearer "+rt.owner)
	return http.DefaultTransport.RoundTrip(request)
}

// Missing exact native membership, an unadvertised implemented scope, or a
// receipt that ignores current parent authority must break this lifecycle.
// Only the native provider and callback destinations are synthetic. The real
// Go SDK client, HTTP adapter, daemon, importer, SQL and signed bytes are used.
func TestNativeMatrixInboxClientSDKCaptureAndRemoval(t *testing.T) {
	for _, revoke := range []string{"leave", "members_denied", "unknown_members"} {
		t.Run(revoke, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			const owner = "synthetic-matrix-sdk-owner"
			const sourceFamily = "msgvault.source_message_archived"
			const inboxFamily = "msgvault.inbox_message_archived"
			native, imp, opts, sourceID := newNativeMatrixInboxFeed(t)
			st := imp.store
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
			daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler)})
			t.Cleanup(func() { Assert.NoError(t, daemon.Shutdown(context.Background())) })
			secret := bytes.Repeat([]byte("s"), 32)
			received := make(chan mcpevents.Envelope, 16)
			var challenges atomic.Int64
			receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if !Assert.NoError(t, err) {
					w.WriteHeader(http.StatusBadRequest)
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
				if !Assert.True(t, valid, "independent Standard Webhooks verification") {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var challenge struct {
					Challenge string `json:"challenge"`
				}
				if json.Unmarshal(raw, &challenge) == nil && challenge.Challenge != "" {
					challenges.Add(1)
					if !Assert.Equal(t, "msg_verification_"+challenge.Challenge, id) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					Assert.NoError(t, json.NewEncoder(w).Encode(challenge))
					return
				}
				var envelope mcpevents.Envelope
				if !Assert.NoError(t, json.Unmarshal(raw, &envelope)) || !Assert.Equal(t, envelope.EventID, id) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				Assert.NotContains(t, string(raw), "synthetic ")
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
			service, err := mcpevents.New(t.Context(), st, mcpevents.Options{Enabled: true, Sources: []string{"matrix"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, WithDeliveryOperation: daemon.MCPEventsDeliveryOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
			}})
			require.NoError(err)
			daemon.SetMCPEvents(service)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var historicalEvents int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&historicalEvents))
			assert.Zero(historicalEvents)
			endpoint := httptest.NewServer(daemon.Router())
			t.Cleanup(endpoint.Close)
			bridge, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: owner, AllowInsecure: true})
			require.NoError(err)
			page, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "inbox", SourceID: strconv.FormatInt(sourceID, 10)})
			require.NoError(err)
			resources := map[string]mcpevents.Resource{}
			for _, resource := range page.Resources {
				resources[resource.ProviderID] = resource
			}
			require.Contains(resources, "$root")
			require.Contains(resources, "!quiet:example.org")
			sourcePage, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "source", SourceID: strconv.FormatInt(sourceID, 10)})
			require.NoError(err)
			require.Len(sourcePage.Resources, 1)
			var conversationID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), sourceID, "!room:example.org").Scan(&conversationID))
			originalConversation, err := st.GetMCPResource(t.Context(), "conversation", conversationID)
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
					Assert.ErrorIs(t, serveErr, context.Canceled)
				case <-time.After(15 * time.Second):
					Assert.Fail(t, "native SDK HTTP listener did not stop")
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
			}, 30*time.Second, 20*time.Millisecond)
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-matrix-inbox", Version: "1"}, nil)
			require.NoError(sdkmcp.AddSendingCustomMethod[*nativeMatrixSubscribeParams, *nativeMatrixSubscribeResult](client, "events/subscribe"))
			session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeMatrixOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, session.Close()) })
			subscribe := func(name string, args map[string]any) (*nativeMatrixSubscribeResult, error) {
				return sdkmcp.CallCustomMethod[*nativeMatrixSubscribeParams, *nativeMatrixSubscribeResult](t.Context(), session, "events/subscribe", &nativeMatrixSubscribeParams{Name: name, Arguments: args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-matrix-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
			}
			for _, binding := range []struct {
				name string
				args map[string]any
			}{
				{"msgvault.message_archived", map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10)}},
				{sourceFamily, map[string]any{"source_id": sourcePage.Resources[0].ID, "source_ref": sourcePage.Resources[0].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["!room:example.org"].ID, "inbox_ref": resources["!room:example.org"].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["$root"].ID, "inbox_ref": resources["$root"].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["!quiet:example.org"].ID, "inbox_ref": resources["!quiet:example.org"].Reference}},
			} {
				result, subscribeErr := subscribe(binding.name, binding.args)
				require.NoError(subscribeErr, "native SDK must admit %s", binding.name)
				require.NotEmpty(result.ID)
				_, parseErr := time.Parse(time.RFC3339Nano, result.RefreshBefore)
				require.NoError(parseErr)
			}
			runCtx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- service.Run(runCtx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case runErr := <-done:
					Assert.NoError(t, runErr)
				case <-time.After(30 * time.Second):
					Assert.Fail(t, "Events workers did not stop")
				}
			})

			native.mu.Lock()
			native.round = 1
			native.quietOmitted = true
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			names := make([]string, 0, 4)
			inboxIDs := make([]string, 0, 2)
			receipts := map[string]mcpevents.Envelope{}
			var sourceReceipt mcpevents.Envelope
			for range 4 {
				select {
				case envelope := <-received:
					names = append(names, envelope.Name)
					if envelope.Name == sourceFamily {
						sourceReceipt = envelope
					}
					var data struct {
						MessageID string `json:"message_id"`
						InboxID   string `json:"inbox_id"`
					}
					require.NoError(json.Unmarshal(envelope.Data, &data))
					if envelope.Name == inboxFamily {
						inboxIDs = append(inboxIDs, data.InboxID)
						receipts[data.InboxID] = envelope
					}
					result, readErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_message", Arguments: map[string]any{"id": data.MessageID, "event_id": envelope.EventID, "offset": 0, "max_chars": 9}})
					require.NoError(readErr)
					require.False(result.IsError)
					raw, marshalErr := json.Marshal(result.StructuredContent)
					require.NoError(marshalErr)
					var content struct {
						BodyText string `json:"body_text"`
					}
					require.NoError(json.Unmarshal(raw, &content))
					assert.Equal("Synthetic", content.BodyText)
				case <-time.After(30 * time.Second):
					require.FailNow("committed native message did not reach signed callback")
				}
			}
			assert.ElementsMatch([]string{"msgvault.message_archived", sourceFamily, inboxFamily, inboxFamily}, names)
			assert.ElementsMatch([]string{resources["!room:example.org"].ID, resources["$root"].ID}, inboxIDs, "the exact room and thread capture, while the quiet channel stays silent")

			native.mu.Lock()
			native.round = 2
			switch revoke {
			case "leave":
				native.leave = true
			case "members_denied":
				native.memberStatus = http.StatusForbidden
			case "unknown_members":
				native.memberStatus = http.StatusBadRequest
			}
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			if revoke == "leave" {
				require.NoError(err)
			} else {
				require.Error(err)
			}
			threadReceipt := receipts[resources["$root"].ID]
			_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
			require.Error(err, "current native permission must gate source receipt reads")
			_, err = bridge.GetMCPEvent(t.Context(), threadReceipt.EventID)
			require.Error(err, "current native room permission must gate exact thread receipt reads")
			currentConversation, err := st.GetMCPResource(t.Context(), "conversation", conversationID)
			require.NoError(err)
			if revoke == "unknown_members" {
				assert.Equal(originalConversation.Reference, currentConversation.Reference, "unknown results pause rather than retire the native origin")
			} else {
				assert.NotEqual(originalConversation.Reference, currentConversation.Reference)
				for _, binding := range []struct {
					name string
					args map[string]any
				}{
					{inboxFamily, map[string]any{"inbox_id": resources["$root"].ID, "inbox_ref": resources["$root"].Reference}},
					{"msgvault.message_archived", map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10), "conversation_ref": originalConversation.Reference}},
				} {
					negative, connectErr := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeMatrixOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
					require.NoError(connectErr)
					before := challenges.Load()
					_, subscribeErr := sdkmcp.CallCustomMethod[*nativeMatrixSubscribeParams, *nativeMatrixSubscribeResult](t.Context(), negative, "events/subscribe", &nativeMatrixSubscribeParams{Name: binding.name, Arguments: binding.args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-matrix-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
					require.Error(subscribeErr)
					assert.NotContains(subscribeErr.Error(), "client is closing")
					require.NoError(negative.Close())
					assert.Equal(before, challenges.Load(), "revoked native room rejects before challenge IO")
				}
				_, err = subscribe(inboxFamily, map[string]any{"inbox_id": resources["!quiet:example.org"].ID, "inbox_ref": resources["!quiet:example.org"].Reference})
				require.NoError(err, "independently readable quiet room survives unrelated room revocation")
			}
			native.mu.Lock()
			native.leave = false
			native.memberStatus = 0
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			_, err = bridge.GetMCPEvent(t.Context(), threadReceipt.EventID)
			if revoke == "unknown_members" {
				assert.NoError(err, "verified permission can resume the unchanged lifetime")
			} else {
				require.Error(err, "restoration cannot revive a positively retired receipt")
			}
		})
	}
}
