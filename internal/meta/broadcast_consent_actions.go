package meta

import (
	"context"
	"crypto/rand"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

type UnsubscribeSelector struct{ ParticipantID, SubscriptionID int64 }
type UnsubscribeResult struct {
	Local      int    `json:"local"`
	ProviderOK bool   `json:"provider_ok"`
	Invalid    int    `json:"invalid"`
	Note       string `json:"note,omitempty"`
}

// Unsubscribe persists suppression before any provider operation. Provider
// failures never roll it back, and a disabled profile can still suppress locally.
func Unsubscribe(ctx context.Context, st *store.Store, api MarketingAPI, p BroadcastPolicy, sel UnsubscribeSelector) (UnsubscribeResult, error) {
	var out UnsubscribeResult
	if sel.ParticipantID < 0 || sel.SubscriptionID < 0 || (sel.ParticipantID == 0) == (sel.SubscriptionID == 0) {
		return out, errors.New("choose one participant or subscription")
	}
	source, err := st.GetSourceByTypeAndIdentifier(SourceTypeFacebookMessages, p.Source.Identifier())
	if err != nil {
		return out, errors.New("resolve Messenger broadcast source failed")
	}
	filter := store.SubscriptionFilter{}
	if sel.ParticipantID > 0 {
		filter.ParticipantIDs = []int64{sel.ParticipantID}
	} else {
		filter.SubscriptionIDs = []int64{sel.SubscriptionID}
	}
	subs, err := st.ListMarketingSubscriptionsContext(ctx, source.ID, filter)
	if err != nil {
		return out, err
	}
	if len(subs) == 0 {
		return out, errors.New("subscription_not_found")
	}
	ref := rand.Text()
	psids, tokens := []string{}, []string{}
	seen := map[string]bool{}
	for _, sub := range subs {
		if err = st.SetSubscriptionStatusContext(ctx, sub.ID, "unsubscribed", "local_unsubscribe", "owner", ref); err != nil {
			return out, err
		}
		out.Local++
		if sub.PSID.Valid && sub.PSID.String != "" {
			if !seen[sub.PSID.String] {
				psids = append(psids, sub.PSID.String)
				seen[sub.PSID.String] = true
			}
		} else {
			tokens = append(tokens, sub.Token)
		}
	}
	if !p.Enabled || !p.AllowSend || api == nil {
		out.Note = ErrSendsDisabled.Error()
		return out, nil
	}
	kind := "provider_unsubscribe_ok"
	var callErr error
	if len(psids) > 0 {
		out.Invalid, callErr = api.UnsubscribePSIDs(ctx, p.Source.PageID, psids)
	}
	if callErr == nil && len(tokens) > 0 {
		var invalid int
		invalid, callErr = api.UnsubscribeTokens(ctx, p.Source.PageID, tokens)
		out.Invalid += invalid
	}
	if callErr != nil {
		kind = "provider_unsubscribe_failed"
	}
	commit, cancel := broadcastCommitContext(ctx)
	defer cancel()
	for _, sub := range subs {
		if err = st.RecordConsentEventContext(commit, sub.ID, kind, "send_result", ref, nil); err != nil {
			return out, err
		}
	}
	if callErr != nil {
		return out, errors.New("provider_unsubscribe_failed")
	}
	out.ProviderOK = out.Invalid == 0
	return out, nil
}
