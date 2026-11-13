package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
)

func TestMeetingDecisionLargeGroupMemory(t *testing.T) {
	r := require.New(t)
	g := meetingDecisionGraph{nodes: map[string]meetingDecisionNode{}}
	var group []meetingDecisionEndpoint
	for i := range 1200 {
		e := meetingDecisionEndpoint{ID: int64(i + 1), Kind: meetingRefMessage}
		group = append(group, e)
		g.nodes[e.ref()] = meetingDecisionNode{endpoint: e}
	}
	extra := meetingDecisionEndpoint{ID: 1201, Kind: meetingRefMessage}
	g.nodes[extra.ref()] = meetingDecisionNode{endpoint: extra}
	g.groups = [][]meetingDecisionEndpoint{group}
	g.candidates = []meetingDecisionEdge{{a: group[0], b: extra, decision: "link"}}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	root, _, _ := g.reduce()
	runtime.ReadMemStats(&after)
	r.Equal(root(group[0].ref()), root(extra.ref()))
	r.Equal(root(group[0].ref()), root(group[1199].ref()))
	r.Less(after.TotalAlloc-before.TotalAlloc, uint64(100<<20), "joining a large group must not retain every pair")
}

func TestMeetingDecisionCopyRootsKeepOccurrencesSeparate(t *testing.T) {
	r := require.New(t)
	first := meetingDecisionEndpoint{ID: 1, Kind: meetingRefEvent, Key: "20270314"}
	copyEvent := meetingDecisionEndpoint{ID: 2, Kind: meetingRefEvent, Key: "20270314"}
	second := meetingDecisionEndpoint{ID: 1, Kind: meetingRefEvent, Key: "20270321"}
	note := meetingDecisionEndpoint{ID: 3, Kind: meetingRefMessage}
	g := meetingDecisionGraph{nodes: map[string]meetingDecisionNode{}}
	for _, endpoint := range []meetingDecisionEndpoint{first, copyEvent, second, note} {
		g.nodes[endpoint.ref()] = meetingDecisionNode{endpoint: endpoint}
	}
	g.groups = [][]meetingDecisionEndpoint{{first, copyEvent}}
	g.candidates = []meetingDecisionEdge{{a: first, b: note, decision: "link"}, {a: second, b: note, decision: "link"}}
	root, _, copies := g.reduce()
	r.Equal(root(first.ref()), root(second.ref()))
	r.Equal(copies[first.ref()], copies[copyEvent.ref()])
	r.NotEqual(copies[first.ref()], copies[second.ref()])
}

func TestMeetingLinkDecisionReadsOnlyRequestedOccurrence(t *testing.T) {
	r := require.New(t)
	base := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=52"]}`
	_, event := calendarFixture(t, base, "separate-series", raw)
	_, duplicate := calendarFixture(t, base, "separate-duplicate", raw)
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
	budget := 20
	edges, err := base.meetingDecisionEdges(t.Context(), endpointFromRef(occurrence), &budget)
	r.NoError(err)
	r.Len(edges, 1)
	r.Contains([]int64{edges[0].a.ID, edges[0].b.ID}, firstNote)
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
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "clear")
	r.NoError(err)
	other, err := st.PersistMessage(&MessagePersistData{Message: &Message{SourceID: visible.ID, SourceMessageID: "other", MessageType: "meeting_transcript"}, Conversation: &ConversationPersistData{SourceConversationID: "other", ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), refs[0].MessageID, []int64{refs[1].MessageID}, "scoped")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(ctx, refs[0], MeetingLinkRef{Kind: "msg", MessageID: other}, "link")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "scoped")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), refs[1])
	r.NoError(err)
	r.NotContains(record.MessageIDs, other)
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
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(groups[0][0]), ref(groups[0][1]), "clear")
	requirements.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(groups[0][1]))
	requirements.NoError(err)
	requirements.Equal(all, record.MessageIDs)
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
	for _, name := range []string{"conflict-a", "conflict-b", "conflict-c", "conflict-d"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	for _, pair := range [][2]int{{0, 1}, {1, 2}, {1, 2}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), refs[pair[0]], refs[pair[1]], "link")
		requirements.NoError(err)
	}
	result, err := st.DecideMeetingLinkContext(t.Context(), refs[1], refs[2], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
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
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[2], refs[1], "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[2], refs[1], "clear")
	requirements.ErrorIs(err, sql.ErrNoRows)
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	result, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "clear")
	requirements.NoError(err)
	assertions.Equal("automatic", result.State)
}

