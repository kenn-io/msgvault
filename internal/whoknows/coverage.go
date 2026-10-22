package whoknows

import (
	"context"
	"fmt"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

func (s *Service) populateCoverage(ctx context.Context, out *Response, meta hybrid.ResultMeta, profiles map[int64]store.PeopleProfile) error {
	c := &out.Coverage
	c.Generation = generationView(meta.Generation)
	c.Scope = Scope{MessageTypes: append([]string{}, s.BuildScope.MessageTypes...), Accounts: append([]int64{}, s.BuildScope.SourceIDs...)}
	c.StaleReasons = []string{}
	coverageGenerationMismatch := false
	if s.CoverageReader != nil {
		status, err := s.CoverageReader(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read embedding coverage: %w", err)
		}
		if status.Generation.ID == meta.Generation.ID {
			c.Messages = MessageCoverage{Eligible: status.Eligible, Current: status.Current, Pending: status.Pending, CoverageSampledAt: status.CoverageSampledAt}
		} else {
			coverageGenerationMismatch = true
		}
	}
	if coverageGenerationMismatch {
		c.StaleReasons = append(c.StaleReasons, "embedding_coverage_generation_mismatch")
	} else if c.Messages.Pending > 0 {
		c.StaleReasons = append(c.StaleReasons, fmt.Sprintf("embedding_backlog:%d", c.Messages.Pending))
	}
	if c.Generation.State != "active" {
		c.StaleReasons = append(c.StaleReasons, "generation_not_active")
	}
	if !s.BuildScope.IsEmpty() {
		c.StaleReasons = append(c.StaleReasons, "index_scoped")
	}
	if !c.PeopleIndex.Complete {
		if c.PeopleIndex.Enabled && c.PeopleIndex.Consented {
			c.StaleReasons = append(c.StaleReasons, "people_index_incomplete")
		} else {
			c.StaleReasons = append(c.StaleReasons, "people_index_unavailable")
		}
	}
	c.ContactState.RankedPeople = len(out.People)
	for _, p := range out.People {
		profile := profiles[p.PersonID]
		if !profile.ContactPresent {
			c.ContactState.Missing++
			continue
		}
		if profile.DirtyAt != nil {
			c.ContactState.Dirty++
		} else if !profile.ContactStateFresh {
			c.ContactState.Outdated++
		}
		if profile.ContactStateFresh && profile.ComputedAt != nil && (c.ContactState.OldestComputedAt == nil || profile.ComputedAt.Before(*c.ContactState.OldestComputedAt)) {
			c.ContactState.OldestComputedAt = profile.ComputedAt
		}
	}
	if c.ContactState.Missing > 0 {
		c.StaleReasons = append(c.StaleReasons, fmt.Sprintf("contact_state_missing:%d", c.ContactState.Missing))
	}
	if c.ContactState.Dirty > 0 {
		c.StaleReasons = append(c.StaleReasons, fmt.Sprintf("contact_state_dirty:%d", c.ContactState.Dirty))
	}
	if c.ContactState.Outdated > 0 {
		c.StaleReasons = append(c.StaleReasons, fmt.Sprintf("contact_state_outdated:%d", c.ContactState.Outdated))
	}
	c.Stale = len(c.StaleReasons) > 0
	return nil
}
