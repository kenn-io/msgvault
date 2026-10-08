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
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"sort"
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
	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestEventsNativeDiscordImportAndReceiptRead(t *testing.T) {
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
		assert.Equal("Synthetic Discord content read through a delivery receipt.", archivedBody)
		assert.Equal("discord_json", rawFormat)
		var links int
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), messageID).Scan(&links)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(1, links)
		var mentions int
		var parent sql.NullInt64
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM message_recipients WHERE message_id=? AND recipient_type='mention'`), messageID).Scan(&mentions)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(1, mentions)
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), messageID).Scan(&parent)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.False(parent.Valid, "unavailable parent must not mute the child's arrival")
		var sourceID, conversationID int64
		var metadata, sourceIdentifier string
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT source_id, conversation_id, metadata FROM messages WHERE id=?`), messageID).Scan(&sourceID, &conversationID, &metadata)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT identifier FROM sources WHERE id=? AND source_type='discord'`), sourceID).Scan(&sourceIdentifier)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal("200", sourceIdentifier)
		var native struct {
			ReferencedMessageID string `json:"referenced_message_id"`
			ReactionSummaries   []struct {
				Count int `json:"count"`
			} `json:"reaction_summaries"`
		}
		if !assert.NoError(json.Unmarshal([]byte(metadata), &native)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal("499", native.ReferencedMessageID)
		if assert.Len(native.ReactionSummaries, 1) {
			assert.Equal(2, native.ReactionSummaries[0].Count)
		}
		var knownMembers, senderMatches int
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=? AND participant_id IN (SELECT participant_id FROM message_recipients WHERE message_id=? AND recipient_type IN ('from','mention'))`), conversationID, messageID).Scan(&knownMembers)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(2, knownMembers)
		if !assert.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM participant_identifiers WHERE participant_id=(SELECT sender_id FROM messages WHERE id=?) AND identifier_type='discord_user_id' AND identifier_value='201'`), messageID).Scan(&senderMatches)) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		assert.Equal(1, senderMatches)

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
	svc, err := mcpevents.New(t.Context(), f.Store, mcpevents.Options{Enabled: true, Sources: []string{"discord"}, OwnerKey: owner, KeyPath: filepath.Join(t.TempDir(), "events.key"), WithOperation: daemon.MCPEventsOperation, TLSConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, LookupIP: func(context.Context, string) ([]netip.Addr, error) {
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
	// The fixture implements Discord's read-only REST pagination contract.
	var live, parentAvailable atomic.Bool
	var guild atomic.Int64
	guild.Store(200)
	const body = "Synthetic Discord content read through a delivery receipt."
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		guildID := strconv.FormatInt(guild.Load(), 10)
		switch r.URL.Path {
		case "/users/@me":
			assert.NoError(json.NewEncoder(w).Encode(discord.User{ID: "101", Username: "Synthetic Owner", Bot: true}))
		case "/guilds/200", "/guilds/201":
			assert.NoError(json.NewEncoder(w).Encode(discord.Guild{ID: guildID, Name: "Synthetic guild"}))
		case "/guilds/200/channels", "/guilds/201/channels":
			assert.NoError(json.NewEncoder(w).Encode([]discord.Channel{{ID: "300", GuildID: guildID, Type: 0, Name: "Synthetic channel"}}))
		case "/guilds/200/threads/active", "/guilds/201/threads/active", "/channels/300/threads/archived/public", "/channels/300/users/@me/threads/archived/private":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"threads": []any{}, "has_more": false}))
		case "/channels/300/messages":
			message := func(id, text, author, name string, bot bool) discord.Message {
				stamp, _ := discord.TimestampFromSnowflake(id)
				return discord.Message{ID: id, ChannelID: "300", GuildID: guildID, Content: text, Timestamp: stamp, Author: discord.User{ID: author, Username: "synthetic-user", GlobalName: name, Bot: bot}}
			}
			messages := []discord.Message{message("501", "Synthetic history", "201", "Synthetic Sender", false)}
			if live.Load() {
				arrival := message("502", body, "201", "Synthetic Sender", false)
				arrival.Attachments = []discord.Attachment{{ID: "601", Filename: "synthetic.txt", ContentType: "text/plain", Size: 4, URL: "https://cdn.discordapp.com/attachments/300/601/synthetic.txt"}}
				arrival.Mentions = []discord.User{{ID: "202", Username: "Synthetic Mention"}}
				arrival.Reactions = []discord.Reaction{{Count: 2, Emoji: discord.Emoji{Name: "synthetic"}}}
				arrival.MessageReference = &discord.MessageReference{MessageID: "499", ChannelID: "300", GuildID: guildID}
				messages = append(messages, arrival, message("503", "Synthetic own message", "101", "Synthetic Owner", true))
			}
			if parentAvailable.Load() {
				messages = append(messages, message("499", "Synthetic historical parent", "201", "Synthetic Sender", false))
			}
			before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
			after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			filtered := make([]discord.Message, 0, len(messages))
			for _, m := range messages {
				id, _ := strconv.ParseUint(m.ID, 10, 64)
				if before != 0 && id >= before || after != 0 && id <= after {
					continue
				}
				filtered = append(filtered, m)
			}
			sort.Slice(filtered, func(i, j int) bool {
				a, _ := strconv.ParseUint(filtered[i].ID, 10, 64)
				b, _ := strconv.ParseUint(filtered[j].ID, 10, 64)
				if after != 0 {
					return a < b
				}
				return a > b
			})
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit > 0 && len(filtered) > limit {
				filtered = filtered[:limit]
			}
			assert.NoError(json.NewEncoder(w).Encode(filtered))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(provider.Close)
	nativeClient, err := discord.NewClient(provider.URL, "synthetic-token")
	require.NoError(err)
	me, err := nativeClient.Me(t.Context())
	require.NoError(err)
	require.True(me.Bot)
	importer := discord.NewImporter(f.Store, nativeClient)
	importOpts := discord.ImportOptions{GuildID: "200", BotUserID: me.ID}
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)
	source, err := f.Store.GetOrCreateSource("discord", importOpts.GuildID)
	require.NoError(err)
	sourceID := source.ID
	var initialOccurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&initialOccurrences))
	assert.Zero(initialOccurrences, "initial history is silent")
	var conversationID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?`), sourceID, "300").Scan(&conversationID))
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
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "502").Scan(&messageID))
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
	var metadata, rawFormat string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT metadata FROM messages WHERE id=?`), messageID).Scan(&metadata))
	var provenance map[string]any
	require.NoError(json.Unmarshal([]byte(metadata), &provenance))
	assert.Equal("499", provenance["referenced_message_id"])
	assert.Equal("300", provenance["referenced_channel_id"])
	assert.Equal("200", provenance["referenced_guild_id"])
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT raw_format FROM message_raw WHERE message_id=?`), messageID).Scan(&rawFormat))
	assert.Equal("discord_json", rawFormat)
	select {
	case unexpected := <-received:
		assert.Fail("own message must not be delivered", "%s", unexpected.envelope.EventID)
	default:
	}
	_, err = importer.Import(t.Context(), importOpts)
	require.NoError(err)

	// Historical repair links the parent silently, without granting a parent read.
	parentAvailable.Store(true)
	repairOpts := importOpts
	repairOpts.Full = true
	_, err = importer.Import(t.Context(), repairOpts)
	require.NoError(err)
	var parentID, linkedParent int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), sourceID, "499").Scan(&parentID))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), messageID).Scan(&linkedParent))
	assert.Equal(parentID, linkedParent)
	_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(parentID, 10), "event_id": envelope.EventID}})
	require.NotNil(wire["error"], "child receipt must not grant its parent's content")

	// Repeated native IDs in a second synthetic guild cannot borrow the receipt.
	guild.Store(201)
	otherImporter := discord.NewImporter(f.Store, nativeClient)
	otherOpts := importOpts
	otherOpts.GuildID = "201"
	_, err = otherImporter.Import(t.Context(), otherOpts)
	require.NoError(err)
	otherSource, err := f.Store.GetOrCreateSource("discord", otherOpts.GuildID)
	require.NoError(err)
	var otherMessageID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), otherSource.ID, "502").Scan(&otherMessageID))
	assert.NotEqual(messageID, otherMessageID)
	_, wire = call("tools/call", map[string]any{"name": "get_message", "arguments": map[string]any{"id": strconv.FormatInt(otherMessageID, 10), "event_id": envelope.EventID}})
	require.NotNil(wire["error"], "another source cannot borrow a receipt")
	var occurrences int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&occurrences))
	assert.Equal(2, occurrences, "re-import does not duplicate either archived message")
}
