// Package keepintouch checks existing contact cadence and prepares reviewable
// person-linked Kata reminders. It never sends messages.
package keepintouch

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/cronexpr"
)

// Config has separate per-invocation caps. It does not inherit the sweep
// worker's durable daily token or cost budgets.
type Config struct {
	Enabled         bool   `toml:"enabled"`
	Schedule        string `toml:"schedule"`
	Owner           string `toml:"owner"`
	ApproachDays    int    `toml:"approach_days"`
	Limit           int    `toml:"limit"`
	MaxDraftsPerRun int    `toml:"max_drafts_per_run"`
}

func DefaultConfig() Config {
	return Config{Schedule: "0 9 * * *", ApproachDays: 7, Limit: 20, MaxDraftsPerRun: 20}
}

func (c Config) Validate() error {
	if c.ApproachDays < 0 || c.ApproachDays > 3660 || c.Limit < 1 || c.Limit > 100 || c.MaxDraftsPerRun < 1 || c.MaxDraftsPerRun > 100 {
		return errors.New("keep-in-touch requires approach_days 0..3660, limit and max_drafts_per_run 1..100")
	}
	if _, err := cronexpr.Parser.Parse(c.Schedule); err != nil {
		return fmt.Errorf("parse keep-in-touch schedule: %w", err)
	}
	if c.Owner != "" && (strings.TrimSpace(c.Owner) != c.Owner || len(c.Owner) > 128 || !utf8.ValidString(c.Owner) || strings.ContainsFunc(c.Owner, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })) {
		return errors.New("keep-in-touch owner must be a valid Kata inbox recipient")
	}
	if c.Enabled && c.Owner == "" {
		return errors.New("keep-in-touch requires an explicit Kata owner")
	}
	return nil
}
