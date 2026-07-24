package api

import (
	"strings"

	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/synctechsms"
)

// sourceTypeBeeper mirrors the unexported sourceTypeBeeper constant in
// internal/beeper (and cmd/msgvault/cmd/constants.go); it can't be imported
// because it isn't exported, so the literal is duplicated here.
const sourceTypeBeeper = "beeper"

// BeeperJobName is the single generic-job name that drives every beeper
// store source. cmd/msgvault/cmd/attachment_maintenance.go registers the
// beeper sync job under this exact name.
const BeeperJobName = sourceTypeBeeper

// SchedulerJobNameForSource returns the scheduler generic-job name that
// drives syncing for a store source of the given type and identifier, and
// whether such a job governs this source type at all. It is the single
// source of truth shared by daemon job registration
// (cmd/msgvault/cmd/serve.go) and source-status reporting (sourceStatus in
// handlers.go), so the two can never drift apart.
//
// Account-scheduler source types (gmail, imap, ...) return ("", false):
// they are governed by the account scheduler, not a generic job.
func SchedulerJobNameForSource(sourceType, identifier string) (string, bool) {
	switch sourceType {
	case synctechsms.SourceType:
		// Store identifier == config OwnerPhone (see
		// internal/synctechsms/importer.go GetOrCreateSource call).
		return "synctech-sms:" + identifier, true
	case gcal.SourceType:
		// Store identifier is "<accountEmail>/<calendarID>" (one store
		// source per calendar; see internal/calsync/calsync.go
		// sourceIdentifier). One scheduler job syncs every calendar for an
		// account, so the job name is keyed on the account-email prefix.
		email, _, found := strings.Cut(identifier, "/")
		if !found || email == "" {
			return "", false
		}
		return gcalJobName(email), true
	case granola.SourceType:
		// Store identifier == config Identifier (see
		// internal/granola/importer.go GetOrCreateSource call).
		return "granola:" + identifier, true
	case circleback.SourceType:
		// Store identifier == config Identifier (see
		// internal/circleback/importer.go GetOrCreateSource call).
		return "circleback:" + identifier, true
	case sourceTypeBeeper:
		// One scheduler job syncs every beeper source (see
		// internal/beeper/importer.go GetOrCreateSource, one store source
		// per beeper AccountID, all driven by the singleton "beeper" job).
		return BeeperJobName, true
	default:
		return "", false
	}
}

// gcalJobName builds the scheduler job name for a gcal account from its
// normalized account email. Both SchedulerJobNameForSource (deriving the
// email from a calendar's store identifier) and GCalJobNameForAccountEmail
// (the registration-side entry point used by serve.go, which only has the
// account email) route through this single builder so the two can't drift.
func gcalJobName(normalizedEmail string) string {
	return "gcal:" + normalizedEmail
}

// GCalJobNameForAccountEmail returns the scheduler job name for the gcal
// account with the given (already normalized) email. serve.go registers one
// job per configured [[gcal]] account — covering every calendar under that
// account — under this exact name.
func GCalJobNameForAccountEmail(normalizedEmail string) string {
	return gcalJobName(normalizedEmail)
}
