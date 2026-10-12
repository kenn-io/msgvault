package imazingcsv

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// prepareNotificationReplies predicts the same final archive that the native
// post-import resolver would scan, without publishing a child before its link.
func prepareNotificationReplies(ctx context.Context, st *store.Store, sourceID int64, sourceIdentifier, timezone string, owner participantIdentity, plans []*plannedMessage, existing map[string]store.MessageMetadataRecord, includeArchived bool) (linked, unresolved int, err error) {
	conversations := make(map[string][]*plannedMessage)
	evidenceKeys := make([]string, 0)
	needsEvidence := make(map[string]bool)
	currentByKey := make(map[string]*plannedMessage, len(plans))
	for _, plan := range plans {
		plan.replaceReply = true
		conversations[plan.chat.key] = append(conversations[plan.chat.key], plan)
		currentByKey[plan.sourceMessageID] = plan
		if strings.TrimSpace(plan.row.ReplyingTo) != "" && !needsEvidence[plan.chat.key] {
			needsEvidence[plan.chat.key] = true
			evidenceKeys = append(evidenceKeys, plan.chat.key)
		}
	}
	candidates := conversations
	if includeArchived {
		candidates, err = loadArchivedReplyPlans(ctx, st, sourceID, sourceIdentifier, timezone, owner, evidenceKeys)
		if err != nil {
			return 0, 0, err
		}
		keyByID := make(map[int64]string, len(existing))
		for key, record := range existing {
			keyByID[record.ID] = key
		}
		for key, archived := range candidates {
			retained := archived[:0]
			for _, plan := range archived {
				plan.sourceMessageID = keyByID[plan.messageID]
				if plan.sourceMessageID == "" {
					return 0, 0, errors.New("archived reply identity is unavailable")
				}
				if currentByKey[plan.sourceMessageID] == nil {
					retained = append(retained, plan)
				}
			}
			candidates[key] = retained
		}
		// Only the final projection of a canonical alias is present in the final
		// archive. Preserve CSV order rather than choosing a row by its timestamp.
		for _, plan := range plans {
			if currentByKey[plan.sourceMessageID] == plan {
				candidates[plan.chat.key] = append(candidates[plan.chat.key], plan)
			}
		}
	}
	for _, key := range evidenceKeys {
		index := newReplyCandidateIndex(candidates[key])
		for _, child := range conversations[key] {
			if err := ctx.Err(); err != nil {
				return linked, unresolved, err
			}
			if strings.TrimSpace(child.row.ReplyingTo) == "" {
				continue
			}
			var parent *plannedMessage
			count := 0
			for _, candidate := range index.candidates(child) {
				if replyEvidenceMatches(child.row.ReplyingTo, candidate) {
					parent = candidate
					count++
				}
			}
			if count == 1 {
				child.replyParent = parent
				linked++
			} else {
				unresolved++
			}
		}
	}
	return linked, unresolved, nil
}

// notificationMessageGroups emits dependency components before their children.
// Retained native aliases may make a cyclic physical reply graph; all projections
// in that component share one transaction while keeping the original CSV order.
func notificationMessageGroups(plans []*plannedMessage) [][]*plannedMessage {
	byKey := make(map[string][]*plannedMessage, len(plans))
	order := make([]string, 0, len(plans))
	positions := make(map[*plannedMessage]int, len(plans))
	for i, plan := range plans {
		if byKey[plan.sourceMessageID] == nil {
			order = append(order, plan.sourceMessageID)
		}
		byKey[plan.sourceMessageID] = append(byKey[plan.sourceMessageID], plan)
		positions[plan] = i
	}
	// Tarjan's dependency walk returns strongly connected components with
	// dependencies first. Distinct fresh occurrences cannot form a cycle:
	// the unchanged native evidence matcher requires a strictly earlier parent.
	index := make(map[string]int, len(byKey))
	low := make(map[string]int, len(byKey))
	onStack := make(map[string]bool, len(byKey))
	stack := make([]string, 0, len(byKey))
	next := 0
	groups := make([][]*plannedMessage, 0, len(byKey))
	var visit func(string)
	visit = func(key string) {
		next++
		index[key] = next
		low[key] = next
		stack = append(stack, key)
		onStack[key] = true
		for _, plan := range byKey[key] {
			if plan.replyParent == nil {
				continue
			}
			parent := plan.replyParent.sourceMessageID
			if byKey[parent] == nil {
				continue
			}
			if index[parent] == 0 {
				visit(parent)
				low[key] = min(low[key], low[parent])
			} else if onStack[parent] {
				low[key] = min(low[key], index[parent])
			}
		}
		if low[key] != index[key] {
			return
		}
		var group []*plannedMessage
		for {
			member := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[member] = false
			group = append(group, byKey[member]...)
			if member == key {
				break
			}
		}
		slices.SortFunc(group, func(a, b *plannedMessage) int { return positions[a] - positions[b] })
		groups = append(groups, group)
	}
	for _, key := range order {
		if index[key] == 0 {
			visit(key)
		}
	}
	return groups
}
