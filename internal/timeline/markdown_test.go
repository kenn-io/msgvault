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
			v := budgetTimeline(0)
			if kind == "title" {
				v.Upcoming = []Entry{sampleTimeline(ModeDay).Upcoming[0]}
				v.Upcoming[0].Meeting.Title.Text = strings.Repeat("猫😀", 250)
			} else {
				v.History = budgetTimeline(1).History
				v.History[0].Message.Snippet.Text = strings.Repeat("猫😀", 250)
			}
			got := Markdown(v)
			assert.True(t, utf8.ValidString(got))
			assert.Contains(t, got, strings.Repeat("猫😀", 40)+"…")
			assert.NotContains(t, got, strings.Repeat("猫😀", 41))
		})
	}
}
func TestMarkdownHiddenTextShowsNoSnippet(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.History = []Entry{v.History[3]}
	v.History[0].Message.Snippet.Text = "secret hidden text"
	got := Markdown(v)
	assert.NotContains(t, got, "secret hidden text")
	assert.Contains(t, got, "[hidden text]")
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
	assert.Contains(t, Markdown(v), "13 people (+13)")
}
func TestMarkdownMultipleCopyGroupsCountExtras(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Days = nil
	v.History = []Entry{v.History[2]}
	v.History[0].Burst.Copies = map[int64]int{1: 2, 2: 2, 3: 2}
	assert.Contains(t, Markdown(v), "(3 extra copies)")
}
