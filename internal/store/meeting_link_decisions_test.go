package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingLinkDecisionReadsOnlyRequestedOccurrence(t *testing.T) {
	r := require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=52"]}`
	_, event := calendarFixture(t, base, "separate-series", raw)
	_, duplicate := calendarFixture(t, base, "separate-duplicate", raw, "duplicate@example.com")
	var firstNote int64
	for i := range 52 {
		_, note := projectionFixture(t, base, fmt.Sprintf("separate-note-%d", i), "meeting_json", `{}`)
		if i == 0 {
			firstNote = note
		}
		date := time.Date(2027, 3, 14+7*i, 10, 0, 0, 0, time.UTC).Format("20060102T150405Z")
		occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@%s", duplicate, date))
		r.NoError(err)
		_, err = base.DecideMeetingLinkContext(t.Context(), occurrence, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
		r.NoError(err)
	}
	_, err := base.MergeDuplicates(t.Context(), event, []int64{duplicate}, "series-dedup")
	r.NoError(err)
	occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", event))
	r.NoError(err)
	record, err := base.MeetingRecordContext(t.Context(), occurrence)
	r.NoError(err)
	r.Equal([]int64{firstNote}, record.MessageIDs)
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
	r.Empty(record.Links)
	_, err = st.DecideMeetingLinkContext(ctx, refs[0], refs[1], "clear")
	r.ErrorIs(err, sql.ErrNoRows)
	record, err = st.MeetingRecordContext(t.Context(), refs[0])
	r.NoError(err)
	r.Equal([]int64{refs[0].MessageID, refs[1].MessageID}, record.MessageIDs)
}

func TestMeetingLinkDecisionGroupsLinkAndSplit(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	groups := [2][]int64{}
	for group := range 2 {
		raw := fmt.Sprintf(`{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"group%d_20270314T100000Z"}}`, group)
		for i := range 3 {
			_, id := projectionFixture(t, st, fmt.Sprintf("group-%d-%d", group, i), "granola_json", raw)
			groups[group] = append(groups[group], id)
		}
	}
	ref := func(id int64) MeetingLinkRef {
		return MeetingLinkRef{Kind: "msg", MessageID: id}
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(groups[0][0]), ref(groups[1][0]), "link")
	requirements.NoError(err)
	all := append(append([]int64{}, groups[0]...), groups[1]...)
	for _, id := range []int64{groups[0][0], groups[0][1], groups[1][0], groups[1][2]} {
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
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(groups[0][0]), ref(groups[0][1]), "clear")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(groups[0][1]))
	requirements.NoError(err)
	requirements.Equal(all, record.MessageIDs)
}

func TestMeetingLinkDecisionConflictIdempotenceAndClear(t *testing.T) {
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("split_order_%v_reverse_%v", order, reverse), func(t *testing.T) {
				r := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				var refs [3]MeetingLinkRef
				for i := range refs {
					_, id := projectionFixture(t, st, fmt.Sprintf("split-permutation-%d", i), "meeting_json", `{}`)
					refs[i] = MeetingLinkRef{Kind: "msg", MessageID: id}
				}
				a, b, c := refs[order[0]], refs[order[1]], refs[order[2]]
				_, err := st.DecideMeetingLinkContext(t.Context(), a, b, "link")
				r.NoError(err)
				_, err = st.DecideMeetingLinkContext(t.Context(), b, c, "link")
				r.NoError(err)
				left, right := a, c
				if reverse {
					left, right = right, left
				}
				_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
				r.ErrorIs(err, ErrMeetingLinkConflict)
				r.Contains(err.Error(), fmt.Sprintf("msg:%d", b.MessageID))
				for _, ref := range refs {
					record, err := st.MeetingRecordContext(t.Context(), ref)
					r.NoError(err)
					r.ElementsMatch([]int64{a.MessageID, b.MessageID, c.MessageID}, record.MessageIDs)
				}
			})
			t.Run(fmt.Sprintf("order_%v_reverse_%v", order, reverse), func(t *testing.T) {
				r := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				var refs [3]MeetingLinkRef
				for i := range refs {
					_, id := projectionFixture(t, st, fmt.Sprintf("permutation-%d", i), "meeting_json", `{}`)
					refs[i] = MeetingLinkRef{Kind: "msg", MessageID: id}
				}
				a, b, c := refs[order[0]], refs[order[1]], refs[order[2]]
				_, err := st.DecideMeetingLinkContext(t.Context(), b, c, "link")
				r.NoError(err)
				_, err = st.DecideMeetingLinkContext(t.Context(), a, c, "unlink")
				r.NoError(err)
				left, right := a, b
				if reverse {
					left, right = right, left
				}
				_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
				r.ErrorIs(err, ErrMeetingLinkConflict)
				r.ErrorContains(err, fmt.Sprintf("msg:%d msg:%d", min(a.MessageID, c.MessageID), max(a.MessageID, c.MessageID)))
				for _, ref := range []MeetingLinkRef{a, b, c} {
					record, err := st.MeetingRecordContext(t.Context(), ref)
					r.NoError(err)
					if ref == a {
						r.Equal([]int64{a.MessageID}, record.MessageIDs)
					} else {
						r.ElementsMatch([]int64{b.MessageID, c.MessageID}, record.MessageIDs)
					}
				}
			})
		}
	}
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs []MeetingLinkRef
	var firstData *MessagePersistData
	for _, name := range []string{"conflict-a", "conflict-b", "conflict-c", "conflict-d"} {
		data, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		if firstData == nil {
			firstData = data
		}
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	requirements.NoError(err)
	assertions.Equal("link", result.State)
	firstData.RawMIME = []byte(`{"summary_text":"updated"}`)
	_, err = st.PersistMessage(firstData)
	requirements.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), refs[0])
	requirements.NoError(err)
	assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID}, record.MessageIDs)
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {1, 2}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), refs[pair[0]], refs[pair[1]], "link")
		requirements.NoError(err)
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[1], refs[2], "unlink")
	requirements.NoError(err)
	for i, ref := range refs[:3] {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		want := []int64{refs[0].MessageID, refs[1].MessageID}
		if i == 2 {
			want = []int64{refs[2].MessageID}
		}
		assertions.Equal(want, record.MessageIDs)
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[3], "link")
	requirements.NoError(err)
	for i, ref := range []MeetingLinkRef{refs[0], refs[2]} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		want := []int64{refs[0].MessageID, refs[1].MessageID, refs[3].MessageID}
		if i == 1 {
			want = []int64{refs[2].MessageID}
		}
		assertions.Equal(want, record.MessageIDs)
	}
	for range 2 {
		result, err := st.DecideMeetingLinkContext(t.Context(), refs[2], refs[1], "clear")
		requirements.NoError(err)
		assertions.Equal("automatic", result.State)
		record, err := st.MeetingRecordContext(t.Context(), refs[2])
		requirements.NoError(err)
		assertions.Equal([]int64{refs[2].MessageID}, record.MessageIDs)
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "link")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), refs[2])
	requirements.NoError(err)
	assertions.Equal([]int64{refs[0].MessageID, refs[1].MessageID, refs[2].MessageID, refs[3].MessageID}, record.MessageIDs)
}

