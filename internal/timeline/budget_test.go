package timeline

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func budgetTimeline(n int) *Timeline {
	v := sampleTimeline(ModeAnchor)
	v.Person = nil
	v.Anchor = nil
	v.Candidates = nil
	v.Freshness = Freshness{AsOf: v.Now, StaleThreshold: 86400}
	v.Gaps = nil
	v.Days = nil
	v.Upcoming = nil
	v.OpenLoops = nil
	v.History = nil
	v.Cursor = ""
	v.Omitted = Omitted{}
	keys := map[string]string{}
	for i := range n {
		e := Entry{Kind: "message", Key: fmt.Sprintf("msg:%d", i+1), Ref: fmt.Sprintf("msg:%d", i+1), AnchorRef: fmt.Sprintf("msg:%d", i+1), Time: v.Now, Open: []Open{{Tool: "get_message", Args: map[string]any{"id": i + 1}}}, Message: &MessageEntry{Direction: "inbound", People: []Person{{Name: "Alice"}}, Snippet: UntrustedText{Text: strings.Repeat("hello ", 14)}}}
		v.History = append(v.History, e)
		keys[e.Key] = fmt.Sprintf("%03d", i+1)
	}
	b := cursorBinding()
	SetContinuation(v, Continuation{Binding: b, SortKeys: keys})
	return v
}
func encodedSize(t *testing.T, v *Timeline, format string) int {
	t.Helper()
	if format == "markdown" {
		return len(Markdown(v))
	}
	b, err := json.Marshal(v, jsontext.EscapeForHTML(true))
	require.NoError(t, err)
	return len(b)
}
func TestFitMeasuresFinalEncoding(t *testing.T) {
	for _, tc := range []struct {
		format          string
		budget, entries int
	}{
		{"json", 4096, 100}, {"markdown", 4096, 100}, {"json", 8192, 50},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.format, tc.budget), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			v := budgetTimeline(tc.entries)
			out, err := Fit(v, tc.format, tc.budget)
			requirements.NoError(err)
			assertions.LessOrEqual(encodedSize(t, out, tc.format), tc.budget)
			if tc.format == "json" {
				cursor, err := DecodeCursor(out.Cursor, cursorBinding())
				requirements.NoError(err)
				assertions.Equal(fmt.Sprintf("%03d", len(out.History)), cursor.Positions["history"])
			}
			assertions.NotEmpty(out.Cursor)
			assertions.NotEmpty(out.History)
			assertions.Less(len(out.History), tc.entries)
			assertions.Len(v.History, tc.entries)
			assertions.Empty(v.Cursor)
		})
	}
}
func TestFitDaySectionsAtMinimumBudget(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	v := sampleTimeline(ModeDay)
	SetContinuation(v, Continuation{Binding: cursorBinding(), SortKeys: map[string]string{
		v.Days[0].Meetings[0].Key: "001", v.Days[0].Meetings[1].Key: "002",
		v.Days[0].Bursts[0].Key: "003", v.Days[0].Calls[0].Key: "004",
		"day:2026-10-20": "005",
	}})
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.LessOrEqual(encodedSize(t, out, "json"), 4096)
	assertions.NotEmpty(out.OpenLoops.Items)
	assertions.NotEmpty(out.Upcoming)
	assertions.NotEmpty(out.Days)
}

func TestFitRelationshipBoundsAgendaAtMinimumBudget(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	v := sampleTimeline(ModeRelationship)
	v.Person.Agenda = nil
	for i := range 20 {
		v.Person.Agenda = append(v.Person.Agenda, UntrustedText{Text: fmt.Sprintf("%02d %s", i, strings.Repeat("a", 80))})
	}
	SetContinuation(v, Continuation{Binding: cursorBinding()})
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.LessOrEqual(encodedSize(t, out, "json"), 4096)
	requirements.Len(out.Person.Agenda, 5)
	assertions.Equal("00 "+strings.Repeat("a", 77), out.Person.Agenda[0].Text)
	assertions.True(out.Person.Agenda[0].Truncated)
	assertions.Equal(15, out.Omitted.Budget["agenda"])
	assertions.NotEmpty(out.OpenLoops.Items)
	assertions.NotEmpty(out.Upcoming)
	assertions.NotEmpty(out.History)
}