func TestMeetingLinkDecisionRejectsSameCopyAndInvalidDecision(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, master := calendarFixture(t, st, "decision-master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, edit := calendarFixture(t, st, "decision-edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	occurrence, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270314T100000Z", master))
	requirements.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, MeetingLinkRef{Kind: "evt", MessageID: edit}, "link")
	requirements.ErrorIs(err, ErrMeetingLinkSameCopy)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, occurrence, "link")
	requirements.ErrorIs(err, ErrMeetingLinkSameCopy)
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, MeetingLinkRef{Kind: "evt", MessageID: edit}, "invalid")
	requirements.ErrorIs(err, ErrInvalidMeetingLinkDecision)
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
	_, err := st.DecideMeetingLinkContext(t.Context(), firstRef, secondRef, "unlink")
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
	for _, link := range record.Links {
		r.NotEqual(fmt.Sprintf("evt:%d", first), link.Ref)
	}
}

func TestMeetingLinkDecisionBridgeDoesNotMakeEventsCopies(t *testing.T) {
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
	var decision string
	r.NoError(st.db.QueryRowContext(t.Context(), `SELECT decision FROM meeting_link_decisions WHERE (left_message_id=? AND right_message_id=?) OR (left_message_id=? AND right_message_id=?)`, note, first, first, note).Scan(&decision))
	r.Equal("split", decision)
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
				requirements.Equal([]int64{ids[0], ids[1], ids[3]}, record.MessageIDs)
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
			if decision == "link" {
				assertions.Equal([]int64{note}, record.MessageIDs)
			}
		})
	}
}

func TestMeetingLinkDecisionMissingOccurrenceWrites(t *testing.T) {
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

func TestMeetingLinkDecisionSubsetSourceWithoutDecisionTables(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "source.db")
	st, err := OpenForTest(t.Context(), path)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchemaContext(t.Context()))
	_, _ = projectionFixture(t, st, "version-note", "meeting_json", `{}`)
	_, err = st.db.ExecContext(t.Context(), `DROP TABLE meeting_copy_survivors`)
	requirements.NoError(err)
	_, err = st.db.ExecContext(t.Context(), `DROP TABLE meeting_link_decisions`)
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
	result, err := st.DecideMeetingLinkContext(t.Context(), left, right, "unlink")
	requirements.NoError(err)
	assertions.Equal("split", result.State)
	for _, ref := range []MeetingLinkRef{left, right} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		requirements.NoError(err)
		assertions.Len(record.Links, 1)
	}
}

func TestMeetingLinkDecisionDropsRemovedOccurrence(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, event := calendarFixture(t, st, "shrinking-series", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, note := projectionFixture(t, st, "series-note", "meeting_json", `{}`)
	left := MeetingLinkRef{Kind: "msg", MessageID: note}
	right, err := ParseMeetingLinkRef(fmt.Sprintf("evt:%d@20270321T100000Z", event))
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	r.NoError(err)
	data.RawMIME = []byte(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=1"]}`)
	_, err = st.PersistMessage(data)
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), left)
	r.NoError(err)
	r.Equal([]int64{note}, record.MessageIDs)
	r.Empty(record.Links)
	_, err = st.MeetingRecordContext(t.Context(), right)
	r.ErrorIs(err, sql.ErrNoRows)
}

