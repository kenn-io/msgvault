package peoplesweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestBriefEventCapacityPreservesActiveEventsAndRetiresEvictedAnchors(t *testing.T) {
	for _, resolveFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(resolveFirst), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			context := &BriefEventContext{AsOf: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC),
				AdmissionThrough: time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)}
			evidence := make(map[string]EvidenceItem)
			for i := range 8 {
				item := briefWindowItem(int64(10+i), SourceConversationText, 10+i, fmt.Sprintf("I have event %d.", i))
				evidence[packetEvidenceID(item)] = item
				context.Events = append(context.Events, eventHighlight(item, "2026-08-25"))
			}
			first := context.Events[0].LifeEvent.AnchorID
			resolution := briefWindowItem(30, SourceConversationText, 19, "I cancelled event zero.")
			resolutionID := packetEvidenceID(resolution)
			evidence[resolutionID] = resolution
			resolvedCandidate := context.Events[0]
			resolvedCandidate.EvidenceIDs = append(append([]string(nil), resolvedCandidate.EvidenceIDs...), resolutionID)
			resolvedEvent := *resolvedCandidate.LifeEvent
			resolvedEvent.Resolution = &BriefEventResolution{EvidenceID: resolutionID, Quote: resolution.Excerpt}
			resolvedCandidate.LifeEvent = &resolvedEvent
			fresh := briefWindowItem(40, SourceConversationText, 21, "I have a new event.")
			evidence[packetEvidenceID(fresh)] = fresh
			freshCandidate := eventHighlight(fresh, "2026-08-25")
			candidates := []BriefHighlight{freshCandidate, resolvedCandidate}
			if resolveFirst {
				candidates[0], candidates[1] = candidates[1], candidates[0]
			}
			kept, _, dropped := mergeBriefEvents(candidates, context, evidence, briefTestPersonID, context.AsOf)
			require.Len(kept, 8)
			assert.Equal(1, dropped, "the resolved event is retired to admit the fresh event")
			keys := make(map[string]bool)
			for _, h := range kept {
				require.NotNil(h.LifeEvent)
				assert.False(keys[h.LifeEvent.AnchorID], "one event per announcement")
				keys[h.LifeEvent.AnchorID] = true
			}
			assert.False(keys[first])
			assert.True(keys[packetEvidenceID(fresh)])
			for _, h := range context.Events[1:] {
				assert.True(keys[h.LifeEvent.AnchorID])
			}
		})
	}
}

func TestBriefEventCapacityKeepsAnOrdinaryHighlightWithEightRetainedEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	context := &BriefEventContext{AsOf: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		AdmissionThrough: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	evidence := make(map[string]EvidenceItem)
	for i := range 8 {
		item := briefWindowItem(int64(10+i), SourceConversationText, 10+i,
			fmt.Sprintf("I have event %d.", i))
		evidence[packetEvidenceID(item)] = item
		context.Events = append(context.Events, eventHighlight(item, "2026-09-15"))
	}
	newest := briefWindowItem(30, SourceConversationText, 25, "I started a new role this week.")
	newestID := packetEvidenceID(newest)
	evidence[newestID] = newest
	ordinary := BriefHighlight{Text: "They started a new role this week.", Speaker: BriefSpeakerPerson,
		EvidenceIDs: []string{newestID}}

	kept, indexes, dropped := mergeBriefEvents([]BriefHighlight{ordinary}, context, evidence,
		briefTestPersonID, context.AsOf)

	requirements.Len(kept, 8)
	assertions.Zero(dropped)
	assertions.Equal(ordinary.Text, kept[indexes[0]].Text)
	assertions.Nil(kept[indexes[0]].LifeEvent)
	assertions.NotContains(kept, context.Events[0], "the oldest event gives way to new provider content")
	for _, event := range context.Events[1:] {
		assertions.Contains(kept, event)
	}
}

