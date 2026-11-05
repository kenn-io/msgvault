package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func timelineContactQuery(f *timelineActivityFixture) store.ContactQuery {
	return store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), Location: time.UTC}
}
func TestMessageAggregateCountsOnlyCountedInScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(-2, 0, 0), false, "")
	f.add(t, f.ConvID, f.Source.ID, q.Now, true, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
	assertions.Equal(1, got.InitiatedByContact)
	assertions.Equal(1, got.InitiatedByOwner)
	requirements.NotNil(got.FirstAt)
	assertions.Equal(q.Now.AddDate(-2, 0, 0), *got.FirstAt)
	assertions.Equal([]store.ActivityChannel{store.ChannelEmail}, got.Channels)
	got, err = f.Store.MessageContactAggregateContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), q)
	requirements.NoError(err)
	assertions.Zero(got.Count)
	assertions.Nil(got.FirstAt)
}
func TestMessageAggregateInitiationUsesSevenDayGap(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for _, v := range []struct {
		days int
		out  bool
	}{{-30, false}, {-24, true}, {-17, true}, {-9, false}} {
		f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(0, 0, v.days), v.out, "")
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 2, got.InitiatedByContact)
	assert.Equal(t, 1, got.InitiatedByOwner)
}
func TestMessageAggregateMonthlyRowEndsAtNow(t *testing.T) {
	for _, tc := range []struct {
		zone string
		year int
		want [12]int
	}{
		{"UTC", 2026, [12]int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"America/New_York", 2026, [12]int{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"Asia/Tokyo", 2026, [12]int{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"America/Asuncion", 2023, [12]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newTimelineActivityFixture(t)
			loc, err := time.LoadLocation(tc.zone)
			require.NoError(err)
			q := timelineContactQuery(f)
			q.Location = loc
			q.Now = time.Date(tc.year, 10, 22, 12, 0, 0, 0, loc)
			start := time.Date(2025, 11, 1, 0, 0, 0, 0, loc)
			month := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
			ats := []time.Time{start.Add(-time.Minute), start, start.AddDate(0, 0, 8), month.Add(-time.Minute), month}
			switch tc.zone {
			case "UTC":
				ats = []time.Time{time.Date(2025, 10, 31, 23, 0, 0, 0, time.UTC), start, month.Add(-time.Hour), q.Now, q.Now.AddDate(0, 1, 0)}
			case "America/Asuncion":
				// October starts at 01:00 after a midnight clock jump.
				ats = []time.Time{time.Date(2023, 10, 1, 3, 30, 0, 0, time.UTC), time.Date(2023, 10, 1, 4, 30, 0, 0, time.UTC)}
			}
			for i, at := range ats {
				f.add(t, f.ConvID, f.Source.ID, at, tc.zone != "UTC" && (i == 2 || tc.zone == "America/Asuncion" && i == 1), "")
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(err)
			assert.Equal(tc.want, got.Monthly)
			if tc.zone == "America/New_York" || tc.zone == "Asia/Tokyo" {
				assert.Equal(5, got.Count)
				assert.Equal(2, got.InitiatedByContact)
				assert.Equal(1, got.InitiatedByOwner)
			}
		})
	}
}

func TestAttendedTranscriptIDsByRole(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for i := range 3 {
		id := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(time.Duration(i-3)*time.Minute), false, "")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), id)
		requirements.NoError(err)
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Len(got, 2)
	assertions.True(truncated)
	q.Subject.ParticipantIDs = []int64{f.owner}
	got, _, err = f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Empty(got)
}
func TestCadenceForPersonUsesSuppliedLastContact(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	// The existing attribute writer validates and stores a real integer frequency.
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	storedLast := now.AddDate(0, 0, -1)
	f.add(t, f.ConvID, f.Source.ID, storedLast, false, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	stored, err := f.Store.ContactStateContext(t.Context(), person.ID, now)
	requirements.NoError(err)
	requirements.Equal(&storedLast, stored.LastContactAt)
	for _, tc := range []struct {
		days int
		want string
	}{{30, store.CadenceOverdue}, {10, store.CadenceOK}} {
		last := now.AddDate(0, 0, -tc.days)
		status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, &last, now)
		requirements.NoError(err)
		assertions.Equal(tc.want, status)
		requirements.NotNil(due)
		assertions.Equal(last.AddDate(0, 0, 21), *due)
	}
}

func TestContactReadersDiscardFutureCopyBeforeRanking(t *testing.T) {
	for _, transcript := range []bool{false, true} {
		t.Run(fmt.Sprintf("transcript=%t", transcript), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			past := q.Now.Add(-time.Hour)
			// The lower-ID future copy wins the normal tie between equal threads.
			futureID := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(time.Hour), false, "<copy@example.com>")
			other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(other.ID, "past-copy", "Past copy")
			require.NoError(err)
			pastID := f.add(t, conv, other.ID, past, false, "<copy@example.com>")
			if transcript {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id IN (?,?)`), futureID, pastID)
				require.NoError(err)
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			if transcript {
				ids, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
				require.NoError(err)
				assert.Equal([]int64{pastID}, ids)
				assert.False(truncated)
				return
			}
			got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(err)
			assert.Equal(1, got.Count)
			assert.Equal(&past, got.FirstAt)
			assert.Equal(&past, got.LastAt)
			assert.Equal(&past, got.LastInboundAt)
			assert.Equal(1, got.Monthly[11])
		})
	}
}
