package timeline

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func budgetTimeline(n int) *Timeline {
	v := sampleTimeline()
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
	requirepkg.NoError(t, err)
	return len(b)
}
func TestFitMeasuresFinalEncoding(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		t.Run(format, func(t *testing.T) {
			assert := assertpkg.New(t)
			v := budgetTimeline(100)
			out, err := Fit(v, format, 4096)
			requirepkg.NoError(t, err)
			assert.LessOrEqual(encodedSize(t, out, format), 4096)
			assert.NotEmpty(out.Cursor)
			assert.NotEmpty(out.History)
			assert.Less(len(out.History), 100)
			assert.Len(v.History, 100)
			assert.Empty(v.Cursor)
		})
	}
}
func TestFitMinimumEnvelopeSucceedsAt2048(t *testing.T) {
	for _, format := range []string{"json", "markdown"} {
		out, err := Fit(budgetTimeline(0), format, 2048)
		requirepkg.NoError(t, err)
		assertpkg.Empty(t, out.Cursor)
		assertpkg.LessOrEqual(t, encodedSize(t, out, format), 2048)
	}
}
func TestFitRejectsDayWithSummaryAndEntries(t *testing.T) {
	for _, part := range []string{"meetings", "bursts", "calls", "notes"} {
		for _, format := range []string{"json", "markdown"} {
			t.Run(part+"/"+format, func(t *testing.T) {
				v := budgetTimeline(1)
				day := Day{Date: "2026-10-22", Summary: &DaySummary{}}
				switch part {
				case "meetings":
					day.Meetings = v.History
				case "bursts":
					day.Bursts = v.History
				case "calls":
					day.Calls = v.History
				case "notes":
					day.Notes = v.History
				}
				v.History = nil
				v.Days = []Day{day}
				out, err := Fit(v, format, 4096)
				requirepkg.ErrorIs(t, err, ErrInvalidRequest)
				assertpkg.Nil(t, out)
			})
		}
	}
}
func TestFitSmallBudgetContinuationSucceeds(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(50)
	out, err := Fit(v, "json", 2048)
	require.NoError(err)
	assert.LessOrEqual(encodedSize(t, out, "json"), 2048)
	c, err := DecodeCursor(out.Cursor, cursorBinding())
	require.NoError(err)
	assert.Equal(fmt.Sprintf("%03d", len(out.History)), c.Positions["history"])
}
func TestFitFallbackCursorKeysPreserveSubsecondOrder(t *testing.T) {
	for _, section := range []string{"history", "upcoming"} {
		t.Run(section, func(t *testing.T) {
			require := requirepkg.New(t)
			var keys []string
			for _, offset := range []time.Duration{0, 500 * time.Millisecond, 550 * time.Millisecond, time.Second} {
				v := budgetTimeline(1)
				v.History[0].Time = v.Now.Add(offset)
				if section == "upcoming" {
					v.Upcoming = v.History
					v.History = nil
				}
				SetContinuation(v, Continuation{Binding: cursorBinding(), More: map[string]bool{section: true}})
				out, err := Fit(v, "json", 4096)
				require.NoError(err)
				cursor, err := DecodeCursor(out.Cursor, cursorBinding())
				require.NoError(err)
				require.NotEmpty(cursor.Positions[section])
				keys = append(keys, cursor.Positions[section])
			}
			for i := 1; i < len(keys); i++ {
				assertpkg.Less(t, keys[i-1], keys[i])
			}
		})
	}
}
func TestFitTooSmall(t *testing.T) {
	v := budgetTimeline(0)
	v.Freshness.Warnings = []string{strings.Repeat("warning ", 2000)}
	_, err := Fit(v, "json", 2048)
	requirepkg.ErrorIs(t, err, ErrBudgetTooSmall)
	for _, budget := range []int{-1, 0, 2047, 65537} {
		_, err := Fit(budgetTimeline(0), "json", budget)
		requirepkg.ErrorIs(t, err, ErrInvalidRequest)
	}
	_, err = Fit(budgetTimeline(0), "html", 4096)
	requirepkg.ErrorIs(t, err, ErrInvalidRequest)
}
func TestFitForwardProgressStub(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(2)
	v.History[0].Open[0].Args["enormous"] = strings.Repeat("x", 20000)
	out, err := Fit(v, "json", 2048)
	require.NoError(err)
	require.NotEmpty(out.History)
	assert.Equal("msg:1", out.History[0].Ref)
	require.NoError(out.History[0].Validate())
	assert.Empty(out.History[0].Open)
	assert.LessOrEqual(encodedSize(t, out, "json"), 2048)
}
func TestFitReportsCountsOrMore(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(100)
	out, err := Fit(v, "json", 2048)
	require.NoError(err)
	assert.Equal(100-len(out.History), out.Omitted.Budget["history"])
	v.continuation.More["history"] = true
	out, err = Fit(v, "json", 2048)
	require.NoError(err)
	assert.Equal("more", out.Omitted.Budget["history"])
	v = budgetTimeline(0)
	v.OpenLoops = &OpenLoops{Truncated: true}
	out, err = Fit(v, "json", 2048)
	require.NoError(err)
	assert.Equal("more", out.Omitted.Budget["open_loops"])
	assert.Empty(out.Cursor)
}
func TestFitJSONEscapingAndMultibyte(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(80)
	for i := range v.History {
		v.History[i].Message.Snippet.Text = strings.Repeat("猫😀<>&\"\\", 100)
	}
	out, err := Fit(v, "json", 4096)
	require.NoError(err)
	b, err := json.Marshal(out, jsontext.EscapeForHTML(true))
	require.NoError(err)
	assert.LessOrEqual(len(b), 4096)
	assert.True(utf8.Valid(b))
	assert.True(out.History[0].Message.Snippet.Truncated)
	assert.False(v.History[0].Message.Snippet.Truncated)
}
func TestFitSectionSharesFlowForward(t *testing.T) {
	assert := assertpkg.New(t)
	v := budgetTimeline(50)
	v.OpenLoops = &OpenLoops{Items: []Entry{sampleTimeline().History[5]}}
	v.Upcoming = []Entry{sampleTimeline().History[0]}
	out, err := Fit(v, "json", 4096)
	requirepkg.NoError(t, err)
	assert.Len(out.OpenLoops.Items, 1)
	assert.Len(out.Upcoming, 1)
	assert.NotEmpty(out.History)
}
func TestFitHiddenTextStripsJSONText(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(1)
	v.History[0].HiddenText = true
	v.History[0].Message.Snippet.Text = "secret text"
	out, err := Fit(v, "json", 4096)
	require.NoError(err)
	b, err := json.Marshal(out)
	require.NoError(err)
	assert.NotContains(string(b), "secret text")
	assert.Equal("secret text", v.History[0].Message.Snippet.Text)
}
func TestFitExistingMoreStillContinues(t *testing.T) {
	v := budgetTimeline(80)
	v.Omitted.Budget = map[string]any{"history": "more"}
	out, err := Fit(v, "json", 2048)
	requirepkg.NoError(t, err)
	assertpkg.NotEmpty(t, out.Cursor)
}
func TestFitStubRendersKind(t *testing.T) {
	v := budgetTimeline(2)
	v.History[0].Open[0].Args["enormous"] = strings.Repeat("x", 20000)
	out, err := Fit(v, "json", 2048)
	requirepkg.NoError(t, err)
	assertpkg.Contains(t, Markdown(out), "13:30 message · #1")
}
func TestFitTruncatedTextStaysMarkedInMarkdown(t *testing.T) {
	v := budgetTimeline(1)
	v.History[0].Message.Snippet.Text = strings.Repeat("猫", 100)
	out, err := Fit(v, "markdown", 4096)
	requirepkg.NoError(t, err)
	assertpkg.Contains(t, Markdown(out), strings.Repeat("猫", 80)+"…")
}
func TestFitDayPrefixesAndCallerSortKeys(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(60)
	entries := v.History
	v.History = nil
	v.Days = []Day{{Date: "2026-10-22", Meetings: entries[:5], Bursts: entries[5:30], Calls: entries[30:45], Notes: entries[45:]}}
	out, err := Fit(v, "json", 2048)
	require.NoError(err)
	assert.NotEmpty(out.Days)
	assert.LessOrEqual(encodedSize(t, out, "json"), 2048)
	var got []Entry
	for _, d := range out.Days {
		got = append(got, d.Meetings...)
		got = append(got, d.Bursts...)
		got = append(got, d.Calls...)
		got = append(got, d.Notes...)
	}
	require.NotEmpty(got)
	for i, e := range got {
		assert.Equal(fmt.Sprintf("msg:%d", i+1), e.Key)
		assert.Equal(i+1, e.Open[0].Args["id"])
	}
	c, err := DecodeCursor(out.Cursor, cursorBinding())
	require.NoError(err)
	assert.Equal(fmt.Sprintf("%03d", len(got)), c.Positions["days"])
}
func TestFitBoundsPeoplePreservingCountsAndTierSource(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(1)
	for i := range 12 {
		v.History[0].Message.People = append(v.History[0].Message.People, Person{PersonID: int64(i + 1), Name: fmt.Sprintf("Person %d", i+1), Tier: 2, TierSource: "observed"})
	}
	out, err := Fit(v, "json", 8192)
	require.NoError(err)
	p := out.History[0].Message
	assert.Len(p.People, 8)
	assert.Equal(13, p.PeopleCount)
	assert.True(p.PeopleTruncated)
	assert.Equal("observed", p.People[1].TierSource)
	assert.Contains(Markdown(out), "+5")
	v = budgetTimeline(0)
	e := sampleTimeline().Upcoming[0]
	for i := range 12 {
		e.Meeting.Attendees = append(e.Meeting.Attendees, Person{Name: fmt.Sprintf("Person %d", i+1)})
	}
	v.Upcoming = []Entry{e}
	out, err = Fit(v, "json", 8192)
	require.NoError(err)
	assert.Len(out.Upcoming[0].Meeting.Attendees, 8)
	assert.Equal(13, out.Upcoming[0].Meeting.AttendeeCount)
	assert.True(out.Upcoming[0].Meeting.AttendeesTruncated)
	assert.Contains(Markdown(out), "+5")
}
func TestFitCursorBindsChosenFormat(t *testing.T) {
	v := budgetTimeline(100)
	out, err := Fit(v, "markdown", 2048)
	requirepkg.NoError(t, err)
	_, err = DecodeCursor(out.Cursor, cursorBinding())
	requirepkg.ErrorIs(t, err, ErrCursorExpired)
	b := cursorBinding()
	b.Request.Format = "markdown"
	_, err = DecodeCursor(out.Cursor, b)
	requirepkg.NoError(t, err)
}
func TestFitCompletePageDoesNotRequireContinuation(t *testing.T) {
	v := budgetTimeline(1)
	v.continuation = nil
	out, err := Fit(v, "json", 4096)
	requirepkg.NoError(t, err)
	assertpkg.Len(t, out.History, 1)
	assertpkg.Empty(t, out.Cursor)
}

func TestFitSameEntryUsesSectionSortKeys(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	v := budgetTimeline(1)
	entry := v.History[0]
	v.History = nil
	v.Upcoming = []Entry{entry}
	v.Days = []Day{{Date: "2026-10-22", Meetings: []Entry{entry}}}
	binding := cursorBinding()
	SetContinuation(v, Continuation{Binding: binding, More: map[string]bool{"days": true}, SortKeys: map[string]string{"days|" + entry.Key: "day-order", "upcoming|" + entry.Key: "upcoming-order"}})
	out, err := Fit(v, "json", 4096)
	require.NoError(err)
	require.NotEmpty(out.Cursor)
	cursor, err := DecodeCursor(out.Cursor, binding)
	require.NoError(err)
	assert.Equal("day-order", cursor.Positions["days"])
	assert.Equal("upcoming-order", cursor.Positions["upcoming"])
}
