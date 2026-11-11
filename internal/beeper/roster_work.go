package beeper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

const rosterMatchFailed = "failed"

type rosterWork struct {
	Phase        string `json:"phase,omitempty"`
	Next         int    `json:"next,omitzero"`
	MemberFirst  bool   `json:"member_first,omitzero"`
	Verified     bool   `json:"verified,omitzero"`
	Version      int    `json:"version"`
	Signature    string `json:"signature"`
	DetailBacked bool   `json:"detail_backed,omitzero"`
}

// shortDigest keeps 128 bits of SHA-256, enough to detect a changed input
// while keeping the per-chat sync state small.
func shortDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
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

func rosterProvider(chatID string) string {
	return fmt.Sprintf("beeper-roster:%x", sha256.Sum256([]byte(chatID)))
}

// matchRosterMembers records failures separately from the resumable scan.
func (imp *Importer) matchRosterMembers(ctx context.Context, sourceID int64, provider string, detail *Chat, items []Participant, opts ImportOptions, bridgePrefix string, work *rosterWork, sum *ImportSummary) error {
	receipts, err := imp.store.BeeperRosterReceiptsContext(ctx, sourceID, provider)
	if err != nil {
		return err
	}
	for i := work.Next; i < len(items); i++ {
		p := &items[i]
		key := imp.res.identifierForUser(p.ID)
		receipt, ok := receipts[key]
		if !ok || (work.Phase == "matching_retry" && receipt.Status != rosterMatchFailed) {
			work.Next = i + 1
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.stopRequested() {
			return errBeeperBudgetExpired
		}
		owners, err := imp.store.BeeperRosterOwnersContext(ctx, []string{key})
		if err != nil {
			return err
		}
		before := sum.Errors
		imp.captureObservations(ctx, owners[key], &p.User, detail, sourceID, opts.AccountID, bridgePrefix, sum)
		if err := ctx.Err(); err != nil {
			return err
		}
		status := "resolved"
		if sum.Errors != before {
			status = rosterMatchFailed
		}
		if receipt.Status != status {
			if err := imp.store.SetBeeperRosterMatchFailureContext(ctx, sourceID, provider, key, receipt.Checksum, status); err != nil {
				return err
			}
			receipt.Status = status
			receipts[key] = receipt
		}
		work.Next = i + 1
	}
	for i := range items {
		if receipts[imp.res.identifierForUser(items[i].ID)].Status == rosterMatchFailed {
			work.Phase = "matching_retry"
			work.Next = 0
			return nil
		}
	}
	if !work.Verified {
		work.Phase = "members"
		work.Next = len(items)
	} else {
		work.Phase = "cleanup"
	}
	return nil
}

// cleanRosterReceipts discards departed members in bounded transactions.
func (imp *Importer) cleanRosterReceipts(ctx context.Context, sourceID int64, provider string, items []Participant, opts ImportOptions) error {
	receipts, err := imp.store.BeeperRosterReceiptsContext(ctx, sourceID, provider)
	if err != nil {
		return err
	}
	for i := range items {
		delete(receipts, imp.res.identifierForUser(items[i].ID))
	}
	keys := make([]string, 0, len(receipts))
	for key := range receipts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for len(keys) > 0 {
		if opts.stopRequested() {
			return errBeeperBudgetExpired
		}
		n := min(16, len(keys))
		if err := imp.store.DeleteBeeperRosterReceiptsContext(ctx, sourceID, provider, keys[:n]); err != nil {
			return err
		}
		keys = keys[n:]
	}
	return nil
}

// replaceRosterMembership repairs changed owners before removing stale members.
func (imp *Importer) replaceRosterMembership(ctx context.Context, sourceID, convID int64, provider string, keys []string, items []Participant, opts ImportOptions) (map[string]int64, error) {
	owners, err := imp.store.BeeperRosterOwnersContext(ctx, keys)
	if err != nil {
		return nil, err
	}
	existing, err := imp.store.ConversationParticipantsContext(ctx, convID)
	if err != nil {
		return nil, err
	}
	present := make(map[int64]bool, len(existing))
	for _, member := range existing {
		present[member.ParticipantID] = true
	}
	for i, key := range keys {
		id := owners[key]
		if id != 0 && present[id] {
			continue
		}
		if opts.stopRequested() {
			return nil, errBeeperBudgetExpired
		}
		if id == 0 {
			id, err = imp.res.resolveUser(ctx, &items[i].User)
			if err != nil {
				return nil, err
			}
			if id == 0 {
				continue
			}
		}
		role := "member"
		if items[i].IsAdmin {
			role = "admin"
		}
		id, err = imp.store.ApplyBeeperRosterMemberContext(ctx, sourceID, convID, key, role, provider, "")
		if err != nil {
			return nil, err
		}
		owners[key] = id
		present[id] = true
	}
	desired := make(map[int64]bool, len(owners))
	for _, id := range owners {
		desired[id] = true
	}
	for _, member := range existing {
		if desired[member.ParticipantID] {
			continue
		}
		if opts.stopRequested() {
			return nil, errBeeperBudgetExpired
		}
		if err := imp.store.RemoveBeeperRosterMemberContext(ctx, convID, member.ParticipantID); err != nil {
			return nil, err
		}
	}
	return owners, nil
}

func (imp *Importer) resolveRosterMembers(ctx context.Context, sourceID, convID int64, provider string, detail *Chat, items []Participant, keys []string, opts ImportOptions, bridgePrefix string, work *rosterWork) error {
	receipts, err := imp.store.BeeperRosterReceiptsContext(ctx, sourceID, provider)
	if err != nil {
		return err
	}
	owners, err := imp.store.BeeperRosterOwnersContext(ctx, keys)
	if err != nil {
		return err
	}
	for i := range items {
		p := &items[i]
		key := keys[i]
		role := "member"
		if p.IsAdmin {
			role = "admin"
		}
		checksum := rosterMemberInputs(p, detail, opts.AccountID, bridgePrefix)
		if receipt, ok := receipts[key]; ok && owners[key] != 0 && receipt.Checksum == store.BeeperRosterReceiptChecksum(checksum, owners[key]) {
			imp.res.rememberRosterOwner(p.ID, owners[key])
			work.Next = i + 1
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if opts.stopRequested() {
			return errBeeperBudgetExpired
		}
		imp.res.forgetUser(p.ID)
		pid, err := imp.res.resolveUser(ctx, &p.User)
		if err != nil {
			return err
		}
		if pid != 0 {
			if _, err := imp.store.ApplyBeeperRosterMemberContext(ctx, sourceID, convID, key, role, provider, checksum); err != nil {
				return err
			}
		}
		work.Next = i + 1
	}
	return nil
}

// verifyRosterMembers rereads a complete roster after its member writes. A
// yield requested during the last write stops before the request, so the next
// visit skips the committed members and verifies.
func (imp *Importer) verifyRosterMembers(ctx context.Context, chatID string, complete bool, opts ImportOptions) (*Chat, error) {
	if !complete {
		return nil, errors.New("beeper roster is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.stopRequested() {
		return nil, errBeeperBudgetExpired
	}
	requestCtx, cancel := opts.requestContext(ctx)
	defer cancel()
	verified, err := imp.client.GetChat(requestCtx, chatID)
	return verified, opts.budgetError(ctx, err)
}

func (imp *Importer) rosterKeys(items []Participant) []string {
	keys := make([]string, len(items))
	for i := range items {
		keys[i] = imp.res.identifierForUser(items[i].ID)
	}
	return keys
}

func (imp *Importer) publishRosterEvidence(ctx context.Context, convID int64, evidence store.MessagingRouteEvidence, items []Participant, keys []string, owners map[string]int64, membership chatMembership) error {
	evidence.MembershipComplete = membership.known && len(owners) == membership.count
	for i, key := range keys {
		if id := owners[key]; id != 0 {
			evidence.ParticipantIDs = append(evidence.ParticipantIDs, id)
			if items[i].IsSelf {
				evidence.SelfParticipantIDs = append(evidence.SelfParticipantIDs, id)
			}
		}
	}
	return imp.store.SetConversationMessagingRouteEvidence(ctx, convID, evidence)
}

func (imp *Importer) rosterOwnersSettled(ctx context.Context, sourceID int64, provider string, detail *Chat, items []Participant, keys []string, owners map[string]int64, accountID string) (bool, error) {
	receipts, err := imp.store.BeeperRosterReceiptsContext(ctx, sourceID, provider)
	if err != nil {
		return false, err
	}
	bridgePrefix := ""
	if !detail.Participants.HasMore {
		bridgePrefix = participantBridgePrefix(detail.Participants.Items)
	}
	for i, key := range keys {
		inputs := rosterMemberInputs(&items[i], detail, accountID, bridgePrefix)
		if owners[key] == 0 || receipts[key].Checksum != store.BeeperRosterReceiptChecksum(inputs, owners[key]) {
			return false, nil
		}
	}
	return true, nil
}

func (imp *Importer) refreshRosterProof(ctx context.Context, sourceID, convID int64, provider string, detail *Chat, items []Participant, keys []string, accountID string, evidence store.MessagingRouteEvidence, membership chatMembership) (bool, error) {
	owners, err := imp.store.BeeperRosterOwnersContext(ctx, keys)
	if err != nil {
		return false, err
	}
	settled, err := imp.rosterOwnersSettled(ctx, sourceID, provider, detail, items, keys, owners, accountID)
	if err != nil || !settled {
		return false, err
	}
	return true, imp.publishRosterEvidence(ctx, convID, evidence, items, keys, owners, membership)
}

func rosterMemberInputs(p *Participant, detail *Chat, accountID, bridgePrefix string) string {
	role := "member"
	if p.IsAdmin {
		role = "admin"
	}
	return "v1:" + rosterMemberSignature(&p.User, detail, accountID, bridgePrefix) + ":" + role
}
