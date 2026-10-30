package peoplesweep

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDerivationDueAt(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	at := func(delta time.Duration) *time.Time {
		value := now.Add(delta)
		return &value
	}
	day := 24 * time.Hour
	type dueCase struct {
		name                            string
		input                           DueInput
		disabled, ineligible, automatic bool
		now, want                       time.Time
	}
	cases := []dueCase{
		{name: "derive disabled", input: DueInput{NewExplicit: 5}, disabled: true},
		{name: "not eligible", input: DueInput{NewExplicit: 5}, ineligible: true},
		{name: "journal gap blocks even manual", input: DueInput{JournalGap: true, ManualRequested: true}},
		{name: "nothing pending"},
		{name: "threshold met", input: DueInput{NewExplicit: 5, OldestUnprocessedAt: at(-day), NewestEvidenceAt: at(-7 * time.Hour)}, want: now},
		{name: "below threshold waits for max wait", input: DueInput{NewExplicit: 4, OldestUnprocessedAt: at(-day), NewestEvidenceAt: at(-7 * time.Hour)}, want: now.Add(13 * day)},
		{name: "max wait elapsed", input: DueInput{NewExplicit: 1, OldestUnprocessedAt: at(-15 * day), NewestEvidenceAt: at(-15 * day)}, want: now},
		{name: "quiet holds", input: DueInput{NewExplicit: 5, NewestEvidenceAt: at(-time.Hour)}, want: now.Add(5 * time.Hour)},
		{name: "persisted evidence gives a fixed deadline", input: DueInput{NewExplicit: 5, OldestUnprocessedAt: at(-time.Hour), NewestEvidenceAt: at(-time.Hour)}, want: now.Add(5 * time.Hour)},
		{name: "repeated evaluation at the quiet deadline is due", input: DueInput{NewExplicit: 5, OldestUnprocessedAt: at(-time.Hour), NewestEvidenceAt: at(-time.Hour)}, now: now.Add(5 * time.Hour), want: now.Add(5 * time.Hour)},
		{name: "future evidence stays unclamped", input: DueInput{NewExplicit: 5, NewestEvidenceAt: at(30 * day)}, want: now.Add(30*day + 6*time.Hour)},
		{name: "future evidence deadline remains stable", input: DueInput{NewExplicit: 5, NewestEvidenceAt: at(30 * day)}, now: now.Add(30*day + 6*time.Hour), want: now.Add(30*day + 6*time.Hour)},
		{name: "owner rows carry no evidence time", input: DueInput{NewExplicit: 5}, want: now},
		{name: "min interval tier 1", input: DueInput{NewExplicit: 5, Tier: 1, LastDeriveAt: at(-2 * time.Hour)}, want: now.Add(18 * time.Hour)},
		{name: "min interval automatic tier 1", input: DueInput{NewExplicit: 5, Tier: 1, LastDeriveAt: at(-2 * time.Hour)}, automatic: true, want: now.Add(18 * time.Hour)},
		{name: "min interval tier 2", input: DueInput{NewExplicit: 5, Tier: 2, LastDeriveAt: at(-2 * time.Hour)}, automatic: true, want: now.Add(70 * time.Hour)},
		{name: "tier 3 never auto derives", input: DueInput{NewExplicit: 5, Tier: 3}, automatic: true},
		{name: "manual provenance at tier 3 uses 20h", input: DueInput{NewExplicit: 5, Tier: 3, LastDeriveAt: at(-2 * time.Hour)}, want: now.Add(18 * time.Hour)},
		{name: "recheck pending", input: DueInput{Rechecks: 1, LastDeriveAt: at(-2 * time.Hour)}, want: now.Add(18 * time.Hour)},
		{name: "recheck waits for quiet", input: DueInput{Rechecks: 1, NewExplicit: 1, OldestUnprocessedAt: at(-time.Hour), NewestEvidenceAt: at(-time.Hour)}, want: now.Add(5 * time.Hour)},
		{name: "reverify after 30 days", input: DueInput{ReverifyContexts: 2, LastDeriveAt: at(-31 * day)}, want: now},
		{name: "reverify before 30 days", input: DueInput{ReverifyContexts: 2, LastDeriveAt: at(-10 * day)}, want: now.Add(20 * day)},
		{name: "never derived reverify", input: DueInput{ReverifyContexts: 1}, want: now},
		{name: "manual request bypasses interval and quiet", input: DueInput{ManualRequested: true, NewExplicit: 1, LastDeriveAt: at(-time.Hour), NewestEvidenceAt: at(-time.Minute)}, want: now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultMemoryConfig()
			enabled := true
			if !tc.disabled {
				cfg.Derive = &enabled
			}
			in := tc.input
			in.Now = now
			if !tc.now.IsZero() {
				in.Now = tc.now
			}
			in.Eligible = !tc.ineligible
			in.ManuallyTracked = !tc.automatic
			got := DerivationDueAt(in, cfg)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDerivationDueAtInvalidThresholdWithoutPendingEvidence(t *testing.T) {
	for _, threshold := range []int{0, -1} {
		t.Run(fmt.Sprintf("threshold_%d", threshold), func(t *testing.T) {
			cfg := DefaultMemoryConfig()
			enabled := true
			cfg.Derive = &enabled
			cfg.Threshold = threshold
			in := DueInput{Now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), Eligible: true, ManuallyTracked: true}
			assert.Equal(t, time.Time{}, DerivationDueAt(in, cfg))
		})
	}
}

func TestDefaultMemoryConfigRequiresOptIn(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultMemoryConfig()
	assert.Nil(cfg.Derive)
	assert.False(cfg.DeriveEnabled())
	value := false
	cfg.Derive = &value
	assert.False(cfg.DeriveEnabled())
	value = true
	assert.True(cfg.DeriveEnabled())
}

func TestMemoryConfigValidate(t *testing.T) {
	require.NoError(t, DefaultMemoryConfig().Validate())
	for _, tc := range []struct {
		name   string
		change func(*MemoryConfig)
	}{
		{"threshold", func(c *MemoryConfig) { c.Threshold = 0 }},
		{"max_wait", func(c *MemoryConfig) { c.MaxWait = 0 }},
		{"quiet", func(c *MemoryConfig) { c.Quiet = 0 }},
		{"min_interval", func(c *MemoryConfig) { c.MinInterval = 0 }},
		{"tier2_interval", func(c *MemoryConfig) { c.Tier2Interval = -time.Hour }},
		{"reverify_interval", func(c *MemoryConfig) { c.ReverifyInterval = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultMemoryConfig()
			tc.change(&cfg)
			assert.ErrorContains(t, cfg.Validate(), "invalid [people.sweep.memory] "+tc.name+": must be positive")
		})
	}
}
