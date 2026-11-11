package peoplesweep

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func eventHighlight(item EvidenceItem, date string) BriefHighlight {
	var day *string
	if date != "" {
		day = &date
	}
	id := packetEvidenceID(item)
	return BriefHighlight{Text: "their move", Speaker: BriefSpeakerPerson,
		EvidenceIDs: []string{id}, ConfidenceBasisPoints: 900,
		LifeEvent: &BriefLifeEvent{Date: day, AnchorID: id, Quote: item.Excerpt,
			Question: "How did the move go?", State: BriefEventUpcoming}}
}

func eventCandidate(t *testing.T, highlights ...BriefHighlight) json.RawMessage {
	t.Helper()
	output := briefOutputJSON{}
	for _, highlight := range highlights {
		encoded, err := json.Marshal(highlight)
		require.NoError(t, err)
		output.Highlights = append(output.Highlights, string(encoded))
	}
	return output.raw()
}

func eventWindow(t *testing.T, archive *briefFakeArchive, previous *BriefBoundary, at string) BriefWindow {
	t.Helper()
	request := briefWindowRequest(t)
	request.MaxItems = 12
	request.OverlapItems = 0
	request.Previous = previous
	var err error
	request.AsOf, err = time.Parse(time.RFC3339, at)
	require.NoError(t, err)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	return window
}

func eventResult(t *testing.T, window BriefWindow, raw json.RawMessage) BriefResult {
	t.Helper()
	parsed, err := ParseBrief(raw, window, window.Request.Profile)
	require.NoError(t, err)
	result, err := renderBriefResult(parsed, window, window.Request.AsOf)
	require.NoError(t, err)
	return result
}

func TestBriefEventLifecycleCarriesEvidenceAndRetiresQuestion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	window := eventWindow(t, archive, nil, "2026-08-14T12:00:00Z")
	highlight := eventHighlight(anchor, "2026-08-15")
	upcoming := eventResult(t, window, eventCandidate(t, highlight))
	assert.Contains(upcoming.Rendered.Text, "Upcoming:")
	assert.NotContains(upcoming.Rendered.Text, highlight.LifeEvent.Question)
	require.NotNil(upcoming.Boundary.NextEventAt)
	assert.Equal("2026-08-16T00:00:00Z", upcoming.Boundary.NextEventAt.Format(time.RFC3339))

	// No new activity and no ordinary overlap: the old anchor is still read,
	// and a provider omission cannot erase its question after the date passes.
	window = eventWindow(t, archive, &upcoming.Boundary, "2026-08-16T00:00:00Z")
	due := eventResult(t, window, eventCandidate(t))
	assert.Contains(due.Rendered.Text, highlight.LifeEvent.Question)
	assert.Nil(due.Boundary.NextEventAt)
	require.Len(due.Evidence, 1)
	require.Len(due.Boundary.EventMemory, 1)
	assert.Equal(BriefEventFollowUp, due.Boundary.EventMemory[0].Highlight.LifeEvent.State)

	resolution := briefWindowItem(11, SourceConversationText, 17, "I cancelled the move.")
	archive.items = append(archive.items, resolution)
	highlight.EvidenceIDs = append(highlight.EvidenceIDs, packetEvidenceID(resolution))
	highlight.LifeEvent.Resolution = &BriefEventResolution{EvidenceID: packetEvidenceID(resolution), Quote: resolution.Excerpt}
	window = eventWindow(t, archive, &due.Boundary, "2026-08-18T12:00:00Z")
	resolved := eventResult(t, window, eventCandidate(t, highlight))
	assert.Contains(resolved.Rendered.Text, "Resolved:")
	assert.NotContains(resolved.Rendered.Text, highlight.LifeEvent.Question)
	require.Len(resolved.Evidence, 2)

	// A model cannot reopen a supported resolution by returning the original
	// announcement alone, even during an explicit regeneration.
	window = eventWindow(t, archive, &resolved.Boundary, "2026-08-19T12:00:00Z")
	stillResolved := eventResult(t, window, eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	assert.NotContains(stillResolved.Rendered.Text, highlight.LifeEvent.Question)
	assert.Equal(BriefEventResolved, stillResolved.Boundary.EventMemory[0].Highlight.LifeEvent.State)
	ordinals, ok := BriefCitationOrdinals(ParsedBriefOutput(t, stillResolved), len(stillResolved.Evidence))
	require.True(ok)
	assert.Equal([]int{0, 1}, ordinals[BriefSentenceKey{Kind: BriefSentenceHighlight, Index: 0}])
}

func ParsedBriefOutput(t *testing.T, result BriefResult) BriefOutput {
	t.Helper()
	var output BriefOutput
	require.NoError(t, json.Unmarshal(result.Structured, &output))
	return output
}

