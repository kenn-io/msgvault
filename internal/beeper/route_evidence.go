package beeper

import (
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// beeperServiceNetworks maps Beeper bridge types to the service names that
// native importers use as their source type, so one route network filter
// selects the same service from either kind of source.
var beeperServiceNetworks = map[string]string{
	"discordgo": "discord",
	"slackgo":   "slack",
}

func routeEvidenceFor(ch *Chat, opts ImportOptions) store.MessagingRouteEvidence {
	e := store.MessagingRouteEvidence{
		ChatID: ch.ID, AccountID: ch.AccountID, ProviderType: ch.Type, NetworkLabel: ch.Network,
		ObservedAt: time.Now().UTC(), ParticipantIDs: []int64{}, SelfParticipantIDs: []int64{},
		MemberChatIDs: []string{}, MergedIntoChatID: ch.MergedIntoChatID,
	}
	if ch.Merge != nil {
		e.Merged = true
		e.MemberChatIDs = ch.Merge.ChatIDs
	}
	// Exact routing/account bindings are independent of labels and ID formats.
	if ch.AccountID != opts.AccountID {
		e.Failure = "account_binding_mismatch"
	}
	if a := opts.routeAccount; a != nil && a.AccountID == opts.AccountID {
		e.Network = routeNetwork(a)
	}
	if failure := accountRouteProofFailure(opts.routeAccount, opts.AccountID); e.Failure == "" {
		e.Failure = failure
	}
	if e.Network == "" && e.Failure == "" {
		e.Failure = "network_unverified"
	}
	return e
}

// routeNetwork returns the account's service network from its authoritative
// bridge type, or "" when the bridge does not name a usable network.
func routeNetwork(account *Account) string {
	if account.Bridge == nil {
		return ""
	}
	network := strings.ToLower(strings.TrimSpace(account.Bridge.Type))
	if service, ok := beeperServiceNetworks[network]; ok {
		network = service
	}
	query := store.PersonMessagingRouteQuery{PersonUID: "validation", Network: network}
	if network == "" || store.ValidatePersonMessagingRouteQuery(query) != nil {
		return ""
	}
	return network
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
	if routeNetwork(account) == "" {
		return "network_unverified"
	}
	switch account.Status {
	case "connected", "connecting", "backfilling":
		return ""
	default:
		return "account_not_connected"
	}
}
