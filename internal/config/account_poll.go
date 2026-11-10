package config

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/cronexpr"
)

// ValidatePollingAccounts checks settings used by enabled polling accounts.
func ValidatePollingAccounts(accounts []AccountSchedule) error {
	seen := make(map[string]bool)
	for _, account := range accounts {
		if !account.Enabled || account.PollInterval == 0 {
			continue
		}
		if account.PollInterval < 15*time.Second || account.PollInterval > 120*time.Second || account.PollInterval%time.Second != 0 {
			return fmt.Errorf("account %q: poll_interval must be whole seconds between 15s and 120s", account.Email)
		}
		key := strings.ToLower(account.Email)
		if seen[key] {
			return fmt.Errorf("duplicate polling account %q", account.Email)
		}
		seen[key] = true
		for field := range strings.FieldsSeq(account.Schedule) {
			if strings.Trim(field, ",") == "" {
				return fmt.Errorf("account %q: invalid cron expression", account.Email)
			}
		}
		if _, err := cronexpr.Parser.Parse(account.Schedule); err != nil {
			return fmt.Errorf("account %q: invalid cron expression: %w", account.Email, err)
		}
	}
	return nil
}