func TestMeetingLinkDecisionDropsReclassifiedNote(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, first := projectionFixture(t, st, "still-note", "meeting_json", `{}`)
	data, second := projectionFixture(t, st, "now-email", "meeting_json", `{}`)
	left, right := MeetingLinkRef{Kind: "msg", MessageID: first}, MeetingLinkRef{Kind: "msg", MessageID: second}
	_, err := st.DecideMeetingLinkContext(t.Context(), left, right, "link")
	r.NoError(err)
	data.Message.MessageType = "email"
	_, err = st.PersistMessage(data)
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), left)
	r.NoError(err)
	r.Equal([]int64{first}, record.MessageIDs)
	r.Empty(record.Links)
	_, err = st.MeetingRecordContext(t.Context(), right)
	r.ErrorIs(err, sql.ErrNoRows)
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
	for _, tc := range []struct {
		name, decision, purge, firstUndo, timing     string
		afterFirst, afterMerge, afterUndo, afterBoth []int
	}{
		{"link_undo_first", "link", "", "first", "", []int{1, 3}, []int{2, 3}, []int{0, 2, 3}, []int{0, 1, 2, 3}},
		{"link_undo_second", "link", "", "second", "", []int{1, 3}, []int{2, 3}, []int{1, 2, 3}, []int{0, 1, 2, 3}},
		{"split_undo_first", "unlink", "", "first", "", []int{3}, []int{3}, []int{3}, []int{3}},
		{"split_undo_second", "unlink", "", "second", "", []int{3}, []int{3}, []int{3}, []int{3}},
		{"survivor_before", "link", "", "first", "before", nil, []int{2, 3}, []int{0, 2, 3}, nil},
		{"survivor_after", "link", "", "first", "after", nil, []int{2, 3}, []int{0, 2, 3}, nil},
		{"purge_first", "link", "first", "second", "", nil, []int{2, 3}, []int{1, 2, 3}, nil},
		{"purge_second", "link", "second", "first", "", nil, []int{2, 3}, []int{0, 2, 3}, nil},
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
			ref := func(id int64) MeetingLinkRef {
				return MeetingLinkRef{Kind: "msg", MessageID: id}
			}
			want := func(indices []int) []int64 {
				result := make([]int64, 0, len(indices))
				for _, i := range indices {
					result = append(result, ids[i])
				}
				return result
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(ids[0]), ref(ids[3]), tc.decision)
			r.NoError(err)
			if tc.timing == "before" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				r.NoError(err)
			}
			_, err = st.MergeDuplicates(t.Context(), ids[1], []int64{ids[0]}, "first")
			r.NoError(err)
			if tc.afterFirst != nil {
				record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
				r.NoError(err)
				r.Equal(want(tc.afterFirst), record.MessageIDs)
			}
			_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
			r.NoError(err)
			if tc.timing == "after" {
				_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[2]), ref(ids[3]), "link")
				r.NoError(err)
			}
			if tc.purge != "" {
				_, err = st.DeleteDedupedBatchContext(t.Context(), tc.purge)
			}
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			r.Equal(want(tc.afterMerge), record.MessageIDs)
			_, err = st.UndoDedup(t.Context(), tc.firstUndo)
			r.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			r.Equal(want(tc.afterUndo), record.MessageIDs)
			if tc.afterBoth == nil {
				return
			}
			remaining := "first"
			if tc.firstUndo == "first" {
				remaining = "second"
			}
			_, err = st.UndoDedup(t.Context(), remaining)
			r.NoError(err)
			record, err = st.MeetingRecordContext(t.Context(), ref(ids[3]))
			r.NoError(err)
			r.Equal(want(tc.afterBoth), record.MessageIDs)
		})
	}
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
	for _, tc := range []struct {
		name, purge string
		reset       bool
	}{{"purge_intermediate", "second", false}, {"purge_opposite", "opposite", false}, {"purge_opposite_reset", "opposite", true}, {"undo_both", "", true}} {
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
	if purge != "second" {
		_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
		r.NoError(err)
	}
	_, err = st.MergeDuplicates(t.Context(), ids[4], []int64{ids[3]}, "opposite")
	r.NoError(err)
	if purge == "second" {
		_, err = st.MergeDuplicates(t.Context(), ids[2], []int64{ids[1]}, "second")
		r.NoError(err)
	}
	if purge != "" {
		_, err = st.DeleteDedupedBatchContext(t.Context(), purge)
		r.NoError(err)
	}
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
		r.Equal([]int64{ids[0], ids[2], ids[4]}, record.MessageIDs)
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
		if remaining == "opposite" {
			r.Equal([]int64{ids[0], ids[2], ids[3], ids[4]}, record.MessageIDs)
		} else {
			r.Equal([]int64{ids[0], ids[1], ids[2], ids[4]}, record.MessageIDs)
		}
	}
	if purge == "" {
		_, err = st.UndoDedup(t.Context(), "opposite")
		r.NoError(err)
		record, err = st.MeetingRecordContext(t.Context(), ref(0))
		r.NoError(err)
		r.Equal([]int64{ids[0]}, record.MessageIDs)
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
			var count int
			r.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
			r.Equal(1, count)
			selected := MeetingLinkRef{Kind: "evt", MessageID: event, Identity: occurrenceIdentity(t, "20270314T100000Z")}
			if action == "clear" {
				_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, bare, "clear")
				r.NoError(err)
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
	for _, rule := range []string{"RRULE:FREQ=WEEKLY;COUNT=2", "RRULE:FREQ=YEARLY;BYWEEKNO=20"} {
		for _, order := range []string{"edit-first", "series-first"} {
			for _, side := range []string{"series", "edit"} {
				t.Run(rule+"/"+order+"/"+side, func(t *testing.T) {
					r := require.New(t)
					st := newRFC822IDBackfillBackendStore(t)
					var series, edit int64
					makeSeries := func() {
						_, series = calendarFixture(t, st, "series", fmt.Sprintf(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":[%q]}`, rule))
					}
					makeEdit := func() {
						_, edit = calendarFixture(t, st, "edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-15T10:00:00Z"},"end":{"dateTime":"2027-03-15T11:00:00Z"}}`)
					}
					if order == "edit-first" {
						makeEdit()
						calendarFixture(t, st, "copy", `{"id":"copy","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "other@example.com")
						makeSeries()
					} else {
						makeSeries()
						makeEdit()
					}
					_, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
					noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
					seriesRef := MeetingLinkRef{Kind: "evt", MessageID: series, Identity: occurrenceIdentity(t, "20270314T100000Z")}
					editRef := MeetingLinkRef{Kind: "evt", MessageID: edit}
					write, other := seriesRef, editRef
					if side == "edit" {
						write, other = other, write
					}
					_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, write, "link")
					r.NoError(err)
					for _, ref := range []MeetingLinkRef{write, other} {
						record, err := st.MeetingRecordContext(t.Context(), ref)
						r.NoError(err)
						r.Contains(record.MessageIDs, note)
						r.NotEmpty(record.Links)
					}
					_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, other, "clear")
					r.NoError(err)
					_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, other, "unlink")
					r.NoError(err)
					record, err := st.MeetingRecordContext(t.Context(), write)
					r.NoError(err)
					r.NotContains(record.MessageIDs, note)
					_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, write, "clear")
					r.NoError(err)
				})
			}
		}
	}
}