func TestMeetingLinkDecisionSplitCopiesKeepsExplicitNoteLink(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	_, first := calendarFixture(t, st, "split-first", raw, "first@example.com")
	_, second := calendarFixture(t, st, "split-second", raw, "second@example.com")
	_, note := projectionFixture(t, st, "split-note", "meeting_json", `{}`)
	firstRef := MeetingLinkRef{Kind: "evt", MessageID: first}
	secondRef := MeetingLinkRef{Kind: "evt", MessageID: second}
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "unlink")
	r.ErrorIs(err, ErrMeetingLinkConflict)
	r.ErrorContains(err, fmt.Sprintf("evt:%d@20270314T100000Z msg:%d", first, note))
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "clear")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), firstRef, secondRef, "unlink")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "link")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), noteRef)
	r.NoError(err)
	r.Len(record.Links, 1)
	r.Equal(fmt.Sprintf("evt:%d", first), record.Links[0].Ref)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "unlink")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	r.NoError(err)
	r.Len(record.Links, 1)
	r.Equal(fmt.Sprintf("evt:%d", first), record.Links[0].Ref)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "clear")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "clear")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "unlink")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), firstRef, secondRef, "clear")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	r.NoError(err)
	r.Empty(record.Links)
	t.Run("transitive event bridge", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, first := calendarFixture(t, st, "bridge-first", `{"id":"first","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "first@example.com")
		_, second := calendarFixture(t, st, "bridge-second", `{"id":"second","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "second@example.com")
		_, bridge := projectionFixture(t, st, "bridge-note", "meeting_json", `{}`)
		_, note := projectionFixture(t, st, "separate-note", "meeting_json", `{}`)
		firstRef := MeetingLinkRef{Kind: "evt", MessageID: first}
		secondRef := MeetingLinkRef{Kind: "evt", MessageID: second}
		bridgeRef := MeetingLinkRef{Kind: "msg", MessageID: bridge}
		noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
		for _, event := range []MeetingLinkRef{firstRef, secondRef} {
			_, err := st.DecideMeetingLinkContext(t.Context(), bridgeRef, event, "link")
			r.NoError(err)
		}
		_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "unlink")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "link")
		r.ErrorIs(err, ErrMeetingLinkConflict)
	})
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
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, first, "unlink")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), noteRef)
	requirements.NoError(err)
	assertions.Empty(record.Links)
}

func TestMeetingLinkDecisionUnsupportedSeries(t *testing.T) {
	for _, tc := range []struct {
		name, initial string
		warnings      bool
	}{
		{"link warning", "single", true},
		{"single writes", "single", false},
		{"series writes", "series", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}`
			if tc.initial == "series" {
				raw += `,"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]`
			}
			data, event := calendarFixture(t, st, "unsupported-event", raw+`}`)
			format, noteRaw := "meeting_json", `{}`
			if tc.warnings {
				format = "twenty_json"
				noteRaw = `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"linked@example.org","startsAt":"2027-03-14T10:00:00Z"}}`
			}
			_, note := projectionFixture(t, st, "unsupported-note", format, noteRaw)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			eventRef := MeetingLinkRef{Kind: "evt", MessageID: event}
			dateRef := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270314T100000Z")}
			original := eventRef
			if tc.initial == "series" {
				original = dateRef
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, original, "link")
			r.NoError(err)
			data.RawMIME = []byte(`{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`)
			_, err = st.PersistMessage(data)
			r.NoError(err)
			if tc.warnings {
				record, err := st.MeetingRecordContext(t.Context(), noteRef)
				r.NoError(err)
				assert.Equal(t, []CalendarUnexpandable{{MessageID: event, Reason: "unsupported recurrence"}}, record.Unexpandable)
				return
			}
			otherDate := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270321T100000Z")}
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
}

func TestMeetingLinkDecisionFollowsOrphanExceptionToMaster(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unlink bool
	}{{"clear after series arrives", false}, {"unlink then clear", true}} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, exception := calendarFixture(t, st, "orphan", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
			_, note := projectionFixture(t, st, "orphan-note", "meeting_json", `{}`)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			oldRef := MeetingLinkRef{Kind: "evt", MessageID: exception}
			_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, oldRef, "link")
			r.NoError(err)
			_, master := calendarFixture(t, st, "master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
			current := MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}
			record, err := st.MeetingRecordContext(t.Context(), current)
			r.NoError(err)
			r.Equal([]int64{note}, record.MessageIDs)
			if tc.unlink {
				_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, current, "unlink")
				r.NoError(err)
				record, err = st.MeetingRecordContext(t.Context(), noteRef)
				r.NoError(err)
				r.Empty(record.Links)
			}
			_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, current, "clear")
			r.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Empty(record.Links)
		})
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
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	requirements.NoError(err)
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Len(record.Links, 1)
	}
}