func TestBriefWindowRetiresEventsWhenCapsShrink(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"unresolved", briefWindowRetiresOldestUnresolvedEventWhenCapsShrink},
		{"resolved", briefWindowRetiresResolvedEventUnderPressure},
	} {
		t.Run(test.name, test.run)
	}
}

func briefWindowRetiresOldestUnresolvedEventWhenCapsShrink(t *testing.T) {
	t.Helper()
	for _, capName := range []string{"items", "bytes"} {
		t.Run(capName, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			oldest := briefWindowItem(10, SourceConversationText, 10, "I have an older event.")
			newer := briefWindowItem(11, SourceConversationText, 20, "I have a newer event.")
			lastContact := briefWindowItem(12, SourceConversationText, 30, "A recent ordinary update.")
			oldestHighlight := eventHighlight(oldest, "2026-09-15")
			newerHighlight := eventHighlight(newer, "2026-09-20")
			admission := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
			previous := &BriefBoundary{EventMemoryVersion: 1, AdmissionThroughEventTime: admission,
				EventMemory: []BriefEventMemory{
					{Highlight: newerHighlight, Sources: []EvidenceRef{newer.Ref}},
					{Highlight: oldestHighlight, Sources: []EvidenceRef{oldest.Ref}},
				}}
			archive := &briefFakeArchive{items: []EvidenceItem{oldest, newer, lastContact},
				hasLastContact: true, lastContact: BriefLastContact{
					MessageID: lastContact.Ref.MessageID, SourceID: lastContact.Ref.SourceID,
				}}
			request := briefWindowRequest(t)
			request.Previous = previous
			request.OverlapItems = 0
			request.AsOf = time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
			request.priorEventMemoryVersion = 7
			request.MaxItems = 3
			if capName == "items" {
				request.MaxItems = 2
			} else {
				baseline, err := BuildBriefWindow(t.Context(), archive, request)
				requirements.NoError(err)
				nextRequest := baseline.Request
				nextRequest.retainedEvents = []BriefHighlight{newerHighlight}
				nextItems := []EvidenceItem{newer, lastContact}
				sortBriefItemsNewestFirst(nextItems)
				_, nextBoundary, err := briefBatch(nextRequest,
					briefWindowLanes(nextRequest.Profile.AllowedSources), nextItems)
				requirements.NoError(err)
				request.MaxBytes = nextBoundary.InputBytes
			}

			window, err := BuildBriefWindow(t.Context(), archive, request)

			requirements.NoError(err)
			requirements.Len(window.Request.retainedEvents, 1)
			assertions.Equal(packetEvidenceID(newer),
				window.Request.retainedEvents[0].LifeEvent.AnchorID)
			assertions.LessOrEqual(window.Boundary.InputBytes, request.MaxBytes)
			assertions.Equal(request.priorEventMemoryVersion,
				window.Request.retainedEventMemoryVersion,
				"retirement still fences apply against the source brief")
			assertions.Equal(admission, window.Batch.Packet.BriefContext.AdmissionThrough,
				"capacity retirement preserves the admission watermark")
		})
	}
}

func TestBriefEventCapacityOmitsUnretainedOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"undated byte limit", eventCapacityUndatedByte},
		{"upcoming item limit", eventCapacityItem},
		{"upcoming byte limit", eventCapacityUpcomingByte},
		{"resolved byte limit", eventCapacityResolvedByte},
	} {
		t.Run(test.name, test.run)
	}
}

func eventCapacityUndatedByte(t *testing.T) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor}}, nil, "2026-08-14T12:00:00Z")
	window.Request.MaxBytes = window.Boundary.InputBytes
	highlight := eventHighlight(anchor, "")
	parsed, err := ParseBrief(eventCandidate(t, highlight), window, window.Request.Profile)
	requirements.NoError(err)
	parsed.Output.LastMeaningfulInteraction = &BriefInteraction{EvidenceID: packetEvidenceID(anchor), Summary: "they discussed a move"}
	result, err := renderBriefResult(parsed, window, window.Request.AsOf)
	requirements.NoError(err)
	assertions.Empty(result.Boundary.EventMemory, "future memory retires when the lowered cap cannot preserve it")
	assertions.NotContains(result.Rendered.Text, highlight.LifeEvent.Question)
}

