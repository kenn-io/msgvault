package timeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovedCalendarRetainsArchivedTitle(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	id := f.calendar("removed", singleDayEvent)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), f.now, id)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.True(entries[0].Meeting.DeletedFromSource)
	assertions.Equal("Planning", entries[0].Meeting.Title.Text)
	assertions.False(entries[0].HiddenText)
}
