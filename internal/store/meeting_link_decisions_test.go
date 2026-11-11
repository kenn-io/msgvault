package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingLinkDecisionReadsOnlyRequestedOccurrence(t *testing.T) {
	r := require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, base, "separate-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=52"]}`)
	var notes []int64
	for i := range 52 {
		_, note := projectionFixture(t, base, fmt.Sprintf("separate-note-%d", i), "meeting_json", `{}`)
		notes = append(notes, note)
		date := time.Date(2027, 3, 14+7*i, 10, 0, 0, 0, time.UTC).Format("20060102T150405Z")
		occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@%s", event, date))
		r.NoError(err)
		_, err = base.DecideMeetingLinkContext(t.Context(), occurrence, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
		r.NoError(err)
	}
	var unrelated []int64
	var sawRequested bool
	gate := &meetingProjectionGate{before: func(_ context.Context, query string, args []driver.NamedValue) error {
		if strings.Contains(query, "SELECT k.key FROM messages m") && len(args) > 0 {
			if id, ok := args[0].Value.(int64); ok {
				if id == notes[0] {
					sawRequested = true
				} else {
					unrelated = append(unrelated, id)
				}
			}
		}
		return nil
	}}
	st := gatedMeetingProjectionStore(t, base, gate)
	occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", event))
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), occurrence)
	r.NoError(err)
	r.Equal([]int64{notes[0]}, record.MessageIDs)
	r.True(sawRequested)
	r.Empty(unrelated)
	budget := 20
	loader := meetingDecisionLoader{store: st}
	_, err = loader.edges(t.Context(), endpointFromRef(occurrence), nil, &budget)
	r.NoError(err)
}

func TestMeetingDecisionScopedReadHidesOtherSource(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	visible, err := st.GetOrCreateSource("meeting_import", "visible@example.com")
	r.NoError(err)
	hidden, err := st.GetOrCreateSource("meeting_import", "hidden@example.com")
	r.NoError(err)
	var refs []MeetingLinkRef
	for i, source := range []int64{visible.ID, hidden.ID} {
		key := fmt.Sprintf("note-%d", i)
		id, err := st.PersistMessage(&MessagePersistData{Message: &Message{SourceID: source, SourceMessageID: key, MessageType: "meeting_transcript"}, Conversation: &ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		r.NoError(err)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	r.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.ID})
	record, err := st.MeetingRecordContext(ctx, refs[0])
	r.NoError(err)
	r.Equal([]int64{refs[0].MessageID}, record.MessageIDs)
}

func TestMeetingLinkDecisionLargeGroupsLinkAndSplit(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	groups := [2][]int64{}
	for group := range 2 {
		raw := fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"group%d_20270314T100000Z"}}`, group)
		count := 510
		if group == 0 {
			count = 720
		}
		for i := range count {
			_, id := projectionFixture(t, st, fmt.Sprintf("group-%d-%d", group, i), "granola_json", raw)
			groups[group] = append(groups[group], id)
		}
	}
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", MessageID: id}
	}
	for _, id := range []int64{groups[0][0], groups[0][719]} {
		record, err := st.MeetingRecordContext(t.Context(), ref(id))
		requirements.NoError(err)
		requirements.Equal(groups[0], record.MessageIDs)
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(groups[0][0]), ref(groups[1][0]), "link")
	requirements.NoError(err)
	all := append(append([]int64{}, groups[0]...), groups[1]...)
	for _, id := range []int64{groups[0][0], groups[0][1], groups[1][0], groups[1][509]} {
		record, err := st.MeetingRecordContext(t.Context(), ref(id))
		requirements.NoError(err)
		requirements.Equal(all, record.MessageIDs)
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(groups[0][0]), ref(groups[0][1]), "unlink")
	requirements.NoError(err)
	joined := append(append([]int64{groups[0][0]}, groups[0][2:]...), groups[1]...)
	for _, id := range []int64{groups[0][0], groups[0][2], groups[1][0]} {
		record, err := st.MeetingRecordContext(t.Context(), ref(id))
		requirements.NoError(err)
		requirements.Equal(joined, record.MessageIDs)
	}
	record, err := st.MeetingRecordContext(t.Context(), ref(groups[0][1]))
	requirements.NoError(err)
	requirements.Equal([]int64{groups[0][1]}, record.MessageIDs)
}

