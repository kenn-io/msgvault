package slack

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

type nativeSlackSubscribeParams struct {
	sdkmcp.ParamsBase

	Name      string             `json:"name"`
	Arguments map[string]any     `json:"arguments"`
	Delivery  mcpevents.Delivery `json:"delivery"`
}

type nativeSlackSubscribeResult struct {
	sdkmcp.ResultBase

	ID            string `json:"id"`
	RefreshBefore string `json:"refreshBefore"`
}

type nativeSlackOwnerTransport struct{ owner string }

func (rt nativeSlackOwnerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request := r.Clone(r.Context())
	request.Header.Set("Authorization", "Bearer "+rt.owner)
	return http.DefaultTransport.RoundTrip(request)
}

// Missing exact native membership, an unadvertised implemented scope, or a
// receipt that ignores current parent authority must break this lifecycle.
// Only the native provider and callback destinations are synthetic. The real
// Go SDK client, HTTP adapter, daemon, importer, SQL and signed bytes are used.
func TestNativeSlackInboxClientSDKCaptureAndParentRemoval(t *testing.T) {
	for _, revoke := range []string{"channel_excluded", "private_grant_partial_inventory"} {
		t.Run(revoke, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			const owner = "synthetic-slack-sdk-owner"
			const sourceFamily = "msgvault.source_message_archived"
			const inboxFamily = "msgvault.inbox_message_archived"
			native := nativeSlackInboxWorkspace(t)
			if revoke == "private_grant_partial_inventory" {
				native.convs[0].Kind = "private"
			}
			imp, opts := testImporter(t, native)
			now := tsBase.Add(24 * time.Hour)
			imp.now = func() time.Time { return now }
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
				Assert.NotContains(t, string(raw), "synthetic live exact-thread reply")
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
			service, err := mcpevents.New(t.Context(), st, mcpevents.Options{Enabled: true, Sources: []string{"slack"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, WithDeliveryOperation: daemon.MCPEventsDeliveryOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
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
			require.Contains(resources, "CEVENTS:"+ts(0))
			require.Contains(resources, "COTHER:"+ts(0))
			require.Contains(resources, "CQUIET")
			sourcePage, err := bridge.DiscoverMCPEventResources(t.Context(), mcpevents.ResourceQuery{Kind: "source", SourceID: strconv.FormatInt(sum.SourceID, 10)})
			require.NoError(err)
			require.Len(sourcePage.Resources, 1)
			var conversationID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), sum.SourceID, "CEVENTS").Scan(&conversationID))
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
			client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "synthetic-slack-inbox", Version: "1"}, nil)
			require.NoError(sdkmcp.AddSendingCustomMethod[*nativeSlackSubscribeParams, *nativeSlackSubscribeResult](client, "events/subscribe"))
			session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeSlackOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, session.Close()) })
			subscribe := func(name string, args map[string]any) (*nativeSlackSubscribeResult, error) {
				return sdkmcp.CallCustomMethod[*nativeSlackSubscribeParams, *nativeSlackSubscribeResult](t.Context(), session, "events/subscribe", &nativeSlackSubscribeParams{Name: name, Arguments: args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-slack-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
			}
			for _, binding := range []struct {
				name string
				args map[string]any
			}{
				{"msgvault.message_archived", map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10)}},
				{sourceFamily, map[string]any{"source_id": sourcePage.Resources[0].ID, "source_ref": sourcePage.Resources[0].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["CEVENTS"].ID, "inbox_ref": resources["CEVENTS"].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["CEVENTS:"+ts(0)].ID, "inbox_ref": resources["CEVENTS:"+ts(0)].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["COTHER:"+ts(0)].ID, "inbox_ref": resources["COTHER:"+ts(0)].Reference}},
				{inboxFamily, map[string]any{"inbox_id": resources["CQUIET"].ID, "inbox_ref": resources["CQUIET"].Reference}},
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
			liveTS := strconv.FormatInt(now.Add(time.Minute).Unix(), 10) + ".000100"
			native.convs[0].Msgs[0].Replies = append(native.convs[0].Msgs[0].Replies, fakeMsg{TS: liveTS, ThreadTS: ts(0), User: "USENDER", Text: "synthetic live exact-thread reply"})
			now = now.Add(time.Hour)
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
			assert.ElementsMatch([]string{resources["CEVENTS"].ID, resources["CEVENTS:"+ts(0)].ID}, inboxIDs, "same timestamp in another channel cannot receive this reply")
			// Remove only this channel through the current configuration. A quiet channel
			// remains present; retained parent and exact-thread receipts both retire.
			if revoke == "private_grant_partial_inventory" {
				// The quiet private channel is beyond the failed continuation. A native
				// positive grant loss still retires its parent, child and origin receipts.
				native.convs = append(native.convs[1:], native.convs[0])
				native.scopes = strings.ReplaceAll(native.scopes, "groups:history,", "")
				native.pageSize = 1
				original := native.serve()
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if (r.URL.Path == "/users.conversations" || r.URL.Path == "/conversations.list") && r.FormValue("cursor") != "" {
						w.Header().Set("X-Oauth-Scopes", native.scopes)
						native.replyErr(w, "internal_error")
						return
					}
					original.Config.Handler.ServeHTTP(w, r)
				}))
				t.Cleanup(proxy.Close)
				imp.client.baseURL = proxy.URL
				_, err = imp.Import(t.Context(), opts)
				require.Error(err)
				_, err = bridge.GetMCPEvent(t.Context(), sourceReceipt.EventID)
				require.Error(err, "source-wide receipt retains the denied native conversation origin")
				sourceResult, subscribeErr := subscribe(sourceFamily, map[string]any{"source_id": sourcePage.Resources[0].ID, "source_ref": sourcePage.Resources[0].Reference})
				require.NoError(subscribeErr)
				assert.NotEmpty(sourceResult.ID, "other current channel domains preserve source-wide authority")
			} else {
				opts.ExcludeChannels = []string{"synthetic-events"}
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
			}
			for _, resource := range []mcpevents.Resource{resources["CEVENTS"], resources["CEVENTS:"+ts(0)]} {
				// The SDK closes an HTTP connection after a 400 response. Give each
				// negative admission a fresh transport so a prior rejection cannot
				// masquerade as the next scope's authorization decision.
				negativeSession, connectErr := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: sdkURL, HTTPClient: &http.Client{Transport: nativeSlackOwnerTransport{owner: owner}, Timeout: 30 * time.Second}, DisableStandaloneSSE: true}, nil)
				require.NoError(connectErr)
				before := challenges.Load()
				_, subscribeErr := sdkmcp.CallCustomMethod[*nativeSlackSubscribeParams, *nativeSlackSubscribeResult](t.Context(), negativeSession, "events/subscribe", &nativeSlackSubscribeParams{Name: inboxFamily, Arguments: map[string]any{"inbox_id": resource.ID, "inbox_ref": resource.Reference}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/native-slack-inbox", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}})
				require.Error(subscribeErr)
				assert.NotContains(subscribeErr.Error(), "client is closing")
				require.NoError(negativeSession.Close())
				assert.Equal(before, challenges.Load(), "retired native scope rejects before callback IO")
				envelope := receipts[resource.ID]
				_, readErr := bridge.GetMCPEvent(t.Context(), envelope.EventID)
				require.Error(readErr, "parent exclusion revokes retained receipt authority")
			}
			quietResult, err := subscribe(inboxFamily, map[string]any{"inbox_id": resources["CQUIET"].ID, "inbox_ref": resources["CQUIET"].Reference})
			require.NoError(err)
			assert.NotEmpty(quietResult.ID)
		})
	}
}
