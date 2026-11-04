package timeline

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestMarkdownDayGolden(t *testing.T) { golden(t, "day.md", []byte(Markdown(sampleTimeline()))) }
func TestMarkdownRelationshipGolden(t *testing.T) {
	v := sampleTimeline()
	v.Mode = ModeRelationship
	v.Days = nil
	golden(t, "relationship.md", []byte(Markdown(v)))
}
func TestMarkdownAnchorGolden(t *testing.T) {
	v := sampleTimeline()
	v.Mode = ModeAnchor
	v.Person = nil
	v.Days = nil
	golden(t, "anchor.md", []byte(Markdown(v)))
}
func TestMarkdownAnchorHeadingDoesNotRepeatMatchingEntryRef(t *testing.T) {
	now := time.Date(2026, 10, 22, 13, 30, 0, 0, time.UTC)
	v := &Timeline{
		Mode:     ModeAnchor,
		Timezone: "UTC",
		Anchor:   &Anchor{Ref: "msg:31"},
		History: []Entry{
			{Kind: "message", Ref: "msg:31", Time: now, Anchor: true, Message: &MessageEntry{Direction: "inbound", Snippet: UntrustedText{Text: "anchor"}}},
			{Kind: "message", Ref: "msg:32", Time: now, Message: &MessageEntry{Direction: "inbound", Snippet: UntrustedText{Text: "other"}}},
		},
	}

	got := Markdown(v)
	assert.Equal(t, 1, strings.Count(got, "#31"))
	assert.Contains(t, got, "· #32")
}
func TestMarkdownTruncatesByRunesAndStaysValidUTF8(t *testing.T) {
	v := sampleTimeline()
	v.Person = nil
	v.History = nil
	v.Days = nil
	v.OpenLoops = nil
	v.Upcoming[0].Meeting.Title.Text = strings.Repeat("猫😀", 250)
	got := Markdown(v)
	assert.True(t, utf8.ValidString(got))
	assert.Contains(t, got, strings.Repeat("猫😀", 40)+"…")
	assert.NotContains(t, got, strings.Repeat("猫😀", 41))
}
func TestMarkdownHiddenTextShowsNoSnippet(t *testing.T) {
	v := sampleTimeline()
	v.History = []Entry{v.History[3]}
	v.History[0].Message.Snippet.Text = "secret hidden text"
	got := Markdown(v)
	assert.NotContains(t, got, "secret hidden text")
	assert.Contains(t, got, "[hidden text]")
}
func TestMarkdownGapFooterGroupsRetryable(t *testing.T) {
	v := sampleTimeline()
	v.Gaps = []Gap{{Section: "calls", Code: "owner-only"}, {Section: "loops", Code: "timeout", Retryable: true}, {Section: "loops", Code: "partial", Retryable: true}}
	assert.Contains(t, Markdown(v), "not shown: calls · retry: loops\n")
	v.Gaps = nil
	assert.NotContains(t, Markdown(v), "retry:")
}

func TestMarkdownMarksSourceDeletedMeeting(t *testing.T) {
	v := sampleTimeline()
	v.Days = []Day{{Date: "2026-10-22", Meetings: []Entry{{
		Kind: "meeting", Key: "occ:evt:42", Ref: "evt:42", AnchorRef: "evt:42",
		Time:    v.Now,
		Meeting: &MeetingEntry{Title: UntrustedText{Text: "Planning"}, DeletedFromSource: true},
	}}}}
	assert.Contains(t, Markdown(v), "[deleted from source]")
	v.Days[0].Meetings[0].HiddenText = true
	assert.Contains(t, Markdown(v), "[deleted from source]")
}

func TestMarkdownDoesNotRepeatOpenLoopTruncationInOmittedFooter(t *testing.T) {
	assertions := assert.New(t)
	v := sampleTimeline()
	got := Markdown(v)
	assertions.Equal(1, strings.Count(got, "more available\n"))
	assertions.Contains(got, "omitted: history 2 · noise 2 · cold 3\n")
	assertions.NotContains(got, "open_loops")

	v.Days = nil
	v.History = nil
	v.Upcoming = nil
	v.Gaps = nil
	v.Omitted = Omitted{Budget: map[string]any{"open_loops": "more"}}
	got = Markdown(v)
	assertions.NotContains(got, "omitted:")
}
func TestMarkdownSummaryStubReportsAllOmittedNames(t *testing.T) {
	v := sampleTimeline()
	v.Days = []Day{{Date: "2026-10-22", Summary: &DaySummary{PeopleCount: 13, Truncated: true}}}
	assert.Contains(t, Markdown(v), "13 people (+13)")
}
func TestMarkdownMultipleCopyGroupsCountExtras(t *testing.T) {
	v := sampleTimeline()
	v.Days = nil
	v.History = []Entry{v.History[2]}
	v.History[0].Burst.Copies = map[int64]int{1: 2, 2: 2, 3: 2}
	assert.Contains(t, Markdown(v), "(3 extra copies)")
}

func TestMarkdownDoesNotRepeatLatestSyncWarning(t *testing.T) {
	assertions := assert.New(t)
	v := &Timeline{
		Timezone: "UTC",
		Freshness: Freshness{
			Sources:  []SourceFreshness{{Source: "gmail:owner@example.test", Status: "stale", LastRun: "failed", SyncCommand: "msgvault sync"}},
			Warnings: []string{"The latest sync failed for gmail source owner@example.test.", "The activity projection is reconciling archive changes."},
		},
	}
	got := Markdown(v)
	assertions.Contains(got, "gmail:owner@example.test stale failed")
	assertions.NotContains(got, "The latest sync failed for gmail source owner@example.test.")
	assertions.Contains(got, "The activity projection is reconciling archive changes.")
	assertions.Equal([]string{"The latest sync failed for gmail source owner@example.test.", "The activity projection is reconciling archive changes."}, v.Freshness.Warnings)
}