func TestBriefEventAdmissionSurvivesRegressingWindowAndJSONRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	newer := briefWindowItem(11, SourceConversationText, 20, "An ordinary update.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor, newer}}
	window := eventWindow(t, archive, nil, "2026-08-21T00:00:00Z")
	first := eventResult(t, window, eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	archive.items = []EvidenceItem{anchor}
	window = eventWindow(t, archive, &first.Boundary, "2026-08-22T00:00:00Z")
	second := eventResult(t, window, eventCandidate(t))
	assert.True(second.Boundary.ThroughEventTime.Before(first.Boundary.ThroughEventTime))
	assert.Equal(first.Boundary.AdmissionThroughEventTime, second.Boundary.AdmissionThroughEventTime)
	raw, err := json.Marshal(second.Boundary)
	require.NoError(err)
	var restarted BriefBoundary
	require.NoError(json.Unmarshal(raw, &restarted))
	assert.Equal(first.Boundary.AdmissionThroughEventTime, restarted.AdmissionThroughEventTime)
}

func TestBriefEventPacketBindingCoversEveryContextCoordinate(t *testing.T) {
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	for name, mutate := range map[string]func(*BriefEventContext){
		"clock":     func(c *BriefEventContext) { c.AsOf = c.AsOf.Add(time.Hour) },
		"admission": func(c *BriefEventContext) { c.AdmissionThrough = c.AsOf },
		"bootstrap": func(c *BriefEventContext) { c.Bootstrap = !c.Bootstrap },
		"memory":    func(c *BriefEventContext) { c.Events = []BriefHighlight{eventHighlight(anchor, "2026-08-15")} },
	} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor}}, nil, "2026-08-14T00:00:00Z")
			mutate(window.Batch.Packet.BriefContext)
			_, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), window, window.Request.Profile)
			assert.ErrorContains(err, "not bound")
		})
	}

	t.Run("paid clock", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
		first := eventResult(t, eventWindow(t, archive, nil, "2026-08-14T00:00:00Z"), eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
		window := eventWindow(t, archive, &first.Boundary, "2026-08-16T00:00:00Z")
		window.Request.AsOf = window.Request.AsOf.Add(-10 * 24 * time.Hour)
		parsed, err := ParseBrief(eventCandidate(t), window, window.Request.Profile)
		require.NoError(err)
		require.Len(parsed.Output.Highlights, 1)
		assert.Equal(BriefEventFollowUp, parsed.Output.Highlights[0].LifeEvent.State, "clock comes from the paid packet")
	})
}

