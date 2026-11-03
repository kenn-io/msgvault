package meta

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type SubscriptionSyncSummary struct {
	WalkID                   int64
	Listed, Absent, Requests int
	Complete                 bool
}

// SyncSubscriptions always starts at the beginning of the token list. Its walk
// records are independent of inbox sync runs and never change a DM checkpoint.
// DMSource avoids a dependency from the provider package back into config.
func SyncSubscriptions(ctx context.Context, st *store.Store, api MarketingAPI, src DMSource, maxRequests int) (summary SubscriptionSyncSummary, retErr error) {
	if src.Platform != PlatformMessenger || !ValidAccountID(src.AppID) || !ValidAccountID(src.PageID) {
		return summary, errors.New("invalid Messenger broadcast source")
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	source, err := st.GetSourceByTypeAndIdentifier(SourceTypeFacebookMessages, src.Identifier())
	if errors.Is(err, store.ErrSourceNotFound) {
		return summary, fmt.Errorf("register the Messenger source for app %s Page %s first", src.AppID, src.PageID)
	}
	if err != nil {
		return summary, errors.New("resolve Messenger broadcast source failed")
	}
	if maxRequests <= 0 {
		maxRequests = 1000
	}
	summary.WalkID, err = st.StartListWalkContext(ctx, source.ID, time.Now().UTC())
	if err != nil {
		return summary, err
	}
	finished := false
	category := ""
	defer func() {
		if finished {
			return
		}
		// Cancellation must still close the local walk; only this bounded DB
		// cleanup outlives the caller. No provider work uses this context.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := st.FinishListWalkContext(cleanup, summary.WalkID, "partial", summary.Requests, summary.Listed, category); err != nil {
			retErr = errors.Join(retErr, errors.New("finish partial marketing list walk failed"))
		}
	}()
	seenTokens, seenCursors := map[string]bool{}, map[string]bool{}
	after := ""
	for summary.Requests < maxRequests {
		if err := ctx.Err(); err != nil {
			category = "cancelled"
			return summary, err
		}
		summary.Requests++
		page, err := api.SubscriptionTokens(ctx, src.PageID, after)
		if err != nil {
			category = string(ClassifyMarketingError(err, false).Category)
			if ctx.Err() != nil {
				category = "cancelled"
				return summary, ctx.Err()
			}
			return summary, fmt.Errorf("marketing subscription list failed: %s", category)
		}
		if len(page.Data) > 1000 {
			category = "invalid_response"
			return summary, errors.New("invalid marketing subscription page")
		}
		for _, token := range page.Data {
			if token.Token == "" || (token.PSID != "" && !ValidAccountID(token.PSID)) {
				category = "invalid_response"
				return summary, errors.New("invalid marketing subscription")
			}
		}
		if page.After != "" {
			if seenCursors[page.After] || page.After == after {
				category = "invalid_response"
				return summary, errors.New("invalid marketing pagination progress")
			}
			for token := range seenTokens {
				if strings.Contains(page.After, token) || (after != "" && strings.Contains(after, token)) {
					category = "invalid_response"
					return summary, errors.New("invalid marketing cursor")
				}
			}
			for _, token := range page.Data {
				if strings.Contains(page.After, token.Token) || (after != "" && strings.Contains(after, token.Token)) {
					category = "invalid_response"
					return summary, errors.New("invalid marketing cursor")
				}
			}
			seenCursors[page.After] = true
		}
		for _, token := range page.Data {
			if seenTokens[token.Token] {
				continue
			}
			participant, err := ResolveDMActor(ctx, st, source.ID, src, token.PSID, "")
			if err != nil {
				category = "store_failed"
				return summary, errors.New("resolve marketing subscriber failed")
			}
			_, err = st.UpsertListedSubscriptionContext(ctx, source.ID, summary.WalkID, store.ListedSubscription{
				Token: token.Token, PSID: token.PSID, TopicTitle: token.TopicTitle, Timezone: token.Timezone, Reoptin: token.Reoptin,
				ParticipantID: sql.NullInt64{Int64: participant, Valid: participant > 0}, Created: token.Created, Expiry: token.Expiry, NextEligible: token.NextEligible, At: time.Now().UTC(),
			})
			if err != nil {
				category = "store_failed"
				return summary, errors.New("persist marketing subscriber failed")
			}
			summary.Listed++
			seenTokens[token.Token] = true
		}
		if page.After == "" {
			summary.Absent, err = st.FinishListWalkContext(ctx, summary.WalkID, "complete", summary.Requests, summary.Listed, "")
			if err != nil {
				category = "store_failed"
				return summary, errors.New("finish marketing list walk failed")
			}
			finished = true
			summary.Complete = true
			return summary, nil
		}
		after = page.After
	}
	category = "budget_exhausted"
	return summary, nil
}
