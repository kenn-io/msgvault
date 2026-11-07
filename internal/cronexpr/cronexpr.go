// Package cronexpr holds the cron grammar msgvault accepts for every
// schedule: five fields (minute, hour, day of month, month, day of week),
// optionally preceded by a CRON_TZ=<zone> or TZ=<zone> prefix. Descriptors
// such as @hourly and extended syntax are not part of it. msgvault takes
// go-cron's rules and run times as they are and adds none of its own.
package cronexpr

import (
	"fmt"
	"strings"

	"github.com/netresearch/go-cron"
)

// Parser reads the shared grammar. DowOrDom keeps crontab's rule that
// restricting both day fields matches a day that satisfies either one.
var Parser = cron.MustNewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.DowOrDom)

// Validate accepts the shared parser grammar, except comma-only fields which
// upstream currently accepts without matching any times. Remove this exception
// when the upstream parser rejects those fields itself.
func Validate(expr string) error {
	for field := range strings.FieldsSeq(expr) {
		if strings.Trim(field, ",") == "" {
			return fmt.Errorf("invalid cron expression: field %q lists no values", field)
		}
	}
	if _, err := Parser.Parse(expr); err != nil {
		return fmt.Errorf("invalid cron expression: %w", err)
	}
	return nil
}
