package config

import (
	"errors"
	"time"
)

// ValidatePollInterval checks an account's optional polling interval.
func ValidatePollInterval(account AccountSchedule) error {
	if account.PollInterval < 15*time.Second || account.PollInterval > 120*time.Second || account.PollInterval%time.Second != 0 {
		return errors.New("poll_interval must be whole seconds between 15s and 120s, such as \"30s\"")
	}
	if account.Enabled && account.Schedule == "" {
		return errors.New("poll_interval requires a schedule")
	}
	return nil
}
