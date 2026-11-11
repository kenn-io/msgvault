package beeper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
)

type rosterMemberWork struct {
	Inputs   string `json:"inputs"`
	Enriched bool   `json:"enriched,omitzero"`
}

// rosterWork is one chat's roster progress in the sync state. Every sync run
// stores the whole state, so Members holds per-member progress only while
// matching is unfinished. Settled replaces it once every member at Signature
// is matched; a later roster change matches every member again.
type rosterWork struct {
	Version           int                         `json:"version"`
	MatchingFirst     bool                        `json:"matching_first,omitzero"`
	RefreshMatches    bool                        `json:"refresh_matches,omitzero"`
	Signature         string                      `json:"signature"`
	Members           map[string]rosterMemberWork `json:"members,omitempty"`
	Settled           bool                        `json:"settled,omitzero"`
	MembershipApplied bool                        `json:"membership_applied,omitzero"`
	MatchingPending   bool                        `json:"matching_pending,omitzero"`
	DetailBacked      bool                        `json:"detail_backed,omitzero"`
}

func (w *rosterWork) setSignature(signature string) {
	if w.Signature != signature {
		w.Signature = signature
		w.MembershipApplied = false
		w.RefreshMatches = true
		w.Settled = false
	}
}

// settle drops per-member progress once matching finished for the signature.
func (w *rosterWork) settle() {
	w.Members = map[string]rosterMemberWork{}
	w.Settled = true
	w.MatchingPending = false
}

// shortDigest keeps 128 bits of SHA-256, enough to detect a changed input
// while keeping the per-chat sync state small.
func shortDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
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
			work.Settled = false
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
	members := make([]jsontext.Value, 0, len(ch.Participants.Items))
	for _, p := range ch.Participants.Items {
		native, _ := providerNativeUserID(p.Raw)
		data, _ := json.Marshal(member{p.ID, p.Username, p.PhoneNumber, p.Email, p.FullName, native, p.IsSelf, p.IsAdmin}, json.Deterministic(true))
		members = append(members, data)
	}
	slices.SortFunc(members, func(a, b jsontext.Value) int { return bytes.Compare(a, b) })
	snapshot := struct {
		Account, ID, ChatAccount, Type, Network string
		Total                                   int
		HasMore                                 bool
		Members                                 []jsontext.Value
	}{accountID, ch.ID, ch.AccountID, ch.Type, ch.Network, ch.Participants.Total, ch.Participants.HasMore, members}
	data, _ := json.Marshal(snapshot, json.Deterministic(true))
	return shortDigest(data)
}

func rosterMemberSignature(u *User, ch *Chat, accountID, bridgePrefix string) string {
	native, _ := providerNativeUserID(u.Raw)
	projection := struct {
		ID, Username, Phone, Email, Name, Native, Network, Account, Bridge string
		Self                                                               bool
	}{u.ID, u.Username, u.PhoneNumber, u.Email, u.FullName, native, ch.Network, accountID, bridgePrefix, u.IsSelf}
	data, _ := json.Marshal(projection, json.Deterministic(true))
	return shortDigest(data)
}
