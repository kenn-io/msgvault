package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Rehydrate each message separately: a gone source must not hide an unrelated
// operational failure, and one missing anchor must not discard other events.
func briefRetainedEvents(
	ctx context.Context, archive BriefArchive, request BriefWindowRequest,
	lanes []SourceClass, after, before *time.Time,
) ([]EvidenceItem, []BriefHighlight, map[string]bool, error) {
	protected := make(map[string]bool)
	if request.Previous == nil || len(request.Previous.EventMemory) == 0 {
		return nil, nil, protected, nil
	}
	if len(request.Previous.EventMemory) > 8 {
		return nil, nil, nil, errors.New("person brief event memory exceeds eight items")
	}
	ids := make([]int64, 0)
	for _, memory := range request.Previous.EventMemory {
		// Match the frozen evidence_ids schema: every accepted citation set
		// must remain replayable on the next run.
		if len(memory.Sources) > 200 {
			return nil, nil, nil, errors.New("person brief event has too many sources")
		}
		for _, ref := range memory.Sources {
			if err := validateEvidenceRef(ref); err != nil {
				return nil, nil, nil, err
			}
			if !slices.Contains(ids, ref.MessageID) {
				ids = append(ids, ref.MessageID)
			}
		}
	}
	slices.Sort(ids)
	byID := make(map[string]EvidenceItem)
	items := make([]EvidenceItem, 0)
	for _, id := range ids {
		hydrated, err := archive.HydratePersonSweepMessages(ctx, request.PersonID, []int64{id}, request.ThroughSequence)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, nil, ctx.Err()
			}
			if errors.Is(err, ErrPersonSweepMessageUnavailable) {
				continue
			}
			return nil, nil, nil, fmt.Errorf("hydrate retained person brief event: %w", err)
		}
		for _, item := range hydrated {
			if item.PersonID != request.PersonID || !sweepClassAllowed(lanes, item.SourceClass) {
				continue
			}
			allowed, err := assemblyEvidenceAllowed(item, request.Profile, after, before)
			if err != nil {
				return nil, nil, nil, err
			}
			if !allowed {
				continue
			}
			evidenceID := packetEvidenceID(item)
			if _, exists := byID[evidenceID]; !exists {
				byID[evidenceID] = item
				items = append(items, item)
			}
		}
	}
	events := make([]BriefHighlight, 0, 8)
	for _, memory := range request.Previous.EventMemory {
		h, ok := normalizeBriefEvent(memory.Highlight, byID, request.PersonID, request.AsOf)
		if !ok || h.LifeEvent == nil {
			continue
		}
		for _, id := range h.EvidenceIDs {
			if _, exists := byID[id]; !exists {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		events = append(events, h)
		for _, id := range h.EvidenceIDs {
			protected[id] = true
		}
	}
	// Only supported memory is replayed. Changed source text can still appear
	// through ordinary retrieval; it cannot inherit the old event assertion.
	kept := make([]EvidenceItem, 0, len(items))
	for _, item := range items {
		if protected[packetEvidenceID(item)] {
			kept = append(kept, item)
		}
	}
	return kept, events, protected, nil
}

// briefRetireCapacityEvent removes one event and refreshes protected sources.
// Resolved events retire first; otherwise capacity retires the oldest event.
func briefRetireCapacityEvent(
	events []BriefHighlight, protected map[string]bool, evidence map[string]EvidenceItem,
) ([]BriefHighlight, int, bool) {
	retired := slices.IndexFunc(events, func(event BriefHighlight) bool {
		return event.LifeEvent != nil && event.LifeEvent.State == BriefEventResolved
	})
	if retired < 0 {
		retired = briefOldestEventIndex(events, evidence)
	}
	if retired < 0 {
		return events, -1, false
	}
	updated, ok := briefRetireEventAt(events, protected, retired)
	return updated, retired, ok
}

func briefRetireEventAt(
	events []BriefHighlight, protected map[string]bool, retired int,
) ([]BriefHighlight, bool) {
	if retired < 0 || retired >= len(events) {
		return events, false
	}
	events = append(slices.Clone(events[:retired]), events[retired+1:]...)
	for id := range protected {
		delete(protected, id)
	}
	for _, event := range events {
		for _, id := range event.EvidenceIDs {
			protected[id] = true
		}
	}
	return events, true
}

func briefEventEvidenceByID(items []EvidenceItem) map[string]EvidenceItem {
	evidence := make(map[string]EvidenceItem, len(items))
	for _, item := range items {
		evidence[packetEvidenceID(item)] = item
	}
	return evidence
}

func briefUnresolvedEventEvidenceCount(events []BriefHighlight) int {
	ids := make(map[string]struct{})
	for _, event := range events {
		if event.LifeEvent == nil || event.LifeEvent.Resolution != nil {
			continue
		}
		for _, id := range event.EvidenceIDs {
			ids[id] = struct{}{}
		}
	}
	return len(ids)
}

// briefReserveEventItems protects supported event sources while preserving the
// mandatory contact item. Under changed caps, resolved memory retires first;
// the oldest remaining event retires only when its sources still cannot fit.
func briefReserveEventItems(
	selected, retained []EvidenceItem, events []BriefHighlight, mandatory *EvidenceItem,
	protected map[string]bool, maxItems int,
) ([]EvidenceItem, []BriefHighlight, error) {
	selected = slices.Clone(selected)
	events = slices.Clone(events)
	for _, item := range retained {
		id := packetEvidenceID(item)
		if !slices.ContainsFunc(selected, func(other EvidenceItem) bool { return packetEvidenceID(other) == id }) {
			selected = append(selected, item)
		}
	}
	sortBriefItemsNewestFirst(selected)
	for len(selected) > maxItems {
		if trimmed, ok := briefDropUnprotected(selected, mandatory, protected); ok {
			selected = trimmed
			continue
		}
		var retired bool
		events, _, retired = briefRetireCapacityEvent(
			events, protected, briefEventEvidenceByID(selected))
		if !retired {
			return nil, nil, ErrEvidenceItemTooLarge
		}
	}
	return selected, events, nil
}

func briefDropUnprotected(items []EvidenceItem, mandatory *EvidenceItem, protected map[string]bool) ([]EvidenceItem, bool) {
	if len(items) < 2 {
		return nil, false
	}
	lastContactID := ""
	if mandatory != nil {
		lastContactID = packetEvidenceID(*mandatory)
	}
	for index := range slices.Backward(items) {
		id := packetEvidenceID(items[index])
		if id == lastContactID || protected[id] {
			continue
		}
		return append(slices.Clone(items[:index]), items[index+1:]...), true
	}
	return nil, false
}

func briefEventContext(request BriefWindowRequest) *BriefEventContext {
	context := &BriefEventContext{AsOf: request.AsOf.UTC(), Bootstrap: true, Events: request.retainedEvents}
	if request.Previous != nil && request.Previous.EventMemoryVersion > 0 {
		context.Bootstrap = false
		context.AdmissionThrough = request.Previous.AdmissionThroughEventTime
	}
	return context
}

// validateBriefEventMemoryCapacity makes sure the memory saved with this
// result can fit in a later paid packet and leaves one item for new mandatory
// evidence such as a last-contact message or an explicit outcome. Capacity
// pressure may retire event memory. The caller removes retired unresolved
// events from the current output and retains resolved statements with alignment.
func validateBriefEventMemoryCapacity(parsed ParsedBrief, window BriefWindow) ([]int, error) {
	eventHighlights := make([]BriefHighlight, 0, len(parsed.Output.Highlights))
	for _, highlight := range parsed.Output.Highlights {
		if highlight.LifeEvent == nil {
			continue
		}
		eventHighlights = append(eventHighlights, highlight)
	}
	if len(eventHighlights) == 0 {
		return nil, nil
	}

	evidence := make(map[string]EvidenceItem, len(window.Items))
	for _, item := range window.Items {
		evidence[packetEvidenceID(item)] = item
	}
	// A maximal whole-second timestamp projects upcoming events to the longer
	// follow_up state without adding fractional-second bytes to either clock.
	futureAsOf := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	projectedEvents := make([]BriefHighlight, 0, len(eventHighlights))
	projectedSourceIndexes := make([]int, 0, len(eventHighlights))
	for index, highlight := range parsed.Output.Highlights {
		if highlight.LifeEvent == nil {
			continue
		}
		projected, ok := normalizeBriefEvent(highlight, evidence, window.Request.PersonID, futureAsOf)
		if !ok || projected.LifeEvent == nil {
			return nil, errors.New("person brief event memory could not be projected")
		}
		projectedEvents = append(projectedEvents, projected)
		projectedSourceIndexes = append(projectedSourceIndexes, index)
	}
	protectedIDs := make(map[string]bool)
	for _, event := range projectedEvents {
		for _, id := range event.EvidenceIDs {
			protectedIDs[id] = true
		}
	}
	for briefUnresolvedEventEvidenceCount(projectedEvents) >= window.Request.MaxItems {
		retiredIndex := briefOldestUnresolvedEventIndex(projectedEvents, evidence)
		if retiredIndex < 0 {
			return nil, ErrEvidenceItemTooLarge
		}
		var ok bool
		projectedEvents, ok = briefRetireEventAt(projectedEvents, protectedIDs, retiredIndex)
		if !ok {
			return nil, ErrEvidenceItemTooLarge
		}
		projectedSourceIndexes = append(projectedSourceIndexes[:retiredIndex],
			projectedSourceIndexes[retiredIndex+1:]...)
	}
	projectedRequest := window.Request
	projectedRequest.AsOf = futureAsOf
	projectedRequest.retainedEvents = projectedEvents
	projectedRequest.Previous = &BriefBoundary{
		EventMemoryVersion: 1, AdmissionThroughEventTime: futureAsOf,
	}
	projectedItems := slices.Clone(window.Items)
	var mandatory *EvidenceItem
	if window.LastContact.Included {
		for _, item := range projectedItems {
			if item.Ref.MessageID == window.LastContact.MessageID && item.Ref.SourceID == window.LastContact.SourceID {
				mandatoryCopy := item
				mandatory = &mandatoryCopy
				break
			}
		}
		if mandatory == nil {
			return nil, errors.New("person brief last-contact item is missing from the event capacity projection")
		}
	}
	projectionProtectedIDs := make(map[string]bool, len(protectedIDs))
	for id := range protectedIDs {
		projectionProtectedIDs[id] = true
	}
	lanes := briefWindowLanes(projectedRequest.Profile.AllowedSources)
	for {
		_, boundary, err := briefBatch(projectedRequest, lanes, projectedItems)
		if err != nil {
			return nil, fmt.Errorf("project next person brief event packet: %w", err)
		}
		if boundary.InputBytes <= window.Request.MaxBytes {
			return projectedSourceIndexes, nil
		}
		trimmed, ok := briefDropUnprotected(projectedItems, mandatory, projectionProtectedIDs)
		if ok {
			projectedItems = trimmed
			continue
		}
		var retiredIndex int
		projectedEvents, retiredIndex, ok = briefRetireCapacityEvent(
			projectedEvents, projectionProtectedIDs, briefEventEvidenceByID(projectedItems))
		if !ok {
			return nil, ErrEvidenceItemTooLarge
		}
		projectedRequest.retainedEvents = projectedEvents
		projectedSourceIndexes = append(projectedSourceIndexes[:retiredIndex],
			projectedSourceIndexes[retiredIndex+1:]...)
	}
}

func freezeBriefEventMemory(window BriefWindow, events []BriefHighlight) BriefBoundary {
	boundary := window.Boundary.Canonical()
	boundary.EventMemoryVersion = 1
	boundary.AdmissionThroughEventTime = boundary.ThroughEventTime
	for _, item := range window.Items {
		if item.EventTime.After(boundary.AdmissionThroughEventTime) {
			boundary.AdmissionThroughEventTime = item.EventTime.UTC()
		}
	}
	if previous := window.Request.Previous; previous != nil && previous.AdmissionThroughEventTime.After(boundary.AdmissionThroughEventTime) {
		boundary.AdmissionThroughEventTime = previous.AdmissionThroughEventTime
	}
	byID := make(map[string]EvidenceItem, len(window.Items))
	for _, item := range window.Items {
		byID[packetEvidenceID(item)] = item
	}
	for _, highlight := range events {
		memory := BriefEventMemory{Highlight: highlight}
		for _, id := range highlight.EvidenceIDs {
			memory.Sources = append(memory.Sources, byID[id].Ref)
		}
		boundary.EventMemory = append(boundary.EventMemory, memory)
		event := highlight.LifeEvent
		if event.State == BriefEventUpcoming && event.Date != nil {
			day, err := time.Parse(time.DateOnly, *event.Date)
			if err == nil {
				due := day.AddDate(0, 0, 1)
				if boundary.NextEventAt == nil || due.Before(*boundary.NextEventAt) {
					boundary.NextEventAt = &due
				}
			}
		}
	}
	return boundary
}

// A current event can lose support through policy changes without a new
// journal row. Revalidation is local; only a changed memory needs inference.
func briefEventMemoryChanged(plan briefPlan, window BriefWindow) bool {
	if plan.previous == nil {
		return false
	}
	if len(plan.previous.EventMemory) != len(window.Request.retainedEvents) {
		return true
	}
	return false
}
