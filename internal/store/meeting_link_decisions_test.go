package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingLinkDecisionLargeAutomaticGroup(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"large_20270314T100000Z"}}`
	ids := make([]int64, 0, 720)
	for i := range 720 {
		_, id := projectionFixture(t, st, fmt.Sprintf("large-%d", i), "granola_json", raw)
		ids = append(ids, id)
	}
	for _, id := range []int64{ids[0], ids[len(ids)-1]} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}})
		requirements.NoError(err)
		requirements.Equal(ids, record.MessageIDs)
	}
}

func TestMeetingLinkDecisionLargeGroupsLinkAndSplit(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	groups := [2][]int64{}
	for group := range 2 {
		raw := fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"group%d_20270314T100000Z"}}`, group)
		for i := range 510 {
			_, id := projectionFixture(t, st, fmt.Sprintf("group-%d-%d", group, i), "granola_json", raw)
			groups[group] = append(groups[group], id)
		}
	}
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
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
	_, first := projectionFixture(t, st, "unscheduled-first", "meeting_json", `{"summary_text":"First"}`)
	_, second := projectionFixture(t, st, "unscheduled-second", "meeting_json", `{"summary_text":"Second"}`)
	left := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: first}}
	right := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: second}}
	result, err := st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	assertions.Equal("link", result.State)
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
}

func TestMeetingLinkDecisionConflictAndClear(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs []MeetingLinkRef
	for _, name := range []string{"conflict-a", "conflict-b", "conflict-c"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}})
	}
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {1, 2}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), refs[pair[0]], refs[pair[1]], "link")
		requirements.NoError(err)
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	assertions.ErrorIs(err, ErrMeetingLinkConflict)
	assertions.ErrorContains(err, fmt.Sprintf("link msg:%d msg:%d", refs[1].MessageID, refs[2].MessageID))
	for _, ref := range refs {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID, refs[2].MessageID}, record.MessageIDs)
	}
	for i := 0; i < 2; i++ {
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
}

func TestMeetingLinkDecisionSplitAppliesToEveryEventCopy(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	_, first := calendarFixture(t, st, "first-copy", raw, "first@example.com")
	_, second := calendarFixture(t, st, "second-copy", raw, "second@example.com")
	_, note := projectionFixture(t, st, "shared-note", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	member := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	event := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: first}}
	_, err := st.DecideMeetingLinkContext(t.Context(), member, event, "link")
	requirements.NoError(err)
	result, err := st.DecideMeetingLinkContext(t.Context(), member, event, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for _, id := range []int64{first, second} {
		record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: id}})
		requirements.NoError(err)
		assertions.Empty(record.MessageIDs)
		assertions.Len(record.Links, 2)
	}
	record, err := st.MeetingRecordContext(t.Context(), member)
	requirements.NoError(err)
	assertions.Empty(record.Links)
	assertions.Equal([]int64{note}, record.MessageIDs)
	_, err = st.DecideMeetingLinkContext(t.Context(), member, MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: second}}, "link")
	assertions.ErrorIs(err, ErrMeetingLinkConflict)
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
				_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
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

func TestMeetingLinkDecisionChainedDedupUndo(t *testing.T) {
	for _, firstUndo := range []string{"first", "second"} {
		t.Run(firstUndo, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			ids := make([]int64, 0, 4)
			for _, name := range []string{"chain-a", "chain-b", "chain-c", "chain-d"} {
				_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), "link")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "first")
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[1], ids[3]}, record.MessageIDs)
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[2], ids[3]}, record.MessageIDs)
			_, err = st.UndoDedup(t.Context(), firstUndo)
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			if firstUndo == "first" {
				assertions.Equal([]int64{ids[0], ids[3]}, record.MessageIDs)
			} else {
				assertions.Equal([]int64{ids[1], ids[3]}, record.MessageIDs)
			}
			remaining := "first"
			if firstUndo == "first" {
				remaining = "second"
			}
			_, err = st.UndoDedup(t.Context(), remaining)
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[0], ids[3]}, record.MessageIDs)
		})
	}
}

func TestMeetingLinkDecisionChainedSurvivorChoices(t *testing.T) {
	for _, timing := range []string{"before", "after"} {
		t.Run(timing, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var ids []int64
			for _, name := range []string{"owned-a", "owned-b", "owned-c", "owned-d"} {
				_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), "link")
			requirements.NoError(err)
			if timing == "before" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				requirements.NoError(err)
			}
			_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "owned-first")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "owned-second")
			requirements.NoError(err)
			if timing == "after" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				requirements.NoError(err)
			}
			_, err = st.UndoDedup(t.Context(), "owned-first")
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[0], ids[2], ids[3]}, record.MessageIDs)
		})
	}
}

