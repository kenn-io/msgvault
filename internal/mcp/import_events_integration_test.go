package mcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
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
	"go.kenn.io/msgvault/internal/fbmessenger"
	"go.kenn.io/msgvault/internal/gvoice"
	"go.kenn.io/msgvault/internal/imazingcsv"
	"go.kenn.io/msgvault/internal/imessage"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/whatsapp"
)

// The HTTP daemon's meeting capability normally comes from its CLI coordinator.
// This bridge delegates directly to the real native importer over the same Store.
// It replaces no parser, validator, authority check, SQL or delivery operation.
type eventsNativeMeetingImportStore struct {
	*store.Store

	result meetingimport.Result
	err    error
	gate   *api.SerialOperationGate
	held   bool
}

func (s *eventsNativeMeetingImportStore) ImportMeeting(ctx context.Context, req meetingimport.Request) (meetingimport.Result, error) {
	_, _, s.held = s.gate.Holder()
	result, err := meetingimport.NewImporter(s.Store, meetingimport.Hooks{}).Import(ctx, req)
	s.result = result
	s.err = err
	return result, err
}

// This fixture replaces only DNS and the dial destination. Requests still pass
// destination validation, real TLS hostname verification, signing, the real
// daemon router and its credential checks, and Store occurrence persistence.
func TestImportEventsNativeParserSignedCallbackAndReceiptPagination(t *testing.T) {
	for _, format := range []string{"eml", "maildir", "mbox", "apple-mail", "pst_text", "pst_media", "imazing_csv", "synctech_sms_sms", "synctech_sms_mms", "synctech_sms_call", "google_voice_text", "google_voice_group", "google_voice_received", "google_voice_placed", "google_voice_missed", "google_voice_voicemail", "apple_messages_sms", "apple_messages_imessage_group", "apple_messages_outgoing", "apple_messages_attributed", "apple_messages_empty", "apple_messages_attachment_only", "facebook_messenger_json", "facebook_messenger_html", "facebook_messenger_e2ee", "facebook_messenger_outgoing", "facebook_messenger_empty", "facebook_messenger_json_media", "facebook_messenger_e2ee_media", "whatsapp_android_text", "whatsapp_android_group", "whatsapp_android_outgoing", "whatsapp_android_empty", "whatsapp_android_media", "whatsapp_android_reply_reaction", "whatsapp_apple_text", "whatsapp_apple_group", "whatsapp_apple_outgoing", "whatsapp_apple_type7", "whatsapp_apple_lid", "slackdump_channel", "slackdump_direct", "slackdump_outgoing", "slackdump_bot", "slackdump_empty", "slackdump_media", "slackdump_reply_reaction", "meeting_import_summary", "meeting_import_transcript", "meeting_import_segments", "meeting_import_outgoing", "meeting_import_actions", "meeting_import_anchor_repair"} {
		t.Run(format, func(t *testing.T) {
			for _, exactSource := range []bool{false, true} {
				name := "archive"
				if exactSource {
					name = "exact_source"
				}
				t.Run(name, func(t *testing.T) {
					testImportEventsNativeParserSignedCallbackAndReceiptPagination(t, exactSource, format)
				})
			}
		})
	}
}