func TestMeetingLinkDecisionConflictNamesBlockingPair(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var refs []MeetingLinkRef
	for _, name := range []string{"a", "b", "c"} {
		_, id := projectionFixture(t, st, name, "meeting_json", `{}`)
		refs = append(refs, MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	_, err := st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[1], refs[2], "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[2], "unlink")
	r.ErrorIs(err, ErrMeetingLinkConflict)
	r.Contains(err.Error(), fmt.Sprintf("msg:%d", refs[1].MessageID))
}

func TestMeetingLinkDecisionSurvivesDeleteAllDeduped(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, original := projectionFixture(t, st, "original", "meeting_json", `{}`)
	_, survivor := projectionFixture(t, st, "survivor", "meeting_json", `{}`)
	_, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(original), ref(note), "link")
	r.NoError(err)
	_, err = st.MergeDuplicates(t.Context(), survivor, []int64{original}, "purge-all")
	r.NoError(err)
	_, _, err = st.DeleteAllDedupedContext(t.Context())
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(note))
	r.NoError(err)
	r.Contains(record.MessageIDs, survivor)
}

func TestMeetingLinkDecisionSurvivesSourceMerge(t *testing.T) {
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
	_, earlier := projectionFixture(t, st, "earlier-survivor", "meeting_json", `{}`)
	_, err = st.MergeDuplicates(t.Context(), earlier, []int64{duplicate}, "undone")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "undone")
	r.NoError(err)
	var destination int64
	r.NoError(st.db.QueryRowContext(t.Context(), `SELECT source_id FROM messages WHERE id=?`, survivor).Scan(&destination))
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: destination})
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	r.NoError(err)
	r.Equal([]int64{survivor, other, earlier}, record.MessageIDs)
}