func TestMeetingLinkDecisionJoinsUnscheduledNotesAndUndoes(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, first := projectionFixture(t, st, "unscheduled-first", "meeting_json", `{"summary_text":"First"}`)
	_, second := projectionFixture(t, st, "unscheduled-second", "meeting_json", `{"summary_text":"Second"}`)
	left := MeetingLinkRef{Kind: "msg", MessageID: first}
	right := MeetingLinkRef{Kind: "msg", MessageID: second}
	result, err := st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	assertions.Equal("link", result.State)
	data.RawMIME = []byte(`{"summary_text":"updated"}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	for _, seed := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		requirements.NoError(err)
		assertions.Equal([]int64{first, second}, record.MessageIDs)
	}
	result, err = st.DecideMeetingLinkContext(t.Context(), right, left, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for _, seed := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		requirements.NoError(err)
		assertions.Equal([]int64{seed.MessageID}, record.MessageIDs)
	}
	result, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	assertions.Equal("link", result.State)
	for _, seed := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), seed)
		requirements.NoError(err)
		assertions.Equal([]int64{first, second}, record.MessageIDs)
	}
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, first)
	requirements.NoError(err)
	result, err = st.DecideMeetingLinkContext(t.Context(), left, right, "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_at=NULL WHERE id=?`, first)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), left)
	requirements.NoError(err)
	assertions.Equal([]int64{first}, record.MessageIDs)
}

func TestMeetingLinkDecisionConflictAndClear(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs []MeetingLinkRef
	for _, name := range []string{"conflict-a", "conflict-b", "conflict-c"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {1, 2}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), refs[pair[0]], refs[pair[1]], "link")
		requirements.NoError(err)
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	requirements.ErrorIs(err, ErrMeetingLinkConflict)
	requirements.ErrorContains(err, fmt.Sprintf("link msg:%d msg:%d", refs[1].MessageID, refs[2].MessageID))
	for _, ref := range refs {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID, refs[2].MessageID}, record.MessageIDs)
	}
	for range 2 {
		result, err := st.DecideMeetingLinkContext(t.Context(), refs[2], refs[1], "clear")
		requirements.NoError(err)
		assertions.Equal("automatic", result.State)
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	_, master := calendarFixture(t, st, "decision-master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, edit := calendarFixture(t, st, "decision-edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", master))
	requirements.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, MeetingLinkRef{Kind: "evt", MessageID: edit}, "link")
	requirements.ErrorIs(err, ErrMeetingLinkSameCopy)
}

func TestMeetingLinkDecisionSplitAppliesToEveryEventCopy(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	_, first := calendarFixture(t, st, "first-copy", raw, "first@example.com")
	_, second := calendarFixture(t, st, "second-copy", raw, "second@example.com")
	_, note := projectionFixture(t, st, "shared-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	member := MeetingLinkRef{Kind: "msg", MessageID: note}
	event := MeetingLinkRef{Kind: "evt", MessageID: first}
	_, err := st.DecideMeetingLinkContext(t.Context(), member, event, "link")
	requirements.NoError(err)
	result, err := st.DecideMeetingLinkContext(t.Context(), member, event, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
		requirements.NoError(err)
		assertions.Empty(record.MessageIDs)
		assertions.Len(record.Links, 2)
	}
	record, err := st.MeetingRecordContext(t.Context(), member)
	requirements.NoError(err)
	assertions.Empty(record.Links)
	assertions.Equal([]int64{note}, record.MessageIDs)
	_, err = st.DecideMeetingLinkContext(t.Context(), member, MeetingLinkRef{Kind: "evt", MessageID: second}, "link")
	requirements.NoError(err)
	for _, ref := range []MeetingLinkRef{member, {Kind: "evt", MessageID: first}, {Kind: "evt", MessageID: second}} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]int64{note}, record.MessageIDs)
		assertions.Len(record.Links, 2)
	}
}

func TestMeetingLinkDecisionUndoKeepsDuplicateChoices(t *testing.T) {
	for _, tc := range []struct {
		decision string
		joined   bool
	}{{"link", true}, {"unlink", false}} {
		t.Run(tc.decision, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var ids []int64
			for _, name := range []string{"survivor", "first-duplicate", "second-duplicate", "other"} {
				format, raw := "meeting_json", `{}`
				if !tc.joined {
					format, raw = "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"opposing_20270314T100000Z"}}`
				}
				_, id := projectionFixture(t, st, name, format, raw)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", MessageID: id}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[1]), ref(ids[3]), "link")
			requirements.NoError(err)
			_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), tc.decision)
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[0], []int64{ids[1]}, "first")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[0], []int64{ids[2]}, "second")
			requirements.NoError(err)
			_, err = st.UndoDedup(t.Context(), "first")
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			if tc.joined {
				requirements.Equal([]int64{ids[0], ids[1], ids[3]}, record.MessageIDs)
			} else {
				requirements.Equal([]int64{ids[1], ids[3]}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingLinkDecisionFollowsStandaloneIntoSeries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "growing", `{"id":"growing","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "growing-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	eventRef := MeetingLinkRef{Kind: "evt", MessageID: event}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, eventRef, "link")
	requirements.NoError(err)
	data.RawMIME = []byte(`{"id":"growing","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	requirements.Len(record.Links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314", event), record.Links[0].Ref)
	first := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270314")}
	result, err := st.DecideMeetingLinkContext(t.Context(), noteRef, first, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	assertions.Empty(record.Links)
	result, err = st.DecideMeetingLinkContext(t.Context(), noteRef, first, "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	second := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270321")}
	record, err = st.MeetingRecordContext(t.Context(), second)
	requirements.NoError(err)
	assertions.Empty(record.MessageIDs)
}

