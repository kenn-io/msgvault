package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"
)

// ListColdActivityMessagesContext supplies individual conversation messages
// using the cold adapter's live eligibility and group noise rule.
func (s *Store) ListColdActivityMessagesContext(ctx context.Context, q MessageQuery) ([]ActivityMessage, error) {
	if q.ConversationID <= 0 || q.Limit < 1 || q.Limit > 500 {
		return nil, fmt.Errorf("%w: messages require a conversation and limit 1..500", ErrInvalidActivityRequest)
	}
	window := BurstQuery{ConversationID: q.ConversationID, After: q.After, Before: q.Before, Filters: q.Filters}
	if window.After == nil && window.Before == nil {
		window.After = new(time.Time)
	}
	groups, _, err := s.coldActivityGroupsContext(ctx, window)
	if err != nil {
		return nil, err
	}
	result := []ActivityMessage{}
	for _, group := range groups {
		for _, m := range group.messages {
			if q.AfterKey != nil {
				comparison := compareColdMessageKey(m.dating.OccurredAt, m.ID, *q.AfterKey)
				if !q.Descending && comparison <= 0 || q.Descending && comparison >= 0 {
					continue
				}
			}
			result = append(result, ActivityMessage{MessageID: m.ID, SourceID: group.burst.SourceID, ConversationID: q.ConversationID, Channel: m.channel, Direction: m.Direction, OccurredAt: m.dating.OccurredAt, LocalDate: m.dating.LocalDate, Counterparts: m.Counterparts})
		}
	}
	slices.SortFunc(result, func(a, b ActivityMessage) int {
		n := compareColdMessageKey(a.OccurredAt, a.MessageID, ActivityKey{OccurredAt: b.OccurredAt, MessageID: b.MessageID})
		if q.Descending {
			return -n
		}
		return n
	})
	return result[:min(len(result), q.Limit)], nil
}

func compareColdMessageKey(at time.Time, id int64, key ActivityKey) int {
	if n := at.Compare(key.OccurredAt); n != 0 {
		return n
	}
	return cmp.Compare(id, key.MessageID)
}
