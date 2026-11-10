package beeper

import (
	"context"
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
	RefreshMatches    bool                        `json:"refresh_matches,omitzero"`
	Signature         string                      `json:"signature"`
	Members           map[string]rosterMemberWork `json:"members"`
	MembershipApplied bool                        `json:"membership_applied,omitzero"`
	MatchingPending   bool                        `json:"matching_pending,omitzero"`
	DetailBacked      bool                        `json:"detail_backed,omitzero"`
}

func (w *rosterWork) setSignature(signature string) {
	if w.Signature != signature {
		w.Signature = signature
		w.MembershipApplied = false
		w.RefreshMatches = true
	}
}

func (imp *Importer) completeRoster(ctx context.Context, convID int64, complete bool, members []resolvedMember, work *rosterWork) error {
	work.MembershipApplied = complete
	if !complete || !work.RefreshMatches {
		return nil
	}
	ids, err := imp.store.ConversationCandidateParticipantsContext(ctx, convID)
	if err != nil {
		work.MembershipApplied = false
		return err
	}
	for _, member := range members {
		if ids[member.participantID] {
			progress := work.Members[member.key]
			progress.Enriched = false
			work.Members[member.key] = progress
			work.MatchingPending = true
		}
	}
	if len(ids) > 0 {
		imp.obs = newObservationRecorder(imp.store)
		imp.matcher = newIdentityMatcher(imp.store)
	}
	work.RefreshMatches = false
	return nil
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