func TestBriefEventCanonicalContextDoesNotAliasPreviousMemory(t *testing.T) {
	assert := assert.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	first := eventResult(t, eventWindow(t, archive, nil, "2026-08-14T00:00:00Z"),
		eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	window := eventWindow(t, archive, &first.Boundary, "2026-08-16T00:00:00Z")
	first.Boundary.EventMemory[0].Highlight.LifeEvent.Question = "changed after capture"
	assert.Equal("How did the move go?", window.Batch.Packet.BriefContext.Events[0].LifeEvent.Question)
}

func briefWindowRetiresResolvedEventUnderPressure(t *testing.T) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	resolution := briefWindowItem(11, SourceConversationText, 17, "I cancelled the move.")
	last := briefWindowItem(12, SourceConversationText, 18, "A later ordinary update.")
	highlight := eventHighlight(anchor, "2026-08-15")
	highlight.EvidenceIDs = append(highlight.EvidenceIDs, packetEvidenceID(resolution))
	highlight.LifeEvent.Resolution = &BriefEventResolution{EvidenceID: packetEvidenceID(resolution), Quote: resolution.Excerpt}
	previous := &BriefBoundary{EventMemoryVersion: 1, ThroughEventTime: last.EventTime,
		AdmissionThroughEventTime: last.EventTime, EventMemory: []BriefEventMemory{{
			Highlight: highlight, Sources: []EvidenceRef{anchor.Ref, resolution.Ref}}}}
	archive := &briefFakeArchive{items: []EvidenceItem{anchor, resolution, last}, hasLastContact: true,
		lastContact: BriefLastContact{MessageID: last.Ref.MessageID, SourceID: last.Ref.SourceID}}
	request := briefWindowRequest(t)
	request.Previous, request.MaxItems, request.OverlapItems = previous, 3, 0
	request.AsOf = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	full, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	require.Len(full.Items, 3)
	request.MaxBytes = full.Boundary.InputBytes
	_, err = BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	withoutEvents := full.Request
	withoutEvents.retainedEvents = nil
	_, mandatoryOnlyBoundary, err := briefBatch(withoutEvents,
		briefWindowLanes(withoutEvents.Profile.AllowedSources), []EvidenceItem{last})
	require.NoError(err)
	assert.Less(mandatoryOnlyBoundary.InputBytes, full.Boundary.InputBytes)
	request.MaxBytes = mandatoryOnlyBoundary.InputBytes
	trimmed, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	require.Len(trimmed.Items, 1)
	assert.Equal(last.Ref.MessageID, trimmed.Items[0].Ref.MessageID)
	assert.True(trimmed.LastContact.Included)
	assert.LessOrEqual(trimmed.Boundary.InputBytes, request.MaxBytes)
	assert.Empty(trimmed.Request.retainedEvents)
	assert.Empty(trimmed.Batch.Packet.BriefContext.Events)
	assert.Equal(previous.AdmissionThroughEventTime, trimmed.Batch.Packet.BriefContext.AdmissionThrough,
		"retiring resolved memory under byte pressure must keep the admission watermark")

	request.MaxBytes = 65536
	request.MaxItems = 2
	trimmed, err = BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	require.Len(trimmed.Items, 2)
	assert.True(trimmed.LastContact.Included)
	assert.Empty(trimmed.Request.retainedEvents)
	assert.Empty(trimmed.Batch.Packet.BriefContext.Events)
	assert.Equal(previous.AdmissionThroughEventTime, trimmed.Batch.Packet.BriefContext.AdmissionThrough,
		"retiring resolved memory must keep the admission watermark")
}

func eventCapacityItem(t *testing.T) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor}}, nil, "2026-08-14T00:00:00Z")
	parsed, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), window, window.Request.Profile)
	require.NoError(err)
	parsed.Output.LastMeaningfulInteraction = &BriefInteraction{EvidenceID: packetEvidenceID(anchor), Summary: "they discussed a move"}
	window.Request.MaxItems = 1
	result, err := renderBriefResult(parsed, window, window.Request.AsOf)
	require.NoError(err)
	assert.Empty(result.Boundary.EventMemory)
	assert.Nil(result.Boundary.NextEventAt)
	assert.NotContains(result.Rendered.Text, "Upcoming:")
	assert.NotContains(string(result.Structured), "life_event")
}

func TestBriefEventCapacityRejectsEmptyParagraph(t *testing.T) {
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor}}, nil, "2026-08-14T00:00:00Z")
	parsed, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), window, window.Request.Profile)
	require.NoError(t, err)
	window.Request.MaxItems = 1
	_, err = renderBriefResult(parsed, window, window.Request.AsOf)
	assert.ErrorIs(t, err, ErrEvidenceItemTooLarge)
}

