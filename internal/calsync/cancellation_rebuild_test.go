package calsync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gcal/gcalmodel"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarSeenCancellationRebuild(t *testing.T) {
	for _, subset := range []bool{false, true} {
		name := "archive"
		if subset {
			name = "subset"
		}
		t.Run(name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			if subset && store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
				t.Skip("SQLite subset copy")
			}
			s, st := newSyncer(t, gcal.NewMockAPI(), Options{})
			var sourcePath string
			if subset {
				sourcePath = filepath.Join(t.TempDir(), "source.db")
				var err error
				st, err = store.OpenForTest(sourcePath)
				requirements.NoError(err)
				t.Cleanup(func() { assertions.NoError(st.Close()) })
				requirements.NoError(st.InitSchemaContext(t.Context()))
				s = New(gcal.NewMockAPI(), st, Options{AccountEmail: testAccount}).WithLogger(quietLogger())
			}
			src, err := st.GetOrCreateSource("gcal", "calendar@example.com")
			requirements.NoError(err)
			cal := gcal.Calendar{ID: "primary", TimeZone: "UTC"}
			for _, raw := range []string{
				`{"id":"master","summary":"Master title","start":{"dateTime":"2027-03-02T09:00:00Z"},"end":{"dateTime":"2027-03-02T10:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"],"attendees":[{"email":"master@example.com"}]}`,
				`{"id":"exception","summary":"Edited title","recurringEventId":"master","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"start":{"dateTime":"2027-03-09T12:00:00Z"},"end":{"dateTime":"2027-03-09T14:00:00Z"},"attendees":[{"email":"edited@example.com","responseStatus":"accepted"}]}`,
			} {
				ev, err := gcalmodel.DecodeEvent([]byte(raw))
				requirements.NoError(err)
				_, err = s.ingestEvent(src.ID, cal, ev)
				requirements.NoError(err)
			}
			ev, err := gcalmodel.DecodeEvent([]byte(`{"id":"exception","recurringEventId":"master","originalStartTime":{"dateTime":"2027-03-09T09:00:00Z"},"status":"cancelled"}`))
			requirements.NoError(err)
			id, inserted, err := s.flagCancelled(src.ID, cal, ev)
			requirements.NoError(err)
			assertions.False(inserted)
			check := func(st *store.Store) {
				t.Helper()
				from := time.Date(2027, 3, 9, 0, 0, 0, 0, time.UTC)
				page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
				requirements.NoError(err)
				requirements.Len(page.Occurrences, 1)
				got := page.Occurrences[0]
				assertions.Equal(id, got.MessageID)
				assertions.Equal("cancelled", got.Status)
				assertions.Equal("Edited title", got.Subject)
				assertions.True(from.Add(12 * time.Hour).Equal(got.Start))
				assertions.True(from.Add(14 * time.Hour).Equal(got.End))
				requirements.Len(got.Attendees, 1)
				assertions.Equal("edited@example.com", got.Attendees[0].Email)
			}
			check(st)
			_, err = st.DB().Exec(`UPDATE calendar_events SET projection_version = 0`)
			requirements.NoError(err)
			_, err = st.DB().Exec(`DELETE FROM applied_migrations WHERE name = 'calendar_event_projection_v1'`)
			requirements.NoError(err)
			requirements.NoError(st.InitSchemaContext(t.Context()))
			check(st)
			if subset {
				requirements.NoError(st.Close())
				dest := filepath.Join(t.TempDir(), "subset")
				_, err = store.CopySubset(sourcePath, dest, 100, false)
				requirements.NoError(err)
				copied, err := store.OpenForTest(filepath.Join(dest, "msgvault.db"))
				requirements.NoError(err)
				defer func() { assertions.NoError(copied.Close()) }()
				check(copied)
			}
		})
	}
}