func TestFitBudgetsFreshness(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			v := budgetTimeline(1)
			for i := range 100 {
				v.Freshness.Sources = append(v.Freshness.Sources, SourceFreshness{SourceID: new(int64(i + 1)), SourceType: "gmail", Identifier: fmt.Sprintf("account-%d@example.com", i), Reason: "stale", SyncCommand: "msgvault sync-incremental account@example.com"})
			}
			out, err := Fit(v, format, 4096)
			requirements.NoError(err)
			assertions.LessOrEqual(encodedSize(t, out, format), 4096)
			assertions.NotEmpty(out.History)
			assertions.NotEmpty(out.Freshness.Sources)
			assertions.Less(len(out.Freshness.Sources), 100)
			assertions.Equal(100-len(out.Freshness.Sources), out.Omitted.Budget["freshness"])
			assertions.Empty(out.Cursor)
		})
	}
}

func TestFitFallbackCursorKeysPreserveSubsecondOrder(t *testing.T) {
	for _, section := range []string{"history", "upcoming"} {
		t.Run(section, func(t *testing.T) {
			requirements := require.New(t)
			var keys []string
			for _, offset := range []time.Duration{0, 500 * time.Millisecond, 550 * time.Millisecond, time.Second} {
				v := budgetTimeline(1)
				v.History[0].Time = v.Now.Add(offset)
				if section == "upcoming" {
					v.Mode = ModeDay
					meeting := sampleTimeline(ModeDay).Upcoming[0]
					meeting.Time = v.Now.Add(offset)
					meeting.Meeting.Start = meeting.Time
					meeting.Meeting.End = meeting.Time.Add(time.Hour)
					v.Upcoming = []Entry{meeting}
					v.History = nil
				}
				SetContinuation(v, Continuation{Binding: cursorBinding(), More: map[string]bool{section: true}})
				out, err := Fit(v, "json", 4096)
				requirements.NoError(err)
				cursor, err := DecodeCursor(out.Cursor, cursorBinding())
				requirements.NoError(err)
				requirements.NotEmpty(cursor.Positions[section])
				keys = append(keys, cursor.Positions[section])
			}
			for i := 1; i < len(keys); i++ {
				assert.Less(t, keys[i-1], keys[i])
			}
		})
	}
}
func TestFitTooSmall(t *testing.T) {
	v := budgetTimeline(0)
	_, err := Fit(v, "json", 1)
	require.ErrorIs(t, err, ErrBudgetTooSmall)
}