func TestMeetingLinkDecisionGroupWarnings(t *testing.T) {
	for _, decision := range []string{"link", "unlink"} {
		t.Run(decision, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			uid := "linked@example.org"
			if decision == "unlink" {
				uid = "shared@example.org"
			}
			data, event := calendarFixture(t, st, "warning-event", `{"id":"warning-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			_, note := projectionFixture(t, st, "warning-note", "twenty_json", fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":"2027-03-14T10:00:00Z"}}`, uid))
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			eventRef := MeetingLinkRef{Kind: "evt", MessageID: event}
			_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, eventRef, decision)
			requirements.NoError(err)
			data.RawMIME = []byte(`{"id":"warning-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
			_, err = st.PersistMessage(data)
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			requirements.NoError(err)
			assertions.Equal([]int64{note}, record.MessageIDs)
			if decision == "unlink" {
				assertions.Empty(record.Links)
				assertions.Empty(record.Unexpandable)
			} else {
				assertions.Equal([]CalendarUnexpandable{{MessageID: event, Reason: "unsupported recurrence"}}, record.Unexpandable)
			}
			eventRef.Identity = occurrenceIdentity(t, "20270314T100000Z")
			record, err = st.MeetingRecordContext(t.Context(), eventRef)
			requirements.NoError(err)
			assertions.Equal([]CalendarUnexpandable{{MessageID: event, Reason: "unsupported recurrence"}}, record.Unexpandable)
		})
	}
	t.Run("clear", func(t *testing.T) {
		for _, initial := range []string{"series", "single"} {
			t.Run(initial, func(t *testing.T) {
				r := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				raw := `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}`
				if initial == "series" {
					raw += `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]`
				}
				data, event := calendarFixture(t, st, "unsupported-clear", raw+`}`)
				_, note := projectionFixture(t, st, "unsupported-clear-note", "meeting_json", `{}`)
				noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
				dateRef, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", event))
				r.NoError(err)
				original := dateRef
				if initial == "single" {
					original = MeetingLinkRef{Kind: "evt", MessageID: event}
				}
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, original, "link")
				r.NoError(err)
				data.RawMIME = []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
				_, err = st.PersistMessage(data)
				r.NoError(err)
				otherDate, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", event))
				r.NoError(err)
				for _, action := range []string{"clear", "link", "unlink"} {
					_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, otherDate, action)
					r.ErrorIs(err, sql.ErrNoRows)
				}
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, dateRef, "clear")
				r.NoError(err)
				record, err := st.MeetingRecordContext(t.Context(), noteRef)
				r.NoError(err)
				r.Empty(record.Links)
				r.Empty(record.Unexpandable)
			})
		}
	})
}

func TestMeetingLinkDecisionSubsetRetainsSelectedPairs(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	data, first := projectionFixture(t, st, "subset-first", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	data, second := projectionFixture(t, st, "subset-second", "meeting_json", `{}`)
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	left := MeetingLinkRef{Kind: "msg", MessageID: first}
	right := MeetingLinkRef{Kind: "msg", MessageID: second}
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	for _, limit := range []int{1, 2} {
		dir := t.TempDir()
		_, err := CopySubset(t.Context(), path, dir, limit, false)
		requirements.NoError(err)
		dst, err := OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
		requirements.NoError(err)
		var count int
		requirements.NoError(dst.db.QueryRow(`SELECT COUNT(*) FROM meeting_link_decisions`).Scan(&count))
		assertions.Equal(limit-1, count)
		requirements.NoError(dst.Close())
	}
}

func TestMeetingLinkDecisionSubsetOmitsHiddenOrigin(t *testing.T) {
	for _, action := range []string{"link", "unlink"} {
		t.Run(action, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			path := filepath.Join(t.TempDir(), "source.db")
			st, err := OpenForTest(t.Context(), path)
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(st.Close()) })
			requirements.NoError(st.InitSchemaContext(t.Context()))
			format, raw := "meeting_json", `{}`
			if action == "unlink" {
				format, raw = "granola_json", `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"subset_20270314T100000Z"}}`
			}
			_, survivor := projectionFixture(t, st, "subset-survivor", format, raw)
			_, other := projectionFixture(t, st, "subset-other", format, raw)
			_, intermediate := projectionFixture(t, st, "subset-intermediate", format, raw)
			_, duplicate := projectionFixture(t, st, "subset-hidden", format, raw)
			dupRef := MeetingLinkRef{Kind: "msg", MessageID: duplicate}
			otherRef := MeetingLinkRef{Kind: "msg", MessageID: other}
			_, err = st.DecideMeetingLinkContext(t.Context(), dupRef, otherRef, action)
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), intermediate, []int64{duplicate}, "subset-undo")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{intermediate}, "subset-later")
			requirements.NoError(err)
			dir := t.TempDir()
			_, err = CopySubset(t.Context(), path, dir, 2, false)
			requirements.NoError(err)
			dst, err := OpenForTest(t.Context(), filepath.Join(dir, "msgvault.db"))
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(dst.Close()) })
			record, err := dst.MeetingRecordContext(t.Context(), otherRef)
			requirements.NoError(err)
			if action == "link" {
				assertions.Equal([]int64{survivor, other}, record.MessageIDs)
			} else {
				assertions.Equal([]int64{other}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingLinkDecisionSubsetSourceVersion(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	_, _ = projectionFixture(t, st, "version-note", "meeting_json", `{}`)
	_, err = st.db.ExecContext(t.Context(), `DROP TABLE meeting_link_decision_dependencies`)
	requirements.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `DROP TABLE meeting_link_decisions`)
	requirements.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE archive_metadata SET value='40' WHERE key='schema_version'`)
	requirements.NoError(err)
	_, err = CopySubset(t.Context(), path, t.TempDir(), 1, false)
	requirements.NoError(err)
}