func TestMeetingLinkDecisionChainedUndoAfterIntermediateDeletion(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var ids []int64
	for _, name := range []string{"delete-a", "delete-b", "delete-c", "delete-d"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		ids = append(ids, id)
	}
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), "link")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "delete-first")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "delete-second")
	requirements.NoError(err)
	_, err = st.DeleteDedupedBatchContext(t.Context(), "delete-second")
	requirements.NoError(err)
	_, err = st.UndoDedup(t.Context(), "delete-first")
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
	requirements.NoError(err)
	assertions.Equal([]int64{ids[0], ids[3]}, record.MessageIDs)
}

func TestMeetingLinkDecisionChainedSplitUndo(t *testing.T) {
	for _, firstUndo := range []string{"first", "second"} {
		t.Run(firstUndo, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var ids []int64
			for i, name := range []string{"split-a", "split-b", "split-c", "split-d"} {
				key := "shared"
				if i == 0 {
					key = "separate"
				}
				raw := fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"%s_20270314T100000Z"}}`, key)
				_, id := projectionFixture(t, st, name, "granola_json", raw)
				ids = append(ids, id)
			}
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), "unlink")
			requirements.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "split-first")
			requirements.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[3]}, record.MessageIDs)
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "split-second")
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[3]}, record.MessageIDs)
			_, err = st.UndoDedup(t.Context(), "split-"+firstUndo)
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			if firstUndo == "first" {
				assertions.Equal([]int64{ids[2], ids[3]}, record.MessageIDs)
			} else {
				assertions.Equal([]int64{ids[3]}, record.MessageIDs)
			}
			remaining := "split-first"
			if firstUndo == "first" {
				remaining = "split-second"
			}
			_, err = st.UndoDedup(t.Context(), remaining)
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			requirements.NoError(err)
			assertions.Equal([]int64{ids[1], ids[2], ids[3]}, record.MessageIDs)
		})
	}
}

func TestMeetingLinkDecisionFollowsStandaloneIntoSeries(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "growing", `{"id":"growing","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`)
	_, note := projectionFixture(t, st, "growing-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	eventRef := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, eventRef, "link")
	requirements.NoError(err)
	data.RawMIME = []byte(`{"id":"growing","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	requirements.Len(record.Links, 1)
	assertions.Equal(fmt.Sprintf("evt:%d@20270314", event), record.Links[0].Ref)
	first := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event, Identity: occurrenceIdentity(t, "20270314")}}
	result, err := st.DecideMeetingLinkContext(t.Context(), noteRef, first, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	assertions.Empty(record.Links)
	result, err = st.DecideMeetingLinkContext(t.Context(), noteRef, first, "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	second := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event, Identity: occurrenceIdentity(t, "20270321")}}
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
			noteRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
			eventRef := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: event}}
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
	left := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: first}}
	right := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: second}}
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
			dupRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: duplicate}}
			otherRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: other}}
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
			_, err = st.UndoDedup(t.Context(), "subset-undo")
			requirements.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), otherRef)
			requirements.NoError(err)
			if action == "link" {
				assertions.Equal([]int64{other, duplicate}, record.MessageIDs)
			} else {
				assertions.Equal([]int64{survivor, other}, record.MessageIDs)
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
	_, err = st.db.ExecContext(t.Context(), `DROP TABLE meeting_link_decisions`)
	requirements.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE archive_metadata SET value='41' WHERE key='schema_version'`)
	requirements.NoError(err)
	_, err = CopySubset(t.Context(), path, t.TempDir(), 1, false)
	requirements.NoError(err)
}

func TestMeetingLinkDecisionFollowsOrphanExceptionToMaster(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, exception := calendarFixture(t, st, "orphan", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	_, note := projectionFixture(t, st, "orphan-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
	oldRef := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: exception}}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, oldRef, "link")
	requirements.NoError(err)
	_, master := calendarFixture(t, st, "master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	current := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}}
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
		refs = append(refs, MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}})
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for i, ref := range refs {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		if i < 2 {
			assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID}, record.MessageIDs)
		} else {
			assertions.Equal([]int64{refs[2].MessageID}, record.MessageIDs)
		}
	}
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
		refs = append(refs, MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}})
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

