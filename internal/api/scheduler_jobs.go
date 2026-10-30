package api

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/granola"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/meetingimport"
	"go.kenn.io/msgvault/internal/muesli"
	"go.kenn.io/msgvault/internal/notionmeetings"
	"go.kenn.io/msgvault/internal/omi"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/pocket"
	"go.kenn.io/msgvault/internal/synctechsms"
	"go.kenn.io/msgvault/internal/twenty"
	"go.kenn.io/msgvault/internal/twilio"
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
	sourceTypeMatrix = "matrix"
	sourceTypeSlack  = "slack"
	sourceTypeInline = "inline"
)

// BeeperJobName is the single generic-job name that drives every beeper
// store source. cmd/msgvault/cmd/attachment_maintenance.go registers the
// beeper sync job under this exact name.
const BeeperJobName = sourceTypeBeeper

// MatrixJobName is the singleton scheduler job for all Matrix accounts.
const MatrixJobName = sourceTypeMatrix

// SlackJobName is the single generic-job name that drives the configured
// Slack workspace source.
const SlackJobName = sourceTypeSlack

// InlineJobName drives every configured Inline account and its chat selection.
const InlineJobName = sourceTypeInline

// IMessageJobName drives the selected Apple Messages source.
const IMessageJobName = "apple-messages"

// WhatsAppAppleJobName identifies a native WhatsApp store by its owner phone.
func WhatsAppAppleJobName(phone string) string {
	return "whatsapp:" + phone
}

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
	case "whatsapp":
		return WhatsAppAppleJobName(identifier), true
	case "apple_messages":
		return IMessageJobName, true
	case line.SourceType:
		return "line:" + identifier, identifier != ""