func TestMeetingLinkDecisionFollowsOrphanExceptionToMaster(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "orphan", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	_, note := projectionFixture(t, st, "orphan-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	oldRef := MeetingLinkRef{Kind: "evt", MessageID: exception}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, oldRef, "link")
	requirements.NoError(err)
	_, master := calendarFixture(t, st, "master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	current := MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}
	record, err := st.MeetingRecordContext(t.Context(), current)
	requirements.NoError(err)
	assertions.Equal([]int64{note}, record.MessageIDs)
	result, err := st.DecideMeetingLinkContext(t.Context(), noteRef, current, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	assertions.Empty(record.Links)
	result, err = st.DecideMeetingLinkContext(t.Context(), noteRef, current, "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
}

func TestMeetingLinkDecisionThreeCopiesAgreeFromEverySeed(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"one_20270314T100000Z"}}`
	refs := make([]MeetingLinkRef, 0, 3)
	for _, key := range []string{"first", "second", "third"} {
		_, id := projectionFixture(t, st, key, "granola_json", raw)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[2], refs[0], "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	for _, ref := range refs {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID, refs[2].MessageID}, record.MessageIDs)
	}
}

func TestMeetingLinkDecisionRemappedConflictRemainsReadable(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs []MeetingLinkRef
	for _, name := range []string{"remap-a", "remap-b", "remap-c", "remap-d"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	for _, pair := range [][2]int{{0, 1}, {2, 3}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), refs[pair[0]], refs[pair[1]], "link")
		requirements.NoError(err)
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), refs[1], refs[3], "unlink")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), refs[2].MessageID, []int64{refs[0].MessageID}, "remap")
	requirements.NoError(err)
	for i, ref := range refs[1:] {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		if i < 2 {
			assertions.Equal([]int64{refs[1].MessageID, refs[2].MessageID}, record.MessageIDs)
		} else {
			assertions.Equal([]int64{refs[3].MessageID}, record.MessageIDs)
		}
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[1], refs[3], "clear")
	requirements.NoError(err)
	for _, ref := range refs[1:] {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]int64{refs[1].MessageID, refs[2].MessageID, refs[3].MessageID}, record.MessageIDs)
	}
}

func TestMeetingLinkDecisionJoinsEventsAndSplits(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "event-a", `{"id":"event-a","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "a@example.com")
	_, second := calendarFixture(t, st, "event-b", `{"id":"event-b","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "b@example.com")
	left := MeetingLinkRef{Kind: "evt", MessageID: first}
	right := MeetingLinkRef{Kind: "evt", MessageID: second}
	_, err := st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		requirements.Len(record.Links, 2)
		assertions.Equal([]string{fmt.Sprintf("evt:%d", first), fmt.Sprintf("evt:%d", second)}, []string{record.Links[0].Ref, record.Links[1].Ref})
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Len(record.Links, 1)
	}
}

func TestMeetingLinkDecisionSurvivesSourceMerge(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, survivor := projectionFixture(t, st, "same-note", "meeting_json", `{}`)
	_, other := projectionFixture(t, st, "other-note", "meeting_json", `{}`)
	source, err := st.GetOrCreateSource("meeting_import", "copy@example.com")
	requirements.NoError(err)
	sender, err := st.EnsureParticipant("speaker@example.com", "Speaker Example", "")
	requirements.NoError(err)
	data.Message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.UpsertMessage(data.Message)
	requirements.NoError(err)
	data.Conversation.SourceConversationID = "!meeting:example.org"
	_, err = st.db.ExecContext(t.Context(), `UPDATE conversations SET source_conversation_id=? WHERE id=?`, data.Conversation.SourceConversationID, data.Message.ConversationID)
	requirements.NoError(err)
	data.Message.ID = 0
	data.Message.SourceID = source.ID
	data.Message.ConversationID = 0
	duplicate, err := st.PersistMessage(data)
	requirements.NoError(err)
	dupRef := MeetingLinkRef{Kind: "msg", MessageID: duplicate}
	otherRef := MeetingLinkRef{Kind: "msg", MessageID: other}
	_, err = st.DecideMeetingLinkContext(t.Context(), dupRef, otherRef, "link")
	requirements.NoError(err)
	_, hidden := projectionFixture(t, st, "source-dedup-hidden", "meeting_json", `{}`)
	_, intermediate := projectionFixture(t, st, "source-dedup-intermediate", "meeting_json", `{}`)
	_, otherTwo := projectionFixture(t, st, "source-dedup-other", "meeting_json", `{}`)
	hiddenRef := MeetingLinkRef{Kind: "msg", MessageID: hidden}
	otherTwoRef := MeetingLinkRef{Kind: "msg", MessageID: otherTwo}
	_, err = st.DecideMeetingLinkContext(t.Context(), hiddenRef, otherTwoRef, "link")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), intermediate, []int64{hidden}, "source-dedup")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), duplicate, []int64{intermediate}, "source-dedup-second")
	requirements.NoError(err)
	_, otherThree := projectionFixture(t, st, "source-independent-other", "meeting_json", `{}`)
	_, hiddenTwo := projectionFixture(t, st, "source-independent-hidden", "meeting_json", `{}`)
	otherThreeRef := MeetingLinkRef{Kind: "msg", MessageID: otherThree}
	hiddenTwoRef := MeetingLinkRef{Kind: "msg", MessageID: hiddenTwo}
	_, err = st.DecideMeetingLinkContext(t.Context(), dupRef, otherThreeRef, "link")
	requirements.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), hiddenTwoRef, otherThreeRef, "link")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{hiddenTwo}, "source-independent")
	requirements.NoError(err)
	var destination int64
	requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT source_id FROM messages WHERE id=?`, survivor).Scan(&destination))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: destination})
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), otherRef)
	requirements.NoError(err)
	assertions.Equal([]int64{survivor, other, otherTwo, otherThree}, record.MessageIDs)
	_, err = st.UndoDedup(t.Context(), "source-dedup")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), otherRef)
	requirements.NoError(err)
	assertions.Equal([]int64{survivor, other, otherThree}, record.MessageIDs)
	record, err = st.MeetingRecordContext(t.Context(), hiddenRef)
	requirements.NoError(err)
	assertions.Equal([]int64{hidden, otherTwo}, record.MessageIDs)
	_, err = st.UndoDedup(t.Context(), "source-independent")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), otherThreeRef)
	requirements.NoError(err)
	assertions.Equal([]int64{survivor, other, otherThree, hiddenTwo}, record.MessageIDs)
}

