package timeline

import "context"

func (c *composer) readFreshness(ctx context.Context) (Freshness, error) {
	result := Freshness{AsOf: c.t.Now, Sources: []SourceFreshness{}, Projections: []ProjectionFreshness{}, Warnings: []string{}}
	if c.src.Freshness != nil {
		var err error
		result, err = c.src.Freshness(ctx, c.t.Now)
		if err != nil {
			return Freshness{}, err
		}
	}
	state, err := c.src.Activity.ActivityProjectionStateContext(ctx)
	if err != nil {
		return Freshness{}, err
	}
	projection := ProjectionFreshness{Name: "activity", State: "current"}
	if state.Queued > 0 {
		projection.State = "incomplete"
		projection.Frontier = state.OldestQueuedAt
	}
	if state.Reconciling {
		projection.State = "reconciling"
	}
	result.Projections = append(result.Projections, projection)
	return result, nil
}