func TestMeetingLinkDecisionDropsInvalidEndpoint(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, note := projectionFixture(t, st, "still-note", "meeting_json", `{}`)
	left := MeetingLinkRef{Kind: "msg", MessageID: note}
	data, event := calendarFixture(t, st, "shrinking-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	right, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", event))
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	r.NoError(err)
	_, other := projectionFixture(t, st, "shrinking-other", "meeting_json", `{}`)
	otherRef := MeetingLinkRef{Kind: "msg", MessageID: other}
	_, err = st.DecideMeetingLinkContext(t.Context(), otherRef, right, "link")
	r.NoError(err)
	data.RawMIME = []byte(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`)
	_, err = st.PersistMessage(data)
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), left, otherRef, "unlink")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), left)
	r.NoError(err)
	r.Equal([]int64{note}, record.MessageIDs)
	r.Empty(record.Links)
	_, err = st.MeetingRecordContext(t.Context(), right)
	r.ErrorIs(err, sql.ErrNoRows)
}

func TestMeetingLinkDecisionClearAfterBothCopiesDedup(t *testing.T) {
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
}

func TestMeetingDecisionClearAfterDedupPreservesOtherOccurrence(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := calendarFixture(t, st, "clear-single", `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	_, duplicate := calendarFixture(t, st, "clear-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, "other@example.com")
	_, note := projectionFixture(t, st, "clear-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	first := MeetingLinkRef{Kind: "evt", MessageID: duplicate, Identity: occurrenceIdentity(t, "20270314T100000Z")}
	second := MeetingLinkRef{Kind: "evt", MessageID: duplicate, Identity: occurrenceIdentity(t, "20270321T100000Z")}
	_, err := st.DecideMeetingLinkContext(t.Context(), first, noteRef, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), second, noteRef, "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "clear-dates")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: survivor}, noteRef, "clear")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "clear-dates")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), noteRef)
	r.NoError(err)
	r.Len(record.Links, 1)
	r.Equal(fmt.Sprintf("evt:%d@20270321T100000Z", duplicate), record.Links[0].Ref)
}

type meetingDecisionWriterKey struct{}

func TestMeetingLinkDecisionConcurrentLinks(t *testing.T) {
	for _, first := range []int{0, 1} {
		t.Run(fmt.Sprintf("first_%d", first), func(t *testing.T) {
			r := require.New(t)
			base := newRFC822IDBackfillBackendStore(t)
			if !base.IsPostgreSQL() {
				t.Skip("PostgreSQL transaction contract")
			}
			var refs [4]MeetingLinkRef
			for i := range refs {
				_, id := projectionFixture(t, base, fmt.Sprintf("concurrent-link-%d", i), "meeting_json", `{}`)
				refs[i] = MeetingLinkRef{Kind: "msg", MessageID: id}
			}
			_, err := base.DecideMeetingLinkContext(t.Context(), refs[1], refs[2], "link")
			r.NoError(err)
			_, err = base.DecideMeetingLinkContext(t.Context(), refs[0], refs[3], "unlink")
			r.NoError(err)
			paused, release := make(chan struct{}), make(chan struct{})
			gate := &meetingProjectionGate{before: func(ctx context.Context, query string, _ []driver.NamedValue) error {
				if ctx.Value(meetingDecisionWriterKey{}) == true && strings.HasPrefix(query, "INSERT INTO meeting_link_decisions(") {
					close(paused)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}}
			st := gatedMeetingProjectionStore(t, base, gate)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			var writers sync.WaitGroup
			defer writers.Wait()
			defer cancel()
			type result struct {
				index int
				err   error
			}
			done := make(chan result, 2)
			pairs := [][2]int{{0, 1}, {2, 3}}
			start := func(i int, pause bool) {
				writers.Go(func() {
					_, err := st.DecideMeetingLinkContext(context.WithValue(ctx, meetingDecisionWriterKey{}, pause), refs[pairs[i][0]], refs[pairs[i][1]], "link")
					done <- result{i, err}
				})
			}
			start(first, true)
			select {
			case <-paused:
			case <-ctx.Done():
				r.FailNow("first writer did not reach insert")
			}
			start(1-first, false)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				var waiting bool
				r.NoError(base.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%archive_metadata%')`).Scan(&waiting))
				if waiting {
					break
				}
				select {
				case result := <-done:
					close(release)
					<-done
					r.FailNowf("second writer did not wait", "result: %v", result.err)
				case <-ticker.C:
				case <-ctx.Done():
					close(release)
					<-done
					<-done
					r.FailNow("second writer did not reach the lock")
				}
			}
			close(release)
			one, two := <-done, <-done
			byIndex := map[int]error{one.index: one.err, two.index: two.err}
			r.NoError(byIndex[first])
			r.ErrorIs(byIndex[1-first], ErrMeetingLinkConflict)
			for i, ref := range refs {
				record, err := base.MeetingRecordContext(t.Context(), ref)
				r.NoError(err)
				want := []int64{refs[0].MessageID, refs[1].MessageID, refs[2].MessageID}
				if first == 1 {
					want = []int64{refs[1].MessageID, refs[2].MessageID, refs[3].MessageID}
				}
				if first == 0 && i == 3 || first == 1 && i == 0 {
					want = []int64{ref.MessageID}
				}
				r.Equal(want, record.MessageIDs)
			}
		})
	}
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
	_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), ids[4], []int64{ids[3]}, "opposite")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(2), ref(4), "clear")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "first")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(4))
	r.NoError(err)
	r.Equal([]int64{ids[4]}, record.MessageIDs)
	_, err = st.UndoDedup(t.Context(), "second")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(4))
	r.NoError(err)
	r.Equal([]int64{ids[4]}, record.MessageIDs)
	_, err = st.UndoDedup(t.Context(), "opposite")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(0))
	r.NoError(err)
	r.Equal([]int64{ids[0]}, record.MessageIDs)
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

func TestMeetingLinkDecisionUntimedCopyBecomesSeries(t *testing.T) {
	for _, action := range []string{"clear", "link"} {
		t.Run(action, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, event := calendarFixture(t, st, "untimed-to-series", `{"id":"event","status":"cancelled"}`)
			_, note := projectionFixture(t, st, "untimed-note", "meeting_json", `{}`)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			bare := MeetingLinkRef{Kind: "evt", MessageID: event}
			_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, bare, "link")
			r.NoError(err)
			data.RawMIME = []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
			_, err = st.PersistMessage(data)
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Equal([]int64{note}, record.MessageIDs)
			r.Empty(record.Links)
			for _, decision := range []string{"link", "unlink"} {
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, bare, decision)
				r.ErrorIs(err, ErrMeetingOccurrenceRequired)
			}
			selected := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270314T100000Z")}
			if action == "clear" {
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, bare, "clear")
				r.NoError(err)
				var count int
				r.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
				r.Zero(count)
			} else {
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, selected, "link")
				r.NoError(err)
				record, err = st.MeetingRecordContext(t.Context(), noteRef)
				r.NoError(err)
				r.Len(record.Links, 1)
				r.Equal(endpointFromRef(selected).ref(), record.Links[0].Ref)
			}
		})
	}
}

func TestMeetingLinkDecisionLongSeriesChoices(t *testing.T) {
	for _, eventPair := range []bool{false, true} {
		t.Run(fmt.Sprintf("event_pair_%v", eventPair), func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := calendarFixture(t, st, "long-first", `{"id":"first","iCalUID":"first@example.org","start":{"dateTime":"2017-01-01T10:00:00Z"},"end":{"dateTime":"2017-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=4000"]}`, "first@example.com")
			var second int64
			if eventPair {
				_, second = calendarFixture(t, st, "long-second", `{"id":"second","iCalUID":"second@example.org","start":{"dateTime":"2017-01-01T10:00:00Z"},"end":{"dateTime":"2017-01-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY;COUNT=4000"]}`, "second@example.com")
			}
			var left, right []MeetingLinkRef
			for i := range 150 {
				date := time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("20060102T150405Z")
				identity := occurrenceIdentity(t, date)
				a := MeetingLinkRef{Kind: "evt", MessageID: first, Identity: identity}
				var b MeetingLinkRef
				if eventPair {
					b = MeetingLinkRef{Kind: "evt", MessageID: second, Identity: identity}
				} else {
					_, note := projectionFixture(t, st, fmt.Sprintf("long-note-%d", i), "meeting_json", `{}`)
					b = MeetingLinkRef{Kind: "msg", MessageID: note}
				}
				left, right = append(left, a), append(right, b)
				_, err := st.DecideMeetingLinkContext(t.Context(), a, b, "link")
				r.NoError(err, "save occurrence %d", i)
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), right[75], left[75], "clear")
			r.NoError(err)
			selected, err := st.MeetingRecordContext(t.Context(), left[75])
			r.NoError(err)
			neighbor, err := st.MeetingRecordContext(t.Context(), left[76])
			r.NoError(err)
			if eventPair {
				r.Len(selected.Links, 1)
				r.Len(neighbor.Links, 2)
				other, err := st.MeetingRecordContext(t.Context(), right[75])
				r.NoError(err)
				r.Len(other.Links, 1)
			} else {
				r.Empty(selected.MessageIDs)
				r.Equal([]int64{right[76].MessageID}, neighbor.MessageIDs)
				note, err := st.MeetingRecordContext(t.Context(), right[75])
				r.NoError(err)
				r.Empty(note.Links)
			}
		})
	}
}

