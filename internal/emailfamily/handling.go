package emailfamily

import (
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/mime"
)

const Unknown = "unknown"

// Activity is a source-scoped published activity fact. PositiveSent
// comes from native Sent membership, never messages.is_sent. Incoming is
// resolved separately from confirmed owner identities in the selected scope.
type Activity struct {
	Member

	OccurredAt            time.Time `json:"occurred_at"`
	Incoming              bool      `json:"incoming"`
	AttributionComplete   bool      `json:"attribution_complete"`
	UnavailableReasons    []string  `json:"unavailable_reasons"`
	ConfirmedSourceSender bool      `json:"confirmed_source_sender"`
	PositiveSent          bool      `json:"positive_sent"`
	Draft                 bool      `json:"draft"`
	ExternalRecipient     bool      `json:"external_recipient"`
	LocalHandled          bool      `json:"local_handled"`
}

// Handling explains outstanding received events and actual outbound
// evidence. Provider read/unread and Inbox flags do not enter this calculation.
type Handling struct {
	State               string   `json:"state"`
	OutstandingEventIDs []int64  `json:"outstanding_event_ids"`
	OutboundEvidenceIDs []int64  `json:"outbound_evidence_ids"`
	Reasons             []string `json:"reasons"`
}

type emailReceivedEvent struct {
	anchor               int64
	incoming             bool
	hasTime              bool
	occurredAt           time.Time
	parents              []string
	incomingCopies       int
	locallyHandledCopies int
}

