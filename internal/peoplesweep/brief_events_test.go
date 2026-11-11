package peoplesweep

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBriefEventStateUsesTheWholeUTCDay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	day := "2026-08-15"
	event := BriefLifeEvent{Date: &day}
	for _, test := range []struct {
		at   string
		want BriefEventState
	}{
		{"2026-08-14T23:59:59Z", BriefEventUpcoming},
		{"2026-08-15T23:59:59Z", BriefEventUpcoming},
		{"2026-08-16T00:00:00Z", BriefEventFollowUp},
		{"2026-08-16T01:00:00+02:00", BriefEventUpcoming},
	} {
		t.Run(test.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, test.at)
			require.NoError(err)
			state, err := briefLifeEventState(event, at)
			require.NoError(err)
			assert.Equal(test.want, state)
		})
	}
	event.Resolution = &BriefEventResolution{EvidenceID: "later", Quote: "I cancelled it"}
	state, err := briefLifeEventState(event, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.Equal(BriefEventResolved, state, "cancellation can resolve an upcoming event")
	event.Date = nil
	event.Resolution = nil
	state, err = briefLifeEventState(event, time.Now())
	require.NoError(err)
	assert.Equal(BriefEventFollowUp, state, "an undated event gets a neutral check-in")
	invalid := "2026-02-30"
	event.Date = &invalid
	_, err = briefLifeEventState(event, time.Now())
	require.Error(err)
}

func TestBriefEventRequiresExactAuthoredQuotesAndLaterResolution(t *testing.T) {
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	later := briefWindowItem(11, SourceConversationText, 12, "I cancelled the move.")
	anchorID, laterID := packetEvidenceID(anchor), packetEvidenceID(later)
	day := "2026-08-15"
	highlight := BriefHighlight{
		Text: "a move", Speaker: BriefSpeakerPerson, EvidenceIDs: []string{anchorID, laterID},
		LifeEvent: &BriefLifeEvent{
			Date: &day, AnchorID: anchorID, Quote: anchor.Excerpt, Question: "How did the move go?",
			Resolution: &BriefEventResolution{EvidenceID: laterID, Quote: later.Excerpt},
			State:      BriefEventUpcoming,
		},
	}
	evidence := map[string]EvidenceItem{anchorID: anchor, laterID: later}
	asOf := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	got, ok := normalizeBriefEvent(highlight, evidence, briefTestPersonID, asOf)
	require.True(t, ok)
	assert.Equal(t, BriefEventResolved, got.LifeEvent.State, "provider state is ignored")
	for name, mutate := range map[string]func(*BriefHighlight, map[string]EvidenceItem){
		"invented quote":      func(h *BriefHighlight, _ map[string]EvidenceItem) { h.LifeEvent.Quote = "invented" },
		"blank quote":         func(h *BriefHighlight, _ map[string]EvidenceItem) { h.LifeEvent.Quote = " " },
		"unknown anchor":      func(h *BriefHighlight, _ map[string]EvidenceItem) { h.LifeEvent.AnchorID = "missing" },
		"uncited resolution":  func(h *BriefHighlight, _ map[string]EvidenceItem) { h.EvidenceIDs = []string{anchorID} },
		"invented resolution": func(h *BriefHighlight, _ map[string]EvidenceItem) { h.LifeEvent.Resolution.Quote = "invented" },
		"older resolution": func(_ *BriefHighlight, items map[string]EvidenceItem) {
			item := items[laterID]
			item.EventTime = anchor.EventTime.Add(-time.Hour)
			items[laterID] = item
		},
		"equal-time resolution": func(_ *BriefHighlight, items map[string]EvidenceItem) {
			item := items[laterID]
			item.EventTime = anchor.EventTime
			items[laterID] = item
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := highlight
			event := *highlight.LifeEvent
			resolution := *event.Resolution
			event.Resolution = &resolution
			h.LifeEvent = &event
			items := map[string]EvidenceItem{anchorID: anchor, laterID: later}
			mutate(&h, items)
			_, valid := normalizeBriefEvent(h, items, briefTestPersonID, asOf)
			assert.False(t, valid)
		})
	}
}
