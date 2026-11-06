package peoplesweep

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// BriefEventState describes conversation context, never an obligation or a
// profile fact. Passage of a date cannot resolve an event.
type BriefEventState string

const (
	BriefEventUpcoming BriefEventState = "upcoming"
	BriefEventFollowUp BriefEventState = "follow_up"
	BriefEventResolved BriefEventState = "resolved"
)

type BriefEventResolution struct {
	EvidenceID string `json:"evidence_id"`
	Quote      string `json:"quote"`
}

// BriefLifeEvent retains an explicit announcement and, when available, a later
// explicit outcome. The parser computes State independently of the provider.
type BriefLifeEvent struct {
	Date       *string               `json:"date"`
	AnchorID   string                `json:"anchor_id"`
	Quote      string                `json:"quote"`
	Question   string                `json:"question"`
	Resolution *BriefEventResolution `json:"resolution"`
	State      BriefEventState       `json:"state"`
}

// BriefEventMemory is derived brief context stored with the version, not a
// second observation store. Refs are rehydrated and checked before reuse.
type BriefEventMemory struct {
	Highlight BriefHighlight `json:"highlight"`
	Sources   []EvidenceRef  `json:"sources"`
}

// BriefEventContext is included only in canonical brief packets. AsOf freezes
// the clock for a paid request; AdmissionThrough is the previous watermark.
type BriefEventContext struct {
	AsOf             time.Time        `json:"as_of"`
	AdmissionThrough time.Time        `json:"admission_through"`
	Bootstrap        bool             `json:"bootstrap,omitzero"`
	Events           []BriefHighlight `json:"events"`
}

// ErrBriefEventEvidenceChanged lets the worker commit extraction while
// deferring a brief whose mandatory source changed at final alignment.
var ErrBriefEventEvidenceChanged = errors.New("person brief event evidence changed before apply")

// ErrBriefEventMemoryChanged reports that a generated result relied on event
// memory from a predecessor brief that is no longer current at apply time.
var ErrBriefEventMemoryChanged = errors.New("person brief event memory changed before apply")

func briefLifeEventState(event BriefLifeEvent, asOf time.Time) (BriefEventState, error) {
	var day time.Time
	if event.Date != nil {
		var err error
		day, err = time.Parse(time.DateOnly, *event.Date)
		if err != nil {
			return "", fmt.Errorf("parse brief event date: %w", err)
		}
	}
	if event.Resolution != nil {
		return BriefEventResolved, nil
	}
	if !day.IsZero() && asOf.UTC().Before(day.AddDate(0, 0, 1)) {
		return BriefEventUpcoming, nil
	}
	return BriefEventFollowUp, nil
}

func normalizeBriefEvent(
	highlight BriefHighlight, evidence map[string]EvidenceItem, personID int64, asOf time.Time,
) (BriefHighlight, bool) {
	if highlight.LifeEvent == nil {
		return highlight, true
	}
	event := *highlight.LifeEvent
	for _, id := range highlight.EvidenceIDs {
		if _, exists := evidence[id]; !exists {
			return BriefHighlight{}, false
		}
	}
	anchor, ok := evidence[event.AnchorID]
	if !ok || highlight.Speaker != BriefSpeakerPerson ||
		!slices.Contains(highlight.EvidenceIDs, event.AnchorID) ||
		!briefEventQuoteSupported(anchor, event.Quote, personID) ||
		strings.TrimSpace(event.Question) == "" {
		return BriefHighlight{}, false
	}
	if event.Resolution != nil {
		resolution, exists := evidence[event.Resolution.EvidenceID]
		if !exists || !slices.Contains(highlight.EvidenceIDs, event.Resolution.EvidenceID) ||
			!briefEventQuoteSupported(resolution, event.Resolution.Quote, personID) ||
			!resolution.EventTime.After(anchor.EventTime) {
			return BriefHighlight{}, false
		}
		resolutionCopy := *event.Resolution
		event.Resolution = &resolutionCopy
	}
	state, err := briefLifeEventState(event, asOf)
	if err != nil {
		return BriefHighlight{}, false
	}
	event.State = state
	highlight.LifeEvent = &event
	return highlight, true
}

func briefEventQuoteSupported(item EvidenceItem, quote string, personID int64) bool {
	return strings.TrimSpace(quote) != "" && strings.Contains(item.Excerpt, quote) &&
		item.PersonID == personID && item.SubjectPersonID != nil &&
		*item.SubjectPersonID == personID && !item.Tombstone &&
		item.SourceClass == SourceConversationText
}

