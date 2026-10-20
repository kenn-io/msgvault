package calsync

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersistControlledEventKeepsCursorAndArchivesImmediately(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "team@example.com", AccessRole: "owner", TimeZone: "UTC"}
	mock.Calendars = []gcal.Calendar{cal}
	s, st := newSyncer(t, mock, Options{OAuthApp: "calendar-app", OAuthAppSet: true})
	_, err := s.RegisterCalendars(context.Background())
	requirements.NoError(err)
	src, err := st.GetSourceByIdentifier(testAccount + "/" + cal.ID)
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncCursor(src.ID, "existing-cursor"))
	event := gcal.Event{ID: "controlled", Summary: "Planning", Status: gcal.StatusConfirmed, Start: gcal.EventDateTime{DateTime: time.Now()}, Organizer: gcal.Person{Email: cal.ID}, Attendees: []gcal.Attendee{{Email: "guest@example.com"}}, Raw: []byte(`{"id":"controlled","summary":"Planning","extendedProperties":{"private":{"example":"preserved"}}}`)}
	id, err := s.PersistEvent(context.Background(), cal, event)
	requirements.NoError(err)
	requirements.Positive(id)
	row, ok := getMsg(t, st, src.ID, event.ID)
	requirements.True(ok)
	assertions.Equal(id, row.id)
	assertions.Equal("Planning", row.subject.String)
	var metadata struct {
		CalendarID string `json:"calendar_id"`
	}
	requirements.NoError(json.Unmarshal([]byte(row.metadata.String), &metadata))
	assertions.Equal("team@example.com", metadata.CalendarID)
	src, err = st.GetSourceByID(src.ID)
	requirements.NoError(err)
	assertions.Equal("existing-cursor", src.SyncCursor.String)
	assertions.Equal("calendar-app", src.OAuthApp.String)
	event.Summary = "Updated planning"
	same, err := s.PersistEvent(context.Background(), cal, event)
	requirements.NoError(err)
	assertions.Equal(id, same)
	cancelled := gcal.Event{ID: event.ID, Status: gcal.StatusCancelled}
	same, err = s.PersistEvent(context.Background(), cal, cancelled)
	requirements.NoError(err)
	assertions.Equal(id, same)
	row, ok = getMsg(t, st, src.ID, event.ID)
	requirements.True(ok)
	assertions.Equal("Updated planning", row.subject.String)
	var cancelledMetadata struct {
		Status string `json:"status"`
	}
	requirements.NoError(json.Unmarshal([]byte(row.metadata.String), &cancelledMetadata))
	assertions.Equal("cancelled", cancelledMetadata.Status)
	assertions.Equal(0, mock.ListEventsCalls())
}

func TestPersistOffsetFreeExceptionResyncReusesLegacyMessage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duplicate bool
		cancelled bool
	}{
		{name: "legacy key"},
		{name: "duplicate from prior resync", duplicate: true},
		{name: "cancelled delta", cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			mock := gcal.NewMockAPI()
			cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
			s, st := newSyncer(t, mock, Options{})
			raw := []byte(`{"id":"instance-example","summary":"Planning","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
			decoded, err := gcalmodel.DecodeEvent(raw)
			requirements.NoError(err)

			// Model an archive written before the decoder could resolve the
			// offset-free originalStartTime: its source key was event.id.
			legacy := decoded
			legacy.OriginalStartTime = gcal.EventDateTime{}
			archivedID, err := s.PersistEvent(t.Context(), cal, legacy)
			requirements.NoError(err)

			source := primarySource(t, st)
			newSourceMessageID := deriveSourceMessageID(decoded)
			if tc.duplicate {
				conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(decoded), gcal.ConversationType, "")
				requirements.NoError(err)
				duplicateID, err := st.UpsertMessage(&store.Message{
					SourceID: source.ID, ConversationID: conversationID,
					SourceMessageID: newSourceMessageID, MessageType: gcal.MessageTypeCalendarEvent,
				})
				requirements.NoError(err)
				assertions.NotEqual(archivedID, duplicateID)
			}

			resync := decoded
			if tc.cancelled {
				cancelledRaw := []byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`)
				resync, err = gcalmodel.DecodeEvent(cancelledRaw)
				requirements.NoError(err)
			}
			resyncedID, err := s.PersistEvent(t.Context(), cal, resync)
			requirements.NoError(err)
			assertions.Equal(archivedID, resyncedID, "resync keeps the archived message ID")

			row, ok := getMsg(t, st, source.ID, newSourceMessageID)
			requirements.True(ok, "the current recurrence key is stored")
			assertions.Equal(archivedID, row.id)
			_, ok = getMsg(t, st, source.ID, decoded.ID)
			assertions.False(ok, "the legacy event.id key is removed")
			assertions.Equal(1, countMessages(t, st, source.ID), "the event remains one archive row")
			if tc.cancelled {
				assertions.Equal("Planning", row.subject.String, "a cancelled delta retains the archived content")
				assertions.Equal("cancelled", parseMeta(t, row)["status"])
			}
		})
	}
}

