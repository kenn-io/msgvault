package config

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/cronexpr"
)

// ValidateAccountPollInterval accepts zero (off) or whole seconds from 15s to 120s.
func ValidateAccountPollInterval(interval time.Duration) error {
	if interval == 0 {
		return nil
	}
	if interval < 15*time.Second || interval > 120*time.Second || interval%time.Second != 0 {
		return fmt.Errorf("poll_interval must be zero or whole seconds between 15s and 120s")
	}
	return nil
}

// ValidateAccountPolling keeps each rapid account on one existing cron lane.
// Ordinary accounts retain their existing schedule validation and dispatch.
func (c *Config) ValidateAccountPolling() error {
	enabled := make(map[string]int)
	polling := make(map[string]bool)
	for _, account := range c.Accounts {
		if err := ValidateAccountPollInterval(account.PollInterval); err != nil {
			return err
		}
		if !account.Enabled {
			continue
		}
		enabled[account.Email]++
		if account.PollInterval == 0 {
			continue
		}
		polling[account.Email] = true
		if strings.TrimSpace(account.Email) == "" {
			return fmt.Errorf("poll_interval requires a canonical account identifier")
		}
		if _, err := cronexpr.Parser.Parse(account.Schedule); err != nil {
			return fmt.Errorf("poll_interval requires a valid five-field schedule: %w", err)
		}
	}
	for key := range polling {
		if enabled[key] > 1 {
			return fmt.Errorf("duplicate enabled account key with poll_interval")
		}
	}
	return nil
}
