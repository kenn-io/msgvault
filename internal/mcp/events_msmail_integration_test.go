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
	"go.kenn.io/msgvault/internal/msmail"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEventsNativeMSMailImportAndReceiptRead(t *testing.T) {
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

		// Query the committed native MIME snapshot before acknowledging delivery.
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
		var archivedBody, rawFormat, metadata string
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageID).Scan(&archivedBody)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&rawFormat)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT metadata FROM messages WHERE id=?`), messageID).Scan(&metadata)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal("Synthetic Microsoft mail content read through a delivery receipt.", archivedBody)
		assert.Equal("mime", rawFormat)
		assert.Contains(metadata, "root@example.com")
		for _, table := range []string{"attachments", "message_recipients", "message_labels"} {
			var count int
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM `+table+` WHERE message_id=?`), messageID).Scan(&count)) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			assert.Positive(count)
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
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"msmail"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
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
	// Native Graph transport uses opaque delta links and immutable message IDs.
	var round atomic.Int64
	const body = "Synthetic Microsoft mail content read through a delivery receipt."
	mimeResponses := make(map[string]string)
	for _, id := range []string{"root", "other", "arrival", "own", "own-later"} {
		from := "Synthetic Sender <sender@example.com>"
		if strings.HasPrefix(id, "own") {
			from = "Synthetic Owner <owner@example.com>"
		}
		reply := ""
		if id != "root" && id != "other" {
			reply = "References: <root@example.com>\r\nIn-Reply-To: <root@example.com>\r\n"
		}
		raw := "From: " + from + "\r\nTo: owner@example.com\r\nSubject: Synthetic mail\r\nMessage-ID: <" + id + "@example.com>\r\nDate: Mon, 1 Jan 2024 10:00:00 +0000\r\n" + reply +
			"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=synthetic\r\n\r\n--synthetic\r\nContent-Type: text/plain\r\n\r\n" + body +
			"\r\n--synthetic\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=synthetic.bin\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\n--synthetic--\r\n"
		mimeResponses["/me/messages/"+id+"/$value"] = raw
	}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		assert.Equal("Bearer synthetic-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/mailFolders":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "inbox", "displayName": "Inbox"}}}))
		case r.URL.Path == "/me/mailFolders/inbox/messages/delta":
			current := round.Load()
			previous := int64(-1)
			if token := r.URL.Query().Get("token"); token != "" {
				previous, _ = strconv.ParseInt(token, 10, 64)
			}
			var ids []string
			if previous < 0 {
				ids = append(ids, "root", "other")
			}
			if previous < 1 && current >= 1 {
				ids = append(ids, "arrival", "own")
			}
			if previous < 2 && current >= 2 {
				ids = append(ids, "own-later")
			}
			rows := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				rows = append(rows, map[string]any{"id": id, "receivedDateTime": "2024-01-01T10:00:00Z"})
			}
			link := "http://" + r.Host + "/me/mailFolders/inbox/messages/delta?token=" + strconv.FormatInt(current, 10)
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"value": rows, "@odata.deltaLink": link}))
		case strings.HasPrefix(r.URL.Path, "/me/mailFolders/"):
			if r.URL.Path == "/me/mailFolders/inbox" {
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": "inbox"}))
				return
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/me/messages/") && strings.HasSuffix(r.URL.Path, "/$value"):
			for path, raw := range mimeResponses {
				if r.URL.Path != path {
					continue
				}
				w.Header().Set("Content-Type", "message/rfc822")
				_, err := io.WriteString(w, raw)
				assert.NoError(err)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	nativeClient := msmail.NewClient(provider.URL, func(context.Context) (string, error) { return "synthetic-token", nil }, 1000)
	importOpts := msmail.Options{Email: "owner@example.com", AttachmentsDir: cfg.AttachmentsDir()}
	syncMailbox := func(opts msmail.Options) {
		t.Helper()
		_, err := msmail.Import(t.Context(), f.Store, nativeClient, opts, slog.New(slog.DiscardHandler))
		require.NoError(err)
	}
	syncMailbox(importOpts)
	source, err := f.Store.GetOrCreateSource("msmail", importOpts.Email)
	require.NoError(err)
	sourceID := source.ID
	var initialOccurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&initialOccurrences))
	assert.Zero(initialOccurrences, "initial history is silent")
	var conversationID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT conversation_id FROM messages WHERE source_id=? AND source_message_id=?`), sourceID, "root").Scan(&conversationID))
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
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "arrival").Scan(&messageID))
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
	var attachmentID json.Number
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
		attachments := eventsWireArray(t, content["attachments"])
		require.Len(attachments, 1)
		var validID bool
		attachmentID, validID = eventsWireObject(t, attachments[0])["ID"].(json.Number)
		require.True(validID)
		chunk := eventsWireString(t, content["body_text"])
		assert.Equal(body[offset:min(offset+8, len(body))], chunk)
		joined.WriteString(chunk)
	}
	assert.Equal(body, joined.String())
	// Attachment bytes use the existing owner-authorized reader, with the ID
	// obtained from the receipt-bound message snapshot. No event_id is accepted.
	var attachment strings.Builder
	digest := ""
	for offset := 0; offset < 5; offset += 2 {
		args := map[string]any{"attachment_id": attachmentID, "offset": offset, "length": 2}
		if offset > 0 {
			args["sha256"] = digest
		}
		_, wire = call("tools/call", map[string]any{"name": "get_attachment", "arguments": args})
		require.Nil(wire["error"])
		result := eventsWireObject(t, wire["result"])
		require.NotEqual(true, result["isError"], "%v", result["content"])
		content := eventsWireObject(t, result["structuredContent"])
		assert.Equal("synthetic.bin", content["filename"])
		digest = eventsWireString(t, content["sha256"])
		chunk, err := base64.StdEncoding.DecodeString(eventsWireString(t, content["data_base64"]))
		require.NoError(err)
		attachment.Write(chunk)
	}
	assert.Equal("hello", attachment.String())
	select {
	case unexpected := <-received:
		assert.Fail("own message must not be delivered", "%s", unexpected.envelope.EventID)
	default:
	}
	syncMailbox(importOpts)
	for _, id := range []string{"root", "other"} {
		var otherID int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), sourceID, id).Scan(&otherID))
		_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherID, 10), "event_id": envelope.EventID}})
		require.NotNil(wire["error"], "a receipt cannot grant a parent or another conversation")
	}
	otherOpts := importOpts
	otherOpts.Email = "other-owner@example.com"
	syncMailbox(otherOpts)
	otherSource, err := f.Store.GetOrCreateSource("msmail", otherOpts.Email)
	require.NoError(err)
	var otherID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), otherSource.ID, "arrival").Scan(&otherID))
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
