package api

import (
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/synctechsms"
)

type sourceScheduleKind uint8

const (
	sourceScheduleNonSchedulable sourceScheduleKind = iota
	sourceScheduleAccount
	sourceScheduleGeneric
)

type sourceScheduleClassification struct {
	kind    sourceScheduleKind
	jobName string
}

// sourceTypeBeeper mirrors the unexported sourceTypeBeeper constant in
// internal/beeper (and cmd/msgvault/cmd/constants.go); it can't be imported
// because it isn't exported, so the literal is duplicated here.
const (
	sourceTypeBeeper = "beeper"
	sourceTypeGmail  = "gmail"
	sourceTypeSlack  = "slack"
)

// BeeperJobName is the single generic-job name that drives every beeper
// store source. cmd/msgvault/cmd/attachment_maintenance.go registers the
// beeper sync job under this exact name.
const BeeperJobName = sourceTypeBeeper

// SlackJobName is the single generic-job name that drives the configured
// Slack workspace source.
const SlackJobName = sourceTypeSlack

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

// classifySourceScheduling determines which scheduler, if any, may operate a
// store source. Account scheduling is opt-in so imported or unknown source
// types cannot borrow a scheduled account merely by sharing its identifier.
func classifySourceScheduling(sourceType, identifier string) sourceScheduleClassification {
	switch sourceType {
	case "", sourceTypeGmail, "imap", "msmail", "teams", "discord":
		return sourceScheduleClassification{kind: sourceScheduleAccount}
	case meetingimport.SourceType:
		return sourceScheduleClassification{kind: sourceScheduleNonSchedulable}
	default:
		jobName, ok := SchedulerJobNameForSource(sourceType, identifier)
		if !ok {
			return sourceScheduleClassification{kind: sourceScheduleNonSchedulable}
		}
		return sourceScheduleClassification{
			kind:    sourceScheduleGeneric,
			jobName: jobName,
		}
	}
}

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
	case chatwoot.SourceType:
		inboxSeparator := strings.LastIndex(identifier, "/inboxes/")
		if inboxSeparator < 0 {
			return "", false
		}
		accountScope, inbox := identifier[:inboxSeparator], identifier[inboxSeparator+len("/inboxes/"):]
		if strings.Contains(inbox, "/") {
			return "", false
		}
		inboxID, err := strconv.ParseInt(inbox, 10, 64)
		if err != nil || inboxID <= 0 {
			return "", false
		}
		separator := strings.LastIndex(accountScope, "/accounts/")
		if separator < 0 {
			return "", false
		}
		baseURL := accountScope[:separator]
		accountID, err := strconv.ParseInt(accountScope[separator+len("/accounts/"):], 10, 64)
		if err != nil || identifier != chatwoot.SourceIdentifier(baseURL, accountID, inboxID) {
			return "", false
		}
		return ChatwootJobNameForAccount(baseURL, accountID)
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
	case plaud.SourceType:
		return "plaud:" + identifier, true
	case circleback.SourceType:
		// Store identifier == config Identifier (see
		// internal/circleback/importer.go GetOrCreateSource call).
		return "circleback:" + identifier, true
	case notionmeetings.SourceType:
		return "notion-meetings:" + identifier, true
	case muesli.SourceType:
		// Store identifier == config Identifier (see
		// internal/muesli/importer.go GetSourceByTypeAndIdentifier call).
		return "muesli:" + identifier, true
	case sourceTypeBeeper:
		// One scheduler job syncs every beeper source (see
		// internal/beeper/importer.go GetOrCreateSource, one store source
		// per beeper AccountID, all driven by the singleton "beeper" job).
		return BeeperJobName, true
	case sourceTypeSlack:
		// One configured Slack workspace maps to one store source and one
		// singleton daemon job.
		return SlackJobName, true
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

// ChatwootJobNameForAccount groups every registered inbox of one account under
// the same stable job, independently of the local configuration label.
func ChatwootJobNameForAccount(baseURL string, accountID int64) (string, bool) {
	canonical, err := chatwoot.CanonicalURL(baseURL)
	if err != nil || accountID <= 0 {
		return "", false
	}
	return fmt.Sprintf("chatwoot:%s/accounts/%d", canonical, accountID), true
}