func TestMeetingLinkDecisionMovedOccurrenceStaysOnOriginalDate(t *testing.T) {
	for _, tc := range []struct{ name, start, end, original, movedStart, movedEnd, first, second string }{
		{"timed", `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-14T11:00:00Z"}`, `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-15T12:00:00Z"}`, `{"dateTime":"2027-03-15T13:00:00Z"}`, "20270314T100000Z", "20270321T100000Z"},
		{"all-day", `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-16"}`, "20270314", "20270321"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, master := calendarFixture(t, st, "master", fmt.Sprintf(`{"id":"series","start":%s,"end":%s,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, tc.start, tc.end))
			_, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
			left := MeetingLinkRef{Kind: "msg", MessageID: note}
			first := MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, tc.first)}
			second := MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, tc.second)}
			_, err := st.DecideMeetingLinkContext(t.Context(), left, first, "link")
			requirements.NoError(err)
			calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edit","recurringEventId":"series","originalStartTime":%s,"start":%s,"end":%s}`, tc.original, tc.movedStart, tc.movedEnd))
			record, err := st.MeetingRecordContext(t.Context(), left)
			requirements.NoError(err)
			requirements.Len(record.Links, 1)
			assertions.Equal(fmt.Sprintf("evt:%d@%s", master, tc.first), record.Links[0].Ref)
			record, err = st.MeetingRecordContext(t.Context(), second)
			requirements.NoError(err)
			assertions.Empty(record.MessageIDs)
		})
	}
}