func TestMeetingLinkDecisionEquivalentCopiesConflict(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
	_, first := calendarFixture(t, st, "first", raw, "first@example.com")
	_, second := calendarFixture(t, st, "second", raw, "second@example.com")
	_, note := projectionFixture(t, st, "note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	firstRef := MeetingLinkRef{Kind: "evt", MessageID: first}
	secondRef := MeetingLinkRef{Kind: "evt", MessageID: second}
	_, err := st.DecideMeetingLinkContext(t.Context(), noteRef, firstRef, "link")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), noteRef, secondRef, "unlink")
	r.ErrorIs(err, ErrMeetingLinkConflict)
	r.ErrorContains(err, fmt.Sprintf("evt:%d msg:%d", first, note))
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
	var count int
	r.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
	r.Equal(2, count)
	_, err = st.UndoDedup(t.Context(), "repeat")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	r.NoError(err)
	r.ElementsMatch([]int64{survivor, duplicate, other}, record.MessageIDs)
	t.Run("first choice after merge", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, survivor := projectionFixture(t, st, "late-survivor", "meeting_json", `{}`)
		_, duplicate := projectionFixture(t, st, "late-duplicate", "meeting_json", `{}`)
		_, other := projectionFixture(t, st, "late-other", "meeting_json", `{}`)
		ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
		_, err := st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "late")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "link")
		r.NoError(err)
		_, err = st.UndoDedup(t.Context(), "late")
		r.NoError(err)
		record, err := st.MeetingRecordContext(t.Context(), ref(other))
		r.NoError(err)
		r.ElementsMatch([]int64{survivor, other}, record.MessageIDs)
	})
	t.Run("single event", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		raw := `{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
		_, survivor := calendarFixture(t, st, "repeat-survivor", raw)
		_, duplicate := calendarFixture(t, st, "repeat-duplicate", raw)
		_, note := projectionFixture(t, st, "repeat-note", "meeting_json", `{}`)
		event := MeetingLinkRef{Kind: "evt", MessageID: duplicate}
		other := MeetingLinkRef{Kind: "msg", MessageID: note}
		_, err := st.DecideMeetingLinkContext(t.Context(), event, other, "link")
		r.NoError(err)
		_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "event-repeat")
		r.NoError(err)
		_, err = st.DecideMeetingLinkContext(t.Context(), event, other, "link")
		r.NoError(err)
		var count int
		r.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM meeting_link_decisions`).Scan(&count))
		r.Equal(2, count)
	})
}

func TestMeetingDecisionSurvivorWinsAfterDedupAndPurge(t *testing.T) {
	for _, purge := range []string{"none", "batch", "all"} {
		t.Run(purge, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"newest_20270314T100000Z"}}`
			_, survivor := projectionFixture(t, st, "newest-survivor", "granola_json", raw)
			_, duplicate := projectionFixture(t, st, "newest-duplicate", "granola_json", raw)
			_, other := projectionFixture(t, st, "newest-other", "granola_json", raw)
			ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "link")
			r.NoError(err)
			_, err = st.DecideMeetingLinkContext(t.Context(), ref(duplicate), ref(other), "unlink")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "newest")
			r.NoError(err)
			switch purge {
			case "batch":
				_, err = st.DeleteDedupedBatchContext(t.Context(), "newest")
				r.NoError(err)
			case "all":
				_, _, err = st.DeleteAllDedupedContext(t.Context())
				r.NoError(err)
			}
			record, err := st.MeetingRecordContext(t.Context(), ref(other))
			r.NoError(err)
			r.Contains(record.MessageIDs, survivor)
		})
	}
}

func TestMeetingDecisionUpdatedChoiceIsNewest(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"updated_20270314T100000Z"}}`
	_, survivor := projectionFixture(t, st, "updated-survivor", "granola_json", raw)
	_, duplicate := projectionFixture(t, st, "updated-duplicate", "granola_json", raw)
	_, other := projectionFixture(t, st, "updated-other", "granola_json", raw)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	for _, step := range []struct {
		left     int64
		decision string
	}{{survivor, "unlink"}, {duplicate, "unlink"}, {survivor, "link"}} {
		_, err := st.DecideMeetingLinkContext(t.Context(), ref(step.left), ref(other), step.decision)
		r.NoError(err)
	}
	_, err := st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "updated")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(other))
	r.NoError(err)
	r.Contains(record.MessageIDs, survivor)
}