func TestBriefEventResolutionFitsTheReservedPacketItem(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	resolution := briefWindowItem(11, SourceConversationText, 11, "I cancelled the move.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	request := briefWindowRequest(t)
	request.MaxItems, request.OverlapItems = 2, 0
	request.AsOf = time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	firstWindow, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	firstParsed, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), firstWindow, request.Profile)
	require.NoError(err)
	first, err := renderBriefResult(firstParsed, firstWindow, request.AsOf)
	require.NoError(err)

	archive.items = append(archive.items, resolution)
	request.Previous = &first.Boundary
	request.AsOf = time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	secondWindow, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	resolved := eventHighlight(anchor, "2026-08-15")
	resolutionID := packetEvidenceID(resolution)
	resolved.EvidenceIDs = append(resolved.EvidenceIDs, resolutionID)
	resolved.LifeEvent.Resolution = &BriefEventResolution{EvidenceID: resolutionID, Quote: resolution.Excerpt}
	secondParsed, err := ParseBrief(eventCandidate(t, resolved), secondWindow, request.Profile)
	require.NoError(err)
	result, err := renderBriefResult(secondParsed, secondWindow, request.AsOf)
	require.NoError(err)
	assert.Equal(BriefEventResolved, result.Boundary.EventMemory[0].Highlight.LifeEvent.State)
}

func TestBriefEventMemoryCapacityTrimsOrdinaryEvidenceBeforeRejecting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	ordinary := briefWindowItem(11, SourceConversationText, 11, "An ordinary update without an event.")
	window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor, ordinary}}, nil,
		"2026-08-14T00:00:00Z")
	parsed, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), window, window.Request.Profile)
	require.NoError(err)

	futureAsOf := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	event := parsed.Output.Highlights[0]
	evidence := map[string]EvidenceItem{packetEvidenceID(anchor): anchor}
	projected, ok := normalizeBriefEvent(event, evidence, window.Request.PersonID, futureAsOf)
	require.True(ok)
	projectedRequest := window.Request
	projectedRequest.AsOf = futureAsOf
	projectedRequest.retainedEvents = []BriefHighlight{projected}
	projectedRequest.Previous = &BriefBoundary{EventMemoryVersion: 1, AdmissionThroughEventTime: futureAsOf}
	lanes := briefWindowLanes(projectedRequest.Profile.AllowedSources)
	_, retainedOnlyBoundary, err := briefBatch(projectedRequest, lanes, []EvidenceItem{anchor})
	require.NoError(err)
	_, withOrdinaryBoundary, err := briefBatch(projectedRequest, lanes, window.Items)
	require.NoError(err)
	assert.Greater(withOrdinaryBoundary.InputBytes, retainedOnlyBoundary.InputBytes)

	window.Request.MaxBytes = retainedOnlyBoundary.InputBytes
	_, err = renderBriefResult(parsed, window, window.Request.AsOf)
	require.NoError(err, "ordinary evidence can be trimmed while retaining event sources")
}

func eventCapacityUpcomingByte(t *testing.T) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	window := eventWindow(t, &briefFakeArchive{items: []EvidenceItem{anchor}}, nil, "2026-08-14T00:00:00Z")
	parsed, err := ParseBrief(eventCandidate(t, eventHighlight(anchor, "2026-08-15")), window, window.Request.Profile)
	require.NoError(err)
	parsed.Output.LastMeaningfulInteraction = &BriefInteraction{EvidenceID: packetEvidenceID(anchor), Summary: "they discussed a move"}
	future := window.Request
	future.AsOf = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	future.Previous = &BriefBoundary{EventMemoryVersion: 1, AdmissionThroughEventTime: future.AsOf}
	future.retainedEvents = nil
	_, empty, err := briefBatch(future, briefWindowLanes(future.Profile.AllowedSources), window.Items)
	require.NoError(err)
	window.Request.MaxBytes = empty.InputBytes
	result, err := renderBriefResult(parsed, window, window.Request.AsOf)
	require.NoError(err)
	assert.Empty(result.Boundary.EventMemory)
	assert.Nil(result.Boundary.NextEventAt)
	assert.NotContains(result.Rendered.Text, "Upcoming:")
}

