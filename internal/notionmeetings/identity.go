package notionmeetings

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

var ErrUnknownIdentity = errors.New("notion user unknown; set token_type=pat for personal tokens or configure user_id")

type Identity struct {
	UserID      string `json:"user_id"`
	Provenance  string `json:"provenance"`
	PrincipalID string `json:"principal_id"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// ResolveIdentity uses only the primary token's verified ownership or explicit configuration.
func ResolveIdentity(self *User, tokenType, configuredID string) (Identity, error) {
	if self == nil || self.Object != "user" || strings.TrimSpace(self.ID) == "" || (self.Type != "bot" && self.Type != "person") {
		return Identity{}, ErrUnknownIdentity
	}
	mode := strings.ToLower(strings.TrimSpace(tokenType))
	if mode != "" && mode != "pat" {
		return Identity{}, ErrUnknownIdentity
	}
	identity := Identity{UserID: strings.TrimSpace(configuredID), Provenance: "configured", PrincipalID: self.ID, WorkspaceID: self.Bot.WorkspaceID}
	if mode == "pat" {
		owner := self.Bot.Owner
		if self.Type != "bot" || owner.Type != "user" || strings.TrimSpace(owner.User.ID) == "" {
			return Identity{}, ErrUnknownIdentity
		}
		if identity.UserID != "" && identity.UserID != owner.User.ID {
			return Identity{}, fmt.Errorf("%w: configured user_id conflicts with verified PAT owner", ErrUnknownIdentity)
		}
		identity.UserID = owner.User.ID
		identity.Provenance = "pat_owner"
	}
	if identity.UserID == "" {
		return Identity{}, ErrUnknownIdentity
	}
	return identity, nil
}
func matchesHuman(m MeetingNote, id string) bool {
	if id == "" {
		return false
	}
	if m.CreatedBy.ID == id {
		return true
	}
	return slices.Contains(m.MeetingNotes.CalendarEvent.Attendees, id)
}
