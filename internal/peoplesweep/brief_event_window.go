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
// Only resolved events may be retired under capacity pressure.
func briefRetireCapacityEvent(
	events []BriefHighlight, protected map[string]bool,
) ([]BriefHighlight, bool) {
	retired := slices.IndexFunc(events, func(event BriefHighlight) bool {
		return event.LifeEvent != nil && event.LifeEvent.State == BriefEventResolved
	})
	if retired < 0 {
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

// briefReserveEventItems protects supported event sources while preserving the
// mandatory contact item. Resolved memory may retire under changed caps;
// supported active memory must fit in full.
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
		events, retired = briefRetireCapacityEvent(events, protected)
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
	context := &BriefEventContext{AsOf: request.AsOf.UTC(), Bootstrap: true, Events: request.retainedEvents, Appreciations: request.retainedAppreciations}
	if request.Previous != nil && request.Previous.EventMemoryVersion > 0 {
		context.Bootstrap = false
		context.AdmissionThrough = request.Previous.AdmissionThroughEventTime
	}
	return context
}

func freezeBriefEventMemory(window BriefWindow, events []BriefHighlight, byID map[string]EvidenceItem) BriefBoundary {
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
				// The day after 9999-12-31 cannot be represented in JSON.
				if due.Year() <= 9999 && (boundary.NextEventAt == nil || due.Before(*boundary.NextEventAt)) {
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
	return plan.previous != nil && len(plan.previous.EventMemory) != len(window.Request.retainedEvents)
}