func TestMeetingLinkDecisionSurvivesSyncAndDeletion(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, first := projectionFixture(t, st, "refresh-first", "meeting_json", `{}`)
	_, second := projectionFixture(t, st, "refresh-second", "meeting_json", `{}`)
	left := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: first}}
	right := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: second}}
	_, err := st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	data.RawMIME = []byte(`{"summary_text":"updated"}`)
	_, err = st.PersistMessage(data)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), right)
	requirements.NoError(err)
	assertions.Equal([]int64{first, second}, record.MessageIDs)
	_, err = st.db.ExecContext(t.Context(), `DELETE FROM messages WHERE id=?`, first)
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), right)
	requirements.NoError(err)
	assertions.Equal([]int64{second}, record.MessageIDs)
	var count int
	requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingLinkDecisionJoinsEventsAndSplits(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := calendarFixture(t, st, "event-a", `{"id":"event-a","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "a@example.com")
	_, second := calendarFixture(t, st, "event-b", `{"id":"event-b","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "b@example.com")
	left := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: first}}
	right := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: second}}
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
	dupRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: duplicate}}
	otherRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: other}}
	_, err = st.DecideMeetingLinkContext(t.Context(), dupRef, otherRef, "link")
	requirements.NoError(err)
	_, hidden := projectionFixture(t, st, "source-dedup-hidden", "meeting_json", `{}`)
	_, intermediate := projectionFixture(t, st, "source-dedup-intermediate", "meeting_json", `{}`)
	_, otherTwo := projectionFixture(t, st, "source-dedup-other", "meeting_json", `{}`)
	hiddenRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: hidden}}
	otherTwoRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: otherTwo}}
	_, err = st.DecideMeetingLinkContext(t.Context(), hiddenRef, otherTwoRef, "link")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), intermediate, []int64{hidden}, "source-dedup")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), duplicate, []int64{intermediate}, "source-dedup-second")
	requirements.NoError(err)
	_, otherThree := projectionFixture(t, st, "source-independent-other", "meeting_json", `{}`)
	_, hiddenTwo := projectionFixture(t, st, "source-independent-hidden", "meeting_json", `{}`)
	otherThreeRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: otherThree}}
	hiddenTwoRef := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: hiddenTwo}}
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
			left := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: note}}
			first := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: master, Identity: occurrenceIdentity(t, tc.first)}}
			second := MeetingLinkRef{Kind: "evt", OccurrenceRef: OccurrenceRef{MessageID: master, Identity: occurrenceIdentity(t, tc.second)}}
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

func TestMeetingLinkDecisionClearAfterDedup(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := projectionFixture(t, st, "clear-survivor", "meeting_json", `{}`)
	_, duplicate := projectionFixture(t, st, "clear-duplicate", "meeting_json", `{}`)
	_, other := projectionFixture(t, st, "clear-other", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
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
}

func TestMeetingLinkDecisionOpposingDuplicateChoices(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var ids []int64
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"opposing_20270314T100000Z"}}`
	for _, name := range []string{"survivor", "linked-copy", "split-copy", "other"} {
		_, id := projectionFixture(t, st, name, "granola_json", raw)
		ids = append(ids, id)
	}
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[1]), ref(ids[3]), "link")
	requirements.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "unlink")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[0], []int64{ids[1]}, "linked")
	requirements.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[0], []int64{ids[2]}, "split")
	requirements.NoError(err)
	_, err = st.UndoDedup(t.Context(), "linked")
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
	requirements.NoError(err)
	requirements.NotContains(record.MessageIDs, ids[0])
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
				return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
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
}

func TestMeetingLinkDecisionClearKeepsOtherOccurrence(t *testing.T) {
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
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
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
}

func TestMeetingLinkDecisionClearHiddenEndpoint(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := projectionFixture(t, st, "hidden-first", "meeting_json", `{}`)
	_, second := projectionFixture(t, st, "hidden-second", "meeting_json", `{}`)
	left := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: first}}
	right := MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: second}}
	_, err := st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	requirements.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, first)
	requirements.NoError(err)
	result, err := st.DecideMeetingLinkContext(t.Context(), left, right, "clear")
	requirements.NoError(err)
	requirements.Equal("automatic", result.State)
	_, err = st.db.ExecContext(t.Context(), `UPDATE messages SET deleted_at=NULL WHERE id=?`, first)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), left)
	requirements.NoError(err)
	requirements.Equal([]int64{first}, record.MessageIDs)
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
		return MeetingLinkRef{Kind: "msg", OccurrenceRef: OccurrenceRef{MessageID: id}}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
	requirements.NoError(err)
	start := make(chan struct{})
	type outcome struct {
		clear bool
		err   error
	}
	done := make(chan outcome, 2)
	go func() {
		<-start
		_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "clear")
		done <- outcome{clear: true, err: err}
	}()
	go func() {
		<-start
		_, err := st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "concurrent")
		done <- outcome{err: err}
	}()
	close(start)
	for range 2 {
		result := <-done
		if st.dialect.IsSerializationFailureError(result.err) {
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
