// Package cronexpr holds the cron grammar msgvault accepts for every
// schedule: five fields (minute, hour, day of month, month, day of week),
// optionally preceded by a CRON_TZ=<zone> or TZ=<zone> prefix. Descriptors
// such as @hourly and extended syntax are not part of it. msgvault takes
// go-cron's rules and run times as they are and adds none of its own.
package cronexpr

import "github.com/netresearch/go-cron"

// Parser reads the shared grammar. DowOrDom keeps crontab's rule that
// restricting both day fields matches a day that satisfies either one.
var Parser = cron.MustNewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.DowOrDom)