func TestMeetingLinkDecisionClearAfterBothCopiesDedup(t *testing.T) {
	for _, first := range []string{"left", "right"} {
		t.Run(first, func(t *testing.T) {
			requirements := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var ids []int64
			for _, name := range []string{"left-survivor", "left-copy", "right-survivor", "right-copy"} {
				_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", MessageID: id}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[1]), ref(ids[3]), "link")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[0], []int64{ids[1]}, "left")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[3]}, "right")
			requirements.NoError(err)
			_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[2]), "clear")
			requirements.NoError(err)
			second := "left"
			if first == "left" {
				second = "right"
			}
			_, err = st.UndoDedup(t.Context(), first)
			requirements.NoError(err)
			_, err = st.UndoDedup(t.Context(), second)
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[1]))
			requirements.NoError(err)
			requirements.Equal([]int64{ids[1]}, record.MessageIDs)
		})
	}
	t.Run("one_endpoint", func(t *testing.T) {
		requirements := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, survivor := projectionFixture(t, st, "clear-survivor", "meeting_json", `{}`)
		_, duplicate := projectionFixture(t, st, "clear-duplicate", "meeting_json", `{}`)
		_, other := projectionFixture(t, st, "clear-other", "meeting_json", `{}`)
		ref := func(id int64) MeetingLinkRef {
			return MeetingLinkRef{Kind: "msg", MessageID: id}
		}
		_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
		requirements.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "clear-batch")
		requirements.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "clear")
		requirements.NoError(err)
		_, err = st.UndoDedup(t.Context(), "clear-batch")
		requirements.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), ref(other))
		requirements.NoError(err)
		requirements.Equal([]int64{other}, record.MessageIDs)
	})
	t.Run("other_occurrence", func(t *testing.T) {
		requirements := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, survivor := projectionFixture(t, st, "date-survivor", "meeting_json", `{}`)
		_, duplicate := projectionFixture(t, st, "date-duplicate", "meeting_json", `{}`)
		_, event := calendarFixture(t, st, "date-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
		first, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", event))
		requirements.NoError(err)
		second, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", event))
		requirements.NoError(err)
		ref := func(id int64) MeetingLinkRef {
			return MeetingLinkRef{Kind: "msg", MessageID: id}
		}
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), first, "link")
		requirements.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), second, "link")
		requirements.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "date")
		requirements.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(survivor), first, "clear")
		requirements.NoError(err)
		_, err = st.UndoDedup(t.Context(), "date")
		requirements.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), ref(duplicate))
		requirements.NoError(err)
		requirements.Len(record.Links, 1)
		requirements.Equal(fmt.Sprintf("evt:%d@20270321T100000Z", event), record.Links[0].Ref)
	})
}

