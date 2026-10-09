package store_test

import (
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strconv"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

var producerObservedAt = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func producerPayloadTime(t *testing.T, data map[string]any, field string) time.Time {
	t.Helper()
	require := Require.New(t)

	value, ok := data[field].(string)
	require.True(ok, "payload %s must be a timestamp string", field)
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(err, "parse payload %s", field)
	return parsed
}

// producerPayloadInt64 parses the retained wire number without float rounding.
func producerPayloadInt64(t *testing.T, event producerEvent, field string) int64 {
	t.Helper()
	require := Require.New(t)

	var fields map[string]jsontext.Value
	require.NoError(json.Unmarshal(event.rawData, &fields))
	require.Contains(fields, field)
	value, err := strconv.ParseInt(string(fields[field]), 10, 64)
	require.NoError(err, "payload %s must be an exact integer", field)
	return value
}

func enableProducerEvents(t *testing.T, st *store.Store, capabilities ...store.MCPEventCapability) {
	t.Helper()
	require := Require.New(t)

	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner", Capabilities: capabilities})
	require.NoError(err)
}

type producerEvent struct {
	kind                                string
	messageID, conversationID, sourceID int64
	fromMe                              bool
	data                                map[string]any
	rawData                             []byte
}

func readProducerEvents(t *testing.T, st *store.Store, family string) []producerEvent {
	t.Helper()
	require := Require.New(t)
	assert := Assert.New(t)

	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT kind, COALESCE(message_id, 0), conversation_id, source_id, from_me, data FROM mcp_event_log WHERE family = ? ORDER BY seq`), family)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	var events []producerEvent
	for rows.Next() {
		var event producerEvent
		require.NoError(rows.Scan(&event.kind, &event.messageID, &event.conversationID, &event.sourceID, &event.fromMe, &event.rawData))
		require.NoError(json.Unmarshal(event.rawData, &event.data))
		assert.Equal(strconv.FormatInt(event.sourceID, 10), event.data["source_id"])
		assert.Equal(strconv.FormatInt(event.conversationID, 10), event.data["conversation_id"])
		if value, present := event.data["message_id"]; present {
			assert.Equal(strconv.FormatInt(event.messageID, 10), value)
		}
		events = append(events, event)
	}
	require.NoError(rows.Err())
	return events
}

func TestMCPMessageProducerExplicitProvenance(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode store.IngestMode
		want int
	}{
		{"live", store.IngestLive, 1}, {"backfill", store.IngestBackfill, 0}, {"unknown", store.IngestUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := Require.New(t)
			assert := Assert.New(t)

			f := storetest.New(t)
			enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
			view := f.Store.WithIngestContext(store.IngestContext{Mode: tc.mode, ObservedAt: producerObservedAt})
			id, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Complete synthetic body", Valid: true}})
			require.NoError(err)
			events := readProducerEvents(t, f.Store, "msgvault.message_archived")
			require.Len(events, tc.want)
			if tc.want == 0 {
				return
			}
			assert.Equal(id, events[0].messageID)
			assert.Equal(f.ConvID, events[0].conversationID)
			assert.Equal(f.Source.ID, events[0].sourceID)
			assert.Contains(events[0].data, "sent_at")
			assert.Nil(events[0].data["sent_at"])
			var archivedAt time.Time
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT archived_at FROM messages WHERE id = ?`), id).Scan(&archivedAt))
			assert.True(archivedAt.Equal(producerPayloadTime(t, events[0].data, "archived_at")), "payload archive time must match the committed message row")
			var occurredAtText string
			require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT occurred_at FROM mcp_event_log WHERE family = ? AND message_id = ?`), "msgvault.message_archived", id).Scan(&occurredAtText))
			occurredAt, err := time.Parse(time.RFC3339Nano, occurredAtText)
			require.NoError(err)
			assert.True(archivedAt.Equal(occurredAt), "a message without sent_at uses archived_at as its event time")
			body, _ := f.GetMessageBody(id)
			assert.Equal("Complete synthetic body", body.String)
		})
	}
}

func TestMCPMessageProducerOnlyProbesInsertionForLiveIngest(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         store.IngestMode
		wantProbes   int
		wantReceipts int
	}{
		{name: "live", mode: store.IngestLive, wantProbes: 1, wantReceipts: 1},
		{name: "backfill", mode: store.IngestBackfill, wantProbes: 1},
		{name: "unknown", mode: store.IngestUnknown, wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			f := storetest.New(t)
			enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
			_, err := f.Store.DB().Exec(`CREATE TABLE synthetic_mcp_message_writes (inserts INTEGER NOT NULL, updates INTEGER NOT NULL)`)
			require.NoError(err)
			_, err = f.Store.DB().Exec(`INSERT INTO synthetic_mcp_message_writes VALUES (0, 0)`)
			require.NoError(err)
			if f.Store.IsPostgreSQL() {
				_, err = f.Store.DB().Exec(`CREATE FUNCTION count_synthetic_mcp_message_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_mcp_message_writes SET inserts=inserts+1; RETURN NEW; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER count_synthetic_mcp_message_insert BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION count_synthetic_mcp_message_insert()`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE FUNCTION count_synthetic_mcp_message_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_mcp_message_writes SET updates=updates+1; RETURN NEW; END $$`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER count_synthetic_mcp_message_update AFTER UPDATE OF subject ON messages FOR EACH ROW EXECUTE FUNCTION count_synthetic_mcp_message_update()`)
			} else {
				_, err = f.Store.DB().Exec(`CREATE TRIGGER count_synthetic_mcp_message_insert BEFORE INSERT ON messages BEGIN UPDATE synthetic_mcp_message_writes SET inserts=inserts+1; END`)
				require.NoError(err)
				_, err = f.Store.DB().Exec(`CREATE TRIGGER count_synthetic_mcp_message_update AFTER UPDATE OF subject ON messages BEGIN UPDATE synthetic_mcp_message_writes SET updates=updates+1; END`)
			}
			require.NoError(err)

			view := f.Store.WithIngestContext(store.IngestContext{Mode: tc.mode, ObservedAt: producerObservedAt})
			message := f.NewMessage().Build()
			id, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic producer probe", Valid: true}})
			require.NoError(err)
			var probes, updates int
			require.NoError(f.Store.DB().QueryRow(`SELECT inserts, updates FROM synthetic_mcp_message_writes`).Scan(&probes, &updates))
			assert.Equal(tc.wantProbes, probes, "only live first-arrival capture needs the insertion outcome probe")
			assert.Zero(updates, "a first-arrival insert must not run the update path")
			events := readProducerEvents(t, f.Store, "msgvault.message_archived")
			require.Len(events, tc.wantReceipts)
			if tc.wantReceipts == 1 {
				assert.Equal(id, events[0].messageID)
				message.Snippet = sql.NullString{String: "Synthetic duplicate update", Valid: true}
				_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic producer probe", Valid: true}})
				require.NoError(err)
				require.NoError(f.Store.DB().QueryRow(`SELECT updates FROM synthetic_mcp_message_writes`).Scan(&updates))
				assert.Equal(1, updates, "a duplicate must continue through the existing update path")
				events = readProducerEvents(t, f.Store, "msgvault.message_archived")
				require.Len(events, 1, "a duplicate update must not create another first-arrival receipt")
			}
		})
	}
}

func TestMCPMessageProducerFinalAttributionAndDuplicate(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := storetest.New(t)
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: producerObservedAt})
	msg := f.NewMessage().Build()
	msg.SenderID = sql.NullInt64{Int64: owner, Valid: true}
	id, err := view.PersistMessage(&store.MessagePersistData{Message: msg, BodyText: sql.NullString{String: "Original", Valid: true}})
	require.NoError(err)
	idAgain, err := view.PersistMessage(&store.MessagePersistData{Message: msg, BodyText: sql.NullString{String: "Edited", Valid: true}})
	require.NoError(err)
	assert.Equal(id, idAgain)
	events := readProducerEvents(t, f.Store, "msgvault.message_archived")
	require.Len(events, 1)
	assert.True(events[0].fromMe)
	assert.Equal(true, events[0].data["from_me"])
}

func TestMCPMessageProducerConcurrentInsertEmitsOnce(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := storetest.New(t)
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: producerObservedAt})
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			msg := &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "same-live-message", MessageType: "email"}
			_, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: msg, BodyText: sql.NullString{String: "Ready", Valid: true}})
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(err)
	}
	assert.Len(readProducerEvents(t, f.Store, "msgvault.message_archived"), 1)
}

func TestMCPMessageProducerCompositeFailureRollsBack(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	testutil.SkipIfPostgres(t, "SQLite trigger injects failure after header persistence")
	f := storetest.New(t)
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	_, err := f.Store.DB().Exec(`CREATE TRIGGER reject_mcp_body BEFORE INSERT ON message_bodies BEGIN SELECT RAISE(ABORT, 'synthetic body failure'); END`)
	require.NoError(err)
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: producerObservedAt})
	_, err = view.PersistMessage(&store.MessagePersistData{Message: f.NewMessage().Build(), BodyText: sql.NullString{String: "Must not publish", Valid: true}})
	require.ErrorContains(err, "synthetic body failure")
	var count int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Zero(count)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.message_archived"))
}

func TestMCPMessageProducerRepairAndHeaderOnlyAreMuted(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f := storetest.New(t)
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}})
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: producerObservedAt})
	msg := f.NewMessage().Build()
	id, err := view.UpsertMessage(msg)
	require.NoError(err)
	_, err = view.PersistRepairMessageWithParticipantsContext(t.Context(), store.MessageIdentityGuard{ID: id, SourceID: f.Source.ID, SourceMessageID: msg.SourceMessageID}, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{Message: msg, BodyText: sql.NullString{String: "Repaired", Valid: true}}
	})
	require.NoError(err)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.message_archived"))
}