func eventCapacityResolvedByte(t *testing.T) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	resolutionText := strings.Repeat("I cancelled the move after plans changed. ", 5)
	resolution := briefWindowItem(11, SourceConversationText, 17, resolutionText)
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	first := eventResult(t, eventWindow(t, archive, nil, "2026-08-14T00:00:00Z"),
		eventCandidate(t, eventHighlight(anchor, "2026-08-15")))

	archive.items = append(archive.items, resolution)
	request := briefWindowRequest(t)
	request.Previous, request.OverlapItems = &first.Boundary, 0
	request.AsOf = time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	resolved := eventHighlight(anchor, "2026-08-15")
	resolutionID := packetEvidenceID(resolution)
	resolved.EvidenceIDs = append(resolved.EvidenceIDs, resolutionID)
	resolved.LifeEvent.Resolution = &BriefEventResolution{
		EvidenceID: resolutionID, Quote: resolution.Excerpt,
	}
	parsed, err := ParseBrief(eventCandidate(t, resolved), window, request.Profile)
	require.NoError(err)
	require.Equal(BriefEventResolved, parsed.Output.Highlights[0].LifeEvent.State)

	parsed.Output.LastMeaningfulInteraction = &BriefInteraction{EvidenceID: resolutionID, Summary: "they discussed their plans"}
	window.Request.MaxBytes = window.Boundary.InputBytes
	result, err := renderBriefResult(parsed, window, window.Request.AsOf)
	require.NoError(err)
	assert.NotContains(result.Rendered.Text, "Resolved:")
	assert.Empty(result.Boundary.EventMemory)
	assert.Empty(ParsedBriefOutput(t, result).Highlights)
}

func TestBriefEventChangedSourceCannotInheritMemoryEvenWithTheSameWords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	announcement := "I move on August 15."
	stableTail := strings.Repeat("same detail ", 180)
	oldFullText := announcement + stableTail + strings.Repeat(" old detail", 20)
	changedFullText := announcement + stableTail + strings.Repeat(" new detail", 20)
	excerpt := string([]rune(oldFullText)[:personfacts.MaxEvidenceExcerptRunes])
	sourceIdentity := func(fullText string) (string, string) {
		hash := sha256.Sum256([]byte(fullText))
		contentHash := hex.EncodeToString(hash[:])
		return "message/v1:" + contentHash, contentHash
	}
	anchor := briefWindowItem(10, SourceConversationText, 10, excerpt)
	anchor.SourceVersion, anchor.ContentSHA256 = sourceIdentity(oldFullText)
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	highlight := eventHighlight(anchor, "2026-08-15")
	highlight.LifeEvent.Quote = announcement
	first := eventResult(t, eventWindow(t, archive, nil, "2026-08-14T00:00:00Z"),
		eventCandidate(t, highlight))
	changed := anchor
	changed.SourceVersion, changed.ContentSHA256 = sourceIdentity(changedFullText)
	assert.Equal(anchor.Excerpt, changed.Excerpt, "the edit is outside the retained excerpt")
	assert.NotEqual(anchor.ContentSHA256, changed.ContentSHA256, "the full source hash changes")
	archive.items = []EvidenceItem{changed}
	request := briefWindowRequest(t)
	request.Previous, request.OverlapItems = &first.Boundary, 4
	request.AsOf = time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(err)
	assert.Empty(window.Batch.Packet.BriefContext.Events)
}

