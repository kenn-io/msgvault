package calsync

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
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