func TestMeetingLinkDecisionEditedOccurrenceReferences(t *testing.T) {
	for _, tc := range []struct{ name, start, end, original, movedStart, movedEnd, first, second string }{
		{"timed", `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-14T11:00:00Z"}`, `{"dateTime":"2027-03-14T10:00:00Z"}`, `{"dateTime":"2027-03-15T10:00:00Z"}`, `{"dateTime":"2027-03-15T11:00:00Z"}`, "20270314T100000Z", "20270321T100000Z"},
		{"all-day", `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-14"}`, `{"date":"2027-03-15"}`, `{"date":"2027-03-16"}`, "20270314", "20270321"},
	} {
		for _, rule := range []string{"RRULE:FREQ=WEEKLY;COUNT=2", "RRULE:FREQ=YEARLY;BYWEEKNO=20"} {
			for _, order := range []string{"edit-first", "series-first", "choice-first"} {
				for _, side := range []string{"series", "edit"} {
					if tc.name == "all-day" && order != "choice-first" {
						continue
					}
					if order == "choice-first" && (side == "edit" || rule != "RRULE:FREQ=WEEKLY;COUNT=2") {
						continue
					}
					t.Run(tc.name+"/"+rule+"/"+order+"/"+side, func(t *testing.T) {
						r := require.New(t)
						st := newRFC822IDBackfillBackendStore(t)
						var series, edit int64
						makeSeries := func() {
							_, series = calendarFixture(t, st, "series", fmt.Sprintf(`{"id":"series","start":%s,"end":%s,"recurrence":[%q]}`, tc.start, tc.end, rule))
						}
						makeEdit := func() {
							_, edit = calendarFixture(t, st, "edit", fmt.Sprintf(`{"id":"edit","recurringEventId":"series","originalStartTime":%s,"start":%s,"end":%s}`, tc.original, tc.movedStart, tc.movedEnd))
						}
						if order == "edit-first" {
							makeEdit()
							makeSeries()
						} else {
							makeSeries()
							if order != "choice-first" {
								makeEdit()
							}
						}
						_, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
						noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
						seriesRef := MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, tc.first)}
						editRef := MeetingLinkRef{Kind: "evt", MessageID: edit}
						write, other := seriesRef, editRef
						if side == "edit" {
							write, other = other, write
						}
						_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, write, "link")
						r.NoError(err)
						if order == "choice-first" {
							makeEdit()
							other = MeetingLinkRef{Kind: "evt", MessageID: edit}
						}
						record, err := st.MeetingRecordContext(t.Context(), noteRef)
						r.NoError(err)
						r.NotEmpty(record.Links)
						if rule == "RRULE:FREQ=WEEKLY;COUNT=2" {
							r.Len(record.Links, 1)
							r.Equal(fmt.Sprintf("evt:%d@%s", series, tc.first), record.Links[0].Ref)
						}
						for _, ref := range []MeetingLinkRef{write, other} {
							record, err := st.MeetingRecordContext(t.Context(), ref)
							r.NoError(err)
							r.Contains(record.MessageIDs, note)
							r.NotEmpty(record.Links)
						}
						if rule == "RRULE:FREQ=WEEKLY;COUNT=2" {
							second := MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, tc.second)}
							record, err = st.MeetingRecordContext(t.Context(), second)
							r.NoError(err)
							r.Empty(record.MessageIDs)
						}
						_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, other, "clear")
						r.NoError(err)
						record, err = st.MeetingRecordContext(t.Context(), write)
						r.NoError(err)
						r.NotContains(record.MessageIDs, note)
						_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, other, "unlink")
						r.NoError(err)
						record, err = st.MeetingRecordContext(t.Context(), write)
						r.NoError(err)
						r.NotContains(record.MessageIDs, note)
						_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, write, "clear")
						r.NoError(err)
					})
				}
			}
		}
	}
}

func TestMeetingDecisionRepeatingOriginalReferencesAfterDedup(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := projectionFixture(t, st, "survivor-repeat", "meeting_json", `{}`)
	_, duplicate := projectionFixture(t, st, "duplicate-repeat", "meeting_json", `{}`)
	_, other := projectionFixture(t, st, "other-repeat", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "repeat")
	r.NoError(err)
	for range 2 {
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
		r.NoError(err)
	}
	_, err = st.UndoDedup(t.Context(), "repeat")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	r.NoError(err)
	r.ElementsMatch([]int64{survivor, other}, record.MessageIDs)
	t.Run("single event", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		raw := `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
		_, survivor := calendarFixture(t, st, "repeat-survivor", raw)
		_, duplicate := calendarFixture(t, st, "repeat-duplicate", raw, "duplicate@example.com")
		_, note := projectionFixture(t, st, "repeat-note", "meeting_json", `{}`)
		event := MeetingLinkRef{Kind: "evt", MessageID: duplicate}
		other := MeetingLinkRef{Kind: "msg", MessageID: note}
		_, err := st.DecideMeetingLinkContext(t.Context(), event, other, "link")
		r.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "event-repeat")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), event, other, "link")
		r.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), other)
		r.NoError(err)
		r.Len(record.Links, 1)
		r.Equal(fmt.Sprintf("evt:%d", survivor), record.Links[0].Ref)
	})
}

func TestMeetingDecisionResetAfterSeriesUpdate(t *testing.T) {
	for _, tc := range []struct {
		name, initial, clearKey                string
		dateDuplicate, keepChoice, otherChoice bool
	}{
		{"different occurrence", `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "20270321T100000Z", false, true, false},
		{"matching occurrence", `{"id":"event","status":"cancelled"}`, "20270314T100000Z", true, false, false},
		{"bare event", `{"id":"event","status":"cancelled"}`, "", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			data, survivor := calendarFixture(t, st, "reset-survivor", tc.initial)
			duplicateData, duplicate := calendarFixture(t, st, "reset-duplicate", tc.initial, "duplicate@example.com")
			_, note := projectionFixture(t, st, "reset-note", "meeting_json", `{}`)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			var err error
			if tc.dateDuplicate {
				duplicateData.RawMIME = []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
				_, err = st.PersistMessage(duplicateData)
				r.NoError(err)
			}
			_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: duplicate}, noteRef, "link")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "reset-series")
			r.NoError(err)
			data.RawMIME = []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
			_, err = st.PersistMessage(data)
			r.NoError(err)
			occurrence := MeetingLinkRef{Kind: "evt", MessageID: survivor, Identity: occurrenceIdentity(t, "20270321T100000Z")}
			var otherRef MeetingLinkRef
			if tc.otherChoice {
				_, other := projectionFixture(t, st, "reset-other", "meeting_json", `{}`)
				otherRef = MeetingLinkRef{Kind: "msg", MessageID: other}
				_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, otherRef, "link")
				r.NoError(err)
			}
			clearRef := MeetingLinkRef{Kind: "evt", MessageID: survivor}
			if tc.clearKey != "" {
				clearRef.Identity = occurrenceIdentity(t, tc.clearKey)
			}
			_, err = st.DecideMeetingLinkContext(t.Context(), clearRef, noteRef, "clear")
			r.NoError(err)
			_, err = st.UndoDedup(t.Context(), "reset-series")
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			if tc.keepChoice {
				r.Len(record.Links, 1)
				r.Equal(fmt.Sprintf("evt:%d", duplicate), record.Links[0].Ref)
			} else {
				r.Empty(record.Links)
			}
			if tc.otherChoice {
				record, err = st.MeetingRecordContext(t.Context(), otherRef)
				r.NoError(err)
				r.Len(record.Links, 1)
				r.Equal(endpointFromRef(occurrence).ref(), record.Links[0].Ref)
			}
		})
	}
}