func TestBriefEventAnnouncementDateCannotDriftDuringReextraction(t *testing.T) {
	anchor := briefWindowItem(10, SourceConversationText, 10, "I move on August 15.")
	archive := &briefFakeArchive{items: []EvidenceItem{anchor}}
	first := eventResult(t, eventWindow(t, archive, nil, "2026-08-14T00:00:00Z"),
		eventCandidate(t, eventHighlight(anchor, "2026-08-15")))
	window := eventWindow(t, archive, &first.Boundary, "2026-08-16T00:00:00Z")
	drifted := eventHighlight(anchor, "2026-08-25")
	result := eventResult(t, window, eventCandidate(t, drifted))
	assert.Equal(t, "2026-08-15", *result.Boundary.EventMemory[0].Highlight.LifeEvent.Date)
	assert.Contains(t, result.Rendered.Text, "How did the move go?")
}

func FuzzBriefEventCitationSetCanReplay(f *testing.F) {
	for _, count := range []uint8{31, 32, 39} {
		f.Add(count)
	}
	f.Fuzz(func(t *testing.T, encoded uint8) {
		briefEventCitationSetReplay(t, 1+int(encoded)%40)
	})
}

func TestBriefEventSourceReplayAcceptsSchemaCitationMaximum(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// The paid packet's byte cap can be tighter than the schema's citation
	// count, so exercise that maximum at the replay validator itself.
	items := make([]EvidenceItem, 200)
	sources := make([]EvidenceRef, len(items))
	for i := range items {
		items[i] = briefWindowItem(int64(10+i), SourceConversationText, 10, "I am moving.")
		sources[i] = items[i].Ref
	}
	highlight := eventHighlight(items[0], "2026-08-15")
	highlight.EvidenceIDs = nil
	for _, item := range items {
		highlight.EvidenceIDs = append(highlight.EvidenceIDs, packetEvidenceID(item))
	}
	request := briefWindowRequest(t)
	request.AsOf = time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	request.Previous = &BriefBoundary{EventMemoryVersion: 1,
		EventMemory: []BriefEventMemory{{Highlight: highlight, Sources: sources}}}
	after, before, err := contextDateBounds(request.Profile.SourceSince, request.Profile.SourceUntil)
	require.NoError(err)
	retained, events, protected, err := briefRetainedEvents(t.Context(), &briefFakeArchive{items: items},
		request, briefWindowLanes(request.Profile.AllowedSources), after, before)
	require.NoError(err)
	assert.Len(retained, 200)
	assert.Len(protected, 200)
	require.Len(events, 1)
	assert.Equal(BriefEventFollowUp, events[0].LifeEvent.State)
}

func briefEventCitationSetReplay(t *testing.T, count int) {
	t.Helper()
	assert := assert.New(t)
	items := make([]EvidenceItem, count)
	for i := range items {
		items[i] = briefWindowItem(int64(10+i), SourceConversationText, 10,
			fmt.Sprintf("I have move update %d.", i))
		items[i].EventTime = items[i].EventTime.Add(time.Duration(i) * time.Minute)
	}
	archive := &briefFakeArchive{items: items}
	request := briefWindowRequest(t)
	request.MaxItems, request.MaxBytes, request.OverlapItems = count+1, 2*1024*1024, 0
	request.AsOf = time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	window, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err)
	require.Len(t, window.Items, count)
	highlight := eventHighlight(items[0], "2026-08-15")
	highlight.EvidenceIDs = nil
	for _, item := range items {
		highlight.EvidenceIDs = append(highlight.EvidenceIDs, packetEvidenceID(item))
	}
	first := eventResult(t, window, eventCandidate(t, highlight))
	require.Len(t, first.Boundary.EventMemory, 1)
	require.Len(t, first.Boundary.EventMemory[0].Sources, count)
	request.Previous = &first.Boundary
	request.AsOf = time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	replayed, err := BuildBriefWindow(t.Context(), archive, request)
	require.NoError(t, err, "every accepted citation set must survive source replay")
	due := eventResult(t, replayed, eventCandidate(t))
	assert.Contains(due.Rendered.Text, highlight.LifeEvent.Question)
	assert.Len(due.Evidence, count)
	require.Len(t, due.Boundary.EventMemory, 1)
	assert.Len(due.Boundary.EventMemory[0].Sources, count)
}
