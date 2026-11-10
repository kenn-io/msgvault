// Package googlechat imports explicitly supplied, extracted Google Chat Takeout parts.
package googlechat

import (
	"encoding/json/jsontext"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"time"
)

const SourceType = "google_chat"

// ImportOptions never defaults to a provider account or a user data directory.
type ImportOptions struct {
	Me             string
	Roots          []string
	AttachmentsDir string
	Logger         *slog.Logger
}

type ImportSummary struct {
	SourceID                                                                            int64
	Duration                                                                            time.Duration
	Conversations, MembershipWithoutMessages, MessagesSupplied                          int
	Duplicates, Unsupported, DatesUnparsed, UnknownSenders                              int
	AttachmentsStored, AttachmentsMissing, AttachmentsUnsupported, AttachmentsOversized int
	MessagesAdded                                                                       int64
}

type person struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	UserType string `json:"user_type"`
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

type provenance struct {
	Root string         `json:"root"`
	Path string         `json:"path"`
	Data jsontext.Value `json:"data,omitempty"`
}

// NormalizeOwner validates a bare account email for both CLI and package callers.
func NormalizeOwner(value string) (string, error) {
	value = strings.TrimSpace(value)
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || !strings.Contains(value, "@") {
		return "", errors.New("googlechat: --me must be a bare email address")
	}
	return strings.ToLower(value), nil
}

func parseDate(value string) time.Time {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\u202f", " "), "\u00a0", " ")
	// Only the observed English UTC representation is interpreted. Other zone
	// names have no trustworthy offset and remain available in the raw record.
	if !strings.HasSuffix(value, " UTC") {
		return time.Time{}
	}
	for _, layout := range []string{"Monday, January 2, 2006 at 3:04:05 PM MST", "Monday, January 2, 2006 at 03:04:05 PM MST"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