func TestFitOmitsOversizedFreshnessWarning(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	v := budgetTimeline(0)
	v.Freshness.Warnings = []string{strings.Repeat("warning ", 2000)}
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.Empty(out.Freshness.Warnings)
	assertions.LessOrEqual(encodedSize(t, out, "json"), 4096)
	assertions.Equal(1, out.Omitted.Budget["freshness"])
	assertions.Empty(out.Cursor)
}
func TestFitForwardProgressStub(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() *Timeline
		check func(*testing.T, *Timeline, *Timeline)
	}{
		{name: "message", build: func() *Timeline {
			v := budgetTimeline(2)
			v.History[0].Message.People = []Person{
				{Name: strings.Repeat("<", 80)}, {Name: strings.Repeat("<", 80)},
				{Name: strings.Repeat("<", 80)}, {Name: strings.Repeat("<", 80)},
				{Name: strings.Repeat("<", 80)}, {Name: strings.Repeat("<", 80)},
				{Name: strings.Repeat("<", 80)}, {Name: strings.Repeat("<", 80)},
			}
			return v
		}, check: func(t *testing.T, v, out *Timeline) {
			t.Helper()
			assertions, requirements := assert.New(t), require.New(t)
			requirements.NotEmpty(out.History)
			assertions.Equal("msg:1", out.History[0].Ref)
			assertions.Contains(Markdown(out), "13:30 message [details omitted] · #1")
			assertions.Equal(v.History[0].Open, out.History[0].Open)
			assertions.LessOrEqual(encodedSize(t, out, "json"), 4096)
		}},
		{name: "burst", build: func() *Timeline {
			v := budgetTimeline(0)
			entry := sampleTimeline(ModeAnchor).History[2]
			entry.Burst.Inbound, entry.Burst.Outbound = 2, 1
			entry.Burst.People = make([]Person, 8)
			for i := range entry.Burst.People {
				entry.Burst.People[i] = Person{Name: strings.Repeat("<", 80)}
			}
			v.History = []Entry{entry}
			return v
		}, check: func(t *testing.T, v, out *Timeline) {
			t.Helper()
			assertions, requirements := assert.New(t), require.New(t)
			requirements.Len(out.History, 1)
			requirements.NotNil(out.History[0].Burst)
			assertions.Equal(2, out.History[0].Burst.Inbound)
			assertions.Equal(1, out.History[0].Burst.Outbound)
			assertions.Contains(out.Omitted.Budget, "entry_fields")
			assertions.Contains(Markdown(out), "details omitted")
			assertions.Len(v.History[0].Burst.People, 8)
		}},
		{name: "upcoming", build: func() *Timeline {
			v := budgetTimeline(0)
			v.Upcoming = []Entry{{Kind: "meeting", Key: "evt:7", Ref: "evt:7", AnchorRef: "evt:7", Time: v.Now,
				Open:    []Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": "evt:7"}}},
				Meeting: &MeetingEntry{Title: UntrustedText{Text: "Synthetic planning"}, Attendees: []Person{{Name: strings.Repeat("A", 80)}, {Name: strings.Repeat("B", 80)}, {Name: strings.Repeat("C", 80)}, {Name: strings.Repeat("D", 80)}}},
			}}
			return v
		}, check: func(t *testing.T, v, out *Timeline) {
			t.Helper()
			assertions, requirements := assert.New(t), require.New(t)
			requirements.Len(out.Upcoming, 1)
			assertions.Empty(out.Upcoming[0].Meeting.Title.Text)
			assertions.Equal(v.Upcoming[0].Open, out.Upcoming[0].Open)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.build()
			out, err := Fit(v, "json", 4096)
			require.NoError(t, err)
			tc.check(t, v, out)
		})
	}
}
func TestFitReportsCountsOrMore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	v := budgetTimeline(100)
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.Equal(100-len(out.History), out.Omitted.Budget["history"])
	v.continuation.More = map[string]bool{"history": true}
	out, err = Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.Equal("more", out.Omitted.Budget["history"])
	v = budgetTimeline(0)
	v.OpenLoops = &OpenLoops{Truncated: true}
	out, err = Fit(v, "json", 4096)
	requirements.NoError(err)
	assertions.Equal("more", out.Omitted.Budget["open_loops"])
	assertions.Empty(out.Cursor)
}
func TestFitJSONEscapingAndMultibyte(t *testing.T) {
	for _, kind := range []string{"snippet", "title"} {
		t.Run(kind, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			v := budgetTimeline(80)
			text := strings.Repeat("猫😀<>&\"\\", 100)
			keys := map[string]string{}
			for i := range v.History {
				if kind == "title" {
					v.History[i].Message = nil
					v.History[i].Kind = "meeting"
					ref := fmt.Sprintf("evt:%d", i+1)
					v.History[i].Key = "occ:" + ref
					v.History[i].Ref, v.History[i].AnchorRef = ref, ref
					v.History[i].Open = []Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": ref}}}
					v.History[i].Meeting = &MeetingEntry{Title: UntrustedText{Text: text}, Start: v.Now, End: v.Now.Add(time.Hour)}
				} else {
					v.History[i].Message.Snippet.Text = text
				}
				keys[v.History[i].Key] = fmt.Sprintf("%03d", i+1)
			}
			SetContinuation(v, Continuation{Binding: cursorBinding(), SortKeys: keys})
			out, err := Fit(v, "json", 4096)
			requirements.NoError(err)
			b, err := json.Marshal(out, jsontext.EscapeForHTML(true))
			requirements.NoError(err)
			assertions.LessOrEqual(len(b), 4096)
			assertions.True(utf8.Valid(b))
			requirements.NotEmpty(out.History)
			var got UntrustedText
			if kind == "title" {
				got = out.History[0].Meeting.Title
			} else {
				got = out.History[0].Message.Snippet
			}
			assertions.True(got.Truncated)
			assertions.Equal(strings.Repeat("猫😀<>&\"\\", 11)+"猫😀<", got.Text)
		})
	}
}
func TestFitSectionSharesFlowForward(t *testing.T) {
	assertions := assert.New(t)
	v := budgetTimeline(50)
	v.OpenLoops = &OpenLoops{Items: []Entry{sampleTimeline(ModeDay).OpenLoops.Items[0]}}
	v.Upcoming = []Entry{sampleTimeline(ModeAnchor).History[0]}
	out, err := Fit(v, "json", 4096)
	require.NoError(t, err)
	assertions.Len(out.OpenLoops.Items, 1)
	assertions.Len(out.Upcoming, 1)
	assertions.NotEmpty(out.History)
}
func TestFitDayPrefixesAndCallerSortKeys(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	v := budgetTimeline(0)
	v.Mode = ModeDay
	day := Day{Date: "2026-10-22"}
	keys := map[string]string{}
	var entries []Entry
	sample := sampleTimeline(ModeDay).Days[0]
	for i := range 60 {
		var e Entry
		switch {
		case i < 5:
			e = sample.Meetings[0]
			e.Key = fmt.Sprintf("occ:evt:%d", i+1)
			e.Ref = fmt.Sprintf("evt:%d", i+1)
			e.AnchorRef = e.Ref
			e.Open = []Open{{Tool: "get_calendar_event", Args: map[string]any{"ref": e.Ref}}}
			day.Meetings = append(day.Meetings, e)
		case i < 30:
			e = sample.Bursts[0]
			e.Key = fmt.Sprintf("burst:1:%d:2026-10-22", i+1)
			e.Ref = fmt.Sprintf("conv:%d", i+1)
			e.AnchorRef = e.Key
			e.Open = []Open{{Tool: "list_messages", Args: map[string]any{"conversation_id": i + 1}}}
			burst := *e.Burst
			burst.LatestMessageIDs = []int64{int64(i + 1)}
			burst.Copies = nil
			e.Burst = &burst
			day.Bursts = append(day.Bursts, e)
		default:
			e = sample.Calls[0]
			e.Key = fmt.Sprintf("m:%d", i+1)
			e.Ref = fmt.Sprintf("msg:%d", i+1)
			e.AnchorRef = e.Ref
			e.Open = []Open{{Tool: "get_message", Args: map[string]any{"id": i + 1}}}
			day.Calls = append(day.Calls, e)
		}
		entries = append(entries, e)
		keys[e.Key] = fmt.Sprintf("%03d", i+1)
	}
	v.Days = []Day{day}
	SetContinuation(v, Continuation{Binding: cursorBinding(), SortKeys: keys})
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	requirements.NotEmpty(out.Days)
	assertions.LessOrEqual(encodedSize(t, out, "json"), 4096)
	var got []Entry
	for _, d := range out.Days {
		got = append(got, d.Meetings...)
		got = append(got, d.Bursts...)
		got = append(got, d.Calls...)
	}
	requirements.NotEmpty(got)
	for i, e := range got {
		assertions.Equal(entries[i].Key, e.Key)
		assertions.Equal(entries[i].Open, e.Open)
	}
	c, err := DecodeCursor(out.Cursor, cursorBinding())
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("%03d", len(got)), c.Positions["days"])
}