func TestBriefEventReplayNeverReintroducesEvictedOrEditedAnchors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	anchor.EventTime = anchor.EventTime.Add(123 * time.Millisecond)
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	initial := eventWindow(t, archive, nil, "2026-08-14T12:00:00Z")
	first := eventResult(t, initial, eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	assert.Equal(anchor.EventTime, first.Boundary.AdmissionThroughEventTime)
	previous := first.Boundary
	previous.EventMemory = nil // resolved event evicted; watermark survives
	request := initial.Request
	request.Previous = &previous
	request.OverlapItems = 4
	request.AsOf = request.AsOf.Add(72 * time.Hour)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	h := eventHighlight(anchor, "2026-08-15")
	h.LifeEvent.Quote = "move on August 15" // another real substring cannot bypass admission
	parsed, err := ParseBrief(eventCandidate(t, h), window, request.Profile)
	require.NoError(err)
	assert.Empty(parsed.Output.Highlights)
	assert.Equal(1, parsed.DroppedItemCount)

	// New evidence ABOVE the previous high water is admitted before the next
	// watermark is advanced; comparing to the current maximum would lose it.
	fresh := briefWindowItem(11, SourceConversationText, 12, "I interview on August 20.")
	archive.items = append(archive.items, fresh)
	window, err = BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	parsed, err = ParseBrief(eventCandidate(t, eventHighlight(fresh, "2026-08-20")), window, request.Profile)
	require.NoError(err)
	require.Len(parsed.Output.Highlights, 1)
	assert.Equal(packetEvidenceID(fresh), parsed.Output.Highlights[0].LifeEvent.AnchorID)
}

func TestBriefEventMemoryRespectsExclusionAndOperationalErrors(t *testing.T) {
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	window := eventWindow(t, archive, nil, "2026-08-14T12:00:00Z")
	first := eventResult(t, window, eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	request := window.Request
	request.Previous = &first.Boundary
	request.OverlapItems = 0
	archive.hydrateFailures = map[int64]error{10: errors.New("database unavailable")}
	_, err := BuildBriefWindow(t.Context(), archive, request)
	require.ErrorContains(err, "database unavailable")
	archive.hydrateFailures = nil
	request.Profile.SourceSince = "2026-08-11"
	_, err = BuildBriefWindow(t.Context(), archive, request)
	require.ErrorIs(err, ErrNoBriefEvidence)
	request.Profile.SourceSince = "2026-01-01"
	archive.items = nil
	_, err = BuildBriefWindow(t.Context(), archive, request)
	require.ErrorIs(err, ErrNoBriefEvidence)

	archive.items = []EvidenceItem{anchor}
	request.MaxBytes = 1
	_, err = BuildBriefWindow(t.Context(), archive, request)
	require.ErrorIs(err, ErrEvidenceItemTooLarge, "the required archive item itself cannot fit in one byte")
}

func TestBriefEventQuestionsSurviveParagraphTrimmingTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	archive := &briefFakeArchive{}
	highlights := make([]BriefHighlight, 0, 8)
	for i := range 8 {
		item := briefWindowItem(int64(10+i), SourceConversationText, 10+i, fmt.Sprintf("I have event %d.", i))
		archive.items = append(archive.items, item)
		h := eventHighlight(item, "2026-08-18")
		h.LifeEvent.Question = fmt.Sprintf("Question %d: %s?", i, strings.Repeat("x", 160))
		highlights = append(highlights, h)
	}
	window := eventWindow(t, archive, nil, "2026-08-20T12:00:00Z")
	window.Request.MaxRenderedRunes = 240
	result := eventResult(t, window, eventCandidate(t, highlights...))
	output := ParsedBriefOutput(t, result)
	require.Len(output.Highlights, 8)
	require.Len(result.Boundary.EventMemory, 8)
	for _, h := range highlights {
		assert.Contains(result.Rendered.Text, h.LifeEvent.Question)
	}
	_, ok := BriefCitationOrdinals(output, len(result.Evidence))
	assert.True(ok)
}

func TestBriefEventPlanRunsOnDateWithoutActivityAndStopsAfterSuccess(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config, _ := workerTestConfig(t)
	profile := briefWindowProfile()
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	generated := now.Add(-time.Hour)
	store := &briefFakeStore{enrolled: true, highWater: 40, changedAfter: 40,
		eligibility: BriefEligibility{Version: 1, Status: BriefStatusCurrent, GeneratedAt: &generated,
			Boundary: &BriefBoundary{ThroughSequence: 40, NextEventAt: &now}}}
	worker := Worker{Config: config, Brief: store, Archive: &briefFakeArchive{}}
	plan, err := worker.planPersonBrief(t.Context(), 7, BriefModeAuto, profile, now)
	require.NoError(err)
	assert.True(plan.run)
	store.eligibility.Boundary.NextEventAt = nil
	plan, err = worker.planPersonBrief(t.Context(), 7, BriefModeAuto, profile, now)
	require.NoError(err)
	assert.False(plan.run)
}

