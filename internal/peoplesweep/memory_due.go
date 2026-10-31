package peoplesweep

import (
	"errors"
	"fmt"
	"time"
)

// MemoryConfig controls person memory derivation. An unset Derive keeps it off.
// The routing layer supplies configuration; this rule schedules no work itself.
type MemoryConfig struct {
	Derive           *bool
	Threshold        int
	MaxWait          time.Duration
	Quiet            time.Duration
	MinInterval      time.Duration
	Tier2Interval    time.Duration
	ReverifyInterval time.Duration
}

// DefaultMemoryConfig returns the derivation thresholds and timing gates.
func DefaultMemoryConfig() MemoryConfig {
	return MemoryConfig{
		Threshold:        5,
		MaxWait:          336 * time.Hour,
		Quiet:            6 * time.Hour,
		MinInterval:      20 * time.Hour,
		Tier2Interval:    72 * time.Hour,
		ReverifyInterval: 720 * time.Hour,
	}
}

// DeriveEnabled requires an explicit opt-in.
func (c MemoryConfig) DeriveEnabled() bool { return c.Derive != nil && *c.Derive }

// Validate checks the positive thresholds required by the due rule.
func (c MemoryConfig) Validate() error {
	if c.Threshold < 1 {
		return errors.New("invalid [people.sweep.memory] threshold: must be positive")
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"max_wait", c.MaxWait},
		{"quiet", c.Quiet},
		{"min_interval", c.MinInterval},
		{"tier2_interval", c.Tier2Interval},
		{"reverify_interval", c.ReverifyInterval},
	} {
		if field.value <= 0 {
			return fmt.Errorf("invalid [people.sweep.memory] %s: must be positive", field.name)
		}
	}
	return nil
}

// DueInput is a snapshot of pending memory work and its scheduling gates.
type DueInput struct {
	Now                 time.Time
	Eligible            bool
	ManuallyTracked     bool // Tracking provenance, distinct from an owner request.
	JournalGap          bool
	ManualRequested     bool
	Tier                int
	NewExplicit         int
	Rechecks            int
	ReverifyContexts    int
	OldestUnprocessedAt *time.Time
	LastDeriveAt        *time.Time
	// NewestEvidenceAt is capped per observation at its persisted creation time
	// by the store. Use that stable instant without a moving clamp to Now.
	NewestEvidenceAt *time.Time
}

// DerivationDueAt returns the earliest permitted derivation time, or zero when
// there is no trigger. Every non-zero result is at least in.Now.
func DerivationDueAt(in DueInput, cfg MemoryConfig) time.Time {
	if !cfg.DeriveEnabled() || !in.Eligible || in.JournalGap {
		return time.Time{}
	}
	if in.ManualRequested {
		return in.Now
	}
	interval, ok := cfg.intervalFor(in.Tier, in.ManuallyTracked)
	if !ok {
		return time.Time{}
	}

	var trigger time.Time
	consider := func(candidate time.Time) {
		if trigger.IsZero() || candidate.Before(trigger) {
			trigger = candidate
		}
	}
	if (in.NewExplicit > 0 && in.NewExplicit >= cfg.Threshold) || in.Rechecks > 0 {
		consider(in.Now)
	}
	if in.NewExplicit > 0 && in.OldestUnprocessedAt != nil {
		consider(in.OldestUnprocessedAt.Add(cfg.MaxWait))
	}
	if in.ReverifyContexts > 0 {
		if in.LastDeriveAt == nil {
			consider(in.Now)
		} else {
			consider(in.LastDeriveAt.Add(cfg.ReverifyInterval))
		}
	}
	if trigger.IsZero() {
		return time.Time{}
	}

	due := trigger
	if due.Before(in.Now) {
		due = in.Now
	}
	if in.LastDeriveAt != nil {
		if next := in.LastDeriveAt.Add(interval); due.Before(next) {
			due = next
		}
	}
	if in.NewExplicit > 0 && in.NewestEvidenceAt != nil {
		if quiet := in.NewestEvidenceAt.Add(cfg.Quiet); due.Before(quiet) {
			due = quiet
		}
	}
	return due
}

func (c MemoryConfig) intervalFor(tier int, manuallyTracked bool) (time.Duration, bool) {
	switch {
	case manuallyTracked || tier <= 1:
		return c.MinInterval, true
	case tier == 2:
		return c.Tier2Interval, true
	default:
		return 0, false
	}
}