func testImportEventsNativeParserSignedCallbackAndReceiptPagination(t *testing.T, exactSource bool, format string) {
	t.Helper()
	fixtureFormat := format
	if strings.HasPrefix(format, "pst_") {
		format = "pst"
	}
	if strings.HasPrefix(format, "synctech_sms_") {
		format = synctechsms.SourceType
	}
	if strings.HasPrefix(format, "google_voice_") {
		format = "google_voice"
	}
	if strings.HasPrefix(format, "apple_messages_") {
		format = "apple_messages"
	}
	if strings.HasPrefix(format, "facebook_messenger_") {
		format = "facebook_messenger"
	}
	if strings.HasPrefix(format, "whatsapp_") {
		format = "whatsapp"
	}
	if strings.HasPrefix(format, "slackdump_") {
		format = "slackdump"
	}
	if strings.HasPrefix(format, "meeting_import_") {
		format = "meeting_import"
	}
	selectedAttachment := fixtureFormat == "pst_media" || fixtureFormat == "slackdump_media" || fixtureFormat == "whatsapp_android_media" || (format == "facebook_messenger" && strings.HasSuffix(fixtureFormat, "_media")) || fixtureFormat == "google_voice_voicemail" || format == "apple-mail" || format == "imazing_csv" || fixtureFormat == "synctech_sms_mms"

	assert := Assert.New(t)
	require := Require.New(t)
	const owner = "synthetic-events-owner"
	f := storetest.New(t)
	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	gate := api.NewSerialOperationGate()
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: owner}}
	meetingStore := &eventsNativeMeetingImportStore{Store: f.Store, gate: gate}
	var daemonStore api.MessageStore = f.Store
	if format == "meeting_import" {
		daemonStore = meetingStore
	}
	daemon := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemonStore, Engine: engine, Logger: slog.New(slog.DiscardHandler), OperationGate: gate})
	t.Cleanup(func() { assert.NoError(daemon.Shutdown(context.Background())) })
	secret := bytes.Repeat([]byte("s"), 32)
	type delivery struct {
		envelope mcpevents.Envelope
		raw      []byte
		headers  http.Header
	}
	received := make(chan delivery, 4)
	var callbackDials atomic.Int64
	var readyParentID atomic.Int64
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
		if format == "pst" {
			var occurrence struct {
				MessageID string `json:"message_id"`
			}
			if !assert.NoError(json.Unmarshal(envelope.Data, &occurrence)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, projection := range []string{"message_bodies", "message_raw"} {
				var count int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM "+projection+" WHERE message_id=?"), occurrence.MessageID).Scan(&count)) || !assert.Equal(1, count) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			if selectedAttachment {
				var count int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND COALESCE(attachment_state,'') IN ('','stored') AND storage_path<>'' AND content_hash<>''`), occurrence.MessageID).Scan(&count)) || !assert.Equal(1, count, "selected PST bytes and refs must be ready at callback") {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		if format == "imazing_csv" {
			var occurrence struct {
				MessageID string `json:"message_id"`
			}
			if !assert.NoError(json.Unmarshal(envelope.Data, &occurrence)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var linked int64
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), occurrence.MessageID).Scan(&linked)) || !assert.Equal(readyParentID.Load(), linked, "reply must already be canonical when callback arrives") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var references int
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND attachment_state='stored' AND storage_path<>'' AND content_hash<>''`), occurrence.MessageID).Scan(&references)) || !assert.Equal(1, references, "selected references must already be ready at delivery") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		if format == "whatsapp" {
			var occurrence struct {
				MessageID int64 `json:"message_id,string"`
			}
			if !assert.NoError(json.Unmarshal(envelope.Data, &occurrence)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var bodies, raw, members int
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`), occurrence.MessageID).Scan(&bodies)) || !assert.Equal(1, bodies) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM message_raw WHERE message_id=?`), occurrence.MessageID).Scan(&raw)) || !assert.Equal(1, raw) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=(SELECT conversation_id FROM messages WHERE id=?)`), occurrence.MessageID).Scan(&members)) || !assert.GreaterOrEqual(members, 2) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if fixtureFormat == "whatsapp_android_reply_reaction" {
				var parent int64
				var reactions int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), occurrence.MessageID).Scan(&parent)) || !assert.Equal(readyParentID.Load(), parent) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id=?`), occurrence.MessageID).Scan(&reactions)) || !assert.Equal(1, reactions) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			if selectedAttachment {
				var refs int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND attachment_state='stored' AND storage_path<>'' AND content_hash<>'' AND width=128 AND height=64 AND duration_ms=7000`), occurrence.MessageID).Scan(&refs)) || !assert.Equal(1, refs) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		if format == "meeting_import" {
			var occurrence struct {
				MessageID int64 `json:"message_id,string"`
			}
			if !assert.NoError(json.Unmarshal(envelope.Data, &occurrence)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, sql := range []string{
				`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`,
				`SELECT COUNT(*) FROM message_raw WHERE message_id=? AND raw_format='meeting_json'`,
				`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=(SELECT conversation_id FROM messages WHERE id=?)`,
			} {
				var n int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(sql), occurrence.MessageID).Scan(&n)) || !assert.Equal(1, n) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			var recipients int
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type IN ('from','to')`), occurrence.MessageID).Scan(&recipients)) || !assert.Equal(2, recipients) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if fixtureFormat == "meeting_import_anchor_repair" {
				var observations int
				if !assert.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM participant_contact_observations`).Scan(&observations)) || !assert.Zero(observations, "the ready receipt does not require later person grouping") {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		if format == "slackdump" {
			var occurrence struct {
				MessageID int64 `json:"message_id,string"`
			}
			if !assert.NoError(json.Unmarshal(envelope.Data, &occurrence)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for _, query := range []string{
				`SELECT COUNT(*) FROM message_bodies WHERE message_id=?`,
				`SELECT COUNT(*) FROM message_raw WHERE message_id=? AND raw_format='slack_json'`,
			} {
				var n int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(query), occurrence.MessageID).Scan(&n)) || !assert.Equal(1, n) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			var members int
			if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=(SELECT conversation_id FROM messages WHERE id=?)`), occurrence.MessageID).Scan(&members)) || !assert.Equal(2, members) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if fixtureFormat == "slackdump_reply_reaction" {
				var parent int64
				var reactions int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), occurrence.MessageID).Scan(&parent)) || !assert.Equal(readyParentID.Load(), parent) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id=?`), occurrence.MessageID).Scan(&reactions)) || !assert.Equal(1, reactions) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			if selectedAttachment {
				var refs int
				if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_attachment_id='slack:F_EXAMPLE' AND attachment_state='stored' AND storage_path<>'' AND content_hash<>''`), occurrence.MessageID).Scan(&refs)) || !assert.Equal(1, refs) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		select {
		case received <- delivery{envelope: envelope, raw: raw, headers: r.Header.Clone()}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, ImportNotifications: true, Sources: []string{"gmail", "imap", "gcal"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, WithDeliveryOperation: daemon.MCPEventsDeliveryOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	}, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		callbackDials.Add(1)
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
		if definition["name"] == "msgvault.import_message_archived" {
			messageDefinition = definition
		}
	}
	require.NotNil(messageDefinition)
	assert.Equal([]any{"webhook"}, messageDefinition["delivery"])
	assert.Equal(false, eventsWireObject(t, messageDefinition["inputSchema"])["additionalProperties"])
	importIdentifier := "synthetic-import@example.net"
	if format == "pst" {
		importIdentifier = "owner@example.net"
	}
	if format == "meeting_import" {
		importIdentifier = "synthetic-meetings"
	}
	if format == "slackdump" {
		importIdentifier = "T_EXAMPLE:U_OWNER"
	}
	if format == "facebook_messenger" {
		importIdentifier = "synthetic.owner@facebook.messenger"
	}
	if format == "apple_messages" {
		importIdentifier = "synthetic-device"
	}
	if format == synctechsms.SourceType || format == "google_voice" || format == "whatsapp" {
		importIdentifier = "+15550000001"
	}
	args := map[string]any{}
	if fixtureFormat == "meeting_import_outgoing" || fixtureFormat == "slackdump_outgoing" || fixtureFormat == "google_voice_placed" || fixtureFormat == "apple_messages_outgoing" || (fixtureFormat == "facebook_messenger_outgoing" || fixtureFormat == "whatsapp_android_outgoing" || fixtureFormat == "whatsapp_apple_outgoing") {
		args["include_from_me"] = true
	}
	if exactSource {
		source, err := f.Store.GetOrCreateSource(format, importIdentifier)
		require.NoError(err)
		_, wire = call("tools/call", map[string]any{"name": "list_mcp_event_resources", "arguments": map[string]any{"kind": "source", "source_id": strconv.FormatInt(source.ID, 10)}})
		require.Nil(wire["error"])
		toolResult := eventsWireObject(t, wire["result"])
		require.NotEqual(true, toolResult["isError"])
		resources := eventsWireArray(t, eventsWireObject(t, toolResult["structuredContent"])["resources"])
		require.Len(resources, 1)
		resource := eventsWireObject(t, resources[0])
		assert.Equal(strconv.FormatInt(source.ID, 10), resource["id"])
		args["source_id"] = strconv.FormatInt(source.ID, 10)
		args["source_ref"] = eventsWireString(t, resource["reference"])
	}
	req := mcpevents.SubscribeRequest{Name: "msgvault.import_message_archived", Arguments: args, Delivery: mcpevents.Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_" + base64.StdEncoding.EncodeToString(secret)}}
	schemaBytes, err := json.Marshal(messageDefinition["inputSchema"])
	require.NoError(err)
	var argumentSchema jsonschema.Schema
	require.NoError(json.Unmarshal(schemaBytes, &argumentSchema))
	resolvedArguments, err := argumentSchema.Resolve(nil)
	require.NoError(err)
	require.NoError(resolvedArguments.Validate(req.Arguments))
	staleReference := "mcp1." + strings.Repeat("A", 43)
	for _, negative := range []struct {
		arguments    map[string]any
		schemaReject bool
	}{
		{map[string]any{"source_ref": staleReference}, true},
		{map[string]any{"source_id": "*", "source_ref": staleReference}, true},
		{map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10)}, true},
		{map[string]any{"source_id": strconv.FormatInt(f.Source.ID, 10), "source_ref": staleReference}, false},
		{map[string]any{"include_reactions": true}, true},
	} {
		if negative.schemaReject {
			require.Error(resolvedArguments.Validate(negative.arguments))
		}
		invalidRequest := req
		invalidRequest.Arguments = negative.arguments
		before := callbackDials.Load()
		_, rejected := call("events/subscribe", invalidRequest)
		require.NotNil(rejected["error"])
		assert.Equal(json.Number("-32602"), eventsWireObject(t, rejected["error"])["code"])
		assert.Equal(before, callbackDials.Load(), "invalid import scope must fail before callback I/O")
	}

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
	body := "Synthetic body split across receipt-bound pages."
	if fixtureFormat == "synctech_sms_call" {
		body = "Call incoming, 42 seconds"
	}
	switch fixtureFormat {
	case "slackdump_empty", "apple_messages_empty", "apple_messages_attachment_only", "facebook_messenger_empty", "whatsapp_android_empty":
		body = ""
	case "google_voice_received":
		body = "Received call from Synthetic Sender (42s)"
	case "google_voice_placed":
		body = "Placed call to Synthetic Sender (42s)"
	case "google_voice_missed":
		body = "Missed call from Synthetic Sender (42s)"
	case "google_voice_voicemail":
		body = "Voicemail from Synthetic Sender (42s)"
	}
	releaseScheduledSync, held := gate.BeginLabeledWorkContext(t.Context(), "scheduled sync")
	require.True(held)
	t.Cleanup(releaseScheduledSync)
	raw := "From: sender@example.net\r\nTo: " + importIdentifier + "\r\nMessage-ID: <synthetic-sdk-import@example.net>\r\nDate: Mon, 1 Jan 2001 12:00:00 +0000\r\nSubject: Synthetic native import\r\n\r\n" + body
	var logicalRunID string
	var retryMeeting func()
	switch format {
	case "pst":
		file := filepath.Join(t.TempDir(), "synthetic-events.pst")
		fixture := testutil.PSTFixture()
		if selectedAttachment {
			fixture = testutil.PSTMediaFixture()
		}
		require.NoError(os.WriteFile(file, fixture, 0o600))
		summary, err := importer.ImportPst(t.Context(), f.Store, file, importer.PstImportOptions{Identifier: importIdentifier, NotifyImports: true, NoResume: true, AttachmentsDir: cfg.AttachmentsDir()})
		require.NoError(err)
		require.NotNil(summary)
		require.False(summary.HardErrors)
		require.Zero(summary.Errors)
		logicalRunID = summary.ImportRunID
	case "meeting_import":
		req := meetingimport.Request{NotifyImports: true, Source: meetingimport.Source{Identifier: importIdentifier, AccountEmail: "owner@example.net"}, Meeting: meetingimport.Meeting{ExternalID: "synthetic-sdk-meeting", Title: "Meeting", StartedAt: "2001-01-01T12:00:00Z", SummaryText: body, Organizer: &meetingimport.MeetingPerson{Email: "organizer@example.net"}, Attendees: []meetingimport.MeetingPerson{{Email: "peer@example.net"}}}}
		switch fixtureFormat {
		case "meeting_import_transcript":
			req.Meeting.SummaryText = ""
			req.Meeting.Transcript = body
		case "meeting_import_segments":
			offset := float64(4)
			req.Meeting.SummaryText = ""
			req.Meeting.TranscriptSegments = []meetingimport.TranscriptSegment{{Speaker: "P", Text: body, OffsetSeconds: &offset}}
		case "meeting_import_outgoing":
			req.Meeting.Organizer.Email = "owner@example.net"
		case "meeting_import_actions":
			actions := []meetingimport.MeetingActionItem{{SourceID: "synthetic-action", Title: "Follow up"}}
			req.Meeting.ActionItems = &actions
		case "meeting_import_anchor_repair":
			req.Meeting.Attendees[0].ID = "synthetic-peer"
			req.Meeting.Attendees[0].Phone = "+15550000002"
			var err error
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION synthetic_sdk_meeting_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic meeting person repair failure'; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_sdk_meeting_abort BEFORE INSERT ON participant_contact_observations FOR EACH ROW EXECUTE FUNCTION synthetic_sdk_meeting_abort()`)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER synthetic_sdk_meeting_abort BEFORE INSERT ON participant_contact_observations BEGIN SELECT RAISE(ABORT,'synthetic meeting person repair failure'); END`)
			}
			require.NoError(err)
		}
		// Expected rendered content is independent of the production builder.
		prefix := "Meeting\nWhen: 2001-01-01 12:00\n\n"
		switch fixtureFormat {
		case "meeting_import_transcript":
			body = prefix + "Transcript:\n" + body
		case "meeting_import_segments":
			body = prefix + "Transcript:\n[00:04] P: " + body
		case "meeting_import_actions":
			body = prefix + body + "\n\nAction items:\n- Follow up"
		default:
			body = prefix + body
		}
		encoded, err := json.Marshal(req)
		require.NoError(err)
		post := func() *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/import/meeting", bytes.NewReader(encoded))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", owner)
			recorder := httptest.NewRecorder()
			daemon.Router().ServeHTTP(recorder, request)
			return recorder
		}
		// The real request operation owns this gate while importing.
		releaseScheduledSync()
		response := post()
		require.True(meetingStore.held, "actual HTTP meeting import owns the daemon mutation gate")
		if fixtureFormat == "meeting_import_anchor_repair" {
			require.Equal(http.StatusInternalServerError, response.Code)
			require.NotZero(meetingStore.result.MessageID, "native meeting error: %v", meetingStore.err)
			require.NotEmpty(meetingStore.result.ImportRunID)
			logicalRunID = meetingStore.result.ImportRunID
			prior := meetingStore.result
			retryMeeting = func() {
				sql := `DROP TRIGGER synthetic_sdk_meeting_abort`
				if f.Store.IsPostgreSQL() {
					sql += ` ON participant_contact_observations`
				}
				_, err := f.Store.DB().Exec(sql)
				require.NoError(err)
				response := post()
				require.Equal(http.StatusOK, response.Code, response.Body.String())
				assert.Equal(prior.MessageID, meetingStore.result.MessageID)
				assert.NotEqual(prior.ImportRunID, meetingStore.result.ImportRunID)
				var events, links int
				require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
				assert.Equal(1, events)
				require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM participant_links`).Scan(&links))
				assert.Equal(1, links)
			}
		} else {
			require.Equal(http.StatusCreated, response.Code, "%s; native meeting error: %v", response.Body.String(), meetingStore.err)
			var result map[string]any
			require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
			logicalRunID = eventsWireString(t, result["import_run_id"])
			require.NotEmpty(logicalRunID, "native HTTP result exposes its fresh opaque run")
		}
	case "slackdump":
		directory := t.TempDir()
		require.NoError(os.WriteFile(filepath.Join(directory, "users.json"), []byte(`[{"id":"U_OWNER","team_id":"T_EXAMPLE","name":"synthetic_owner","profile":{"email":"owner@example.net","display_name":"Synthetic Owner"}},{"id":"U_PEER","team_id":"T_EXAMPLE","name":"synthetic_peer","profile":{"email":"peer@example.net","display_name":"Synthetic Peer"}}]`), 0o600))
		conversationID, conversationDir := "C_EXAMPLE", "example-channel"
		if fixtureFormat == "slackdump_direct" {
			conversationID, conversationDir = "D_EXAMPLE", "D_EXAMPLE"
			require.NoError(os.WriteFile(filepath.Join(directory, "dms.json"), []byte(`[{"id":"D_EXAMPLE","members":["U_OWNER","U_PEER"]}]`), 0o600))
		} else {
			require.NoError(os.WriteFile(filepath.Join(directory, "channels.json"), []byte(`[{"id":"C_EXAMPLE","name":"example-channel","is_channel":true,"is_member":true,"members":["U_OWNER","U_PEER"]}]`), 0o600))
		}
		require.NoError(os.MkdirAll(filepath.Join(directory, conversationDir), 0o700))
		filename := filepath.Join(directory, conversationDir, "2024-01-01.json")
		opts := slack.SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true}
		if fixtureFormat == "slackdump_reply_reaction" {
			require.NoError(os.WriteFile(filename, []byte(`[{"type":"message","ts":"1704067100.000001","user":"U_PEER","text":"Synthetic prior parent"}]`), 0o600))
			silent := opts
			silent.NotifyImports = false
			_, err := slack.ImportSlackdump(t.Context(), f.Store, directory, silent)
			require.NoError(err)
			var parentIDForSlack int64
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_message_id=?`), conversationID+":1704067100.000001").Scan(&parentIDForSlack))
			readyParentID.Store(parentIDForSlack)
		}
		message := map[string]any{"type": "message", "ts": "1704067200.000001", "user": "U_PEER", "text": body}
		if fixtureFormat == "slackdump_outgoing" {
			message["user"] = "U_OWNER"
		}
		if fixtureFormat == "slackdump_bot" {
			delete(message, "user")
			message["bot_id"] = "B_EXAMPLE"
			message["username"] = "Synthetic Bot"
			message["subtype"] = "bot_message"
		}
		if fixtureFormat == "slackdump_reply_reaction" {
			message["thread_ts"] = "1704067100.000001"
			message["reactions"] = []any{map[string]any{"name": "thumbsup", "users": []string{"U_OWNER"}, "count": 1}}
		}
		if selectedAttachment {
			message["files"] = []any{map[string]any{"id": "F_EXAMPLE", "name": "synthetic.bin", "mimetype": "application/octet-stream", "size": 27}}
			require.NoError(os.MkdirAll(filepath.Join(directory, conversationDir, "attachments"), 0o700))
			require.NoError(os.WriteFile(filepath.Join(directory, conversationDir, "attachments/F_EXAMPLE-synthetic.bin"), []byte("Synthetic cached attachment"), 0o600))
			opts.AttachmentsDir = cfg.AttachmentsDir()
		}
		encoded, err := json.Marshal([]any{message})
		require.NoError(err)
		require.NoError(os.WriteFile(filename, encoded, 0o600))
		summary, err := slack.ImportSlackdump(t.Context(), f.Store, directory, opts)
		require.NoError(err)
		require.Equal(1, summary.MessagesAdded)
		if selectedAttachment {
			assert.Equal(1, summary.AttachmentsDownloaded)
		}
		logicalRunID = summary.ImportRunID
	case "whatsapp":
		path := filepath.Join(t.TempDir(), "native-whatsapp.sqlite")
		database, err := sql.Open("sqlite3", path)
		require.NoError(err)
		fromMe := 0
		if strings.HasSuffix(fixtureFormat, "_outgoing") {
			fromMe = 1
		}
		options := whatsapp.ImportOptions{Phone: importIdentifier, DisplayName: "Synthetic Owner", NotifyImports: true, BatchSize: 2}
		if strings.HasPrefix(fixtureFormat, "whatsapp_android_") {
			_, err = database.Exec(`CREATE TABLE jid(_id INTEGER PRIMARY KEY,user TEXT,server TEXT,raw_string TEXT);
CREATE TABLE chat(_id INTEGER PRIMARY KEY,jid_row_id INTEGER,hidden INTEGER,subject TEXT,sort_timestamp INTEGER);
CREATE TABLE message(_id INTEGER PRIMARY KEY,chat_row_id INTEGER,from_me INTEGER,key_id TEXT,sender_jid_row_id INTEGER,timestamp INTEGER,message_type INTEGER,text_data TEXT,status INTEGER,starred INTEGER);
CREATE TABLE message_media(message_row_id INTEGER PRIMARY KEY,mime_type TEXT,file_size INTEGER,file_path TEXT,width INTEGER,height INTEGER,media_duration INTEGER);
INSERT INTO jid VALUES(1,'15550000002','s.whatsapp.net','15550000002@s.whatsapp.net');
INSERT INTO chat VALUES(10,1,0,'Synthetic Conversation',1704067200000)`)
			require.NoError(err)
			if fixtureFormat == "whatsapp_android_group" {
				_, err = database.Exec(`INSERT INTO jid VALUES(2,'120363000000000000','g.us','120363000000000000@g.us'); UPDATE chat SET jid_row_id=2; CREATE TABLE group_participants(gjid TEXT,jid TEXT,admin INTEGER); INSERT INTO group_participants VALUES('120363000000000000@g.us','15550000002@s.whatsapp.net',1)`)
				require.NoError(err)
			}
			messageType := 0
			if selectedAttachment {
				messageType = 1
			}
			_, err = database.Exec(`INSERT INTO message VALUES(100,10,?,'synthetic-sdk-whatsapp',1,1704067200000,?,?,0,0)`, fromMe, messageType, body)
			require.NoError(err)
			if selectedAttachment {
				_, err = database.Exec(`INSERT INTO message_media VALUES(100,'application/octet-stream',27,'synthetic.bin',128,64,7)`)
				require.NoError(err)
				options.MediaDir, options.AttachmentsDir = t.TempDir(), cfg.AttachmentsDir()
				require.NoError(os.WriteFile(filepath.Join(options.MediaDir, "synthetic.bin"), []byte("Synthetic cached attachment"), 0o600))
			}
			if fixtureFormat == "whatsapp_android_reply_reaction" {
				_, err = database.Exec(`INSERT INTO message VALUES(50,10,0,'synthetic-sdk-whatsapp-parent',1,1704067100000,0,'Synthetic prior parent',0,0);
CREATE TABLE message_quoted(message_row_id INTEGER,key_id TEXT);INSERT INTO message_quoted VALUES(100,'synthetic-sdk-whatsapp-parent');
CREATE TABLE message_add_on(_id INTEGER PRIMARY KEY,parent_message_row_id INTEGER,sender_jid_row_id INTEGER);INSERT INTO message_add_on VALUES(1,100,1);
CREATE TABLE message_add_on_reaction(message_add_on_row_id INTEGER,reaction TEXT,sender_timestamp INTEGER);INSERT INTO message_add_on_reaction VALUES(1,'+1',1704067201000)`)
				require.NoError(err)
			}
		} else {
			_, err = database.Exec(`CREATE TABLE ZWACHATSESSION(Z_PK INTEGER PRIMARY KEY,ZCONTACTJID TEXT,ZPARTNERNAME TEXT,ZSESSIONTYPE INTEGER,ZLASTMESSAGEDATE TIMESTAMP);
CREATE TABLE ZWAGROUPMEMBER(Z_PK INTEGER PRIMARY KEY,ZCHATSESSION INTEGER,ZMEMBERJID TEXT,ZCONTACTNAME TEXT,ZFIRSTNAME TEXT,ZISADMIN INTEGER);
CREATE TABLE ZWAMESSAGE(Z_PK INTEGER PRIMARY KEY,ZCHATSESSION INTEGER,ZGROUPMEMBER INTEGER,ZSTANZAID TEXT,ZISFROMME INTEGER,ZMESSAGEDATE TIMESTAMP,ZTEXT TEXT,ZMESSAGETYPE INTEGER,ZFROMJID TEXT);
INSERT INTO ZWACHATSESSION VALUES(10,'15550000002@s.whatsapp.net','Synthetic Conversation',0,725760000)`)
			require.NoError(err)
			messageType := 0
			if fixtureFormat == "whatsapp_apple_type7" {
				messageType = 7
			}
			var groupMember any
			jid := "15550000002@s.whatsapp.net"
			if fixtureFormat == "whatsapp_apple_group" {
				_, err = database.Exec(`UPDATE ZWACHATSESSION SET ZCONTACTJID='120363000000000000@g.us',ZSESSIONTYPE=1; INSERT INTO ZWAGROUPMEMBER VALUES(1,10,'15550000002@s.whatsapp.net','Synthetic Sender','',1)`)
				require.NoError(err)
				groupMember = 1
				jid = "120363000000000000@g.us"
			}
			if fixtureFormat == "whatsapp_apple_lid" {
				jid = "999999999999999@lid"
				_, err = database.Exec(`UPDATE ZWACHATSESSION SET ZCONTACTJID=?`, jid)
				require.NoError(err)
				lid, err := sql.Open("sqlite3", filepath.Join(filepath.Dir(path), "LID.sqlite"))
				require.NoError(err)
				_, err = lid.Exec(`CREATE TABLE ZWAZACCOUNT(ZIDENTIFIER TEXT,ZPHONENUMBER TEXT);INSERT INTO ZWAZACCOUNT VALUES('999999999999999@lid','15550000002')`)
				require.NoError(err)
				require.NoError(lid.Close())
			}
			_, err = database.Exec(`INSERT INTO ZWAMESSAGE VALUES(100,10,?,'synthetic-sdk-whatsapp',?,725760000,?,?,?)`, groupMember, fromMe, body, messageType, jid)
			require.NoError(err)
		}
		require.NoError(database.Close())
		if fixtureFormat == "whatsapp_android_reply_reaction" {
			prior := options
			prior.NotifyImports = false
			prior.Limit = 1
			summary, err := whatsapp.NewImporter(f.Store, nil).Import(t.Context(), path, prior)
			require.NoError(err)
			require.Zero(summary.Errors)
			require.Equal(int64(1), summary.MessagesAdded)
			var parent int64
			require.NoError(f.Store.DB().QueryRow(`SELECT id FROM messages WHERE source_message_id='synthetic-sdk-whatsapp-parent'`).Scan(&parent))
			readyParentID.Store(parent)
		}
		summary, err := whatsapp.NewImporter(f.Store, nil).Import(t.Context(), path, options)
		require.NoError(err)
		require.Zero(summary.Errors)
		require.Positive(summary.MessagesAdded)
		logicalRunID = summary.ImportRunID
	case "facebook_messenger":
		directory := t.TempDir()
		nativeFormat := "json"
		sender := "Synthetic Sender"
		if fixtureFormat == "facebook_messenger_outgoing" || fixtureFormat == "whatsapp_android_outgoing" || fixtureFormat == "whatsapp_apple_outgoing" {
			sender = "Synthetic Owner"
		}
		if strings.HasPrefix(fixtureFormat, "facebook_messenger_e2ee") {
			thread := map[string]any{"participants": []string{"Synthetic Owner", "Synthetic Sender"}, "threadName": "Synthetic native conversation"}
			message := map[string]any{"senderName": sender, "text": body, "timestamp": 1704067200000, "type": "Generic"}
			if selectedAttachment {
				message["media"] = []map[string]any{{"uri": "./media/synthetic.bin"}}
			}
			thread["messages"] = []map[string]any{message}
			content, err := json.Marshal(thread)
			require.NoError(err)
			threadDir := filepath.Join(directory, "your_activity_across_facebook/messages")
			require.NoError(os.MkdirAll(threadDir, 0700))
			require.NoError(os.WriteFile(filepath.Join(threadDir, "synthetic.json"), content, 0600))
			if selectedAttachment {
				require.NoError(os.MkdirAll(filepath.Join(threadDir, "media"), 0700))
				require.NoError(os.WriteFile(filepath.Join(threadDir, "media/synthetic.bin"), []byte("Synthetic cached attachment"), 0600))
			}
		} else {
			threadDir := filepath.Join(directory, "your_activity_across_facebook/messages/inbox/synthetic_THREAD")
			require.NoError(os.MkdirAll(threadDir, 0700))
			if fixtureFormat == "facebook_messenger_html" {
				nativeFormat = "html"
				content := `<!doctype html><html><head><meta charset="utf-8"><title>Synthetic native conversation</title></head><body><div class="_a706"><div class="_a70e"><div class="_3b0d">Participants: Synthetic Owner, Synthetic Sender</div></div><div class="_a6-g"><div class="_a6-p"><div class="_2ph_ _a6-h _a6-i"><div>Synthetic Sender</div></div><div class="_2ph_ _a6-p"><div><div>` + body + `</div></div></div><div class="_a72d">Jan 1, 2024, 12:00 AM</div></div></div></div></body></html>`
				require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.html"), []byte(content), 0600))
			} else {
				message := map[string]any{"sender_name": sender, "content": body, "timestamp_ms": 1704067200000, "type": "Generic"}
				if selectedAttachment {
					message["files"] = []map[string]any{{"uri": "your_activity_across_facebook/messages/inbox/synthetic_THREAD/files/synthetic.bin"}}
				}
				thread := map[string]any{"participants": []map[string]any{{"name": "Synthetic Owner"}, {"name": "Synthetic Sender"}}, "title": "Synthetic native conversation", "thread_type": "Regular", "messages": []map[string]any{message}}
				content, err := json.Marshal(thread)
				require.NoError(err)
				require.NoError(os.WriteFile(filepath.Join(threadDir, "message_1.json"), content, 0600))
				if selectedAttachment {
					require.NoError(os.MkdirAll(filepath.Join(threadDir, "files"), 0700))
					require.NoError(os.WriteFile(filepath.Join(threadDir, "files/synthetic.bin"), []byte("Synthetic cached attachment"), 0600))
				}
			}
		}
		summary, err := fbmessenger.ImportDYI(t.Context(), f.Store, fbmessenger.ImportOptions{Me: importIdentifier, RootDir: directory, Format: nativeFormat, AttachmentsDir: cfg.AttachmentsDir(), NotifyImports: true, NoResume: true})
		require.NoError(err)
		require.False(summary.HardErrors)
		require.Equal(int64(1), summary.MessagesAdded)
		if selectedAttachment {
			assert.Equal(1, summary.AttachmentsStored)
		}
		logicalRunID = summary.ImportRunID
	case "apple_messages":
		path := filepath.Join(t.TempDir(), "chat.db")
		database, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
		require.NoError(err)
		_, err = database.Exec(`CREATE TABLE message(guid TEXT NOT NULL DEFAULT 'synthetic-sdk-message',text TEXT,attributedBody BLOB,date INTEGER NOT NULL,is_from_me INTEGER NOT NULL DEFAULT 0,service TEXT DEFAULT 'SMS',cache_has_attachments INTEGER NOT NULL DEFAULT 0,handle_id INTEGER DEFAULT 1);
   CREATE TABLE handle(id TEXT);CREATE TABLE chat(guid TEXT,display_name TEXT,chat_identifier TEXT);CREATE TABLE chat_message_join(chat_id INTEGER,message_id INTEGER);CREATE TABLE chat_handle_join(chat_id INTEGER,handle_id INTEGER);
   INSERT INTO handle(id) VALUES('peer@example.net');`)
		require.NoError(err)
		var text any = body
		var attributed []byte
		date := int64(757425600)
		fromMe, attachment := 0, 0
		service := "SMS"
		switch fixtureFormat {
		case "apple_messages_imessage_group":
			service = "iMessage"
			_, err = database.Exec(`INSERT INTO chat(guid,display_name,chat_identifier) VALUES('any;+;synthetic-sdk-group','','synthetic-sdk-group');INSERT INTO chat_message_join(chat_id,message_id) VALUES(1,1);INSERT INTO chat_handle_join(chat_id,handle_id) VALUES(1,1)`)
			require.NoError(err)
		case "apple_messages_outgoing":
			fromMe = 1
		case "apple_messages_attributed":
			text = nil
			attributed = append([]byte{4, 11}, []byte("streamtyped")...)
			attributed = append(attributed, 0x84, 0x01, '+', byte(len(body)))
			attributed = append(attributed, []byte(body)...)
		case "apple_messages_empty":
			text = nil
			date = 0
		case "apple_messages_attachment_only":
			text = nil
			attachment = 1
		}
		_, err = database.Exec(`INSERT INTO message(date,text,attributedBody,is_from_me,service,cache_has_attachments) VALUES(?,?,?,?,?,?)`, date, text, attributed, fromMe, service, attachment)
		require.NoError(err)
		require.NoError(database.Close())
		opts := []imessage.ClientOption{imessage.WithNotifyImports(true)}
		if fixtureFormat == "apple_messages_outgoing" || (fixtureFormat == "facebook_messenger_outgoing" || fixtureFormat == "whatsapp_android_outgoing" || fixtureFormat == "whatsapp_apple_outgoing") {
			opts = append(opts, imessage.WithOwnerHandle("+15550000001"))
		}
		native, err := imessage.NewClient(path, opts...)
		require.NoError(err)
		t.Cleanup(func() { assert.NoError(native.Close()) })
		source, err := f.Store.GetOrCreateSource("apple_messages", importIdentifier)
		require.NoError(err)
		summary, err := native.Import(t.Context(), f.Store, source.ID)
		require.NoError(err)
		require.Equal(1, summary.MessagesImported)
		assert.Zero(summary.Skipped)
		logicalRunID = summary.ImportRunID
	case "google_voice":
		directory := t.TempDir()
		require.NoError(os.Mkdir(filepath.Join(directory, "Calls"), 0o700))
		require.NoError(os.WriteFile(filepath.Join(directory, "Phones.vcf"), []byte("BEGIN:VCARD\nVERSION:3.0\nitem1.TEL:"+importIdentifier+"\nitem1.X-ABLabel:Google Voice\nEND:VCARD\n"), 0o600))
		var html, filename string
		switch fixtureFormat {
		case "google_voice_text", "google_voice_group":
			prefix := ""
			filename = "Synthetic Sender - Text - 2024-06-01T12_00_00Z.html"
			if fixtureFormat == "google_voice_group" {
				prefix = `<div class="participants"><a class="tel" href="tel:+15550000002"></a><a class="tel" href="tel:+15550000003"></a></div>`
				filename = "Group Conversation - 2024-06-01T12_00_00Z.html"
			}
			html = `<html><body>` + prefix + `<div class="message"><abbr class="dt" title="2024-06-01T12:00:00.000Z"></abbr><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a><q>` + body + `</q></div></body></html>`
		default:
			kind, title := "Received", "Received call from Synthetic Sender"
			switch fixtureFormat {
			case "google_voice_placed":
				kind, title = "Placed", "Placed call to Synthetic Sender"
			case "google_voice_missed":
				kind, title = "Missed", "Missed call from Synthetic Sender"
			case "google_voice_voicemail":
				kind, title = "Voicemail", "Voicemail from Synthetic Sender"
			}
			audio := ""
			if selectedAttachment {
				audio = `<audio src="synthetic.mp3"></audio>`
				require.NoError(os.WriteFile(filepath.Join(directory, "Calls", "synthetic.mp3"), []byte("Synthetic cached attachment"), 0o600))
			}
			filename = "Synthetic Sender - " + kind + " - 2024-06-01T12_00_00Z.html"
			html = `<html><head><title>` + title + `</title></head><body><div class="haudio"><div class="contributor vcard"><a class="tel" href="tel:+15550000002"><span class="fn">Synthetic Sender</span></a></div><abbr class="published" title="2024-06-01T12:00:00.000Z"></abbr><abbr class="duration" title="PT42S"></abbr>` + audio + `</div></body></html>`
		}
		require.NoError(os.WriteFile(filepath.Join(directory, "Calls", filename), []byte(html), 0o600))
		native, err := gvoice.NewClient(directory, gvoice.WithNotifyImports(true), gvoice.WithAttachmentsDir(cfg.AttachmentsDir()))
		require.NoError(err)
		source, err := f.Store.GetOrCreateSource("google_voice", native.Identifier())
		require.NoError(err)
		summary, err := native.Import(t.Context(), f.Store, source.ID)
		require.NoError(err)
		require.Equal(1, summary.MessagesImported)
		assert.Zero(summary.Skipped)
		logicalRunID = summary.ImportRunID
	case "synctech_sms":
		backup := filepath.Join(t.TempDir(), "messages.xml")
		native := synctechsms.ImportOptions{OwnerPhone: importIdentifier, NotifyImports: true, AttachmentsDir: cfg.AttachmentsDir()}
		var xml string
		switch fixtureFormat {
		case "synctech_sms_sms":
			native.IncludeSMS = true
			xml = `<smses count="1"><sms address="+15550000002" date="978350400000" type="1" body="` + body + `" /></smses>`
		case "synctech_sms_mms":
			native.IncludeMMS = true
			native.IncludeAttachments = true
			attachment := base64.StdEncoding.EncodeToString([]byte("Synthetic cached attachment"))
			xml = `<smses count="1"><mms date="978350400000" msg_box="1" m_id="synthetic-sdk-mms"><parts><part seq="0" ct="text/plain" text="` + body + `" /><part seq="1" ct="application/octet-stream" cl="synthetic.bin" data="` + attachment + `" /></parts><addrs><addr address="+15550000002" type="137" /><addr address="+15550000001" type="151" /></addrs></mms></smses>`
		case "synctech_sms_call":
			native.IncludeCalls = true
			xml = `<calls count="1"><call number="+15550000002" duration="42" date="978350400000" type="1" presentation="1" /></calls>`
		}
		require.NoError(os.WriteFile(backup, []byte(xml), 0o600))
		summary, err := synctechsms.NewImporter(f.Store, native).ImportPath(backup)
		require.NoError(err)
		require.Len(summary.MessageIDs, 1)
		if native.IncludeAttachments {
			assert.Equal(1, summary.AttachmentsImported)
		}
		logicalRunID = summary.ImportRunID
	case "imazing_csv":
		directory := t.TempDir()
		require.NoError(os.MkdirAll(filepath.Join(directory, "csv"), 0o700))
		require.NoError(os.MkdirAll(filepath.Join(directory, "attachments"), 0o700))
		writeCSV := func(rows [][]string) {
			t.Helper()
			file, err := os.Create(filepath.Join(directory, "csv", "messages.csv"))
			require.NoError(err)
			writer := csv.NewWriter(file)
			require.NoError(writer.Write([]string{"Chat Session", "Message Date", "Delivered Date", "Read Date", "Service", "Type", "Sender ID", "Sender Name", "Status", "Replying to", "Subject", "Text", "Attachment", "Attachment type"}))
			require.NoError(writer.WriteAll(rows))
			require.NoError(writer.Error())
			require.NoError(file.Close())
		}
		writeCSV([][]string{{"Synthetic Conversation", "2001-01-01 11:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "Synthetic prior parent", "", ""}})
		native := imazingcsv.Options{Owner: importIdentifier, Timezone: "UTC", AttachmentsDir: cfg.AttachmentsDir()}
		_, err := imazingcsv.NewImporter(f.Store, native).ImportPath(t.Context(), directory)
		require.NoError(err)
		var parentID int64
		require.NoError(f.Store.DB().QueryRow(`SELECT id FROM messages WHERE snippet='Synthetic prior parent'`).Scan(&parentID))
		readyParentID.Store(parentID)
		require.NoError(os.WriteFile(filepath.Join(directory, "attachments", "synthetic.bin"), []byte("Synthetic cached attachment"), 0o600))
		writeCSV([][]string{{"Synthetic Conversation", "2001-01-01 12:00:00", "", "", "iMessage", "Incoming", "sender@example.net", "Synthetic Sender", "", "Replying to:\nsynthetic-import@example.net\n2001-01-01 11:00:00\nSynthetic prior parent", "", body, "synthetic.bin", "application/octet-stream"}})
		native.NotifyImports = true
		summary, err := imazingcsv.NewImporter(f.Store, native).ImportPath(t.Context(), directory)
		require.NoError(err)
		assert.Equal(1, summary.RepliesLinked)
		assert.Zero(summary.RepliesUnresolved)
		assert.Equal(1, summary.AttachmentsStored)
		logicalRunID = summary.ImportRunID
	case "eml":
		directory := filepath.Join(t.TempDir(), "Synthetic.mailbox")
		require.NoError(os.MkdirAll(directory, 0o700))
		require.NoError(os.WriteFile(filepath.Join(directory, "message.eml"), []byte(raw), 0o600))
		summary, err := importer.ImportEMLDir(t.Context(), f.Store, directory, importer.EMLImportOptions{Identifier: importIdentifier, NotifyImports: true, NoResume: true, AttachmentsDir: cfg.AttachmentsDir()})
		require.NoError(err)
		require.NotNil(summary)
		require.False(summary.HardErrors)
		logicalRunID = summary.ImportRunID
	case "maildir":
		directory := t.TempDir()
		for _, part := range []string{"cur", "new", "tmp"} {
			require.NoError(os.MkdirAll(filepath.Join(directory, part), 0o700))
		}
		require.NoError(os.WriteFile(filepath.Join(directory, "cur", "synthetic:2,S"), []byte(raw), 0o600))
		summary, err := importer.ImportMaildir(t.Context(), f.Store, directory, importer.MaildirImportOptions{Identifier: importIdentifier, NotifyImports: true, NoResume: true, AttachmentsDir: cfg.AttachmentsDir()})
		require.NoError(err)
		require.NotNil(summary)
		require.False(summary.HardErrors)
		logicalRunID = summary.ImportRunID
	case "apple-mail":
		directory := t.TempDir()
		mailbox := filepath.Join(directory, "Mailboxes", "Synthetic.mbox", "Messages")
		require.NoError(os.MkdirAll(mailbox, 0o700))
		const attachmentBody = "Synthetic cached attachment"
		raw = strings.Replace(raw, "\r\n\r\n"+body, "\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=synthetic-ready\r\n\r\n--synthetic-ready\r\nContent-Type: text/plain\r\n\r\n"+body+"\r\n--synthetic-ready\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=synthetic.bin\r\nContent-Transfer-Encoding: base64\r\nX-Apple-Content-Length: 36\r\n\r\n\r\n--synthetic-ready--\r\n", 1)
		require.NoError(os.WriteFile(filepath.Join(mailbox, "1.partial.emlx"), []byte(strconv.Itoa(len(raw))+"\n"+raw), 0o600))
		sibling := filepath.Join(directory, "Mailboxes", "Synthetic.mbox", "Attachments", "1", "2")
		require.NoError(os.MkdirAll(sibling, 0o700))
		require.NoError(os.WriteFile(filepath.Join(sibling, "synthetic.bin"), []byte(attachmentBody), 0o600))
		summary, err := importer.ImportEmlxDir(t.Context(), f.Store, directory, importer.EmlxImportOptions{Identifier: importIdentifier, NotifyImports: true, NoResume: true, AttachmentsDir: cfg.AttachmentsDir()})
		require.NoError(err)
		require.NotNil(summary)
		require.False(summary.HardErrors)
		require.Equal(int64(1), summary.AttachmentsRestored)
		logicalRunID = summary.ImportRunID
	case "mbox":
		file := filepath.Join(t.TempDir(), "synthetic.mbox")
		require.NoError(os.WriteFile(file, []byte("From sender@example.net Mon Jan 1 12:00:00 2001\n"+raw+"\n"), 0o600))
		summary, err := importer.ImportMbox(t.Context(), f.Store, file, importer.MboxImportOptions{Identifier: importIdentifier, NotifyImports: true, NoResume: true, AttachmentsDir: cfg.AttachmentsDir()})
		require.NoError(err)
		require.NotNil(summary)
		require.False(summary.HardErrors)
		logicalRunID = summary.ImportRunID
	}
	require.NotEmpty(logicalRunID)
	var importOccurrences int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&importOccurrences))
	require.Equal(1, importOccurrences, "a complete explicitly opted-in native parser must append its import occurrence")

	if format != "meeting_import" {
		// File parsers run under the scheduled-work lease. Meeting imports
		// instead take their own real HTTP request lease before publication.
		require.Eventually(gate.HasRequestWaiters, 30*time.Second, 10*time.Millisecond, "delivery should tell a scheduled sync to yield")
	}
	releaseScheduledSync()
	var delivered delivery
	select {
	case delivered = <-received:
	case <-time.After(30 * time.Second):
		require.FailNow("committed native import occurrence was not delivered")
	}
	envelope := delivered.envelope
	require.NotEmpty(envelope.EventID)
	assert.Equal(subscriptionID, delivered.headers.Get("X-Mcp-Subscription-Id"))
	assert.Equal(envelope.EventID, delivered.headers.Get("Webhook-Id"))
	if body != "" {
		assert.NotContains(string(delivered.raw), body)
	}
	encodedSchema, err := json.Marshal(messageDefinition["payloadSchema"])
	require.NoError(err)
	var payloadSchema jsonschema.Schema
	require.NoError(json.Unmarshal(encodedSchema, &payloadSchema))
	resolvedPayload, err := payloadSchema.Resolve(nil)
	require.NoError(err)
	var payload map[string]any
	require.NoError(json.Unmarshal(envelope.Data, &payload))
	assert.NoError(resolvedPayload.Validate(payload), "actual occurrence must match its advertised schema")
	if body != "" {
		assert.NotContains(string(envelope.Data), body)
	}
	assert.Equal("import", payload["ingestion_mode"])
	assert.Equal(format, payload["import_format"])
	assert.Equal(logicalRunID, payload["import_run_id"])
	messageID, err := strconv.ParseInt(eventsWireString(t, payload["message_id"]), 10, 64)
	require.NoError(err)
	sourceID, err := strconv.ParseInt(eventsWireString(t, payload["source_id"]), 10, 64)
	require.NoError(err)
	assert.NotEqual(f.Source.ID, sourceID, "archive admission must include a newly created native import source")
	require.Eventually(func() bool {
		row, err := f.Store.GetMCPSubscription(t.Context(), subscriptionID)
		return err == nil && row != nil && row.CursorSeq > 0 && row.PendingSeq == 0
	}, 30*time.Second, 20*time.Millisecond, "delivery success was not durably recorded")
	_, wire = call("tools/call", map[string]any{"name": "get_mcp_event", "arguments": map[string]any{"event_id": envelope.EventID}})
	require.Nil(wire["error"])
	assert.Equal(envelope.EventID, eventsWireObject(t, eventsWireObject(t, wire["result"])["structuredContent"])["eventId"])
	var attachmentID json.Number
	var joined strings.Builder
	for offset := 0; ; offset += 8 {
		require.Less(offset, 128, "synthetic receipt pagination must terminate")
		_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(messageID, 10), "event_id": envelope.EventID, "offset": offset, "max_chars": 8}})
		require.Nil(wire["error"])
		result := eventsWireObject(t, wire["result"])
		require.NotEqual(true, result["isError"], "%v", result["content"])
		content := eventsWireObject(t, result["structuredContent"])
		assert.Equal(json.Number(strconv.FormatInt(sourceID, 10)), content["source_id"])
		assert.Equal(fixtureFormat == "meeting_import_outgoing" || fixtureFormat == "slackdump_outgoing" || fixtureFormat == "google_voice_placed" || fixtureFormat == "apple_messages_outgoing" || (fixtureFormat == "facebook_messenger_outgoing" || fixtureFormat == "whatsapp_android_outgoing" || fixtureFormat == "whatsapp_apple_outgoing"), content["is_from_me"])
		if selectedAttachment {
			attachments := eventsWireArray(t, content["attachments"])
			require.Len(attachments, 1, "selected native reference belongs to the ready snapshot")
			var ok bool
			attachmentID, ok = eventsWireObject(t, attachments[0])["ID"].(json.Number)
			require.True(ok)
		}
		chunk := eventsWireString(t, content["body_text"])
		joined.WriteString(chunk)
		more, ok := content["has_more"].(bool)
		require.True(ok, "receipt reads expose the real pagination boundary")
		if !more {
			break
		}
	}
	assert.Equal(body, strings.TrimRight(joined.String(), "\r\n"))
	if selectedAttachment {
		_, wire = call("tools/call", map[string]any{"name": "get_attachment", "arguments": map[string]any{"attachment_id": attachmentID, "offset": 0, "length": 64}})
		require.Nil(wire["error"])
		result := eventsWireObject(t, wire["result"])
		require.NotEqual(true, result["isError"], "%v", result["content"])
		content := eventsWireObject(t, result["structuredContent"])
		filename := "synthetic.bin"
		if fixtureFormat == "google_voice_voicemail" {
			filename = "synthetic.mp3"
		}
		assert.Equal(filename, content["filename"])
		attachment, err := base64.StdEncoding.DecodeString(eventsWireString(t, content["data_base64"]))
		require.NoError(err)
		wantAttachment := "Synthetic cached attachment"
		if format == "pst" {
			wantAttachment = "Synthetic PST attachment"
		}
		assert.Equal(wantAttachment, string(attachment))
	}
	wrongID, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Unrelated synthetic content must not be read", Valid: true}})
	require.NoError(err)
	for _, args := range []map[string]any{{"id": strconv.FormatInt(messageID, 10), "event_id": envelope.EventID + "forged"}, {"id": strconv.FormatInt(wrongID, 10), "event_id": envelope.EventID}} {
		rec, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": args})
		require.NotNil(wire["error"])
		assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
		if body != "" {
			assert.NotContains(rec.Body.String(), body)
		}
		assert.NotContains(rec.Body.String(), "Unrelated synthetic content")
	}
	if retryMeeting != nil {
		retryMeeting()
	}
	_, wire = call("events/unsubscribe", mcpevents.UnsubscribeRequest{Name: req.Name, Arguments: req.Arguments, Delivery: mcpevents.Delivery{Mode: req.Delivery.Mode, URL: req.Delivery.URL}})
	require.Nil(wire["error"])
	rec, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(messageID, 10), "event_id": envelope.EventID, "offset": 8, "max_chars": 8}})
	require.NotNil(wire["error"])
	assert.Equal(json.Number("-32602"), eventsWireObject(t, wire["error"])["code"])
	assert.Equal("event_unavailable", eventsWireObject(t, eventsWireObject(t, wire["error"])["data"])["reason"])
	if len(body) >= 16 {
		assert.NotContains(rec.Body.String(), body[8:16])
	}

	independent := newMCPHTTPServer(ServeOptions{Engine: daemonclient.NewEngineAdapter(client), Events: client}, HTTPOptions{APIKey: "owner-fixture", IndependentCredential: true}).Handler
	_, independentWire := eventsHTTPCall(t, independent, "2026-07-28", "server/discover", `{}`)
	require.Nil(independentWire["error"])
	assert.NotContains(eventsWireObject(t, independentWire["result"])["capabilities"], "events")
	_, independentWire = eventsHTTPCall(t, independent, "2026-07-28", "events/list", `{}`)
	require.NotNil(independentWire["error"])
	assert.Equal(json.Number("-32601"), eventsWireObject(t, independentWire["error"])["code"])
	// A valid frontend bearer cannot replace the daemon's owner credential.
	wrongClient, err := daemonclient.New(daemonclient.Config{URL: endpoint.URL, APIKey: "wrong-synthetic-owner", AllowInsecure: true})
	require.NoError(err)
	badHandler := newMCPHTTPServer(ServeOptions{Events: wrongClient}, HTTPOptions{APIKey: "owner-fixture"}).Handler
	_, denied := eventsHTTPCall(t, badHandler, "2026-07-28", "events/list", `{}`)
	require.NotNil(denied["error"])
	assert.Equal(json.Number("-32012"), eventsWireObject(t, denied["error"])["code"])
}