func TestMeetingDecisionWrongKindResetDoesNotClearChoice(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := projectionFixture(t, st, "wrong-kind-first", "meeting_json", `{}`)
	_, second := projectionFixture(t, st, "wrong-kind-second", "meeting_json", `{}`)
	a := MeetingLinkRef{Kind: "msg", MessageID: first}
	b := MeetingLinkRef{Kind: "msg", MessageID: second}
	_, err := st.DecideMeetingLinkContext(t.Context(), a, b, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: first}, b, "clear")
	r.ErrorIs(err, sql.ErrNoRows)
	record, err := st.MeetingRecordContext(t.Context(), a)
	r.NoError(err)
	r.ElementsMatch([]int64{first, second}, record.MessageIDs)
}

func TestMeetingDecisionRejectsWrongOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name, raw, valid, wrong string
		exception               bool
	}{
		{"single timed", `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "20270314T100000Z", "20270321T100000Z", false},
		{"single all-day", `{"id":"single","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"}}`, "20270314", "20270321", false},
		{"exception timed", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`, "20270314T100000Z", "20270321T100000Z", true},
		{"exception all-day", `{"id":"edit","recurringEventId":"series","originalStartTime":{"date":"2027-03-14"},"status":"cancelled"}`, "20270314", "20270321", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, event := calendarFixture(t, st, "wrong-occurrence", tc.raw)
			_, note := projectionFixture(t, st, "wrong-occurrence-note", "meeting_json", `{}`)
			wrong := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, tc.wrong)}
			_, err := st.MeetingRecordContext(t.Context(), wrong)
			r.ErrorIs(err, sql.ErrNoRows)
			_, err = st.DecideMeetingLinkContext(t.Context(), wrong, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
			r.ErrorIs(err, sql.ErrNoRows)
			valid := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, tc.valid)}
			_, err = st.MeetingRecordContext(t.Context(), valid)
			r.NoError(err)
			if tc.exception {
				_, err = st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event})
				r.NoError(err)
			}
		})
	}
}

func TestMeetingDecisionRejectsSameCopyAndInvalidDecision(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "decision-master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, event := calendarFixture(t, st, "decision-edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	occurrence := MeetingLinkRef{Kind: "evt", MessageID: master, Identity: occurrenceIdentity(t, "20270314T100000Z")}
	edit := MeetingLinkRef{Kind: "evt", MessageID: event}
	_, err := st.DecideMeetingLinkContext(t.Context(), occurrence, edit, "link")
	r.ErrorIs(err, ErrMeetingLinkSameCopy)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, occurrence, "link")
	r.ErrorIs(err, ErrMeetingLinkSameCopy)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, edit, "invalid")
	r.ErrorIs(err, ErrInvalidMeetingLinkDecision)
}

func TestMeetingDecisionRescheduledReset(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		replace    bool
	}{
		{"single", "single", false},
		{"single replaced", "single", true},
		{"hidden copy", "hidden", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"single","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			data, survivor := calendarFixture(t, st, "rescheduled-survivor", raw)
			_, note := projectionFixture(t, st, "rescheduled-note", "meeting_json", `{}`)
			eventRef := MeetingLinkRef{Kind: "evt", MessageID: survivor}
			if tc.kind == "hidden" {
				_, duplicate := calendarFixture(t, st, "rescheduled-duplicate", raw, "duplicate@example.com")
				eventRef = MeetingLinkRef{Kind: "evt", MessageID: duplicate, Identity: occurrenceIdentity(t, "20270314T100000Z")}
				_, err := st.DecideMeetingLinkContext(t.Context(), eventRef, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
				r.NoError(err)
				_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "rescheduled-hidden")
				r.NoError(err)
			} else {
				_, err := st.DecideMeetingLinkContext(t.Context(), eventRef, MeetingLinkRef{Kind: "msg", MessageID: note}, "link")
				r.NoError(err)
			}
			data.RawMIME = []byte(`{"id":"single","start":{"dateTime":"2027-03-15T10:00:00Z"},"end":{"dateTime":"2027-03-15T11:00:00Z"}}`)
			_, err := st.PersistMessage(data)
			r.NoError(err)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			if tc.kind == "hidden" {
				_, err := st.MeetingRecordContext(t.Context(), eventRef)
				r.ErrorIs(err, sql.ErrNoRows)
				wrong := MeetingLinkRef{Kind: "evt", MessageID: eventRef.MessageID, Identity: occurrenceIdentity(t, "20270321T100000Z")}
				_, err = st.MeetingRecordContext(t.Context(), wrong)
				r.ErrorIs(err, sql.ErrNoRows)
			}
			if tc.replace || tc.kind == "hidden" {
				_, err = st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: survivor}, noteRef, "unlink")
				r.NoError(err)
			}
			_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "clear")
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Empty(record.Links)
			if tc.kind == "hidden" {
				_, err = st.UndoDedup(t.Context(), "rescheduled-hidden")
				r.NoError(err)
				record, err = st.MeetingRecordContext(t.Context(), noteRef)
				r.NoError(err)
				r.Empty(record.Links)
			}
		})
	}
}