// mergeBriefEvents carries supported memory even when the model omits it.
// Existing events take priority, but an ordinary highlight can displace one
// oldest event when all eight slots are occupied so retained memory cannot
// starve new conversation content. Resolved entries are evicted first.
// Mapping preserves the model's original highlight indexes for follow-ups.
func mergeBriefEvents(
	candidates []BriefHighlight, context *BriefEventContext, evidence map[string]EvidenceItem,
	personID int64, fallback time.Time,
) ([]BriefHighlight, map[int]int, int) {
	asOf := fallback
	if context != nil {
		asOf = context.AsOf
	}
	kept := make([]BriefHighlight, 0, 8)
	byAnchor := make(map[string]int)
	indexes := make(map[int]int)
	dropped := 0
	var resolved []BriefHighlight
	if context != nil {
		for _, old := range context.Events {
			h, ok := normalizeBriefEvent(old, evidence, personID, asOf)
			if !ok || h.LifeEvent == nil {
				continue
			}
			if h.LifeEvent.State == BriefEventResolved {
				resolved = append(resolved, h)
				continue
			}
			if _, duplicate := byAnchor[h.LifeEvent.AnchorID]; duplicate {
				continue
			}
			byAnchor[h.LifeEvent.AnchorID] = len(kept)
			kept = append(kept, h)
		}
	}
	// Supported resolved entries are absorbing even if output reopens them.
	for _, old := range resolved {
		if len(kept) == 8 {
			break
		}
		byAnchor[old.LifeEvent.AnchorID] = len(kept)
		kept = append(kept, old)
	}
	// Resolve or update retained anchors first so a new announcement can use a
	// slot freed by a resolution regardless of provider candidate order.
	retainedUpdates := make([]int, 0, len(candidates))
	newEvents := make([]int, 0, len(candidates))
	ordinary := make([]int, 0, len(candidates))
	for index, candidate := range candidates {
		if candidate.LifeEvent != nil {
			if _, retained := byAnchor[candidate.LifeEvent.AnchorID]; retained {
				retainedUpdates = append(retainedUpdates, index)
			} else {
				newEvents = append(newEvents, index)
			}
		} else {
			ordinary = append(ordinary, index)
		}
	}
	order := make([]int, 0, len(candidates))
	order = append(order, retainedUpdates...)
	order = append(order, newEvents...)
	order = append(order, ordinary...)
	forgetCandidateAt := func(outputIndex int) {
		for candidateIndex, mappedIndex := range indexes {
			if mappedIndex == outputIndex {
				delete(indexes, candidateIndex)
				dropped++
			}
		}
	}
	for _, original := range order {
		candidate := candidates[original]
		h, ok := normalizeBriefEvent(candidate, evidence, personID, asOf)
		if !ok {
			dropped++
			continue
		}
		if h.LifeEvent != nil {
			event := h.LifeEvent
			if index, exists := byAnchor[event.AnchorID]; exists {
				old := kept[index].LifeEvent
				if old.Quote != event.Quote {
					dropped++
					continue
				}
				if old.State != BriefEventResolved {
					// An announcement's date is stable; a reschedule needs a new
					// announcement and an explicit resolution of the old one.
					h.LifeEvent.Date = old.Date
					h.LifeEvent.State, _ = briefLifeEventState(*h.LifeEvent, asOf)
					kept[index] = h
				}
				indexes[original] = index
				continue
			}
			if context != nil && !context.Bootstrap &&
				!evidence[event.AnchorID].EventTime.After(context.AdmissionThrough) {
				dropped++
				continue
			}
			if len(kept) == 8 {
				// Free a resolved slot before declining a new live event.
				index := slices.IndexFunc(kept, func(item BriefHighlight) bool {
					return item.LifeEvent != nil && item.LifeEvent.State == BriefEventResolved
				})
				if index >= 0 {
					delete(byAnchor, kept[index].LifeEvent.AnchorID)
					forgetCandidateAt(index)
					kept[index] = h
					byAnchor[event.AnchorID] = index
					indexes[original] = index
					continue
				}
			}
			if len(kept) < 8 {
				byAnchor[event.AnchorID] = len(kept)
			}
		}
		if len(kept) == 8 {
			// Keep one provider slot available even when prior memory already
			// fills the highlight list. Only the first ordinary candidate may
			// displace an event; additional candidates still respect the limit.
			if h.LifeEvent == nil && !slices.ContainsFunc(kept, func(item BriefHighlight) bool {
				return item.LifeEvent == nil
			}) {
				index := slices.IndexFunc(kept, func(item BriefHighlight) bool {
					return item.LifeEvent != nil && item.LifeEvent.State == BriefEventResolved
				})
				if index < 0 {
					index = briefOldestEventIndex(kept, evidence)
				}
				if index >= 0 {
					delete(byAnchor, kept[index].LifeEvent.AnchorID)
					forgetCandidateAt(index)
					kept[index] = h
					indexes[original] = index
					continue
				}
			}
			dropped++
			continue
		}
		indexes[original] = len(kept)
		kept = append(kept, h)
	}
	return kept, indexes, dropped
}

func briefOldestEventIndex(events []BriefHighlight, evidence map[string]EvidenceItem) int {
	return briefOldestMatchingEventIndex(events, evidence, false)
}

func briefOldestUnresolvedEventIndex(events []BriefHighlight, evidence map[string]EvidenceItem) int {
	return briefOldestMatchingEventIndex(events, evidence, true)
}

func briefOldestMatchingEventIndex(
	events []BriefHighlight, evidence map[string]EvidenceItem, unresolvedOnly bool,
) int {
	oldestIndex := -1
	var oldest time.Time
	for index, highlight := range events {
		if highlight.LifeEvent == nil || unresolvedOnly && highlight.LifeEvent.Resolution != nil {
			continue
		}
		anchor, ok := evidence[highlight.LifeEvent.AnchorID]
		if !ok {
			continue
		}
		if oldestIndex < 0 || anchor.EventTime.Before(oldest) {
			oldestIndex = index
			oldest = anchor.EventTime
		}
	}
	if oldestIndex >= 0 {
		return oldestIndex
	}
	return slices.IndexFunc(events, func(highlight BriefHighlight) bool {
		return highlight.LifeEvent != nil && (!unresolvedOnly || highlight.LifeEvent.Resolution == nil)
	})
}
