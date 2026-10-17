package loops

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// Waiting walks bounded pages in age order. The first top distinct groups are
// therefore the oldest groups; later pages can still contribute to their
// counts. It never truncates the total count at a database page boundary.
func (s *Service) Waiting(ctx context.Context, opts WaitingOptions) (WaitingResponse, error) {
	if err := s.check(); err != nil {
		return WaitingResponse{}, err
	}
	if opts.Top == 0 {
		opts.Top = 10
	}
	if opts.Top < 1 || opts.Top > 50 {
		return WaitingResponse{}, fmt.Errorf("%w: top must be 1..50", store.ErrInvalidLoop)
	}
	current := s.now().UTC()
	now := opts.AsOf
	if now.IsZero() {
		now = current
	}
	now = now.UTC()
	result := WaitingResponse{Groups: []WaitingGroup{}}
	indices := make(map[string]int)
	var sources []int64
	// AsOf evaluates age and freshness of the current projection; it does not
	// reconstruct which snoozes were active at that evaluation time.
	filter := store.LoopListFilter{Status: "open", Limit: 200, Now: current}
	for {
		rows, err := s.st.ListOpenLoopsContext(ctx, filter)
		if err != nil {
			return WaitingResponse{}, err
		}
		for _, row := range rows {
			if row.Kind != ReplyOwed && row.Kind != "commitment_owed_by_me" {
				continue
			}
			result.Count++
			key := "participant:"
			if row.CounterpartyPersonID != nil {
				key = fmt.Sprintf("person:%d", *row.CounterpartyPersonID)
			} else if row.CounterpartyParticipantID != nil {
				key += fmt.Sprint(*row.CounterpartyParticipantID)
			} else {
				key = fmt.Sprintf("loop:%d", row.ID)
			}
			index, exists := indices[key]
			if !exists {
				if len(result.Groups) == opts.Top {
					continue
				}
				index = len(result.Groups)
				indices[key] = index
				result.Groups = append(result.Groups, WaitingGroup{OldestOwedSinceAt: row.OwedSinceAt, Loops: []Loop{}})
			}
			loop, err := publicLoop(row, now)
			if err != nil {
				return WaitingResponse{}, err
			}
			group := &result.Groups[index]
			group.Counterparty = loop.Counterparty
			group.LoopCount++
			group.Loops = append(group.Loops, loop)
			sources = append(sources, row.SourceID)
		}
		if len(rows) < filter.Limit {
			break
		}
		last := rows[len(rows)-1]
		filter.CursorAt, filter.CursorID, filter.HasCursor = last.OwedSinceAt, last.ID, true
	}
	report, err := s.Freshness(ctx, now, sources)
	if err != nil {
		return WaitingResponse{}, err
	}
	result.Freshness = report
	if report.Stale && !opts.AllowStale {
		return WaitingResponse{}, &StaleError{Response: LoopsStaleResponse{Error: "loops_stale",
			Message: "Archive or derived progress is stale; refresh the reported sources or explicitly allow stale results.", Freshness: report}}
	}
	return result, nil
}