func TestMeetingDecisionDedupCollisionPriority(t *testing.T) {
	t.Run("newest hidden choice wins", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, survivor := projectionFixture(t, st, "priority-kept", "meeting_json", `{}`)
		_, duplicate := projectionFixture(t, st, "priority-hidden", "meeting_json", `{}`)
		_, other := projectionFixture(t, st, "priority-other", "meeting_json", `{}`)
		ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
		_, err := st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "link")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "unlink")
		r.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "priority")
		r.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), ref(other))
		r.NoError(err)
		r.NotContains(record.MessageIDs, survivor)
	})
	for _, survivorFirst := range []bool{false, true} {
		name := "duplicate choice first"
		if survivorFirst {
			name = "survivor choice first"
		}
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, survivor := calendarFixture(t, st, "survivor", `{"id":"survivor","start":{"dateTime":"2027-03-15T10:00:00Z"},"end":{"dateTime":"2027-03-15T11:00:00Z"}}`)
			_, duplicate := calendarFixture(t, st, "duplicate", `{"id":"duplicate","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
			_, note := projectionFixture(t, st, "choice-note", "meeting_json", `{}`)
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			kept := MeetingLinkRef{Kind: "evt", MessageID: survivor, Identity: occurrenceIdentity(t, "20270315T100000Z")}
			removed := MeetingLinkRef{Kind: "evt", MessageID: duplicate, Identity: occurrenceIdentity(t, "20270314T100000Z")}
			writeKept := func() { _, err := st.DecideMeetingLinkContext(t.Context(), kept, noteRef, "link"); r.NoError(err) }
			writeRemoved := func() { _, err := st.DecideMeetingLinkContext(t.Context(), removed, noteRef, "unlink"); r.NoError(err) }
			if survivorFirst {
				writeKept()
				writeRemoved()
			} else {
				writeRemoved()
				writeKept()
			}
			_, err := st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "collision")
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Len(record.Links, 1)
			r.Equal(fmt.Sprintf("evt:%d", survivor), record.Links[0].Ref)
		})
	}
	t.Run("notes", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"shared_20270314T100000Z"}}`
		_, survivor := projectionFixture(t, st, "collision-survivor", "granola_json", raw)
		_, first := projectionFixture(t, st, "collision-first", "meeting_json", `{}`)
		_, second := projectionFixture(t, st, "collision-second", "meeting_json", `{}`)
		_, other := projectionFixture(t, st, "collision-other", "granola_json", raw)
		ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
		record, err := st.MeetingRecordContext(t.Context(), ref(other))
		r.NoError(err)
		r.Contains(record.MessageIDs, survivor)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(first), ref(other), "link")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(second), ref(other), "unlink")
		r.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{first, second}, "collision")
		r.NoError(err)
		record, err = st.MeetingRecordContext(t.Context(), ref(other))
		r.NoError(err)
		r.NotContains(record.MessageIDs, survivor)
	})
	t.Run("successive dedups", func(t *testing.T) {
		for _, keptChoice := range []bool{false, true} {
			name := "copied choices retain order"
			if keptChoice {
				name = "kept choice wins"
			}
			t.Run(name, func(t *testing.T) {
				r := require.New(t)
				st := newRFC822IDBackfillBackendStore(t)
				var ids []int64
				for _, name := range []string{"priority-hidden", "priority-survivor", "priority-link", "priority-split", "priority-destination"} {
					_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
					ids = append(ids, id)
				}
				hidden, survivor, linked, split, destination := ids[0], ids[1], ids[2], ids[3], ids[4]
				ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
				_, err := st.DecideMeetingLinkContext(t.Context(), ref(hidden), ref(linked), "link")
				r.NoError(err)
				splitFrom := hidden
				if keptChoice {
					splitFrom = survivor
				}
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(splitFrom), ref(split), "unlink")
				r.NoError(err)
				_, err = st.MergeDuplicates(t.Context(), survivor, []int64{hidden}, "priority-carry")
				r.NoError(err)
				_, err = st.MergeDuplicates(t.Context(), destination, []int64{linked, split}, "priority-collapse")
				r.NoError(err)
				record, err := st.MeetingRecordContext(t.Context(), ref(survivor))
				r.NoError(err)
				r.Equal([]int64{survivor}, record.MessageIDs)
			})
		}
	})
}

func TestMeetingDecisionClearRejectsLostSurvivor(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "delete batch"
		if all {
			name = "delete all"
		}
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, first := projectionFixture(t, st, "purge-first", "meeting_json", `{}`)
			_, middle := projectionFixture(t, st, "purge-middle", "meeting_json", `{}`)
			_, survivor := projectionFixture(t, st, "purge-survivor", "meeting_json", `{}`)
			_, note := projectionFixture(t, st, "purge-note", "meeting_json", `{}`)
			ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(first), ref(note), "link")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), middle, []int64{first}, "first")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{middle}, "middle")
			r.NoError(err)
			if all {
				_, _, err = st.DeleteAllDedupedContext(t.Context())
			} else {
				_, err = st.DeleteDedupedBatchContext(t.Context(), "middle")
			}
			r.NoError(err)
			if !all {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(first), ref(note), "clear")
				r.ErrorIs(err, sql.ErrNoRows)
			}
			record, err := st.MeetingRecordContext(t.Context(), ref(note))
			r.NoError(err)
			r.Contains(record.MessageIDs, survivor)
		})
	}
}

func TestMeetingDecisionPurgeKeepsNewestChoice(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := projectionFixture(t, st, "purge-priority-kept", "meeting_json", `{}`)
	_, duplicate := projectionFixture(t, st, "purge-priority-hidden", "meeting_json", `{}`)
	_, other := projectionFixture(t, st, "purge-priority-other", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "unlink")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "purge-priority")
	r.NoError(err)
	_, err = st.DeleteDedupedBatchContext(t.Context(), "purge-priority")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	r.NoError(err)
	assert.Equal(t, []int64{survivor, other}, record.MessageIDs)
}