func TestMeetingLinkDecisionSurvivesPermanentDuplicateDeletion(t *testing.T) {
	for _, tc := range []struct{ name, decision, purge, firstUndo, timing string }{
		{"link_undo_first", "link", "", "first", ""}, {"link_undo_second", "link", "", "second", ""},
		{"split_undo_first", "unlink", "", "first", ""}, {"split_undo_second", "unlink", "", "second", ""},
		{"survivor_before", "link", "", "first", "before"}, {"survivor_after", "link", "", "first", "after"},
		{"purge_first", "link", "first", "", ""}, {"purge_second", "link", "second", "", ""},
		{"purge_all", "link", "all", "", ""}, {"purge_gc", "link", "gc", "", ""},
		{"purge_source", "link", "source", "", ""}, {"purge_source_serialized", "link", "source-serialized", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var ids []int64
			for i, name := range []string{"chain-original", "chain-intermediate", "chain-survivor", "chain-other"} {
				format, raw := "meeting_json", `{}`
				if tc.decision == "unlink" {
					key := "shared"
					if i == 0 {
						key = "separate"
					}
					format, raw = "granola_json", fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"%s_20270314T100000Z"}}`, key)
				}
				_, id := projectionFixture(t, st, name, format, raw)
				ids = append(ids, id)
			}
			var sourceID int64
			if tc.purge == "source" || tc.purge == "source-serialized" {
				source, err := st.GetOrCreateSource("meeting_import", "purge@example.com")
				r.NoError(err)
				sourceID = source.ID
				_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET source_id=? WHERE id=?`, sourceID, ids[0])
				r.NoError(err)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", MessageID: id}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), tc.decision)
			r.NoError(err)
			if tc.timing == "before" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				r.NoError(err)
			}
			_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "first")
			r.NoError(err)
			if tc.firstUndo != "" && tc.timing == "" {
				record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
				r.NoError(err)
				if tc.decision == "unlink" {
					r.Equal([]int64{ids[3]}, record.MessageIDs)
				} else {
					r.Equal([]int64{ids[1], ids[3]}, record.MessageIDs)
				}
			}
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
			r.NoError(err)
			if tc.timing == "after" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				r.NoError(err)
			}
			switch tc.purge {
			case "source":
				err = st.RemoveSource(sourceID)
			case "source-serialized":
				_, _, err = st.RemoveSourceSerialized(t.Context(), sourceID)
			case "gc":
				if st.IsPostgreSQL() {
					t.Skip("archive garbage collection is SQLite-only")
				}
				_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, ids[0])
				r.NoError(err)
				plan, err := st.PlanGCContext(t.Context())
				r.NoError(err)
				deleted, err := st.ExecuteGCContext(t.Context(), plan)
				r.NoError(err)
				r.Equal(int64(1), deleted)
				var remaining int
				r.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE id=?`, ids[0]).Scan(&remaining))
				r.Zero(remaining)
			case "all":
				_, _, err = st.DeleteAllDedupedContext(t.Context())
			case "first", "second":
				_, err = st.DeleteDedupedBatchContext(t.Context(), tc.purge)
			}
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			if tc.decision == "unlink" {
				r.Equal([]int64{ids[3]}, record.MessageIDs)
			} else {
				r.Equal([]int64{ids[2], ids[3]}, record.MessageIDs)
			}
			firstUndo := tc.firstUndo
			if tc.purge == "first" || tc.purge == "gc" || tc.purge == "source" || tc.purge == "source-serialized" {
				firstUndo = "second"
			}
			if tc.purge == "second" {
				firstUndo = "first"
			}
			if firstUndo == "" {
				return
			}
			_, err = st.UndoDedup(t.Context(), firstUndo)
			r.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			if tc.timing != "" {
				r.Equal([]int64{ids[0], ids[2], ids[3]}, record.MessageIDs)
				return
			}
			if tc.decision == "unlink" {
				if firstUndo == "first" {
					r.Equal([]int64{ids[2], ids[3]}, record.MessageIDs)
				} else {
					r.Equal([]int64{ids[3]}, record.MessageIDs)
				}
			} else if firstUndo == "first" {
				r.Equal([]int64{ids[0], ids[3]}, record.MessageIDs)
			} else {
				r.Equal([]int64{ids[1], ids[3]}, record.MessageIDs)
			}
			if tc.purge != "" {
				return
			}
			remaining := "first"
			if firstUndo == "first" {
				remaining = "second"
			}
			_, err = st.UndoDedup(t.Context(), remaining)
			r.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			if tc.decision == "unlink" {
				r.Equal([]int64{ids[1], ids[2], ids[3]}, record.MessageIDs)
			} else {
				r.Equal([]int64{ids[0], ids[3]}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingLinkDecisionConcurrentContradiction(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL transaction contract")
	}
	var refs []MeetingLinkRef
	for _, key := range []string{"concurrent-a", "concurrent-b", "concurrent-c"} {
		_, id := projectionFixture(t, st, key, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	blocker, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var pid int
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	var locked int64
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=? FOR UPDATE`, refs[2].MessageID).Scan(&locked))
	type outcome struct {
		index int
		err   error
	}
	done := make(chan outcome, 2)
	for i, decision := range []string{"unlink", "link"} {
		workers.Go(func() {
			_, err := st.DecideMeetingLinkContext(ctx, refs[i], refs[2], decision)
			done <- outcome{i, err}
		})
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked int
		requirements.NoError(st.db.QueryRowContext(ctx, `WITH RECURSIVE waiting(pid) AS (SELECT pid FROM pg_stat_activity WHERE ?=ANY(pg_blocking_pids(pid)) UNION SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid=ANY(pg_blocking_pids(a.pid))) SELECT count(*) FROM waiting`, pid).Scan(&blocked))
		if blocked >= 2 {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			requirements.FailNow("decisions did not overlap")
		}
	}
	requirements.NoError(blocker.Commit())
	results := []outcome{<-done, <-done}
	var accepted, rejected, successes int
	for _, result := range results {
		if result.err == nil {
			successes++
			accepted = result.index
		} else {
			requirements.ErrorIs(result.err, ErrMeetingLinkConflict)
			rejected = result.index
		}
	}
	requirements.Equal(1, successes, "one decision must succeed and one must conflict")
	for i, ref := range refs {
		record, err := st.MeetingRecordContext(ctx, ref)
		requirements.NoError(err)
		want := []int64{refs[0].MessageID, refs[1].MessageID}
		if accepted == 1 {
			want = append(want, refs[2].MessageID)
		} else if i == 2 {
			want = []int64{refs[2].MessageID}
		}
		requirements.Equal(want, record.MessageIDs)
	}
	var count int
	requirements.NoError(st.db.QueryRowContext(ctx, `SELECT count(*) FROM meeting_link_decisions WHERE left_message_id=? AND right_message_id=?`, refs[rejected].MessageID, refs[2].MessageID).Scan(&count))
	requirements.Zero(count)
	_, err = st.DecideMeetingLinkContext(ctx, refs[rejected], refs[2], []string{"unlink", "link"}[rejected])
	requirements.ErrorIs(err, ErrMeetingLinkConflict)
}

