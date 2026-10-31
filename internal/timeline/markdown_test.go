package timeline

import (
	"strings"
	"testing"
	"unicode/utf8"

	assertpkg "github.com/stretchr/testify/assert"
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
func TestMarkdownTruncatesByRunesAndStaysValidUTF8(t *testing.T) {
	v := sampleTimeline()
	v.Person = nil
	v.History = nil
	v.Days = nil
	v.OpenLoops = nil
	v.Upcoming[0].Meeting.Title.Text = strings.Repeat("猫😀", 250)
	got := Markdown(v)
	assertpkg.True(t, utf8.ValidString(got))
	assertpkg.Contains(t, got, strings.Repeat("猫😀", 40)+"…")
	assertpkg.NotContains(t, got, strings.Repeat("猫😀", 41))
}
func TestMarkdownHiddenTextShowsNoSnippet(t *testing.T) {
	v := sampleTimeline()
	v.History = []Entry{v.History[3]}
	v.History[0].Message.Snippet.Text = "secret hidden text"
	got := Markdown(v)
	assertpkg.NotContains(t, got, "secret hidden text")
	assertpkg.Contains(t, got, "[hidden text]")
}
func TestMarkdownGapFooterGroupsRetryable(t *testing.T) {
	v := sampleTimeline()
	v.Gaps = []Gap{{Section: "calls", Code: "owner-only"}, {Section: "loops", Code: "timeout", Retryable: true}, {Section: "loops", Code: "partial", Retryable: true}}
	assertpkg.Contains(t, Markdown(v), "not shown: calls · retry: loops\n")
	v.Gaps = nil
	assertpkg.NotContains(t, Markdown(v), "retry:")
}
func TestMarkdownUpcomingHasDayAndEmailVolume(t *testing.T) {
	v := sampleTimeline()
	got := Markdown(v)
	assertpkg.Contains(t, got, "- Thu 13:30 Weekly sync")
	assertpkg.Contains(t, got, "×3")
}
func TestMarkdownSummaryStubReportsAllOmittedNames(t *testing.T) {
	v := sampleTimeline()
	v.Days = []Day{{Date: "2026-10-22", Summary: &DaySummary{PeopleCount: 13, Truncated: true}}}
	assertpkg.Contains(t, Markdown(v), "13 people (+13)")
}
func TestMarkdownMultipleCopyGroupsCountExtras(t *testing.T) {
	v := sampleTimeline()
	v.Days = nil
	v.History = []Entry{v.History[2]}
	v.History[0].Burst.Copies = map[int64]int{1: 2, 2: 2, 3: 2}
	assertpkg.Contains(t, Markdown(v), "(3 extra copies)")
}