func TestMeetingDecisionSourceMergeReplacesPreviouslyVisibleCopy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mergeSecond bool
	}{
		{"single merge", false},
		{"replace visible copy", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"shared","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			a, older := calendarFixture(t, st, "older", raw, "a@example.com")
			b, newer := calendarFixture(t, st, "newer", raw, "b@example.com")
			destinationRaw := `{"id":"other"}`
			if !tc.mergeSecond {
				destinationRaw = raw
			}
			destination, destinationID := calendarFixture(t, st, "destination", destinationRaw, "destination@example.com")
			_, note := projectionFixture(t, st, "source-note", "meeting_json", `{}`)
			eventRef := MeetingLinkRef{Kind: "evt", MessageID: newer}
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			_, err := st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "link")
			r.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: b.Message.SourceID, IntoSourceID: destination.Message.SourceID})
			r.NoError(err)
			if tc.mergeSecond {
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: a.Message.SourceID, IntoSourceID: destination.Message.SourceID})
				r.NoError(err)
			}
			ids := []int64{newer}
			if !tc.mergeSecond {
				ids = append(ids, destinationID)
			}
			if tc.mergeSecond {
				ids = append(ids, older)
			}
			for _, id := range ids {
				record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: id})
				r.NoError(err)
				r.Contains(record.MessageIDs, note)
			}
			_, err = st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "clear")
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Empty(record.Links)
		})
	}
	t.Run("notes", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		data, survivor := projectionFixture(t, st, "same-note", "meeting_json", `{}`)
		_, other := projectionFixture(t, st, "other-note", "meeting_json", `{}`)
		source, err := st.GetOrCreateSource("meeting_import", "copy@example.com")
		r.NoError(err)
		sender, err := st.EnsureParticipant("speaker@example.com", "Speaker Example", "")
		r.NoError(err)
		data.Message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		data.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
		_, err = st.UpsertMessage(data.Message)
		r.NoError(err)
		data.Conversation.SourceConversationID = "!meeting:example.org"
		_, err = st.db.ExecContext(t.Context(), `UPDATE conversations SET source_conversation_id=? WHERE id=?`, data.Conversation.SourceConversationID, data.Message.ConversationID)
		r.NoError(err)
		data.Message.ID, data.Message.SourceID, data.Message.ConversationID = 0, source.ID, 0
		duplicate, err := st.PersistMessage(data)
		r.NoError(err)
		ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "link")
		r.NoError(err)
		var destination int64
		r.NoError(st.db.QueryRowContext(t.Context(), `SELECT source_id FROM messages WHERE id=?`, survivor).Scan(&destination))
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: destination})
		r.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), ref(other))
		r.NoError(err)
		r.Equal([]int64{survivor, other}, record.MessageIDs)
	})
}

func TestMeetingDecisionSourceMergeKeepsNewerSplit(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	from, err := st.GetOrCreateSource("meeting_import", "history@example.test")
	r.NoError(err)
	into, err := st.GetOrCreateSource("meeting_import", "live@example.test")
	r.NoError(err)
	sender, err := st.EnsureParticipant("owner@example.test", "Example Owner", "example.test")
	r.NoError(err)
	sentAt := sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}
	add := func(sourceID int64, providerID string) int64 {
		conversation, err := st.EnsureConversationWithType(sourceID, "!shared:example.test", "meeting", "Shared meeting")
		r.NoError(err)
		content := "Transcript " + providerID
		id, err := st.PersistMessage(&MessagePersistData{
			Message:      &Message{SourceID: sourceID, ConversationID: conversation, SourceMessageID: providerID, MessageType: "meeting_transcript", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sentAt, Subject: sql.NullString{String: content, Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "!shared:example.test", ConversationType: "meeting", Title: "Shared meeting"},
			BodyText:     sql.NullString{String: content, Valid: true},
			RawMIME:      []byte(fmt.Sprintf(`{"summary_text":%q}`, content)), RawFormat: "meeting_json",
		})
		r.NoError(err)
		return id
	}
	a, b := add(from.ID, "meeting-a"), add(from.ID, "meeting-b")
	keptA, keptB := add(into.ID, "meeting-a"), add(into.ID, "meeting-b")
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(a), ref(keptB), "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(keptA), ref(b), "unlink")
	r.NoError(err)
	merged, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	r.NoError(err)
	r.Equal(int64(2), merged.DuplicatesHidden)
	record, err := st.MeetingRecordContext(t.Context(), ref(keptA))
	r.NoError(err)
	r.Equal([]int64{keptA}, record.MessageIDs)
}

func TestMeetingDecisionSourceMergeNewestHiddenChoice(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	from, err := st.GetOrCreateSource("meeting_import", "incoming@example.test")
	r.NoError(err)
	into, err := st.GetOrCreateSource("meeting_import", "destination@example.test")
	r.NoError(err)
	sender, err := st.EnsureParticipant("owner@example.test", "Example Owner", "example.test")
	r.NoError(err)
	add := func(sourceID int64, key string) int64 {
		conversation, err := st.EnsureConversationWithType(sourceID, "!shared:example.test", "meeting", "Shared meeting")
		r.NoError(err)
		content := "Transcript " + key
		data := &MessagePersistData{
			Message:      &Message{SourceID: sourceID, ConversationID: conversation, SourceMessageID: key, MessageType: "meeting_transcript", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}, Subject: sql.NullString{String: content, Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "!shared:example.test", ConversationType: "meeting", Title: "Shared meeting"},
			BodyText:     sql.NullString{String: content, Valid: true}, RawMIME: []byte(fmt.Sprintf(`{"summary_text":%q}`, content)), RawFormat: "meeting_json",
		}
		id, err := st.PersistMessage(data)
		r.NoError(err)
		return id
	}
	a, b := add(from.ID, "first"), add(from.ID, "second")
	keptA, keptB := add(into.ID, "first"), add(into.ID, "second")
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(keptA), ref(keptB), "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(a), ref(b), "unlink")
	r.NoError(err)
	result, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	r.NoError(err)
	r.Equal(int64(2), result.DuplicatesHidden)
	record, err := st.MeetingRecordContext(t.Context(), ref(keptA))
	r.NoError(err)
	r.Equal([]int64{keptA}, record.MessageIDs)
}

func TestMeetingDecisionDedupBetweenDuplicates(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, survivor := projectionFixture(t, st, "pair-survivor", "meeting_json", `{}`)
	_, first := projectionFixture(t, st, "pair-first", "meeting_json", `{}`)
	_, second := projectionFixture(t, st, "pair-second", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(first), ref(second), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{first, second}, "pair")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "pair")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(first))
	r.NoError(err)
	r.Contains(record.MessageIDs, second)
	r.NotContains(record.MessageIDs, survivor)
}

func TestMeetingDecisionNotesOnlySavedChoices(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"shared_20270314T100000Z"}}`
	_, _ = calendarFixture(t, st, "notes-only-calendar", `{"id":"shared_20270314T100000Z","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "owner@example.com")
	_, first := projectionFixture(t, st, "notes-only-first", "granola_json", raw)
	_, second := projectionFixture(t, st, "notes-only-second", "granola_json", raw)
	_, third := projectionFixture(t, st, "notes-only-third", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(first), ref(second), "unlink")
	require.NoError(t, err)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(first), ref(third), "link")
	require.NoError(t, err)
	full, err := st.MeetingRecordContext(t.Context(), ref(first))
	require.NoError(t, err)
	require.Len(t, full.Links, 1)
	record, err := st.MeetingNotesContext(t.Context(), ref(first))
	require.NoError(t, err)
	assert.Equal(t, []int64{first, third}, record.MessageIDs)
	assert.Empty(t, record.Links)
}

