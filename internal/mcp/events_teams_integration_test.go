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
	"go.kenn.io/msgvault/internal/teams"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEventsNativeTeamsImportAndReceiptRead(t *testing.T) {
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

		// Verify the mandatory snapshot through real Store SQL before acknowledging
		// the first callback; optional media readiness is outside this event.
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
		var archivedBody, rawFormat string
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&archivedBody)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&rawFormat)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal("Synthetic Teams content read through a delivery receipt.", archivedBody)
		assert.Equal("teams_json", rawFormat)
		var links int
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), messageID).Scan(&links)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(1, links)
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
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"teams"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
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
	// Only Graph read routes are implemented by the synthetic provider.
	var live atomic.Bool
	const body = "Synthetic Teams content read through a delivery receipt."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"synthetic-chat","chatType":"group","topic":"Synthetic Events"}]}`))
		case "/chats/synthetic-chat/members":
			_, _ = w.Write([]byte(`{"value":[{"id":"owner","userId":"owner","email":"owner@example.test","displayName":"Synthetic Owner"},{"id":"sender","userId":"sender","email":"sender@example.test","displayName":"Synthetic Sender"}]}`))
		case "/me/chats/synthetic-chat/messages":
			message := func(id, when, text, email, name string) map[string]any {
				return map[string]any{"id": id, "createdDateTime": when, "lastModifiedDateTime": when, "from": map[string]any{"user": map[string]any{"id": email, "displayName": name, "userIdentityType": "emailUser"}}, "body": map[string]any{"contentType": "text", "content": text}}
			}
			messages := []any{message("old", "2026-01-01T00:00:00Z", "Synthetic history", "sender@example.test", "Synthetic Sender")}
			if live.Load() {
				arrival := message("live", "2026-01-02T00:00:00Z", body, "sender@example.test", "Synthetic Sender")
				arrival["attachments"] = []map[string]any{{"id": "link", "name": "Synthetic reference", "contentType": "reference", "contentUrl": "https://example.test/reference"}}
				messages = append(messages, arrival, message("own", "2026-01-02T00:00:00Z", "Synthetic own message", "owner@example.test", "Synthetic Owner"))
			}
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"value": messages}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	importer := teams.NewImporter(f.Store, teams.NewClient(provider.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50))
	importOpts := teams.ImportOptions{Email: "owner@example.test"}
	source, err := f.Store.GetOrCreateSource("teams", importOpts.Email)
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(source.ID, importOpts.Email, "manual"))
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)
	sourceID := source.ID
	var initialOccurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&initialOccurrences))
	assert.Zero(initialOccurrences, "initial history is silent")
	var conversationID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, "synthetic-chat").Scan(&conversationID))
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
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "chat:synthetic-chat:live").Scan(&messageID))
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
	var metadata, rawFormat string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT metadata FROM messages WHERE id=?`), messageID).Scan(&metadata))
	var provenance map[string]string
	require.NoError(json.Unmarshal([]byte(metadata), &provenance))
	assert.Equal("synthetic-chat", provenance["teams_chat_id"])
	assert.Equal("live", provenance["teams_message_id"])
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&rawFormat))
	assert.Equal("teams_json", rawFormat)
	select {
	case unexpected := <-received:
		assert.Fail("own message must not be delivered", "%s", unexpected.envelope.EventID)
	default:
	}
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)

	// An identical native chat/message identity in a second account cannot
	// borrow the first source's receipt.
	otherImporter := teams.NewImporter(f.Store, teams.NewClient(provider.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 50))
	otherOpts := importOpts
	otherOpts.Email = "other-owner@example.test"
	_, err = otherImporter.Import(t.Context(), otherOpts)
	require.NoError(err)
	otherSource, err := f.Store.GetOrCreateSource("teams", otherOpts.Email)
	require.NoError(err)
	var otherMessageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), otherSource.ID, "chat:synthetic-chat:live").Scan(&otherMessageID))
	assert.NotEqual(messageID, otherMessageID)
	_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherMessageID, 10), "event_id": envelope.EventID}})
	require.NotNil(wire["error"], "another source cannot borrow a receipt")
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Equal(2, occurrences, "re-import does not duplicate either archived message")
}
