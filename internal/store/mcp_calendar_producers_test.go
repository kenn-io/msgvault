package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"sync"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type calendarEventPersister interface {
	PersistCalendarEventContext(ctx context.Context, data *store.MessagePersistData, changedAt time.Time, cancelled bool) (int64, bool, error)
}

func calendarProducerFixture(t *testing.T) (*storetest.Fixture, calendarEventPersister) {
	t.Helper()
	require := Require.New(t)

	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("gcal", "calendar@example.test/events")
	require.NoError(err)
	conv, err := f.Store.EnsureConversationWithType(source.ID, "event:example-event", "calendar", "Synthetic calendar event")
	require.NoError(err)
	f.Source, f.ConvID = source, conv
	enableProducerEvents(t, f.Store, store.MCPEventCapability{Family: "msgvault.calendar_event_changed", SourceType: "gcal", Kinds: []string{"created", "updated", "cancelled"}})
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestLive, ObservedAt: producerObservedAt})
	persister, ok := any(view).(calendarEventPersister)
	require.True(ok, "Store must expose atomic calendar persistence")
	return f, persister
}

func calendarSnapshot(f *storetest.Fixture, metadata string) *store.MessagePersistData {
	meta := sql.NullString{String: metadata, Valid: true}
	return &store.MessagePersistData{Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "example-event", MessageType: "calendar_event", Subject: sql.NullString{String: "Synthetic meeting", Valid: true}}, Metadata: &meta, BodyText: sql.NullString{String: "Synthetic meeting body", Valid: true}}
}

func TestMCPCalendarProducerComparesScheduledState(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, persister := calendarProducerFixture(t)
	states := []struct {
		metadata  string
		changedAt time.Time
		wantKind  string
	}{
		{`{"status":"confirmed","start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false,"sequence":7,"ical_uid":"event@example.test"}`, producerObservedAt, "created"},
		// A provider updated timestamp alone is not a semantic change.
		{`{"status":"confirmed","start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false,"sequence":7,"ical_uid":"event@example.test"}`, producerObservedAt.Add(time.Minute), ""},
		// A changed time emits even when sequence stays the same.
		{`{"status":"confirmed","start":"2026-10-06T14:00:00Z","end":"2026-10-06T15:00:00Z","all_day":false,"sequence":7,"ical_uid":"event@example.test"}`, producerObservedAt.Add(2 * time.Minute), "updated"},
		// A sequence decrease alone does not emit.
		{`{"status":"confirmed","start":"2026-10-06T14:00:00Z","end":"2026-10-06T15:00:00Z","all_day":false,"sequence":2,"ical_uid":"event@example.test"}`, producerObservedAt.Add(3 * time.Minute), ""},
		{`{"status":"confirmed","start":"2026-10-06T14:00:00Z","end":"2026-10-06T15:00:00Z","all_day":false,"sequence":3,"ical_uid":"event@example.test"}`, producerObservedAt.Add(4 * time.Minute), "updated"},
	}
	var expectedKinds []string
	var firstID int64
	for _, state := range states {
		id, inserted, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, state.metadata), state.changedAt, false)
		require.NoError(err)
		if firstID == 0 {
			firstID = id
			assert.True(inserted)
		} else {
			assert.False(inserted)
			assert.Equal(firstID, id)
		}
		if state.wantKind != "" {
			expectedKinds = append(expectedKinds, state.wantKind)
		}
		events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
		var kinds []string
		for _, event := range events {
			kinds = append(kinds, event.kind)
		}
		assert.Equal(expectedKinds, kinds)
	}
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.message_archived"))
}

func TestMCPCalendarProducerEmitsSequenceIncreaseFromOmittedPrior(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, live := calendarProducerFixture(t)
	backfillStore := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill, ObservedAt: producerObservedAt})
	backfill, ok := any(backfillStore).(calendarEventPersister)
	require.True(ok)
	priorMetadata := `{"status":"confirmed","start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false}`
	id, inserted, err := backfill.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, priorMetadata), producerObservedAt, false)
	require.NoError(err)
	assert.True(inserted)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.calendar_event_changed"), "backfill remains muted")

	updatedMetadata := `{"status":"confirmed","start":"2026-10-06T12:00:00Z","end":"2026-10-06T13:00:00Z","all_day":false,"sequence":1}`
	updatedID, inserted, err := live.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, updatedMetadata), producerObservedAt.Add(time.Minute), false)
	require.NoError(err)
	assert.False(inserted)
	assert.Equal(id, updatedID)
	events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 1)
	assert.Equal("updated", events[0].kind)
	assert.Equal(int64(1), producerPayloadInt64(t, events[0], "sequence"))

	_, _, err = live.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, updatedMetadata), producerObservedAt.Add(2*time.Minute), false)
	require.NoError(err)
	assert.Len(readProducerEvents(t, f.Store, "msgvault.calendar_event_changed"), 1, "identical redelivery does not append another occurrence")
}