func TestParseMeetingLinkRef(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, tc := range []struct {
		input string
		want  MeetingLinkRef
	}{
		{"msg:5", MeetingLinkRef{Kind: "msg", MessageID: 5}},
		{"evt:9@20270314", MeetingLinkRef{Kind: "evt", MessageID: 9, Identity: calrecur.Identity{Date: "2027-03-14", AllDay: true}}},
		{"evt:10@20270314T100000Z", MeetingLinkRef{Kind: "evt", MessageID: 10, Identity: calrecur.Identity{Instant: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)}}},
	} {
		ref, err := ParseMeetingLinkRef(tc.input)
		require.NoError(err)
		assert.Equal(tc.want, ref)
	}
	for _, input := range []string{"msg:5@20270314", "evt:9@", "evt:9@20270230", "evt:0", "other:5"} {
		_, err := ParseMeetingLinkRef(input)
		require.Error(err, input)
	}
}

func TestMeetingDecisionFractionalException(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, series := calendarFixture(t, st, "fraction-master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00.500Z"},"end":{"dateTime":"2027-03-14T11:00:00.500Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, exception := calendarFixture(t, st, "fraction-exception", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00.500Z"},"start":{"dateTime":"2027-03-14T12:00:00Z"},"end":{"dateTime":"2027-03-14T13:00:00Z"}}`)
	_, note := projectionFixture(t, st, "fraction-note", "meeting_json", `{}`)
	eventRef := MeetingLinkRef{Kind: "evt", MessageID: exception}
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	decision, err := st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "link")
	r.NoError(err)
	r.Contains([]string{decision.Left, decision.Right}, fmt.Sprintf("evt:%d@20270314T100000.5Z", series))
	record, err := st.MeetingRecordContext(t.Context(), eventRef)
	r.NoError(err)
	r.Equal([]int64{note}, record.MessageIDs)
	r.Len(record.Links, 1)
	r.Equal(fmt.Sprintf("evt:%d", exception), record.Links[0].Ref)
}

func TestMeetingReferencePreservesFractionalSeconds(t *testing.T) {
	first, err := ParseMeetingLinkRef("evt:1@20270314T100000.1Z")
	require.NoError(t, err)
	second, err := ParseMeetingLinkRef("evt:1@20270314T100000.9Z")
	require.NoError(t, err)
	assert.Equal(t, "evt:1@20270314T100000.1Z", formatMeetingOccurrence(first.OccurrenceRef))
	assert.Equal(t, "evt:1@20270314T100000.9Z", formatMeetingOccurrence(second.OccurrenceRef))
}

func TestMeetingDecisionCleanupPreservesMergedChoice(t *testing.T) {
	for _, operation := range []string{"gc", "source", "serialized source"} {
		t.Run(operation, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			if operation == "gc" && st.IsPostgreSQL() {
				t.Skip("GC is SQLite-only")
			}
			raw := `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			_, kept := calendarFixture(t, st, "cleanup-kept", raw, "kept@example.com")
			data, hidden := calendarFixture(t, st, "cleanup-hidden", raw, "hidden@example.com")
			_, note := projectionFixture(t, st, "cleanup-note", "meeting_json", `{}`)
			eventRef := MeetingLinkRef{Kind: "evt", MessageID: hidden}
			noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
			_, err := st.DecideMeetingLinkContext(t.Context(), eventRef, noteRef, "link")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), kept, []int64{hidden}, "cleanup")
			r.NoError(err)
			switch operation {
			case "gc":
				r.NoError(st.MarkMessageDeleted(data.Message.SourceID, data.Message.SourceMessageID))
				plan, err := st.PlanGCContext(t.Context())
				r.NoError(err)
				_, err = st.ExecuteGCContext(t.Context(), plan)
				r.NoError(err)
			case "source":
				r.NoError(st.RemoveSource(data.Message.SourceID))
			case "serialized source":
				_, _, err = st.RemoveSourceSerialized(t.Context(), data.Message.SourceID)
				r.NoError(err)
			}
			record, err := st.MeetingRecordContext(t.Context(), noteRef)
			r.NoError(err)
			r.Len(record.Links, 1)
			assert.Equal(t, fmt.Sprintf("evt:%d", kept), record.Links[0].Ref)
		})
	}
}

func TestMeetingNotesSplitFromEarlierEvent(t *testing.T) {
	r := require.New(t)
	a := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, event := calendarFixture(t, st, "earlier-event", `{"id":"event","iCalUID":"shared@example.com","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	raw := `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"shared@example.com","startsAt":"2027-03-14T10:00:00Z"}}`
	_, first := projectionFixture(t, st, "twenty-first", "twenty_json", raw)
	_, second := projectionFixture(t, st, "twenty-second", "twenty_json", raw)
	firstRef := MeetingLinkRef{Kind: "msg", MessageID: first}
	_, err := st.DecideMeetingLinkContext(t.Context(), MeetingLinkRef{Kind: "evt", MessageID: event}, firstRef, "unlink")
	r.NoError(err)
	record, err := st.MeetingNotesContext(t.Context(), firstRef)
	r.NoError(err)
	a.Equal([]int64{first}, record.MessageIDs)
	a.Empty(record.Links)
	record, err = st.MeetingNotesContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: second})
	r.NoError(err)
	a.Equal([]int64{second}, record.MessageIDs)
	a.Empty(record.Links)
}

func TestMeetingDecisionChannelPurgePreservesChoice(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("slack", "channel@example.com")
	r.NoError(err)
	_, kept := projectionFixture(t, st, "channel-kept", "meeting_json", `{}`)
	data := &MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "channel-hidden", MessageType: "meeting_transcript"}, Conversation: &ConversationPersistData{SourceConversationID: "channel", ConversationType: "channel"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"}
	hidden, err := st.PersistMessage(data)
	r.NoError(err)
	_, note := projectionFixture(t, st, "channel-note", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(hidden), ref(note), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), kept, []int64{hidden}, "channel-dedup")
	r.NoError(err)
	r.NoError(st.PurgeChannelContext(t.Context(), source.ID, "channel"))
	record, err := st.MeetingRecordContext(t.Context(), ref(note))
	r.NoError(err)
	assert.Equal(t, []int64{kept, note}, record.MessageIDs)
}

func TestMeetingDecisionSourceRemovalPreservesMergedNote(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "removed@example.com")
	r.NoError(err)
	_, kept := projectionFixture(t, st, "removed-kept", "meeting_json", `{}`)
	data := &MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "removed-hidden", MessageType: "meeting_transcript"}, Conversation: &ConversationPersistData{SourceConversationID: "removed-hidden", ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"}
	hidden, err := st.PersistMessage(data)
	r.NoError(err)
	_, note := projectionFixture(t, st, "removed-other", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(hidden), ref(note), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), kept, []int64{hidden}, "removed-dedup")
	r.NoError(err)
	r.NoError(st.RemoveSource(source.ID))
	record, err := st.MeetingRecordContext(t.Context(), ref(note))
	r.NoError(err)
	assert.Equal(t, []int64{kept, note}, record.MessageIDs)
}
