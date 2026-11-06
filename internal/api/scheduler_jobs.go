package api

import (
	"strings"

	"go.kenn.io/msgvault/internal/sourcescheduling"
)

// sourceTypeBeeper mirrors the unexported sourceTypeBeeper constant in
// internal/beeper (and cmd/msgvault/cmd/constants.go); it can't be imported
// because it isn't exported, so the literal is duplicated here.
const (
	sourceTypeBeeper = "beeper"
	sourceTypeGmail  = "gmail"
	sourceTypeMatrix = "matrix"
	sourceTypeSlack  = "slack"
	sourceTypeInline = "inline"
)

// CardDAVJobName is the scheduler identity for the default CardDAV connection.
const CardDAVJobName = "carddav"

// CardDAVJobNameForConnection keeps the legacy default job and gives named
// connections independent scheduler identities. Names are validated on setup.
func CardDAVJobNameForConnection(name string) string {
	if name == "" || name == "default" {
		return CardDAVJobName
	}
	return CardDAVJobName + ":" + name
}

// isSourceSyncJobName reports whether a generic scheduler job syncs a source.
func isSourceSyncJobName(name string) bool {
	kind, _, _ := strings.Cut(name, ":")
	if kind == CardDAVJobName {
		return true
	}
	_, ok := sourcescheduling.SchedulerJobNameForSource(strings.ReplaceAll(kind, "-", "_"), "source/calendar")
	return ok
}