func TestMCPCalendarProducerSparseCancellationPreservesKnownState(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, persister := calendarProducerFixture(t)
	id, _, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","end":"2026-10-07","all_day":true,"time_zone":"Etc/UTC","sequence":4,"ical_uid":"event@example.test"}`), producerObservedAt, false)
	require.NoError(err)
	sparse := calendarSnapshot(f, `{"status":"cancelled"}`)
	sparse.Message.Subject = sql.NullString{}
	sparse.BodyText = sql.NullString{}
	_, inserted, err := persister.PersistCalendarEventContext(t.Context(), sparse, producerObservedAt.Add(time.Minute), true)
	require.NoError(err)
	assert.False(inserted)
	_, _, err = persister.PersistCalendarEventContext(t.Context(), sparse, producerObservedAt.Add(2*time.Minute), true)
	require.NoError(err)
	events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 2)
	assert.Equal("cancelled", events[1].kind)
	assert.Equal("2026-10-06", events[1].data["starts_at"])
	assert.Equal(true, events[1].data["all_day"])
	assert.Equal(int64(4), producerPayloadInt64(t, events[1], "sequence"))
	assert.Equal("event@example.test", events[1].data["ical_uid"])
	body, _ := f.GetMessageBody(id)
	assert.Equal("Synthetic meeting body", body.String)
	metadata, err := f.Store.GetMessageMetadata(id)
	require.NoError(err)
	var projection map[string]any
	require.NoError(json.Unmarshal([]byte(metadata.String), &projection))
	assert.Equal("cancelled", projection["status"])
	assert.Equal("2026-10-06", projection["start"])

	_, _, err = persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","end":"2026-10-07","all_day":true,"time_zone":"Etc/UTC","sequence":4,"ical_uid":"event@example.test"}`), producerObservedAt.Add(3*time.Minute), false)
	require.NoError(err)
	_, _, err = persister.PersistCalendarEventContext(t.Context(), sparse, producerObservedAt.Add(4*time.Minute), true)
	require.NoError(err)
	events = readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 4)
	assert.Equal("updated", events[2].kind)
	assert.Equal("cancelled", events[3].kind)
}

func TestMCPCalendarProducerUnseenTombstoneKeepsUnknownStartNull(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, persister := calendarProducerFixture(t)
	sparse := calendarSnapshot(f, `{"status":"cancelled","all_day":false}`)
	sparse.Message.Subject = sql.NullString{}
	sparse.BodyText = sql.NullString{}
	_, inserted, err := persister.PersistCalendarEventContext(t.Context(), sparse, time.Time{}, true)
	require.NoError(err)
	assert.True(inserted)
	events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 1)
	assert.Equal("cancelled", events[0].kind)
	for _, field := range []string{"starts_at", "all_day", "sequence", "ical_uid"} {
		assert.Contains(events[0].data, field)
		assert.Nil(events[0].data[field], field)
	}
	assert.True(producerObservedAt.Equal(producerPayloadTime(t, events[0].data, "changed_at")))
}

func TestMCPCalendarProducerFullSyncMuted(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, _ := calendarProducerFixture(t)
	view := f.Store.WithIngestContext(store.IngestContext{Mode: store.IngestBackfill, ObservedAt: producerObservedAt})
	persister, ok := any(view).(calendarEventPersister)
	require.True(ok)
	_, _, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","end":"2026-10-07","all_day":true}`), producerObservedAt, false)
	require.NoError(err)
	assert.Empty(readProducerEvents(t, f.Store, "msgvault.calendar_event_changed"))
}

func TestMCPCalendarProducerConcurrentRedeliveryComparesUnderLock(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, persister := calendarProducerFixture(t)
	_, _, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","all_day":true,"sequence":1}`), producerObservedAt, false)
	require.NoError(err)
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			_, _, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-07","all_day":true,"sequence":1}`), producerObservedAt.Add(time.Minute), false)
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(err)
	}
	events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 2)
	assert.Equal("created", events[0].kind)
	assert.Equal("updated", events[1].kind)
}

func TestMCPCalendarProducerBodyFailureRollsBackProjectionAndOccurrence(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	testutil.SkipIfPostgres(t, "SQLite trigger injects failure after calendar metadata persistence")
	f, persister := calendarProducerFixture(t)
	id, _, err := persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","all_day":true}`), producerObservedAt, false)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`CREATE TRIGGER reject_calendar_body BEFORE UPDATE ON message_bodies BEGIN SELECT RAISE(ABORT, 'synthetic calendar body failure'); END`)
	require.NoError(err)
	_, _, err = persister.PersistCalendarEventContext(t.Context(), calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-07","all_day":true}`), producerObservedAt.Add(time.Minute), false)
	require.ErrorContains(err, "synthetic calendar body failure")
	metadata, err := f.Store.GetMessageMetadata(id)
	require.NoError(err)
	var projection map[string]any
	require.NoError(json.Unmarshal([]byte(metadata.String), &projection))
	assert.Equal("2026-10-06", projection["start"])
	assert.Len(readProducerEvents(t, f.Store, "msgvault.calendar_event_changed"), 1)
}

func TestMCPCalendarProducerFTSFailureKeepsReadyProjectionAndOccurrence(t *testing.T) {
	require := Require.New(t)
	assert := Assert.New(t)

	f, persister := calendarProducerFixture(t)
	var err error
	if f.Store.IsPostgreSQL() {
		// A real index UPDATE fails without rejecting canonical archive columns.
		_, err = f.Store.DB().Exec(`ALTER TABLE messages ADD CONSTRAINT synthetic_calendar_fts_failure CHECK (search_fts IS NULL)`)
	} else {
		_, err = f.Store.DB().Exec(`DROP TABLE messages_fts`)
	}
	require.NoError(err)
	data := calendarSnapshot(f, `{"status":"confirmed","start":"2026-10-06","all_day":true,"sequence":4}`)
	data.FTS = &store.FTSDoc{Subject: "Synthetic meeting", Body: data.BodyText.String}
	id, inserted, err := persister.PersistCalendarEventContext(t.Context(), data, producerObservedAt, false)
	require.NoError(err, "derived indexing failure must preserve the complete archive snapshot")
	assert.True(inserted)
	body, _ := f.GetMessageBody(id)
	assert.Equal(data.BodyText, body)
	metadata, err := f.Store.GetMessageMetadata(id)
	require.NoError(err)
	assert.JSONEq(data.Metadata.String, metadata.String)
	events := readProducerEvents(t, f.Store, "msgvault.calendar_event_changed")
	require.Len(events, 1)
	assert.Equal("created", events[0].kind)
}