func TestBriefEventRejectionDiscardsDerivedMemoryWithoutBootstrappingOldAnchors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	config, _ := workerTestConfig(t)
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	boundary := &BriefBoundary{EventMemoryVersion: 1, AdmissionThroughEventTime: anchor.EventTime,
		EventMemory: []BriefEventMemory{{Highlight: eventHighlight(anchor, "2026-08-15"), Sources: []EvidenceRef{anchor.Ref}}}}
	store := &briefFakeStore{enrolled: true, eligibility: BriefEligibility{
		Version: 1, Status: BriefStatusRejected, Boundary: boundary}}
	worker := Worker{Config: config, Brief: store, Archive: &briefFakeArchive{items: []EvidenceItem{anchor}}}
	plan, err := worker.planPersonBrief(t.Context(), 7, BriefModeAuto, briefWindowProfile(), now)
	require.NoError(err)
	assert.True(plan.run)
	require.NotNil(plan.previous)
	assert.Empty(plan.previous.EventMemory, "rejection cannot endlessly carry the same unwanted event")
	assert.Equal(boundary.AdmissionThroughEventTime, plan.previous.AdmissionThroughEventTime)
	assert.Equal(1, plan.previous.EventMemoryVersion, "rejection does not reset admission")
}

func FuzzBriefEventQuoteMustExist(f *testing.F) {
	f.Add("I move on August 15.")
	f.Add("invented")
	f.Add(" ")
	f.Fuzz(func(t *testing.T, quote string) {
		assert := assert.New(t)
		anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
		highlight := eventHighlight(anchor, "2026-08-15")
		highlight.LifeEvent.Quote = quote
		_, valid := normalizeBriefEvent(highlight, map[string]EvidenceItem{packetEvidenceID(anchor): anchor},
			briefTestPersonID, anchor.EventTime)
		assert.Equal(strings.TrimSpace(quote) != "" && strings.Contains(anchor.Excerpt, quote), valid)
	})
}

func TestBriefEventPreflightFailureDoesNotBlockExtraction(t *testing.T) {
	for _, name := range []string{"packet size", "historical source read"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			items := briefWorkerArchiveItems()
			highlight := eventHighlight(items[0], "2026-09-02")
			brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 91,
				eligibility: BriefEligibility{Version: 1, Status: BriefStatusCurrent,
					Boundary: &BriefBoundary{ThroughSequence: 40, EventMemoryVersion: 1,
						EventMemory: []BriefEventMemory{{Highlight: highlight, Sources: []EvidenceRef{items[0].Ref}}}}}}
			archive := &briefFakeArchive{items: items}
			config := func(c *BriefConfig) { c.MaxBytes = 1 }
			if name == "historical source read" {
				config = nil
				archive.hydrateFailures = map[int64]error{items[0].Ref.MessageID: errors.New("historical source unavailable")}
			}
			outcome := runBriefWorkerCase(t, briefWorkerCase{brief: brief, archive: archive, briefConfig: config,
				responses: []DriverResponse{briefDriverResponse(json.RawMessage(`{"claims":[]}`), "extraction")}})
			require.NoError(outcome.err)
			require.Len(outcome.sink.requests, 1, "usable extraction still commits")
			applied := outcome.sink.requests[0]
			assert.Nil(applied.Brief)
			assert.NotEmpty(applied.BriefFailureClass)
			assert.False(applied.BriefRetryAt.IsZero())
			assert.NotEmpty(applied.CursorAdvances)
			assert.Len(outcome.archive.hydrateCalls, 1, "the failed preflight read is not repeated in this attempt")
		})
	}
}

func TestBriefEventPlannedRefreshBuildsItsArchiveWindowOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	items := briefWorkerArchiveItems()
	highlight := eventHighlight(items[0], "2026-09-02")
	brief := &briefFakeStore{enrolled: true, highWater: 90, changedAfter: 91,
		eligibility: BriefEligibility{Version: 1, Status: BriefStatusCurrent,
			Boundary: &BriefBoundary{ThroughSequence: 40, EventMemoryVersion: 1,
				EventMemory: []BriefEventMemory{{Highlight: highlight, Sources: []EvidenceRef{items[0].Ref}}}}}}
	outcome := runBriefWorkerCase(t, briefWorkerCase{brief: brief, archive: &briefFakeArchive{items: items},
		responses: []DriverResponse{
			briefDriverResponse(json.RawMessage(`{"claims":[]}`), "extraction"),
			briefDriverResponse(eventCandidate(t), "brief"),
		}})
	require.NoError(outcome.err)
	require.Len(outcome.sink.requests, 1)
	require.NotNil(outcome.sink.requests[0].Brief)
	assert.Equal([]ProviderCallCoordinate{
		{BatchOrdinal: 0, CallOrdinal: 0, Purpose: ProviderCallPurposePrimary},
		{BatchOrdinal: 1, CallOrdinal: 0, Purpose: ProviderCallPurposeBrief},
	}, briefCallCoordinates(outcome.sink.requests[0]))
	assert.Len(outcome.archive.candidateCalls, 1, "one refresh must not repeat archive listing")
	assert.Len(outcome.archive.hydrateCalls, 1, "retained sources are read once for the paid packet")
}