func TestPersistCancelledOffsetFreeExceptionPreservesNewerDuplicateSnapshot(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
	s, st := newSyncer(t, mock, Options{})
	legacyRaw := []byte(`{"id":"instance-example","updated":"2027-03-10T12:00:00Z","summary":"Planning","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
	legacyEvent, err := gcalmodel.DecodeEvent(legacyRaw)
	requirements.NoError(err)
	newSourceMessageID := deriveSourceMessageID(legacyEvent)
	legacyEvent.OriginalStartTime = gcal.EventDateTime{}
	archivedID, err := s.PersistEvent(t.Context(), cal, legacyEvent)
	requirements.NoError(err)

	source := primarySource(t, st)
	newerRaw := []byte(`{"id":"instance-example","updated":"2027-03-11T12:00:00Z","summary":"Rescheduled planning","description":"Updated meeting details","location":"Room 7","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T23:00:00Z","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-10T00:30:00Z","timeZone":"America/New_York"}}`)
	newerEvent, err := gcalmodel.DecodeEvent(newerRaw)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(newerEvent), gcal.ConversationType, "")
	requirements.NoError(err)
	body := serializeBody(newerEvent)
	duplicateID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: newSourceMessageID, MessageType: gcal.MessageTypeCalendarEvent,
		SentAt:       eventSentAt(newerEvent),
		Subject:      sql.NullString{String: newerEvent.Summary, Valid: true},
		Snippet:      sql.NullString{String: Snippet(body), Valid: body != ""},
		SizeEstimate: int64(len(body)),
	})
	requirements.NoError(err)
	assertions.NotEqual(archivedID, duplicateID)
	newerMetadata := buildMetadata(newerEvent, cal, testAccount)
	// Cancellation is merged into existing metadata without replacing the
	// complete provider snapshot. Reconciliation must still retain that newer
	// snapshot when its metadata status is cancelled.
	newerMetadata.Status = gcal.StatusCancelled
	metadata, err := json.Marshal(newerMetadata, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(duplicateID, sql.NullString{String: string(metadata), Valid: true}))
	requirements.NoError(st.UpsertMessageBody(duplicateID, sql.NullString{String: body, Valid: body != ""}, sql.NullString{}))
	requirements.NoError(st.UpsertMessageRawWithFormat(duplicateID, newerRaw, gcal.RawFormat))
	requirements.NoError(st.UpsertFTS(duplicateID, newerEvent.Summary, body, "", "", ""))

	cancelledRaw := []byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`)
	cancelledEvent, err := gcalmodel.DecodeEvent(cancelledRaw)
	requirements.NoError(err)
	resyncedID, err := s.PersistEvent(t.Context(), cal, cancelledEvent)
	requirements.NoError(err)
	assertions.Equal(archivedID, resyncedID, "the legacy archive message ID is retained")

	row, ok := getMsg(t, st, source.ID, newSourceMessageID)
	requirements.True(ok, "the canonical recurrence key is retained")
	assertions.Equal(archivedID, row.id)
	assertions.Equal("Rescheduled planning", row.subject.String)
	assertions.Equal("cancelled", parseMeta(t, row)["status"])
	assertions.Contains(bodyText(t, st, archivedID), "Updated meeting details")
	_, ok = getMsg(t, st, source.ID, legacyEvent.ID)
	assertions.False(ok, "the legacy event.id key is removed")
	var duplicateCount int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM messages WHERE id = ?`), duplicateID).Scan(&duplicateCount))
	assertions.Zero(duplicateCount, "the duplicate recurrence-key row is removed")
	if st.FTS5Available() {
		matches, _, err := st.SearchMessages("rescheduled", 0, 10)
		requirements.NoError(err)
		var retainedFound bool
		for _, match := range matches {
			if match.ID == archivedID {
				retainedFound = true
				break
			}
		}
		assertions.True(retainedFound, "the retained row remains searchable from the newer snapshot")
	}

	detail, err := st.GetCalendarOccurrenceContext(t.Context(), "evt:"+strconv.FormatInt(archivedID, 10))
	requirements.NoError(err)
	assertions.Equal("Rescheduled planning", detail.Subject)
	assertions.Equal("cancelled", detail.Status)
	assertions.Equal(time.Date(2027, 3, 9, 23, 0, 0, 0, time.UTC), detail.Start)
	assertions.Equal("Room 7", detail.Location)
	assertions.Equal("Updated meeting details", detail.Description)
}

func TestPersistCancelledOffsetFreeExceptionPreservesNewerLegacySnapshot(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
	s, st := newSyncer(t, mock, Options{})
	legacyRaw := []byte(`{"id":"instance-example","updated":"2027-03-11T12:00:00Z","summary":"Newer retained planning","description":"Newer archived details","location":"Room 12","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T23:00:00Z","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-10T00:30:00Z","timeZone":"America/New_York"}}`)
	legacyEvent, err := gcalmodel.DecodeEvent(legacyRaw)
	requirements.NoError(err)
	newSourceMessageID := deriveSourceMessageID(legacyEvent)
	legacyEvent.OriginalStartTime = gcal.EventDateTime{}
	archivedID, err := s.PersistEvent(t.Context(), cal, legacyEvent)
	requirements.NoError(err)

	source := primarySource(t, st)
	olderCanonicalRaw := []byte(`{"id":"instance-example","updated":"2027-03-10T12:00:00Z","summary":"Older canonical planning","description":"Older canonical details","location":"Room 6","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00Z","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00Z","timeZone":"America/New_York"}}`)
	olderCanonicalEvent, err := gcalmodel.DecodeEvent(olderCanonicalRaw)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(olderCanonicalEvent), gcal.ConversationType, "")
	requirements.NoError(err)
	body := serializeBody(olderCanonicalEvent)
	duplicateID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: newSourceMessageID, MessageType: gcal.MessageTypeCalendarEvent,
		SentAt:       eventSentAt(olderCanonicalEvent),
		Subject:      sql.NullString{String: olderCanonicalEvent.Summary, Valid: true},
		Snippet:      sql.NullString{String: Snippet(body), Valid: body != ""},
		SizeEstimate: int64(len(body)),
	})
	requirements.NoError(err)
	assertions.NotEqual(archivedID, duplicateID)
	metadata, err := json.Marshal(buildMetadata(olderCanonicalEvent, cal, testAccount), json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(duplicateID, sql.NullString{String: string(metadata), Valid: true}))
	requirements.NoError(st.UpsertMessageBody(duplicateID, sql.NullString{String: body, Valid: body != ""}, sql.NullString{}))
	requirements.NoError(st.UpsertMessageRawWithFormat(duplicateID, olderCanonicalRaw, gcal.RawFormat))
	requirements.NoError(st.UpsertFTS(duplicateID, olderCanonicalEvent.Summary, body, "", "", ""))

	cancelledRaw := []byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`)
	cancelledEvent, err := gcalmodel.DecodeEvent(cancelledRaw)
	requirements.NoError(err)
	resyncedID, err := s.PersistEvent(t.Context(), cal, cancelledEvent)
	requirements.NoError(err)
	assertions.Equal(archivedID, resyncedID, "the legacy archive message ID is retained")

	row, ok := getMsg(t, st, source.ID, newSourceMessageID)
	requirements.True(ok, "the canonical recurrence key is retained")
	assertions.Equal(archivedID, row.id)
	assertions.Equal("Newer retained planning", row.subject.String)
	assertions.Equal("cancelled", parseMeta(t, row)["status"])
	assertions.Contains(bodyText(t, st, archivedID), "Newer archived details")
	_, ok = getMsg(t, st, source.ID, legacyEvent.ID)
	assertions.False(ok, "the legacy event.id key is removed")
	assertions.Equal(1, countMessages(t, st, source.ID), "the event remains one archive row")

	gotRaw, err := st.GetMessageRaw(archivedID)
	requirements.NoError(err)
	assertions.Equal(legacyRaw, gotRaw, "the latest provider snapshot is retained")
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), "evt:"+strconv.FormatInt(archivedID, 10))
	requirements.NoError(err)
	assertions.Equal("Newer retained planning", detail.Subject)
	assertions.Equal("cancelled", detail.Status)
	assertions.Equal(time.Date(2027, 3, 9, 23, 0, 0, 0, time.UTC), detail.Start)
	assertions.Equal("Room 12", detail.Location)
	assertions.Equal("Newer archived details", detail.Description)
}

