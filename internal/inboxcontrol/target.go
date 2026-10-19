// Package inboxcontrol defines exact provider targets for daemon-owned inbox actions.
package inboxcontrol

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalid reports an incomplete or ambiguous inbox operation input.
var ErrInvalid = errors.New("invalid inbox request")

// Scope distinguishes one archived message from one provider chat.
type Scope string

const (
	ScopeMessage Scope = "message"
	ScopeChat    Scope = "chat"
)

// Target binds local archive identity to an exact provider account and item.
// Validate checks input shape only. Before provider access the daemon must
// resolve the source and item through Store, authorize that source, and verify
// the binding against live provider identity. Caller-supplied IDs are not proof
// of ownership. IMAP membership uses mailbox/UIDVALIDITY/UID, never Message-ID.
type Target struct {
	SourceID         int64  `json:"source_id"`
	SourceType       string `json:"source_type"`
	SourceIdentifier string `json:"source_identifier"`
	AccountID        string `json:"account_id"`
	Scope            Scope  `json:"scope"`
	ItemID           int64  `json:"item_id"`
	ProviderID       string `json:"provider_id"`
	Mailbox          string `json:"mailbox,omitempty"`
	UIDValidity      uint32 `json:"uidvalidity,omitzero" format:"int64" maximum:"4294967295"`
	UID              uint32 `json:"uid,omitzero" format:"int64" maximum:"4294967295"`
}

// Validate rejects missing identities and conflicting message/chat scope.
// Errors name fields without echoing account identifiers or provider input.
func (t Target) Validate() error {
	if t.SourceID <= 0 || t.ItemID <= 0 {
		return fmt.Errorf("%w: source_id and item_id must be positive", ErrInvalid)
	}
	for _, field := range []struct{ name, value string }{
		{"source_identifier", t.SourceIdentifier},
		{"account_id", t.AccountID},
		{"provider_id", t.ProviderID},
	} {
		if !validIdentity(field.value) {
			return fmt.Errorf("%w: %s must be nonblank UTF-8 without control characters and at most 4096 bytes", ErrInvalid, field.name)
		}
	}
	switch t.SourceType {
	case "gmail", "msmail", sourceTypeIMAP:
		if t.Scope != ScopeMessage {
			return fmt.Errorf("%w: mail requires message scope", ErrInvalid)
		}
	case "beeper":
		if t.Scope != ScopeChat {
			return fmt.Errorf("%w: Beeper requires chat scope", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unsupported source_type", ErrInvalid)
	}
	if t.SourceType == sourceTypeIMAP {
		if !validIdentity(t.Mailbox) || t.UIDValidity == 0 || t.UID == 0 {
			return fmt.Errorf("%w: IMAP requires exact mailbox, UIDVALIDITY and UID", ErrInvalid)
		}
	} else if t.Mailbox != "" || t.UIDValidity != 0 || t.UID != 0 {
		return fmt.Errorf("%w: mailbox and UID identity require an IMAP source", ErrInvalid)
	}
	return nil
}

func validIdentity(s string) bool {
	if len(s) > 4096 || !utf8.ValidString(s) || strings.TrimSpace(s) == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
