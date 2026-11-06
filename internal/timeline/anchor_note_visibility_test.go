package timeline

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type failedPersonLookup struct {
	PeopleSource

	err error
}

func (s failedPersonLookup) ResolvePersonReferenceContext(context.Context, store.PersonRefKind, string, []int64, int) ([]store.PersonCandidate, error) {
	return nil, s.err
}

func TestAnchorPersonLookupFailureStopsRequest(t *testing.T) {
	f := newTimelineFixture(t)
	event := f.calendar("planning", singleDayEvent)
	failure := errors.New("person reader unavailable")
	f.src.People = failedPersonLookup{PeopleSource: f.src.People, err: failure}
	req := anchorRequest(fmt.Sprintf("evt:%d", event))
	req.With = "person:7"
	got, err := Build(t.Context(), f.src, req)
	require := require.New(t)
	require.ErrorIs(err, failure)
	assert := assert.New(t)
	assert.Nil(got)
}

func TestCalendarMissingDisplayKeepsTextSuppressed(t *testing.T) {
	for _, missing := range []string{"failed read", "missing row"} {
		t.Run(missing, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			event := f.calendar("planning", singleDayEvent)
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET hidden_text_present=TRUE WHERE id=?`), event)
			requirements.NoError(err)
			if missing == "failed read" {
				f.src.Calendar = missingCalendarDisplay{CalendarSource: f.src.Calendar, err: context.DeadlineExceeded}
			} else {
				f.src.Calendar = missingCalendarDisplay{CalendarSource: f.src.Calendar, id: event}
			}
			got, err := Build(t.Context(), f.src, dayRequest())
			requirements.NoError(err)
			entries := allDayEntries(got)
			requirements.Len(entries, 1)
			assertions.True(entries[0].HiddenText)
			assertions.Empty(entries[0].Meeting.Title.Text)
			assertions.NotContains(Markdown(got), "Planning")
		})
	}
}

type missingCalendarDisplay struct {
	CalendarSource

	err error
	id  int64
}

func (s missingCalendarDisplay) CalendarMessageDisplayContext(ctx context.Context, ids []int64) (map[int64]store.MessageDisplay, error) {
	if s.err != nil {
		return nil, s.err
	}
	rows, err := s.CalendarSource.CalendarMessageDisplayContext(ctx, ids)
	delete(rows, s.id)
	return rows, err
}

func TestEventAnchorSurvivesTierFilter(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	event := f.calendar("planning", singleDayEvent)
	req := anchorRequest(fmt.Sprintf("evt:%d", event))
	req.Tiers = []int{1}
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.True(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.True(got.History[0].Anchor)
	assertions.True(got.History[0].Filtered)
	assertions.Equal(fmt.Sprintf("evt:%d", event), got.History[0].Ref)
	assertions.Equal("meeting", got.History[0].Kind)
	requirements.NotEmpty(got.History[0].Open)
	assertions.Equal("get_calendar_event", got.History[0].Open[0].Tool)
}

func TestDailyNoteHasNoToolTarget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	note, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Synthetic note", Author: "owner"})
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	requirements.Len(got.Days[0].Notes, 1)
	assertions.Equal(fmt.Sprintf("note:%d", note.ID), got.Days[0].Notes[0].Key)
	assertions.Empty(got.Days[0].Notes[0].Open)
}

func TestDailyNoteDetailBeyondFirstPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var id int64
	for range 101 {
		note, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Full synthetic note", Author: "owner"})
		requirements.NoError(err)
		id = note.ID
	}
	note, err := ReadDailyNote(t.Context(), f.src.Notes, "2026-10-22", id)
	requirements.NoError(err)
	requirements.NotNil(note)
	assertions.Equal(id, note.ID)
	assertions.Equal("Full synthetic note", note.Body)
	_, err = ReadDailyNote(t.Context(), f.src.Notes, "2026-10-21", id)
	assertions.ErrorIs(err, store.ErrDailyNoteEntryNotFound)
}