func TestMeetingDecisionResetUntimedDedupAfterSeriesUpdate(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	data, survivor := calendarFixture(t, st, "reset-series-survivor", `{"id":"event","status":"cancelled"}`)
	_, duplicate := calendarFixture(t, st, "reset-series-duplicate", `{"id":"event","status":"cancelled"}`)
	_, note := projectionFixture(t, st, "reset-series-note", "meeting_json", `{}`)
	_, otherNote := projectionFixture(t, st, "reset-series-other", "meeting_json", `{}`)
	event := MeetingLinkRef{Kind: "evt", MessageID: survivor}
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	_, err := st.MergeDuplicates(t.Context(), survivor, []int64{duplicate}, "reset-series")
	r.NoError(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), event, noteRef, "link")
	r.NoError(err)
	data.RawMIME = []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	_, err = st.PersistMessage(data)
	r.NoError(err)
	otherRef := MeetingLinkRef{Kind: "msg", MessageID: otherNote}
	occurrence := MeetingLinkRef{Kind: "evt", MessageID: survivor, Identity: occurrenceIdentity(t, "20270321T100000Z")}
	_, err = st.DecideMeetingLinkContext(t.Context(), occurrence, otherRef, "link")
	r.NoError(err)
	scoped := sourcescope.WithSourceIDs(t.Context(), []int64{data.Message.SourceID})
	_, err = st.DecideMeetingLinkContext(scoped, event, noteRef, "clear")
	r.Error(err)
	_, err = st.DecideMeetingLinkContext(t.Context(), event, noteRef, "clear")
	r.NoError(err)
	_, err = st.UndoDedup(t.Context(), "reset-series")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), noteRef)
	r.NoError(err)
	r.Empty(record.Links)
	record, err = st.MeetingRecordContext(t.Context(), otherRef)
	r.NoError(err)
	r.Len(record.Links, 1)
	r.Equal(endpointFromRef(occurrence).ref(), record.Links[0].Ref)
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

func TestMeetingDecisionFollowsCalendarHiddenBySourceMerge(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	into, visible := calendarFixture(t, st, "visible-event", `{"id":"shared","summary":"Visible","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "visible@example.com")
	from, hidden := calendarFixture(t, st, "hidden-event", `{"id":"shared","summary":"Archived","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`, "archived@example.com")
	_, note := projectionFixture(t, st, "source-merge-note", "meeting_json", `{}`)
	noteRef := MeetingLinkRef{Kind: "msg", MessageID: note}
	hiddenRef := MeetingLinkRef{Kind: "evt", MessageID: hidden}
	visibleRef := MeetingLinkRef{Kind: "evt", MessageID: visible}
	_, err := st.DecideMeetingLinkContext(t.Context(), hiddenRef, noteRef, "link")
	r.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from.Message.SourceID, IntoSourceID: into.Message.SourceID})
	r.NoError(err)
	for _, ref := range []MeetingLinkRef{hiddenRef, visibleRef} {
		record, err := st.MeetingRecordContext(t.Context(), ref)
		r.NoError(err)
		r.Contains(record.MessageIDs, note)
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), hiddenRef, noteRef, "clear")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), visibleRef)
	r.NoError(err)
	r.NotContains(record.MessageIDs, note)
}