func TestFitBoundsPeoplePreservingCountsAndTierSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	v := budgetTimeline(1)
	for i := range 12 {
		v.History[0].Message.People = append(v.History[0].Message.People, Person{PersonID: int64(i + 1), Name: fmt.Sprintf("Person %d", i+1), Tier: 2, TierSource: "observed"})
	}
	out, err := Fit(v, "json", 8192)
	requirements.NoError(err)
	p := out.History[0].Message
	assertions.Len(p.People, 8)
	assertions.Equal(13, p.PeopleCount)
	assertions.True(p.PeopleTruncated)
	assertions.Equal("observed", p.People[1].TierSource)
	assertions.Contains(Markdown(out), "+5")
	v = budgetTimeline(0)
	e := sampleTimeline(ModeDay).Upcoming[0]
	for i := range 12 {
		e.Meeting.Attendees = append(e.Meeting.Attendees, Person{Name: fmt.Sprintf("Person %d", i+1)})
	}
	v.Upcoming = []Entry{e}
	out, err = Fit(v, "json", 8192)
	requirements.NoError(err)
	assertions.Len(out.Upcoming[0].Meeting.Attendees, 8)
	assertions.Equal(13, out.Upcoming[0].Meeting.AttendeeCount)
	assertions.True(out.Upcoming[0].Meeting.AttendeesTruncated)
	assertions.Contains(Markdown(out), "+5")
}
func TestFitCursorBindsChosenFormat(t *testing.T) {
	v := budgetTimeline(100)
	out, err := Fit(v, "markdown", 4096)
	require.NoError(t, err)
	_, err = DecodeCursor(out.Cursor, cursorBinding())
	require.ErrorIs(t, err, ErrCursorExpired)
	b := cursorBinding()
	b.Request.Format = "markdown"
	_, err = DecodeCursor(out.Cursor, b)
	require.NoError(t, err)
}
func TestFitCompletePageDoesNotRequireContinuation(t *testing.T) {
	v := budgetTimeline(1)
	v.continuation = nil
	out, err := Fit(v, "json", 4096)
	require.NoError(t, err)
	assert.Len(t, out.History, 1)
	assert.Empty(t, out.Cursor)
}

