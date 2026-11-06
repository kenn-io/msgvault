package timeline

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestMarkdownDayGolden(t *testing.T) {
	v := sampleTimeline(ModeDay)
	v.Omitted.Budget["entry_fields"] = 1
	golden(t, "day.md", []byte(Markdown(v)))
}
func TestMarkdownSingleDayHeading(t *testing.T) {
	v := sampleTimeline(ModeDay)
	v.TimezoneIsDefault = false
	v.Window.From = v.Window.To
	v.Days = []Day{{Date: v.Window.To, Meetings: v.Days[0].Meetings}}
	got := Markdown(v)
	assertions := assert.New(t)
	assertions.Contains(got, "# Thu 2026-10-22 · UTC\n")
	assertions.Equal(1, strings.Count(got, "2026-10-22"))
	assertions.NotContains(got, "Thu Oct 22")
	assertions.Contains(got, "1 unparseable\n\n- 13:30 Weekly sync")
}
func TestMarkdownRelationshipGolden(t *testing.T) {
	v := sampleTimeline(ModeRelationship)
	golden(t, "relationship.md", []byte(Markdown(v)))
}
func TestMarkdownAnchorGolden(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	golden(t, "anchor.md", []byte(Markdown(v)))
}
func TestMarkdownTruncatesByRunesAndStaysValidUTF8(t *testing.T) {
	for _, kind := range []string{"title", "snippet"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			v := budgetTimeline(0)
			if kind == "title" {
				v.Upcoming = []Entry{sampleTimeline(ModeDay).Upcoming[0]}
				v.Upcoming[0].Meeting.Title.Text = strings.Repeat("猫😀", 250)
			} else {
				v.History = budgetTimeline(1).History
				v.History[0].Message.Snippet.Text = strings.Repeat("猫😀", 250)
			}
			got := Markdown(v)
			assert.True(utf8.ValidString(got))
			assert.Contains(got, strings.Repeat("猫😀", 40)+"…")
			assert.NotContains(got, strings.Repeat("猫😀", 41))
		})
	}
}
func TestMarkdownGapFooterGroupsRetryable(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Gaps = []Gap{{Section: "calls", Code: "owner-only"}, {Section: "loops", Code: "timeout", Retryable: true}, {Section: "loops", Code: "partial", Retryable: true}}
	assert.Contains(t, Markdown(v), "not shown: calls · retry: loops\n")
	v.Gaps = nil
	assert.NotContains(t, Markdown(v), "retry:")
}
func TestMarkdownSummaryStubReportsAllOmittedNames(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Days = []Day{{Date: "2026-10-22", Summary: &DaySummary{PeopleCount: 13, Truncated: true}}}
	assert.Contains(t, Markdown(v), "Thu Oct 22 · 13 people\n")
}

func TestMarkdownSummaryCountsMessagesAndCalls(t *testing.T) {
	v := sampleTimeline(ModeDay)
	v.Days = []Day{{Date: "2026-10-20", Summary: &DaySummary{Messages: 3, PeopleCount: 1}}}
	assert.Contains(t, Markdown(v), "Tue Oct 20 · 3 messages · 1 people")
	v.Days[0].Summary.Calls = 1
	assert.Contains(t, Markdown(v), "Tue Oct 20 · 3 messages · 1 calls · 1 people")
}
func TestMarkdownMultipleCopyGroupsCountExtras(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Days = nil
	v.History = []Entry{v.History[2]}
	v.History[0].Burst.Copies = map[int64]int{1: 2, 2: 2, 3: 2}
	assert.Contains(t, Markdown(v), "(3 extra copies)")
}

func TestMarkdownUsesAnchorAndShortRelatedRefs(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Days = nil
	v.Upcoming = nil
	v.History = []Entry{v.History[2], v.History[0]}
	v.History[0].Ref = "conv:412"
	v.History[0].AnchorRef = "msg:9031"
	v.History[1].Meeting.PossiblyRelated = []string{"msg:9032", "evt:8812@20261022T133000Z"}
	got := Markdown(v)
	assert.Contains(t, got, "#9031")
	assert.NotContains(t, got, "c412")
	assert.Contains(t, got, "possibly related #9032, e8812@20261022T1330Z")
}

func TestMarkdownPartialSyncOutcome(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Freshness.Sources = []SourceFreshness{{SourceType: "gmail", Identifier: "owner@example.com", Scheduling: "manual", Outcome: "partial", LastSyncErrors: 1, LastSuccessfulSyncAt: &v.Now}}
	got := Markdown(v)
	assert.Contains(t, got, "gmail:owner@example.com partial")
	assert.NotContains(t, got, "gmail:owner@example.com current")
}