func TestPersistCancelledOffsetFreeExceptionPreservesLegacySnapshotOverSparseTombstone(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
	s, st := newSyncer(t, mock, Options{})
	legacyRaw := []byte(`{"id":"instance-example","summary":"Planning","description":"Retained event details","location":"Room 7","attendees":[{"email":"guest@example.com","displayName":"Guest","responseStatus":"accepted"}],"status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
	legacyEvent, err := gcalmodel.DecodeEvent(legacyRaw)
	requirements.NoError(err)
	newSourceMessageID := deriveSourceMessageID(legacyEvent)
	legacyEvent.OriginalStartTime = gcal.EventDateTime{}
	archivedID, err := s.PersistEvent(t.Context(), cal, legacyEvent)
	requirements.NoError(err)

	source := primarySource(t, st)
	cancelledRaw := []byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`)
	cancelledEvent, err := gcalmodel.DecodeEvent(cancelledRaw)
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(cancelledEvent), gcal.ConversationType, "")
	requirements.NoError(err)
	tombstoneID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: newSourceMessageID, MessageType: gcal.MessageTypeCalendarEvent,
	})
	requirements.NoError(err)
	assertions.NotEqual(archivedID, tombstoneID)
	requirements.NoError(st.UpsertMessageBody(tombstoneID, sql.NullString{String: serializeBody(cancelledEvent), Valid: true}, sql.NullString{}))
	metadata, err := json.Marshal(buildMetadata(cancelledEvent, cal, testAccount), json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(tombstoneID, sql.NullString{String: string(metadata), Valid: true}))
	requirements.NoError(st.UpsertMessageRawWithFormat(tombstoneID, cancelledRaw, gcal.RawFormat))

	resyncedID, err := s.PersistEvent(t.Context(), cal, cancelledEvent)
	requirements.NoError(err)
	assertions.Equal(archivedID, resyncedID, "the legacy archive message ID is retained")

	row, ok := getMsg(t, st, source.ID, newSourceMessageID)
	requirements.True(ok, "the canonical recurrence key is retained")
	assertions.Equal(archivedID, row.id)
	assertions.Equal("Planning", row.subject.String)
	assertions.Equal("cancelled", parseMeta(t, row)["status"])
	assertions.Contains(bodyText(t, st, archivedID), "Retained event details")
	_, ok = getMsg(t, st, source.ID, legacyEvent.ID)
	assertions.False(ok, "the legacy event.id key is removed")
	assertions.Equal(1, countMessages(t, st, source.ID), "the event remains one archive row")

	detail, err := st.GetCalendarOccurrenceContext(t.Context(), "evt:"+strconv.FormatInt(archivedID, 10))
	requirements.NoError(err)
	assertions.Equal("Planning", detail.Subject)
	assertions.Equal("cancelled", detail.Status)
	assertions.Equal(time.Date(2027, 3, 9, 22, 0, 0, 0, time.UTC), detail.Start)
	assertions.Equal("Room 7", detail.Location)
	assertions.Equal("Retained event details", detail.Description)
	requirements.Len(detail.Attendees, 1)
	assertions.Equal("guest@example.com", detail.Attendees[0].Email)
	if st.FTS5Available() {
		matches, _, err := st.SearchMessages("retained event details", 0, 10)
		requirements.NoError(err)
		var retainedFound bool
		for _, match := range matches {
			if match.ID == archivedID {
				retainedFound = true
				break
			}
		}
		assertions.True(retainedFound, "the retained event details remain searchable")
	}
}