func TestFitSameEntryUsesSectionSortKeys(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	v := budgetTimeline(0)
	v.Mode = ModeDay
	entry := sampleTimeline(ModeDay).Upcoming[0]
	v.Upcoming = []Entry{entry}
	v.Days = []Day{{Date: "2026-10-22", Meetings: []Entry{entry}}}
	binding := cursorBinding()
	SetContinuation(v, Continuation{Binding: binding, More: map[string]bool{"days": true}, SortKeys: map[string]string{"days|" + entry.Key: "day-order", "upcoming|" + entry.Key: "upcoming-order"}})
	out, err := Fit(v, "json", 4096)
	requirements.NoError(err)
	requirements.NotEmpty(out.Cursor)
	cursor, err := DecodeCursor(out.Cursor, binding)
	requirements.NoError(err)
	assertions.Equal("day-order", cursor.Positions["days"])
	assertions.Equal("upcoming-order", cursor.Positions["upcoming"])
}

func TestFitMarksClippedInvalidUTF8(t *testing.T) {
	v := budgetTimeline(1)
	v.History[0].Message.Snippet.Text = strings.Repeat("\xff", 81)
	v.History[0].Message.People[0].Name = strings.Repeat("\xff", 81)
	out, err := Fit(v, "json", 8192)
	require := require.New(t)
	require.NoError(err)
	require.Len(out.History, 1)
	assert := assert.New(t)
	assert.True(out.History[0].Message.Snippet.Truncated)
	assert.Equal(strings.Repeat("\ufffd", 80), out.History[0].Message.Snippet.Text)
	assert.True(out.History[0].Message.People[0].Truncated)
	assert.Equal(strings.Repeat("\ufffd", 80), out.History[0].Message.People[0].Name)
	markdown := Markdown(out)
	assert.True(utf8.ValidString(markdown))
	assert.Contains(markdown, strings.Repeat("\ufffd", 80)+"…")
}

func TestFitBoundsGapItemsAtMinimumBudget(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			v := budgetTimeline(0)
			items := make([]string, 5000)
			for i := range items {
				items[i] = fmt.Sprintf("evt:%d", i+1)
			}
			v.Gaps = []Gap{{Section: "meetings", Code: "partial", Items: items}}
			got, err := Fit(v, format, 4096)
			requirements.NoError(err)
			requirements.Len(got.Gaps, 1)
			assertions.Equal("partial", got.Gaps[0].Code)
			assertions.Equal(items[:10], got.Gaps[0].Items)
			assertions.Equal(4990, got.Omitted.Budget["gap_items"])
			assertions.LessOrEqual(encodedSize(t, got, format), 4096)
		})
	}
}