func TestMeetingDecisionDedupCollisionPriority(t *testing.T) {
	for _, survivorChoice := range []bool{false, true} {
		t.Run(fmt.Sprintf("survivor-choice-%t", survivorChoice), func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, survivor := projectionFixture(t, st, "collision-survivor", "meeting_json", `{}`)
			_, first := projectionFixture(t, st, "collision-first", "meeting_json", `{}`)
			_, second := projectionFixture(t, st, "collision-second", "meeting_json", `{}`)
			_, other := projectionFixture(t, st, "collision-other", "meeting_json", `{}`)
			ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
			if survivorChoice {
				_, err := st.DecideMeetingLinkContext(t.Context(), ref(survivor), ref(other), "link")
				r.NoError(err)
			}
			_, err := st.DecideMeetingLinkContext(t.Context(), ref(first), ref(other), "link")
			r.NoError(err)
			_, err = st.DecideMeetingLinkContext(t.Context(), ref(second), ref(other), "unlink")
			r.NoError(err)
			_, err = st.MergeDuplicates(t.Context(), survivor, []int64{first, second}, "collision")
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), ref(other))
			r.NoError(err)
			if survivorChoice {
				r.Contains(record.MessageIDs, survivor)
			} else {
				r.NotContains(record.MessageIDs, survivor)
			}
		})
	}
}

func TestMeetingDecisionSplitAndJoinKeepAutomaticMembers(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"owner":{"email":"owner@example.com"},"calendar_event":{"calendar_event_id":"group_20270314T100000Z"}}`
	_, first := projectionFixture(t, st, "group-first", "granola_json", raw)
	_, automatic := projectionFixture(t, st, "group-automatic", "granola_json", raw)
	_, incoming := projectionFixture(t, st, "group-incoming", "meeting_json", `{}`)
	ref := func(id int64) MeetingLinkRef { return MeetingLinkRef{Kind: "msg", MessageID: id} }
	_, err := st.DecideMeetingLinkContext(t.Context(), ref(first), ref(incoming), "link")
	r.NoError(err)
	record, err := st.MeetingRecordContext(t.Context(), ref(incoming))
	r.NoError(err)
	r.ElementsMatch([]int64{first, automatic, incoming}, record.MessageIDs)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(first), ref(automatic), "unlink")
	r.NoError(err)
	record, err = st.MeetingRecordContext(t.Context(), ref(automatic))
	r.NoError(err)
	r.Equal([]int64{automatic}, record.MessageIDs)
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(automatic), ref(incoming), "link")
	r.ErrorIs(err, ErrMeetingLinkConflict)
	record, err = st.MeetingRecordContext(t.Context(), ref(first))
	r.NoError(err)
	r.ElementsMatch([]int64{first, incoming}, record.MessageIDs)
}

func TestMeetingDecisionSourceMergeCollisionPriority(t *testing.T) {
	for _, survivorChoice := range []bool{false, true} {
		t.Run(fmt.Sprintf("survivor-choice-%t", survivorChoice), func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := `{"id":"shared","summary":%q,"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			into, survivor := calendarFixture(t, st, "visible", fmt.Sprintf(raw, "visible"), "visible@example.com")
			from, first := calendarFixture(t, st, "first", fmt.Sprintf(raw, "first"), "archived@example.com")
			_, second := calendarFixture(t, st, "second", fmt.Sprintf(raw, "second"), "archived@example.com")
			_, note := projectionFixture(t, st, "source-collision-note", "meeting_json", `{}`)
			// Conflicting archived choices can arise before sources are combined.
			insert := func(event int64, choice string) {
				a := meetingDecisionEndpoint{ID: event, Kind: "evt", Key: "20270314T100000Z"}
				b := meetingDecisionEndpoint{ID: note, Kind: "msg"}
				if compareMeetingEndpoints(a, b) > 0 {
					a, b = b, a
				}
				_, err := st.db.ExecContext(t.Context(), `INSERT INTO meeting_link_decisions(left_message_id,left_kind,left_key,right_message_id,right_kind,right_key,decision) VALUES (?,?,?,?,?,?,?)`, a.ID, a.Kind, a.Key, b.ID, b.Kind, b.Key, choice)
				r.NoError(err)
			}
			if survivorChoice {
				insert(survivor, "link")
			}
			insert(first, "link")
			insert(second, "split")
			_, err := st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from.Message.SourceID, IntoSourceID: into.Message.SourceID})
			r.NoError(err)
			record, err := st.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: note})
			r.NoError(err)
			if survivorChoice {
				r.Len(record.Links, 1)
			} else {
				r.Empty(record.Links)
			}
		})
	}
}