func TestPersistCancelledOffsetFreeExceptionPreservesLegacySnapshotWhenCanonicalRawBoundsAreStale(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
	s, st := newSyncer(t, mock, Options{})
	legacyRaw := []byte(`{"id":"instance-example","summary":"Planning","description":"Retained event details","location":"Room 7","status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
	legacyEvent, err := gcalmodel.DecodeEvent(legacyRaw)
	requirements.NoError(err)
	newSourceMessageID := deriveSourceMessageID(legacyEvent)
	legacyEvent.OriginalStartTime = gcal.EventDateTime{}
	archivedID, err := s.PersistEvent(t.Context(), cal, legacyEvent)
	requirements.NoError(err)

	source := primarySource(t, st)
	cancelledRaw := []byte(`{"id":"instance-example","summary":"Stale cancellation","description":"Stale event details","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
	cancelledEvent, err := gcalmodel.DecodeEvent([]byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`))
	requirements.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(cancelledEvent), gcal.ConversationType, "")
	requirements.NoError(err)
	tombstoneID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID, SourceMessageID: newSourceMessageID,
		MessageType: gcal.MessageTypeCalendarEvent, Subject: sql.NullString{String: "Stale cancellation", Valid: true},
	})
	requirements.NoError(err)
	assertions.NotEqual(archivedID, tombstoneID)
	requirements.NoError(st.UpsertMessageBody(tombstoneID, sql.NullString{String: "Stale event details", Valid: true}, sql.NullString{}))
	metadata := buildMetadata(cancelledEvent, cal, testAccount)
	// A partial write can leave confirmed metadata over stale cancellation raw.
	// Matching bounds between raw and metadata are required before replacement.
	metadata.Status = gcal.StatusConfirmed
	metadata.Start = "2027-03-09T18:00:00-05:00"
	metadata.End = "2027-03-09T19:00:00-05:00"
	metadataJSON, err := json.Marshal(metadata, json.Deterministic(true))
	requirements.NoError(err)
	requirements.NoError(st.SetMessageMetadata(tombstoneID, sql.NullString{String: string(metadataJSON), Valid: true}))
	requirements.NoError(st.UpsertMessageRawWithFormat(tombstoneID, cancelledRaw, gcal.RawFormat))

	resyncedID, err := s.PersistEvent(t.Context(), cal, cancelledEvent)
	requirements.NoError(err)
	assertions.Equal(archivedID, resyncedID)
	row, ok := getMsg(t, st, source.ID, newSourceMessageID)
	requirements.True(ok)
	assertions.Equal("Planning", row.subject.String)
	assertions.Contains(bodyText(t, st, archivedID), "Retained event details")
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), "evt:"+strconv.FormatInt(archivedID, 10))
	requirements.NoError(err)
	assertions.Equal("cancelled", detail.Status)
	assertions.Equal(time.Date(2027, 3, 9, 22, 0, 0, 0, time.UTC), detail.Start)
}

func TestPersistCancelledOffsetFreeExceptionPreservesLegacySnapshotAfterInterruptedCanonicalInsert(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	mock := gcal.NewMockAPI()
	cal := gcal.Calendar{ID: "primary", AccessRole: "owner", TimeZone: "America/New_York"}
	s, st := newSyncer(t, mock, Options{})
	legacyRaw := []byte(`{"id":"instance-example","summary":"Planning","description":"Retained event details","location":"Room 7","attendees":[{"email":"guest@example.com","displayName":"Guest","responseStatus":"accepted"}],"status":"confirmed","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"start":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"},"end":{"dateTime":"2027-03-09T18:00:00","timeZone":"America/New_York"}}`)
	legacyEvent, err := gcalmodel.DecodeEvent(legacyRaw)
	requirements.NoError(err)
	newSourceMessageID := deriveSourceMessageID(legacyEvent)
	legacyEvent.OriginalStartTime = gcal.EventDateTime{}
	archivedID, err := s.PersistEvent(t.Context(), cal, legacyEvent)
	requirements.NoError(err)

	source := primarySource(t, st)
	conversationID, err := st.EnsureConversationWithType(source.ID, conversationKey(legacyEvent), gcal.ConversationType, "")
	requirements.NoError(err)
	interruptedID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: newSourceMessageID, MessageType: gcal.MessageTypeCalendarEvent,
	})
	requirements.NoError(err)
	assertions.NotEqual(archivedID, interruptedID)

	cancelledRaw := []byte(`{"id":"instance-example","status":"cancelled","recurringEventId":"series-example","originalStartTime":{"dateTime":"2027-03-09T17:00:00","timeZone":"America/New_York"}}`)
	cancelledEvent, err := gcalmodel.DecodeEvent(cancelledRaw)
	requirements.NoError(err)
	resyncedID, err := s.PersistEvent(t.Context(), cal, cancelledEvent)
	requirements.NoError(err)
	assertions.Equal(archivedID, resyncedID, "the legacy archive message ID is retained")

	row, ok := getMsg(t, st, source.ID, newSourceMessageID)
	requirements.True(ok, "the canonical recurrence key is retained")
	assertions.Equal(archivedID, row.id)
	assertions.Equal("Planning", row.subject.String)
	assertions.Equal("cancelled", parseMeta(t, row)["status"])
	assertions.Contains(bodyText(t, st, archivedID), "Retained event details")
	assertions.Equal(1, countMessages(t, st, source.ID), "the interrupted duplicate is removed")

	gotRaw, err := st.GetMessageRaw(archivedID)
	requirements.NoError(err)
	assertions.Equal(legacyRaw, gotRaw, "the complete provider snapshot is retained")
	detail, err := st.GetCalendarOccurrenceContext(t.Context(), "evt:"+strconv.FormatInt(archivedID, 10))
	requirements.NoError(err)
	assertions.Equal("Planning", detail.Subject)
	assertions.Equal("cancelled", detail.Status)
	assertions.Equal(time.Date(2027, 3, 9, 22, 0, 0, 0, time.UTC), detail.Start)
	assertions.Equal("Room 7", detail.Location)
	assertions.Equal("Retained event details", detail.Description)
	requirements.Len(detail.Attendees, 1)
	assertions.Equal("guest@example.com", detail.Attendees[0].Email)
}