// Evaluate receives one already-authorized scoped component.
// Grouping references and provider threads do not confer handling authority.
func Evaluate(members []Activity) Handling {
	result := Handling{State: Unknown, OutstandingEventIDs: []int64{}, OutboundEvidenceIDs: []int64{}, Reasons: []string{}}
	events := map[string]*emailReceivedEvent{}
	identityEvent := map[string]string{}
	memberEvent := map[int64]string{}
	incomplete := false
	reason := func(value string) {
		if !slices.Contains(result.Reasons, value) {
			result.Reasons = append(result.Reasons, value)
		}
	}
	for _, member := range members {
		key := fmt.Sprintf("message:%d", member.MessageID)
		if member.Linkage.Version == mime.EmailLinkageVersion && member.Linkage.MessageID != "" {
			key = "rfc:" + member.Linkage.MessageID
			identityEvent[member.Linkage.MessageID] = key
		}
		memberEvent[member.MessageID] = key
		event, found := events[key]
		if !found {
			event = &emailReceivedEvent{anchor: member.MessageID, hasTime: true}
			events[key] = event
		}
		event.anchor = min(event.anchor, member.MessageID)
		event.incoming = event.incoming || member.Incoming
		if member.Incoming {
			event.incomingCopies++
			if member.LocalHandled {
				event.locallyHandledCopies++
			}
		}
		if member.OccurredAt.IsZero() {
			event.hasTime = false
			if !member.LocalHandled {
				incomplete = true
				reason("chronology_unavailable")
			}
		} else if member.OccurredAt.After(event.occurredAt) {
			event.occurredAt = member.OccurredAt
		}
		if !member.AttributionComplete {
			if !member.LocalHandled {
				incomplete = true
			}
			if len(member.UnavailableReasons) == 0 {
				reason("identity_unavailable")
			} else {
				for _, unavailable := range member.UnavailableReasons {
					reason(unavailable)
				}
			}
		}
		if member.Linkage.Version != mime.EmailLinkageVersion || member.Linkage.Outcome != "complete" {
			if !member.LocalHandled {
				incomplete = true
			}
			reason("linkage_unavailable")
		}
		// Only direct verified replies establish handling ancestry. References may
		// group sibling branches, but cannot close them as a side effect.
		if member.Linkage.Version == mime.EmailLinkageVersion && member.Linkage.Outcome == "complete" {
			for _, parent := range member.Linkage.Replies {
				if !slices.Contains(event.parents, parent) {
					event.parents = append(event.parents, parent)
				}
			}
		}
	}
	closed := map[string]bool{}
	for key, event := range events {
		if event.incomingCopies > 0 && event.incomingCopies == event.locallyHandledCopies {
			closed[key] = true
			reason("local_confirmation")
		}
	}

	// Chronology is evidence even before a family contains a qualifying reply.
	// A local confirmation covers the exact incomplete event it already closed.
	for _, member := range members {
		if member.LocalHandled || member.Linkage.Version != mime.EmailLinkageVersion || member.Linkage.Outcome != "complete" {
			continue
		}
		for _, parent := range slices.Concat(member.Linkage.Replies, member.Linkage.AttachedOriginals) {
			key, found := identityEvent[parent]
			if !found || closed[key] {
				continue
			}
			event := events[key]
			if member.OccurredAt.IsZero() || !event.hasTime || !member.OccurredAt.After(event.occurredAt) {
				incomplete = true
				reason("chronology_unavailable")
			}
		}
	}

	// Process completed activity chronologically. A valid direct ancestry is
	// walked once; later replies cannot create quadratic rescans of long threads.
	walked := map[string]bool{}
	outbounds := slices.Clone(members)
	slices.SortFunc(outbounds, func(a, b Activity) int {
		if order := a.OccurredAt.Compare(b.OccurredAt); order != 0 {
			return order
		}
		if a.MessageID < b.MessageID {
			return -1
		}
		if a.MessageID > b.MessageID {
			return 1
		}
		return 0
	})
	for _, outbound := range outbounds {
		if !outbound.AttributionComplete || !outbound.ConfirmedSourceSender || !outbound.PositiveSent || outbound.Draft || !outbound.ExternalRecipient || outbound.OccurredAt.IsZero() || outbound.Linkage.Version != mime.EmailLinkageVersion || outbound.Linkage.Outcome != "complete" {
			continue
		}
		targetIDs := slices.Concat(outbound.Linkage.Replies, outbound.Linkage.AttachedOriginals)
		pending := make([]string, 0, len(targetIDs))
		for _, id := range targetIDs {
			if key, found := identityEvent[id]; found {
				pending = append(pending, key)
			}
		}
		visited := map[string]bool{}
		contributed := false
		for len(pending) > 0 {
			key := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if visited[key] || key == memberEvent[outbound.MessageID] {
				continue
			}
			visited[key] = true
			event := events[key]
			if walked[key] {
				continue
			}
			if !event.hasTime || !outbound.OccurredAt.After(event.occurredAt) {
				if closed[key] {
					continue
				}
				reason("chronology_unavailable")
				incomplete = true
				continue
			}
			walked[key] = true
			if event.incoming && !closed[key] {
				closed[key] = true
				contributed = true
			}
			for _, parent := range event.parents {
				if ancestor, found := identityEvent[parent]; found {
					ancestorEvent := events[ancestor]
					if closed[ancestor] {
						// The local decision closes this event, but its known
						// ancestry can still contribute older received events.
						pending = append(pending, ancestor)
						continue
					}
					if !ancestorEvent.hasTime || !event.occurredAt.After(ancestorEvent.occurredAt) {
						reason("chronology_unavailable")
						incomplete = true
						continue
					}
					pending = append(pending, ancestor)
				}
			}
		}
		if contributed {
			result.OutboundEvidenceIDs = append(result.OutboundEvidenceIDs, outbound.MessageID)
		}
	}
	received := 0
	for key, event := range events {
		if !event.incoming {
			continue
		}
		received++
		if !closed[key] {
			result.OutstandingEventIDs = append(result.OutstandingEventIDs, event.anchor)
		}
	}
	if received == 0 {
		reason("no_received_item")
	}
	switch {
	case incomplete:
	case len(result.OutstandingEventIDs) > 0:
		result.State = "actionable"
	case received == 0:
		reason("no_received_item")
	case !incomplete:
		result.State = "handled"
	}
	slices.Sort(result.OutstandingEventIDs)
	slices.Sort(result.OutboundEvidenceIDs)
	slices.Sort(result.Reasons)
	return result
}
