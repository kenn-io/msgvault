package calsync

import (
	"context"
	"encoding/json/v2"
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
