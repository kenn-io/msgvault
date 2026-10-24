package query_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	editSeriesRaw = `{"id":"series","iCalUID":"series@example.test","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]%s}`
	editMovedRaw  = `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"start":{"dateTime":%q},"end":{"dateTime":%q}%s}`
)

type editFixture struct {
	t  *testing.T
	st *store.Store
}

func newEditFixture(t *testing.T) *editFixture {
	t.Helper()
	return &editFixture{t: t, st: testutil.NewTestStore(t)}
}

func (f *editFixture) event(id, subject, raw string) int64 {
	f.t.Helper()
	source, err := f.st.GetOrCreateSource("gcal", "calendar@example.test")
	require.NoError(f.t, err)
	require.NoError(f.t, f.st.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"calendar@example.test","time_zone":"UTC"}`))
	message, err := f.st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: id, MessageType: "calendar_event", Subject: sql.NullString{String: subject, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: id, ConversationType: "calendar"},
		Metadata:     &sql.NullString{String: `{"ical_uid":"series@example.test"}`, Valid: true},
		RawFormat:    "gcal_json", RawMIME: []byte(raw),
	})
	require.NoError(f.t, err)
	return message
}

func (f *editFixture) rewrite(id int64, raw string) {
	f.t.Helper()
	require.NoError(f.t, f.st.UpsertMessageRawWithFormat(id, []byte(raw), "gcal_json"))
}

func (f *editFixture) note(name string, ref store.OccurrenceRef) int64 {
	f.t.Helper()
	source, err := f.st.GetOrCreateSource("meeting_import", "notes@example.test")
	require.NoError(f.t, err)
	id, err := f.st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: name, MessageType: "meeting_transcript"},
		Conversation: &store.ConversationPersistData{SourceConversationID: name, ConversationType: "meeting"},
		RawFormat:    "plaud_json", RawMIME: []byte(`{}`),
	})
	require.NoError(f.t, err)
	require.NoError(f.t, f.st.LinkMeetingContext(f.t.Context(), id, ref))
	return id
}

// keys exports meeting_keys for the notes and returns it with the number of
// distinct meetings it names.
func (f *editFixture) keys(notes ...int64) (map[int64]string, int) {
	f.t.Helper()
	keys, err := f.st.MeetingCountKeysContext(f.t.Context())
	require.NoError(f.t, err)
	distinct := map[string]bool{}
	for _, note := range notes {
		require.NotEmpty(f.t, keys[note], "note %d must keep a meeting key", note)
		distinct[keys[note]] = true
	}
	return keys, len(distinct)
}

// A former series retains its exact occurrence targets after recurrence is cleared.
func TestMeetingCountsFormerSeriesKeepsExactTargets(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newEditFixture(t)
	master := f.event("series", "Weekly sync", fmt.Sprintf(editSeriesRaw, ""))
	edit := f.event("edit", "Weekly sync", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	note := f.note("note", store.OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})
	before, _ := f.keys(note)
	requirements.Equal("uid:series@example.test/20270314T100000Z", before[note])

	// The series becomes a standalone at its first slot.
	f.rewrite(master, `{"id":"series","iCalUID":"series@example.test","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)

	after, _ := f.keys(note)
	assertions.Equal(before[note], after[note])
	assertions.Equal(after[note], after[edit], "the exception carries the meeting")
	assertions.NotContains(after, master, "the suppressed standalone mints no key of its own")
	standalone := f.note("standalone", store.OccurrenceRef{MessageID: master})
	keys, count := f.keys(note, standalone)
	assertions.Equal(2, count)
	assertions.Equal("uid:series@example.test/", keys[standalone])
	assertions.Equal("uid:series@example.test/", keys[master])
	assertions.Equal(before[note], keys[note])
}

// An exception inherits the cancellation of its master, and a sparse
// cancelled exception inherits the master's details. Cancelling never splits
// or re-keys the meetings the notes already identify.
func TestMeetingCountsCancellationInheritedByException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newEditFixture(t)
	master := f.event("series", "Weekly sync", fmt.Sprintf(editSeriesRaw, ""))
	edit := f.event("edit", "Stale title", fmt.Sprintf(editMovedRaw, "2027-03-21T12:00:00Z", "2027-03-21T13:00:00Z", ""))
	first := f.note("first", store.OccurrenceRef{MessageID: master, Key: "20270314T100000Z"})
	second := f.note("second", store.OccurrenceRef{MessageID: edit})
	baseline, count := f.keys(first, second)
	requirements.Equal(2, count)
	requirements.Equal("uid:series@example.test/20270314T100000Z", baseline[first])
	requirements.Equal("uid:series@example.test/20270321T100000Z", baseline[second])
	requirements.Equal(baseline[second], baseline[edit])

	// A cancelled master cancels the exception that still has its own bounds.
	f.rewrite(master, fmt.Sprintf(editSeriesRaw, `,"status":"cancelled"`))
	keys, count := f.keys(first, second)
	assertions.Equal(2, count)
	assertions.Equal(baseline[first], keys[first])
	assertions.Equal(baseline[second], keys[second])
	assertions.Equal(keys[second], keys[edit])

	// A sparse cancelled exception drops only its own slot and borrows the title.
	f.rewrite(master, fmt.Sprintf(editSeriesRaw, ""))
	f.rewrite(edit, `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled"}`)
	keys, count = f.keys(first, second)
	assertions.Equal(2, count)
	assertions.Equal(baseline[second], keys[second])
	assertions.Equal(keys[second], keys[edit])
}

// Moving, dropping and restoring an exception's bounds never changes which
// single meeting its note identifies.
func TestMeetingCountsExceptionBoundsKeepOneMeeting(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newEditFixture(t)
	master := f.event("series", "Weekly sync", fmt.Sprintf(editSeriesRaw, ""))
	edit := f.event("edit", "Stale title", fmt.Sprintf(editMovedRaw, "2027-03-21T12:00:00Z", "2027-03-21T13:00:00Z", ""))
	note := f.note("note", store.OccurrenceRef{MessageID: edit})
	baseline, _ := f.keys(note)
	requirements.Equal("uid:series@example.test/20270321T100000Z", baseline[note])

	for _, step := range []struct {
		name string
		raw  string
	}{
		{"moved again", fmt.Sprintf(editMovedRaw, "2027-03-22T14:00:00Z", "2027-03-22T15:30:00Z", "")},
		{"bounds dropped", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"}}`},
		{"bounds restored", fmt.Sprintf(editMovedRaw, "2027-03-21T12:00:00Z", "2027-03-21T13:00:00Z", "")},
	} {
		f.rewrite(edit, step.raw)
		keys, _ := f.keys(note)
		assertions.Equal(baseline[note], keys[note], step.name)
		assertions.Equal(keys[note], keys[edit], step.name)
		assertions.NotContains(keys, master, step.name)
	}
}
