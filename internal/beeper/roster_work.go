package beeper

import (
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"slices"
)

type rosterMemberWork struct {
	Inputs   string `json:"inputs"`
	Enriched bool   `json:"enriched,omitzero"`
}

type rosterWork struct {
	Version           int                         `json:"version"`
	MatchingFirst     bool                        `json:"matching_first,omitzero"`
	Signature         string                      `json:"signature"`
	Members           map[string]rosterMemberWork `json:"members"`
	MembershipApplied bool                        `json:"membership_applied,omitzero"`
	MatchingPending   bool                        `json:"matching_pending,omitzero"`
	DetailBacked      bool                        `json:"detail_backed,omitzero"`
}

func rosterSignature(ch *Chat, accountID string) string {
	type member struct {
		ID, Username, Phone, Email, Name, Native string
		Self, Admin                              bool
	}
	members := make([]member, 0, len(ch.Participants.Items))
	for _, p := range ch.Participants.Items {
		native, _ := providerNativeUserID(p.Raw)
		members = append(members, member{p.ID, p.Username, p.PhoneNumber, p.Email, p.FullName, native, p.IsSelf, p.IsAdmin})
	}
	slices.SortFunc(members, func(a, b member) int {
		x, _ := json.Marshal(a, json.Deterministic(true))
		y, _ := json.Marshal(b, json.Deterministic(true))
		return slices.Compare(x, y)
	})
	snapshot := struct {
		Account, ID, ChatAccount, Type, Network string
		Total                                   int
		HasMore                                 bool
		Members                                 []member
	}{accountID, ch.ID, ch.AccountID, ch.Type, ch.Network, ch.Participants.Total, ch.Participants.HasMore, members}
	data, _ := json.Marshal(snapshot, json.Deterministic(true))
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func rosterMemberSignature(u *User, ch *Chat, accountID, bridgePrefix string) string {
	native, _ := providerNativeUserID(u.Raw)
	projection := struct {
		ID, Username, Phone, Email, Name, Native, Network, Account, Bridge string
		Self                                                               bool
	}{u.ID, u.Username, u.PhoneNumber, u.Email, u.FullName, native, ch.Network, accountID, bridgePrefix, u.IsSelf}
	data, _ := json.Marshal(projection, json.Deterministic(true))
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
