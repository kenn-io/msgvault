package discord

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

type nativeDiscordSubscribeParams struct {
	sdkmcp.ParamsBase

	Name      string             `json:"name"`
	Arguments map[string]any     `json:"arguments"`
	Delivery  mcpevents.Delivery `json:"delivery"`
}

type nativeDiscordSubscribeResult struct {
	sdkmcp.ResultBase

	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
}

type nativeDiscordOwnerTransport struct{ owner string }

func (rt nativeDiscordOwnerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	request.Header.Set("Authorization", "Bearer "+rt.owner)
	return http.DefaultTransport.RoundTrip(request)
}

// Missing exact native membership, an unadvertised implemented scope, or a
// receipt that ignores current parent authority must break this lifecycle.
// Only the native provider and callback destinations are synthetic. The real
// Go SDK client, HTTP adapter, daemon, importer, SQL and signed bytes are used.
func TestNativeDiscordInboxClientSDKCaptureAndRemoval(t *testing.T) {
	for _, revoke := range []string{"parent_removed", "thread_read_denied", "parent_history_denied", "parent_rebound"} {
		t.Run(revoke, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			const owner = "synthetic-discord-sdk-owner"
			const sourceFamily = "msgvault.source_message_archived"
			const inboxFamily = "msgvault.inbox_message_archived"
			native, imp, opts := newNativeDiscordInboxFeed(t)
			now, err := TimestampFromSnowflake("1500000000000000000")
			require.NoError(err)
			imp.now = func() time.Time { return now.Add(time.Hour) }
			native.messages["400"] = []Message{nativeDiscordInboxMessage("1500000000000000000", "400", "synthetic history")}
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
			service, err := mcpevents.New(t.Context(), st, mcpevents.Options{Enabled: true, Sources: []string{"discord"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, WithDeliveryOperation: daemon.MCPEventsDeliveryOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
			}})
			require.NoError(err)
			daemon.SetMCPEvents(service)
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			var historicalEvents int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&historicalEvents))
			assert.Zero(historicalEvents)
			endpoint := httptest.NewServer(daemon.Router())
			t.Cleanup(endpoint.Close)
			bridge, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: owner, AllowInsecure: true})
			require.NoError(err)
			page, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "inbox", SourceID: strconv.FormatInt(sum.SourceID, 10)})
			require.NoError(err)
			resources := map[string]mcpevents.Resource{}
			for _, resource := range page.Resources {
				resources[resource.ProviderID] = resource
			}
			require.Contains(resources, "400")
			require.Contains(resources, "301")
			sourcePage, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "source", SourceID: strconv.FormatInt(sum.SourceID, 10)})
			require.NoError(err)
			require.Len(sourcePage.Resources, 1)
			var conversationID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), sum.SourceID, "400").Scan(&conversationID))
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
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-discord-inbox", Version: "1"}, nil)
			require.NoError(sdkmcp.AddSendingCustomMethod[*nativeDiscordSubscribeParams, *nativeDiscordSubscribeResult](client, "events/subscribe"))
			session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeDiscordOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, session.Close()) })
			subscribe := func(name string, args map[string]any) (*nativeDiscordSubscribeResult, error) {
				return sdkmcp.CallCustomMethod[*nativeDiscordSubscribeParams, *nativeDiscordSubscribeResult](t.Context(), session, "events/subscribe", &nativeDiscordSubscribeParams{Name: name, Arguments: args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-discord-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
			}
			for _, binding := range []struct {
				name string
				args map[string]any
			}{
				{"msgvault.message_archived", map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10)}},
				{sourceFamily, map[string]any{"source_id": sourcePage.Resources[0].ID, "source_ref": sourcePage.Resources[0].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["300"].ID, "inbox_ref": resources["300"].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["400"].ID, "inbox_ref": resources["400"].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["301"].ID, "inbox_ref": resources["301"].Reference}},
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
			native.messages["400"] = append(native.messages["400"], nativeDiscordInboxMessage("1500000000000000001", "400", "synthetic live exact-thread message"))
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
					assert.Equal("synthetic", content.BodyText)
				case <-time.After(30 * time.Second):
					require.FailNow("committed native message did not reach signed callback")
				}
			}
			assert.ElementsMatch([]string{"msgvault.message_archived", sourceFamily, inboxFamily, inboxFamily}, names)
			assert.ElementsMatch([]string{resources["300"].ID, resources["400"].ID}, inboxIDs, "the exact parent and thread capture, while the quiet channel stays silent")
			native.mu.Lock()
			switch revoke {
			case "parent_rebound":
				native.channels = append(native.channels, Channel{ID: "303", GuildID: "200", Type: channelTypeGuildText, Name: "Synthetic replacement parent"})
				native.threads[0].ParentID = "303"
			case "parent_history_denied":
				native.channels[0].PermissionOverwrites = []PermissionOverwrite{{ID: "200", Type: 0, Allow: "0", Deny: "65536"}}
			case "thread_read_denied":
				native.denied["400"] = true
			default:
				native.channels = native.channels[1:]
				native.threads = nil
			}
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
			require.Error(err, "source-wide receipt retains the removed native origin")
			envelope := receipts[resources["400"].ID]
			_, err = bridge.GetMCPEvent(t.Context(), envelope.EventID)
			require.Error(err, "removed native Discord container revokes the inbox receipt")
			freshConversations, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "conversation", SourceID: strconv.FormatInt(sum.SourceID, 10)})
			require.NoError(err)
			var currentConversation mcpevents.Resource
			for _, row := range freshConversations.Resources {
				if row.ID == strconv.FormatInt(conversationID, 10) {
					currentConversation = row
				}
			}
			require.NotEmpty(currentConversation.ID)
			if revoke == "parent_rebound" {
				assert.Contains(currentConversation.Capabilities.Events, "msgvault.message_archived", "the replacement route is independently readable")
				assert.NotEqual(originalConversation.Reference, currentConversation.Reference, "a changed native parent cannot revive the old conversation origin")
				currentThread, lookupErr := st.GetMCPResource(t.Context(), "inbox", mustDiscordResourceID(t, resources["400"].ID))
				require.NoError(lookupErr)
				assert.Equal("303", currentThread.ParentProviderID)
			} else {
				assert.NotContains(currentConversation.Capabilities.Events, "msgvault.message_archived", "archived conversations do not prove current native read access after removal")
			}

			for _, negative := range []struct {
				name string
				args map[string]any
			}{
				{inboxFamily, map[string]any{"inbox_id": resources["400"].ID, "inbox_ref": resources["400"].Reference}},
				{"msgvault.message_archived", map[string]any{"conversation_id": currentConversation.ID, "conversation_ref": originalConversation.Reference}},
			} {
				negativeSession, connectErr := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeDiscordOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
				require.NoError(connectErr)
				before := challenges.Load()
				_, subscribeErr := sdkmcp.CallCustomMethod[*nativeDiscordSubscribeParams, *nativeDiscordSubscribeResult](t.Context(), negativeSession, "events/subscribe", &nativeDiscordSubscribeParams{Name: negative.name, Arguments: negative.args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-discord-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
				require.Error(subscribeErr, "removed native Discord container must reject %s", negative.name)
				assert.NotContains(subscribeErr.Error(), "client is closing")
				require.NoError(negativeSession.Close())
				assert.Equal(before, challenges.Load(), "removed native Discord container rejects before callback challenge IO")
			}

			quietResult, err := subscribe(inboxFamily, map[string]any{"inbox_id": resources["301"].ID, "inbox_ref": resources["301"].Reference})
			require.NoError(err)
			assert.NotEmpty(quietResult.ID)
			sourceResult, err := subscribe(sourceFamily, map[string]any{"source_id": sourcePage.Resources[0].ID, "source_ref": sourcePage.Resources[0].Reference})
			require.NoError(err)
			assert.NotEmpty(sourceResult.ID)
			native.mu.Lock()
			native.denied["400"] = false
			switch revoke {
			case "parent_rebound":
				native.threads[0].ParentID = "300"
			}
			if revoke == "parent_history_denied" {
				native.channels[0].PermissionOverwrites = nil
			}
			if revoke == "parent_removed" {
				native.channels = append(native.channels, Channel{ID: "300", GuildID: "200", Type: channelTypeGuildText, Name: "Restored synthetic channel"})
				native.threads = []Channel{{ID: "400", GuildID: "200", Type: channelTypePublicThread, ParentID: "300", Name: "Restored synthetic thread"}}
			}
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
			require.Error(err, "restoration cannot revive a retired source occurrence origin")
			_, err = bridge.GetMCPEvent(t.Context(), envelope.EventID)
			require.Error(err, "restoration cannot revive the old inbox receipt")
			// Source scope also covers a conversation first created after subscription.
			// Its verified empty history establishes the preserved live-only boundary;
			// there is no conversation or inbox subscription for this new native channel.
			native.mu.Lock()
			native.channels = append(native.channels, Channel{ID: "302", GuildID: "200", Type: channelTypeGuildText, Name: "Synthetic later channel"})
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			native.mu.Lock()
			native.messages["302"] = []Message{nativeDiscordInboxMessage("1500000000000000002", "302", "synthetic new conversation arrival")}
			native.mu.Unlock()
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			select {
			case later := <-received:
				assert.Equal(sourceFamily, later.Name, "an existing source subscription includes a newly created conversation")
				var data struct {
					MessageID string `json:"message_id"`
				}
				require.NoError(json.Unmarshal(later.Data, &data))
				result, readErr := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "get_message", Arguments: map[string]any{"id": data.MessageID, "event_id": later.EventID, "offset": 0, "max_chars": 9}})
				require.NoError(readErr)
				require.False(result.IsError)
				raw, marshalErr := json.Marshal(result.StructuredContent)
				require.NoError(marshalErr)
				var body struct {
					BodyText string `json:"body_text"`
				}
				require.NoError(json.Unmarshal(raw, &body))
				assert.Equal("synthetic", body.BodyText)
			case <-time.After(30 * time.Second):
				require.FailNow("source subscription missed the new conversation's native live arrival")
			}
		})
	}
}

func mustDiscordResourceID(t *testing.T, value string) int64 {
	t.Helper()
	require := Require.New(t)
	id, err := strconv.ParseInt(value, 10, 64)
	require.NoError(err)
	return id
}
