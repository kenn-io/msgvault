package timeline

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/store"
)

func TestRelationshipHistoryKeepsLatestBeyondGroupCap(t *testing.T) {
	t.Skip("Sol P1: select capped relationship history by latest activity")
	require := require.New(t)
	f := newTimelineFixture(t)
	old := f.now.Add(-10 * time.Hour)
	f.add(old, false)
	latest := f.add(f.now.Add(-time.Minute), false)
	for i := range 5000 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("recent-%d", i), "Recent conversation")
		require.NoError(err)
		f.add(old.Add(time.Duration(i+1)*time.Second), false, func(m *store.Message) { m.ConversationID = conv })
	}
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	require.NoError(err)
	require.NotEmpty(got.History)
	require.NotNil(got.History[0].Burst)
	assert.Contains(t, got.History[0].Burst.LatestMessageIDs, latest)
}

func TestRelationshipRejectsOwnerAddress(t *testing.T) {
	t.Skip("Sonnet P3: reject the owner's own address in resolvePerson")
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.project()
	_, err := Build(t.Context(), f.src, relationshipRequest("email:owner@example.com"))
	require.ErrorIs(t, err, ErrInvalidRequest)
}

func TestMarkdownPartialSyncOutcome(t *testing.T) {
	v := sampleTimeline()
	v.Freshness.Sources = []freshness.SourceFreshness{{SourceType: "gmail", Identifier: "owner@example.com", Scheduling: "manual", Outcome: "partial", LastSyncErrors: 1, LastSuccessfulSyncAt: &v.Now}}
	got := Markdown(v)
	assert.Contains(t, got, "gmail:owner@example.com partial")
	assert.NotContains(t, got, "gmail:owner@example.com current")
}

func TestFitTruncatedNoteKeepsToolTarget(t *testing.T) {
	t.Skip("Sol P2: advertise a callable get_daily_note opener with its date")
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
	assert.Equal([]Open{{Tool: "get_daily_note", Args: map[string]any{"date": "2026-10-22"}}}, note.Open)
}
