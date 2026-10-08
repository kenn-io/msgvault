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
	"go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestEventsNativeMatrixImportAndReceiptRead(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	const owner = "synthetic-events-owner"
	f := storetest.New(t)
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Engine: engine, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { assert.NoError(daemon.Shutdown(context.Background())) })
	secret := bytes.Repeat([]byte("s"), 32)
	type delivery struct {
		envelope mcpevents.Envelope
		raw      []byte
		headers  http.Header
	}
	received := make(chan delivery, 8)
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

		// Inspect the committed room and reply snapshot before acknowledging.
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if !assert.NoError(json.Unmarshal(envelope.Data, &payload)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		messageID, err := strconv.ParseInt(payload.MessageID, 10, 64)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var archivedBody, rawFormat, title, roomType, metadata string
		var conversationID, parentID int64
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT body_text FROM message_bodies WHERE message_id=?"), messageID).Scan(&archivedBody)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT raw_format FROM message_raw WHERE message_id=?"), messageID).Scan(&rawFormat)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT conversation_id,reply_to_message_id FROM messages WHERE id=?"), messageID).Scan(&conversationID, &parentID)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT title,conversation_type,metadata FROM conversations WHERE id=?"), conversationID).Scan(&title, &roomType, &metadata)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal("Synthetic Matrix content read through a delivery receipt.", archivedBody)
		assert.Equal("matrix_json", rawFormat)
		archivedRaw, err := f.Store.GetMessageRaw(messageID)
		if !assert.NoError(err) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var nativeEvent map[string]any
		assert.NoError(json.Unmarshal(archivedRaw, &nativeEvent))
		assert.Equal("m.room.message", nativeEvent["type"])
		assert.Contains(string(archivedRaw), archivedBody)
		assert.Equal("Synthetic live room", title)
		assert.Equal("direct_chat", roomType)
		var roomMetadata struct {
			MemberCount int `json:"member_count"`
		}
		assert.NoError(json.Unmarshal([]byte(metadata), &roomMetadata))
		assert.Equal(3, roomMetadata.MemberCount)
		assert.Positive(parentID)
		var members int
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?"), conversationID).Scan(&members)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(3, members)

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
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"matrix"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
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
	h := newMCPHTTPServer(ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Events: client, AttachmentReader: client}, HTTPOptions{APIKey: owner}).Handler
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
	// A synthetic homeserver exercises the real mautrix and native import path.
	var round atomic.Int64
	const body = "Synthetic Matrix content read through a delivery receipt."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		current := round.Load()
		message := func(eventID, sender string) map[string]any {
			content := map[string]any{"msgtype": "m.text", "body": body}
			if eventID != "$root" && eventID != "$other" {
				content["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": "$root"}}
			}
			return map[string]any{"type": "m.room.message", "event_id": eventID, "sender": sender, "origin_server_ts": int64(1700000000000), "content": content}
		}
		var response any
		switch {
		case r.URL.Path == "/_matrix/client/v3/sync":
			title := "Synthetic history room"
			events := []any{message("$root", "@member:example.org")}
			if current == 1 {
				title = "Synthetic live room"
				events = []any{message("$arrival", "@member:example.org"), message("$own", "@archive:example.org"),
					map[string]any{"type": "m.room.encrypted", "event_id": "$encrypted", "sender": "@member:example.org", "content": map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "synthetic"}}}
			} else if current > 1 {
				title = "Synthetic live room"
				events = []any{message("$own-later", "@archive:example.org")}
			}
			response = map[string]any{"next_batch": strconv.FormatInt(current, 10), "rooms": map[string]any{"join": map[string]any{
				"!room:example.org":  map[string]any{"state": map[string]any{"events": []any{map[string]any{"type": "m.room.name", "state_key": "", "content": map[string]any{"name": title}}}}, "timeline": map[string]any{"events": events}},
				"!other:example.org": map[string]any{"timeline": map[string]any{"events": []any{message("$other", "@member:example.org")}}},
			}}}
		case strings.HasSuffix(r.URL.Path, "/account_data/m.direct"):
			response = map[string]any{"@member:example.org": []string{"!room:example.org"}}
		case strings.HasSuffix(r.URL.Path, "/joined_members"):
			response = map[string]any{"joined": map[string]any{
				"@archive:example.org": map[string]any{"display_name": "Synthetic Owner"},
				"@member:example.org":  map[string]any{"display_name": "Synthetic Sender"},
				"@other:example.org":   map[string]any{"display_name": "Synthetic Member"},
			}}
		default:
			http.NotFound(w, r)
			return
		}
		assert.NoError(json.NewEncoder(w).Encode(response))
	}))
	t.Cleanup(provider.Close)
	nativeClient, err := mautrix.NewClient(provider.URL, id.UserID("@archive:example.org"), "synthetic-token")
	require.NoError(err)
	importOpts := matrix.ImportOptions{UserID: "@archive:example.org"}
	syncMailbox := func(opts matrix.ImportOptions) {
		t.Helper()
		_, err := f.Store.GetOrCreateSource("matrix", opts.UserID)
		require.NoError(err)
		nativeClient.UserID = id.UserID(opts.UserID)
		_, err = matrix.NewImporter(f.Store, &matrix.Runtime{Client: nativeClient}).Import(t.Context(), opts)
		require.NoError(err)
	}
	syncMailbox(importOpts)
	source, err := f.Store.GetOrCreateSource("matrix", importOpts.UserID)
	require.NoError(err)
	sourceID := source.ID
	var initialOccurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&initialOccurrences))
	assert.Zero(initialOccurrences, "initial history is silent")
	var conversationID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT conversation_id FROM messages WHERE source_id=? AND source_message_id=?`), sourceID, "$root").Scan(&conversationID))
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
	round.Store(1)
	syncMailbox(importOpts)
	var messageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "$arrival").Scan(&messageID))
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
		from := eventsWireArray(t, content["from"])
		require.Len(from, 1)
		assert.Equal("Synthetic Sender", eventsWireObject(t, from[0])["Name"])
		chunk := eventsWireString(t, content["body_text"])
		assert.Equal(body[offset:min(offset+8, len(body))], chunk)
		joined.WriteString(chunk)
	}
	assert.Equal(body, joined.String())
	select {
	case unexpected := <-received:
		assert.Fail("own message must not be delivered", "%s", unexpected.envelope.EventID)
	default:
	}
	syncMailbox(importOpts)
	for _, id := range []string{"$root", "$other"} {
		var otherID int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), sourceID, id).Scan(&otherID))
		_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherID, 10), "event_id": envelope.EventID}})
		require.NotNil(wire["error"], "a receipt cannot grant a parent or another conversation")
	}
	otherOpts := importOpts
	otherOpts.UserID = "@second-owner:example.org"
	syncMailbox(otherOpts)
	otherSource, err := f.Store.GetOrCreateSource("matrix", otherOpts.UserID)
	require.NoError(err)
	var otherID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), otherSource.ID, "$arrival").Scan(&otherID))
	_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherID, 10), "event_id": envelope.EventID}})
	require.NotNil(wire["error"], "another account cannot borrow a receipt")
	bad := task3ModernRequest("tools/call", "get_mcp_event", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_mcp_event","arguments":{"event_id":"`+envelope.EventID+`"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`)
	bad.Header.Set("Authorization", "Bearer synthetic-other-owner")
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, bad)
	assert.Equal(http.StatusUnauthorized, denied.Code)

	// A second explicit subscription can opt into subsequent own arrivals.
	req.Arguments["include_from_me"] = true
	_, wire = call("events/subscribe", req)
	require.Nil(wire["error"])
	ownSubscriptionID := eventsWireString(t, eventsWireObject(t, wire["result"])["id"])
	round.Store(2)
	syncMailbox(importOpts)
	select {
	case own := <-received:
		assert.Equal(ownSubscriptionID, own.headers.Get("X-Mcp-Subscription-Id"))
		var ownPayload map[string]any
		require.NoError(json.Unmarshal(own.envelope.Data, &ownPayload))
		assert.Equal(true, ownPayload["from_me"])
	case <-time.After(30 * time.Second):
		require.FailNow("explicit own-message subscription was not delivered")
	}
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Equal(3, occurrences, "re-import and a second account's history do not duplicate live arrivals")
}