func TestMeetingLinkDecisionConcurrentClearAndDedup(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL transaction contract")
	}
	_, survivor := projectionFixture(t, st, "concurrent-survivor", "meeting_json", `{}`)
	_, duplicate := projectionFixture(t, st, "concurrent-duplicate", "meeting_json", `{}`)
	_, other := projectionFixture(t, st, "concurrent-other", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", MessageID: id}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	var locked int64
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=? FOR UPDATE`, duplicate).Scan(&locked))
	type outcome struct {
		clear bool
		err   error
	}
	done := make(chan outcome, 2)
	go func() {
		_, err := st.DecideMeetingLinkContext(ctx, ref(duplicate), ref(other), "clear")
		done <- outcome{clear: true, err: err}
	}()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	waitBlocked := func(count int) {
		for {
			var blocked int
			requirements.NoError(st.db.QueryRowContext(ctx, `WITH RECURSIVE waiting(pid) AS (SELECT pid FROM pg_stat_activity WHERE ?=ANY(pg_blocking_pids(pid)) UNION SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid=ANY(pg_blocking_pids(a.pid))) SELECT count(*) FROM waiting`, blockerPID).Scan(&blocked))
			if blocked >= count {
				return
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				requirements.FailNow("decision and dedup did not overlap")
			}
		}
	}
	waitBlocked(1)
	go func() {
		_, err := st.MergeDuplicates(ctx, survivor, []int64{duplicate}, "concurrent")
		done <- outcome{err: err}
	}()
	waitBlocked(2)
	_, err = blocker.ExecContext(ctx, `UPDATE messages SET subject=subject WHERE id=?`, duplicate)
	requirements.NoError(err)
	requirements.NoError(blocker.Commit())
	results := []outcome{<-done, <-done}
	for _, result := range results {
		if st.dialect.IsSerializationFailureError(result.err) || errors.Is(result.err, ErrMeetingLinkConflict) {
			if result.clear {
				_, result.err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "clear")
			} else {
				_, result.err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "concurrent")
			}
		}
		requirements.NoError(result.err)
	}
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	requirements.NoError(err)
	requirements.Equal([]int64{other}, record.MessageIDs)
	_, err = st.UndoDedup(t.Context(), "concurrent")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(other))
	requirements.NoError(err)
	requirements.Equal([]int64{other}, record.MessageIDs)
}

func TestMeetingLinkDecisionInterleavedCleanupUndo(t *testing.T) {
	for _, tc := range []struct {
		name, purge string
		reset       bool
	}{{"purge_intermediate", "second", false}, {"purge_opposite", "opposite", false}, {"purge_opposite_reset", "opposite", true}} {
		t.Run(tc.name, func(t *testing.T) { checkMeetingInterleavedCleanupUndo(t, tc.purge, tc.reset) })
	}
}

func checkMeetingInterleavedCleanupUndo(t *testing.T, purge string, reset bool) {
	t.Helper()
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var ids []int64
	for _, name := range []string{"history-a", "history-b", "history-c", "history-x", "history-y"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		ids = append(ids, id)
	}
	ref := func(i int) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", MessageID: ids[i]}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(0), ref(3), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "first")
	r.NoError(err)
	if purge == "opposite" {
		_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
		r.NoError(err)
	}
	_, err = st.MergeDuplicates(t.Context(), ids[4], []int64{ids[3]}, "opposite")
	r.NoError(err)
	if purge == "second" {
		_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
		r.NoError(err)
	}
	_, err = st.DeleteDedupedBatchContext(t.Context(), purge)
	r.NoError(err)
	if reset {
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(2), ref(4), "clear")
		r.NoError(err)
	}
	_, err = st.UndoDedup(t.Context(), "first")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(4))
	r.NoError(err)
	if reset {
		r.Equal([]int64{ids[4]}, record.MessageIDs)
	} else {
		r.Equal([]int64{ids[0], ids[4]}, record.MessageIDs)
	}
	remaining := "second"
	if purge == "second" {
		remaining = "opposite"
	}
	_, err = st.UndoDedup(t.Context(), remaining)
	r.NoError(err)
	seed := 4
	if remaining == "opposite" {
		seed = 3
	}
	record, err = st.MeetingRecordContext(t.Context(), ref(seed))
	r.NoError(err)
	if reset {
		r.Equal([]int64{ids[seed]}, record.MessageIDs)
	} else {
		r.Equal([]int64{ids[0], ids[seed]}, record.MessageIDs)
	}
}

func TestMeetingLinkDecisionDiscoversSavedDates(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "moving", `{"id":"moving","iCalUID":"moving@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, a := projectionFixture(t, st, "moving-a", "meeting_json", `{}`)
	_, b := projectionFixture(t, st, "moving-b", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", MessageID: id}
	}
	eventRef := MeetingLinkRef{Kind: "evt", MessageID: event}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(a), eventRef, "link")
	r.NoError(err)
	data.RawMIME = []byte(`{"id":"moving","iCalUID":"moving@example.org","start":{"dateTime":"2027-03-21T10:00:00Z"},"end":{"dateTime":"2027-03-21T11:00:00Z"}}`)
	_, err = st.PersistMessage(data)
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(b), eventRef, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(a), ref(b), "link")
	r.NoError(err)
	data.RawMIME = []byte(`{"id":"moving","iCalUID":"moving@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, err = st.PersistMessage(data)
	r.NoError(err)
	_, c := projectionFixture(t, st, "moving-c", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"moving@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	first, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", event))
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(c), first, "unlink")
	r.NoError(err)
	for _, id := range []int64{a, b} {
		record, err := st.MeetingRecordContext(t.Context(), ref(id))
		r.NoError(err)
		r.Equal([]int64{a, b}, record.MessageIDs)
		r.Len(record.Links, 2)
		r.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", event), record.Links[0].Ref)
		r.Equal(fmt.Sprintf("evt:%d@20270321T100000Z", event), record.Links[1].Ref)
	}
}
