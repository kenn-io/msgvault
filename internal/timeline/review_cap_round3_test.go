package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/store"
)

func TestRelationshipHistoryKeepsLatestBeyondGroupCap(t *testing.T) {
	require := require.New(t)
	f := newTimelineFixture(t)
	old := f.now.Add(-10 * time.Hour)
	f.add(old, false)
	latest := f.add(f.now.Add(-time.Minute), false)
	conv, err := f.Store.EnsureConversation(f.Source.ID, "recent", "Recent conversation")
	require.NoError(err)
	f.add(old.Add(time.Hour), false, func(m *store.Message) { m.ConversationID = conv })
	f.project()
	// A one-group cap exercises the reader's ordering with just two groups.
	got, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{
		Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}},
		After:   new(time.Time), Until: &f.now, Descending: true, Limit: 1,
	})
	require.NoError(err)
	require.Len(got.Bursts, 1)
	require.NotNil(got.NextKey)
	assert.Contains(t, got.Bursts[0].LatestMessageIDs, latest)
}

func TestRelationshipRejectsOwnerAddress(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.project()
	_, err := Build(t.Context(), f.src, relationshipRequest("email:owner@example.com"))
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestMarkdownPartialSyncOutcome(t *testing.T) {
	v := sampleTimeline(ModeAnchor)
	v.Freshness.Sources = []freshness.SourceFreshness{{SourceType: "gmail", Identifier: "owner@example.com", Scheduling: "manual", Outcome: "partial", LastSyncErrors: 1, LastSuccessfulSyncAt: &v.Now}}
	got := Markdown(v)
	assert.Contains(t, got, "gmail:owner@example.com partial")
	assert.NotContains(t, got, "gmail:owner@example.com current")
}

// MCP has no daily-note reader, so a truncated note must not name an
// uncallable tool; its date anchor re-centers the timeline instead.
func TestFitTruncatedNoteNamesNoUncallableTool(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: strings.Repeat("<", 2000), Author: "owner"})
	require.NoError(err)
	req := dayRequest()
	req.Budget = 2048
	got, err := Build(t.Context(), f.src, req)
	require.NoError(err)
	require.Len(got.Days, 1)
	require.Len(got.Days[0].Notes, 1)
	note := got.Days[0].Notes[0]
	assert.True(note.Truncated)
	assert.Equal([]Open{}, note.Open)
	assert.Equal("2026-10-22", note.AnchorRef)
}
