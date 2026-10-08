package mcp

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

	"github.com/google/jsonschema-go/jsonschema"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEventsNativeSlackImportAndReceiptRead(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	const owner = "synthetic-events-owner"
	f := storetest.New(t)
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}, Store: f.Store, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(daemon.Shutdown(context.Background())) })
	secret := bytes.Repeat([]byte("s"), 32)
	type delivery struct {
		envelope mcpevents.Envelope
		raw      []byte
		headers  http.Header
	}
	received := make(chan delivery, 4)
	var retryOnce atomic.Bool
	retryOnce.Store(true)
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id, timestamp := r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp")
		if !assert.NotEmpty(id) || !assert.NotEmpty(r.Header.Get("X-Mcp-Subscription-Id")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seconds, err := strconv.ParseInt(timestamp, 10, 64)
		if !assert.NoError(err) || !assert.Positive(seconds) || !assert.Equal(strconv.FormatInt(seconds, 10), timestamp) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// This receiver implements the Standard Webhooks wire contract itself.
		// It never invokes the sender's signing helper or re-encodes signed JSON.
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write([]byte(id + "." + timestamp + "."))
		_, _ = mac.Write(raw)
		verified := false
		for signature := range strings.FieldsSeq(r.Header.Get("Webhook-Signature")) {
			if !strings.HasPrefix(signature, "v1,") {
				continue
			}
			value, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(signature, "v1,"))
			verified = verified || err == nil && hmac.Equal(mac.Sum(nil), value)
		}
		if !assert.True(verified, "callback bytes failed independent Standard Webhooks verification") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var data map[string]json.RawMessage
		if !assert.NoError(json.Unmarshal(raw, &data)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if challenge, ok := data["challenge"]; ok {
			var nonce string
			if !assert.NoError(json.Unmarshal(challenge, &nonce)) || !assert.Equal("msg_verification_"+nonce, id) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]json.RawMessage{"challenge": challenge})
			return
		}
		var envelope mcpevents.Envelope
		if !assert.NoError(json.Unmarshal(raw, &envelope)) || !assert.Equal(envelope.EventID, id) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case received <- delivery{envelope: envelope, raw: raw, headers: r.Header.Clone()}:
		default:
		}
		if retryOnce.Swap(false) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"slack"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}})
	require.NoError(err)
	runCtx, cancel := context.WithCancel(t.Context())
	runDone := make(chan error, 1)
	go func() { runDone <- svc.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runDone:
			assert.NoError(err)
		case <-time.After(30 * time.Second):
			assert.Fail("Events service did not join its workers")
		}
	})
	daemon.SetMCPEvents(svc)
	endpoint := httptest.NewServer(daemon.Router())
	t.Cleanup(endpoint.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: owner, AllowInsecure: true})
	require.NoError(err)
	catalog, err := client.MCPEventsList(t.Context())
	require.NoError(err)
	require.NotEmpty(catalog.Events)
	h := newMCPHTTPServer(ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Events: client}, HTTPOptions{APIKey: owner}).Handler
	call := func(method string, params any) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		data, err := json.Marshal(params)
		require.NoError(err)
		var fields map[string]any
		require.NoError(json.Unmarshal(data, &fields))
		fields["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
		data, err = json.Marshal(fields)
		require.NoError(err)
		name := ""
		if method == "tools/call" {
			name = eventsWireString(t, fields["name"])
		}
		req := task3ModernRequest(method, name, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":`+string(data)+`}`)
		req.Header.Set("Authorization", "Bearer "+owner)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var wire map[string]any
		decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		decoder.UseNumber()
		require.NoError(decoder.Decode(&wire), rec.Body.String())
		return rec, wire
	}
	rec, wire := call("server/discover", map[string]any{})
	require.Nil(wire["error"], rec.Body.String())
	assert.Equal("no-store", rec.Header().Get("Cache-Control"))
	discovery := eventsWireObject(t, wire["result"])
	assert.Contains(discovery["capabilities"], "events")
	assert.Equal("public", discovery["cacheScope"])

	_, wire = call("events/list", map[string]any{})
	require.Nil(wire["error"])
	entries := eventsWireArray(t, eventsWireObject(t, wire["result"])["events"])
	var messageDefinition map[string]any
	for _, entry := range entries {
		definition := eventsWireObject(t, entry)
		if definition["name"] == "msgvault.message_archived" {
			messageDefinition = definition
		}
	}
	require.NotNil(messageDefinition)
	assert.Equal([]any{"message"}, eventsWireObject(t, eventsWireObject(t, eventsWireObject(t, messageDefinition["payloadSchema"])["properties"])["kind"])["enum"])
	assert.Equal([]any{"webhook"}, messageDefinition["delivery"])
	assert.Equal(false, eventsWireObject(t, messageDefinition["inputSchema"])["additionalProperties"])
	// Exercise native Slack HTTP decoding and checkpoints. Only allowlisted
	// read methods are implemented; sending is outside this provider's scope.
	var live atomic.Bool
	const body = "Synthetic Slack content for @Synthetic Owner read through a delivery receipt."
	historyTime := time.Now().UTC().Add(-48 * time.Hour)
	liveTime := time.Now().UTC()
	ts := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) + ".000100" }
	rootTS, replyTS := ts(historyTime), ts(liveTime)
	ownTS := strconv.FormatInt(liveTime.Unix(), 10) + ".000200"
	root := func() map[string]any {
		return map[string]any{"type": "message", "ts": rootTS, "user": "USENDER", "text": "Synthetic history", "thread_ts": rootTS, "reply_count": 0}
	}
	child := func() map[string]any {
		return map[string]any{"type": "message", "ts": replyTS, "thread_ts": rootTS, "user": "USENDER", "text": "Synthetic Slack content for <@UOWNER> read through a delivery receipt."}
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Oauth-Scopes", "channels:read,channels:history,users:read,users:read.email,search:read")
		output := map[string]any{"ok": true, "response_metadata": map[string]any{"next_cursor": ""}}
		switch r.URL.Path {
		case "/users.list":
			output["members"] = []any{
				map[string]any{"id": "UOWNER", "name": "owner", "real_name": "Synthetic Owner", "tz": "UTC", "tz_offset": 0, "profile": map[string]any{"email": "owner@example.test"}},
				map[string]any{"id": "USENDER", "name": "sender", "real_name": "Synthetic Sender", "profile": map[string]any{"email": "sender@example.test"}},
			}
		case "/users.conversations", "/conversations.list":
			output["channels"] = []any{map[string]any{"id": "CSYNTHETIC", "name": "synthetic-events", "is_channel": true, "is_member": true}}
		case "/conversations.members":
			output["members"] = []string{"UOWNER", "USENDER"}
		case "/conversations.history":
			items := []any{}
			oldest, latest := r.FormValue("oldest"), r.FormValue("latest")
			visible := func(stamp string) bool { return (oldest == "" || stamp >= oldest) && (latest == "" || stamp <= latest) }
			if live.Load() && visible(ownTS) {
				items = append(items, map[string]any{"type": "message", "ts": ownTS, "user": "UOWNER", "text": "Synthetic own message"})
			}
			if visible(rootTS) {
				items = append(items, root())
			}
			output["messages"] = items
			output["has_more"] = false
		case "/search.messages":
			hits := []any{}
			if live.Load() && strings.Contains(r.FormValue("query"), "on:"+liveTime.Format("2006-01-02")) {
				hits = append(hits, map[string]any{"ts": replyTS, "channel": map[string]any{"id": "CSYNTHETIC"}, "permalink": "https://example.slack.com/archives/CSYNTHETIC/p" + strings.ReplaceAll(replyTS, ".", "") + "?thread_ts=" + rootTS + "&cid=CSYNTHETIC"})
			}
			output["messages"] = map[string]any{"matches": hits, "pagination": map[string]any{"total_count": len(hits), "page_count": 1, "page": 1, "per_page": 100}, "paging": map[string]any{"total": len(hits), "pages": 1, "page": 1, "count": 100}}
		case "/conversations.replies":
			items := []any{root()}
			if live.Load() && (r.FormValue("oldest") == "" || replyTS > r.FormValue("oldest")) {
				items = append(items, child())
			}
			output["messages"] = items
			output["has_more"] = false
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.NoError(json.NewEncoder(w).Encode(output))
	}))
	t.Cleanup(provider.Close)
	importer := slack.NewImporter(f.Store, slack.NewClient(provider.URL, "synthetic-token"), "TSYNTHETIC")
	importOpts := slack.ImportOptions{TeamID: "TSYNTHETIC", UserID: "UOWNER", NoMedia: true}
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)
	source, err := f.Store.GetOrCreateSource("slack", "TSYNTHETIC:UOWNER")
	require.NoError(err)
	sourceID := source.ID
	var initialOccurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&initialOccurrences))
	assert.Zero(initialOccurrences, "initial history is silent")
	var conversationID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, "CSYNTHETIC").Scan(&conversationID))
	req := mcpevents.SubscribeRequest{Name: "msgvault.message_archived", Arguments: map[string]any{"conversation_id": strconv.FormatInt(conversationID, 10)}, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}}

	forged, err := json.Marshal(req)
	require.NoError(err)
	var forgedRequest map[string]any
	require.NoError(json.Unmarshal(forged, &forgedRequest))
	forgedRequest["principal"] = "owner:forged"
	_, wire = call("events/subscribe", forgedRequest)
	require.NotNil(wire["error"])
	assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
	subscriptions, err := f.Store.ListMCPSubscriptions(t.Context(), mcpevents.Principal(owner))
	require.NoError(err)
	assert.Empty(subscriptions)
	_, wire = call("events/subscribe", req)
	require.Nil(wire["error"])
	subscriptionID := eventsWireString(t, eventsWireObject(t, wire["result"])["id"])
	require.NotEmpty(subscriptionID)
	live.Store(true)
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)
	var messageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "CSYNTHETIC:"+replyTS).Scan(&messageID))
	var delivered delivery
	select {
	case delivered = <-received:
	case <-time.After(30 * time.Second):
		require.FailNow("committed live occurrence was not delivered")
	}
	var retried delivery
	select {
	case retried = <-received:
	case <-time.After(30 * time.Second):
		require.FailNow("retry was not delivered")
	}
	assert.Equal(delivered.envelope.EventID, retried.envelope.EventID)
	assert.Equal(delivered.raw, retried.raw, "retry preserves the signed occurrence payload")
	envelope := delivered.envelope
	require.NotEmpty(envelope.EventID)
	assert.Equal(subscriptionID, delivered.headers.Get("X-Mcp-Subscription-Id"))
	assert.Equal(envelope.EventID, delivered.headers.Get("Webhook-Id"))
	assert.NotContains(string(delivered.raw), body)
	encodedSchema, err := json.Marshal(messageDefinition["payloadSchema"])
	require.NoError(err)
	var payloadSchema jsonschema.Schema
	require.NoError(json.Unmarshal(encodedSchema, &payloadSchema))
	resolvedPayload, err := payloadSchema.Resolve(nil)
	require.NoError(err)
	var payload map[string]any
	require.NoError(json.Unmarshal(envelope.Data, &payload))
	assert.Equal(strconv.FormatInt(messageID, 10), payload["message_id"])
	require.NoError(resolvedPayload.Validate(payload), "actual occurrence must match its advertised schema")
	assert.NotContains(string(envelope.Data), body)
	require.Eventually(func() bool {
		row, err := f.Store.GetMCPSubscription(t.Context(), subscriptionID)
		var head int64
		if queryErr := f.Store.DB().QueryRow(`SELECT head_seq FROM mcp_event_clock WHERE singleton=1`).Scan(&head); queryErr != nil {
			return false
		}
		return err == nil && row != nil && row.CursorSeq == head && row.PendingSeq == 0
	}, 30*time.Second, 20*time.Millisecond, "delivery success was not durably recorded")
	_, wire = call("tools/call", map[string]any{"name": "get_mcp_event", "arguments": map[string]any{"event_id": envelope.EventID}})
	require.Nil(wire["error"])
	assert.Equal(envelope.EventID, eventsWireObject(t, eventsWireObject(t, wire["result"])["structuredContent"])["eventId"])
	var joined strings.Builder
	for offset := 0; offset < len(body); offset += 8 {
		_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(messageID, 10), "event_id": envelope.EventID, "offset": offset, "max_chars": 8}})
		require.Nil(wire["error"])
		result := eventsWireObject(t, wire["result"])
		require.NotEqual(true, result["isError"], "%v", result["content"])
		content := eventsWireObject(t, result["structuredContent"])
		assert.Equal(json.Number(strconv.FormatInt(sourceID, 10)), content["source_id"])
		assert.Equal(false, content["is_from_me"])
		assert.Equal([]any{map[string]any{"Email": "sender@example.test", "Name": "Synthetic Sender"}}, content["from"])
		chunk := eventsWireString(t, content["body_text"])
		assert.Equal(body[offset:min(offset+8, len(body))], chunk)
		joined.WriteString(chunk)
	}
	assert.Equal(body, joined.String())
	// At first delivery, the archived native snapshot also has its thread,
	// provenance and mention rows; the receipt continues to authorize only
	// this message through the existing content reader.
	var parentID int64
	var metadata string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id, metadata FROM messages WHERE id=?`), messageID).Scan(&parentID, &metadata))
	var nativeParent string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT source_message_id FROM messages WHERE id=?`), parentID).Scan(&nativeParent))
	assert.Equal("CSYNTHETIC:"+rootTS, nativeParent)
	var provenance map[string]string
	require.NoError(json.Unmarshal([]byte(metadata), &provenance))
	assert.Equal("TSYNTHETIC", provenance["slack_team_id"])
	assert.Equal(rootTS, provenance["slack_thread_ts"])
	var mentions int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type='mention'`), messageID).Scan(&mentions))
	assert.Equal(1, mentions)
	select {
	case unexpected := <-received:
		assert.Fail("own message must not be delivered", "%s", unexpected.envelope.EventID)
	default:
	}
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)

	// The same native conversation/message IDs in a different workspace cannot
	// be read through the first source's receipt.
	otherImporter := slack.NewImporter(f.Store, slack.NewClient(provider.URL, "synthetic-token"), "TOTHER")
	otherOpts := importOpts
	otherOpts.TeamID = "TOTHER"
	_, err = otherImporter.Import(t.Context(), otherOpts)
	require.NoError(err)
	otherSource, err := f.Store.GetOrCreateSource("slack", "TOTHER:UOWNER")
	require.NoError(err)
	var otherMessageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), otherSource.ID, "CSYNTHETIC:"+replyTS).Scan(&otherMessageID))
	assert.NotEqual(messageID, otherMessageID)
	_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherMessageID, 10), "event_id": envelope.EventID}})
	require.NotNil(wire["error"], "another source cannot borrow a receipt")
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Equal(2, occurrences, "re-import does not duplicate either archived message")
}
