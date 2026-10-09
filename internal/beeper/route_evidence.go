package beeper

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func routeEvidenceFor(ch *Chat, opts ImportOptions) store.MessagingRouteEvidence {
	e := store.MessagingRouteEvidence{ChatID: ch.ID, AccountID: ch.AccountID, ProviderType: ch.Type, NetworkLabel: ch.Network, ObservedAt: time.Now().UTC(), ParticipantIDs: []int64{}, SelfParticipantIDs: []int64{}, MemberChatIDs: []string{}, MergedIntoChatID: ch.MergedIntoChatID}
	if ch.Merge != nil {
		e.Merged = true
		e.MemberChatIDs = ch.Merge.ChatIDs
	}
	// Exact routing/account bindings are independent of labels and ID formats.
	if ch.AccountID != opts.AccountID {
		e.Failure = "account_binding_mismatch"
	}
	if a := opts.routeAccount; a != nil && a.AccountID == opts.AccountID && a.Bridge != nil {
		network := strings.ToLower(strings.TrimSpace(a.Bridge.Type))
		if store.ValidatePersonMessagingRouteQuery(store.PersonMessagingRouteQuery{PersonUID: "validation", Network: network}) == nil {
			e.Network = network
		}
	}
	if failure := accountRouteProofFailure(opts.routeAccount, opts.AccountID); e.Failure == "" {
		e.Failure = failure
	}
	if e.Network == "" && e.Failure == "" {
		e.Failure = "network_unverified"
	}
	return e
}

// accountRouteProofFailure classifies whether the account response provides
// authoritative bridge proof for the source. A successful HTTP response is
// insufficient when it names another account, has no usable bridge, or does
// not confirm an active connection.
func accountRouteProofFailure(account *Account, expectedAccountID string) string {
	if account == nil {
		return "account_lookup_failed"
	}
	if account.AccountID != expectedAccountID {
		return "account_binding_mismatch"
	}
	if account.Bridge == nil {
		return "network_unverified"
	}
	network := strings.ToLower(strings.TrimSpace(account.Bridge.Type))
	if network == "" || store.ValidatePersonMessagingRouteQuery(store.PersonMessagingRouteQuery{PersonUID: "validation", Network: network}) != nil {
		return "network_unverified"
	}
	switch account.Status {
	case "connected", "connecting", "backfilling":
		return ""
	default:
		return "account_not_connected"
	}
}
