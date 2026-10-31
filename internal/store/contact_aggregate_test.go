package store_test

import (
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
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
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
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 2, got.InitiatedByContact)
	assert.Equal(t, 1, got.InitiatedByOwner)
}
func TestMessageAggregateMonthlyRowEndsAtNow(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for _, at := range []time.Time{time.Date(2025, 10, 31, 23, 0, 0, 0, time.UTC), time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC), q.Now, q.Now.AddDate(0, 1, 0)} {
		f.add(t, f.ConvID, f.Source.ID, at, false, "")
	}
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, [12]int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}, got.Monthly)
}
func TestMessageAggregateUnsavedCluster(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	projectTimelineActivity(t, f, "UTC")
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, got.Count)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	saved, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(got, saved)
}
func TestMessageAggregateIgnoresIncludeNoise(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), id)
	require.NoError(t, err)
	projectTimelineActivity(t, f, "UTC")
	q.Filters.IncludeNoise = true
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Zero(t, got.Count)
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
	projectTimelineActivity(t, f, "UTC")
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
	for _, tc := range []struct {
		days int
		want string
	}{{30, "overdue"}, {10, "ok"}} {
		last := now.AddDate(0, 0, -tc.days)
		status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, &last, now)
		requirements.NoError(err)
		assertions.Equal(tc.want, status)
		requirements.NotNil(due)
		assertions.Equal(last.AddDate(0, 0, 21), *due)
	}
	status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, nil, now)
	requirements.NoError(err)
	assertions.Equal("unknown", status)
	assertions.Nil(due)
}
