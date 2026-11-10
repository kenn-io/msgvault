// Package googlechat imports explicitly supplied, extracted Google Chat Takeout parts.
package googlechat

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/apiimport"
)

const SourceType = "google_chat"

// ImportOptions never defaults to a provider account or a user data directory.
type ImportOptions struct {
	Me             string
	Roots          []string
	AttachmentsDir string
}

type ImportSummary struct {
	SourceID                                                                            int64
	Duration                                                                            time.Duration
	Conversations, MembershipWithoutMessages, MessagesSupplied                          int
	Duplicates, Unsupported, DatesUnparsed, UnknownSenders, UnknownMembers              int
	AttachmentsStored, AttachmentsMissing, AttachmentsUnsupported, AttachmentsOversized int
	MessagesAdded                                                                       int64
}

type person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type message struct {
	ID      string         `json:"message_id"`
	Topic   string         `json:"topic_id"`
	Creator person         `json:"creator"`
	Created string         `json:"created_date"`
	Text    string         `json:"text"`
	Files   []attachedFile `json:"attached_files"`
}

type attachedFile struct {
	Original string `json:"original_name"`
	Export   string `json:"export_name"`
}

type groupInfo struct {
	Name    string   `json:"name"`
	Members []person `json:"members"`
}

type userInfo struct {
	User        person `json:"user"`
	Memberships []struct {
		Group string `json:"group_id"`
		State string `json:"membership_state"`
	} `json:"membership_info"`
}

// NormalizeOwner validates a bare account email for both CLI and package callers.
func NormalizeOwner(value string) (string, error) {
	return apiimport.NormalizeEmail("googlechat: --me", value)
}

func parseDate(value string) time.Time {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\u202f", " "), "\u00a0", " ")
	// Only the observed English UTC representation is interpreted. Other zone
	// names have no trustworthy offset and remain available in the raw record.
	if !strings.HasSuffix(value, " UTC") {
		return time.Time{}
	}
	parsed, err := time.Parse("Monday, January 2, 2006 at 3:04:05 PM MST", value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
