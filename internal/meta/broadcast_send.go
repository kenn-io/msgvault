package meta

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrSendsDisabled     = errors.New("broadcast_sends_disabled")
	ErrPlanChanged       = errors.New("plan_changed")
	ErrBudgetOverCap     = errors.New("budget_over_cap")
	ErrCampaignUnknown   = errors.New("campaign_unknown")
	ErrCampaignForeign   = errors.New("campaign_foreign")
	ErrCampaignWarming   = errors.New("campaign_warming")
	ErrPermissionMissing = errors.New("permission_missing")
)

// BroadcastPolicy is the validated, credential-free policy used by the sender.
// The daemon converts its configuration at the boundary to avoid a package cycle.
type BroadcastPolicy struct {
	Source              DMSource
	AdAccountID         string
	Enabled, AllowSend  bool
	MaxDailyBudgetCents int64
	PlanPolicy          PlanPolicy
}
type BroadcastPlan struct {
	ID               string              `json:"id"`
	Revision         int64               `json:"revision"`
	PlanDigest       string              `json:"plan_digest"`
	PageID           string              `json:"page_id"`
	DailyBudgetCents int64               `json:"daily_budget_cents"`
	WorstDailyCents  float64             `json:"worst_daily_cents"`
	WorstWeeklyCents float64             `json:"worst_weekly_cents"`
	CampaignEndsAt   time.Time           `json:"campaign_ends_at"`
	Eligible         int                 `json:"eligible"`
	Refused          int                 `json:"refused"`
	Recipients       []RecipientDecision `json:"recipients"`
}
type BroadcastSendRequest struct {
	ID         string
	Revision   int64
	PlanDigest string
	Limit      int
}
type RecipientResult struct {
	SubscriptionID int64  `json:"subscription_id"`
	State          string `json:"state"`
	Reason         string `json:"reason,omitempty"`
	Fingerprint    string `json:"token_fingerprint"`
	TrackingID     string `json:"tracking_id,omitempty"`
}
type BroadcastSendResult struct {
	ID                string            `json:"id"`
	State             string            `json:"state"`
	CampaignID        string            `json:"campaign_id,omitempty"`
	Accepted          int               `json:"accepted"`
	Failed            int               `json:"failed"`
	Unknown           int               `json:"unknown"`
	Refused           int               `json:"refused"`
	Remaining         int               `json:"remaining"`
	RetryAfterSeconds int64             `json:"retry_after_seconds,omitempty"`
	ErrorCode         string            `json:"error_code,omitempty"`
	Recipients        []RecipientResult `json:"recipients"`
}
type BroadcastSender struct {
	store  *store.Store
	api    MarketingAPI
	policy BroadcastPolicy
	now    func() time.Time
}

func NewBroadcastSender(st *store.Store, api MarketingAPI, policy BroadcastPolicy, now func() time.Time) *BroadcastSender {
	if now == nil {
		now = time.Now
	}
	return &BroadcastSender{store: st, api: api, policy: policy, now: now}
}
func (s *BroadcastSender) load(ctx context.Context, id string) (store.Broadcast, []store.BroadcastRecipient, error) {
	b, rows, err := s.store.GetBroadcastContext(ctx, id)
	if err != nil {
		return b, nil, err
	}
	source, err := s.store.GetSourceByIDContext(ctx, b.SourceID)
	if err != nil {
		return b, nil, err
	}
	if source.SourceType != SourceTypeFacebookMessages || source.Identifier != s.policy.Source.Identifier() || source.HistoryOnly || source.MergedIntoSourceID != 0 || b.AdAccountID != s.policy.AdAccountID {
		return b, nil, ErrPlanChanged
	}
	return b, rows, nil
}
func (s *BroadcastSender) decisions(ctx context.Context, b store.Broadcast, rows []store.BroadcastRecipient) ([]RecipientDecision, map[int64]store.MarketingSubscription, error) {
	ids := make([]int64, len(rows))
	for i := range rows {
		ids[i] = rows[i].SubscriptionID
	}
	subs, err := s.store.ListMarketingSubscriptionsContext(ctx, b.SourceID, store.SubscriptionFilter{SubscriptionIDs: ids})
	if err != nil {
		return nil, nil, err
	}
	events, err := s.store.LatestConsentEventsContext(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	last, err := s.store.LastAcceptedSendContext(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[int64]store.MarketingSubscription, len(subs))
	for _, sub := range subs {
		byID[sub.ID] = sub
	}
	return PlanRecipients(s.now(), s.policy.PlanPolicy, subs, events, last), byID, nil
}
func (s *BroadcastSender) Plan(ctx context.Context, id string, revision int64) (BroadcastPlan, error) {
	b, rows, err := s.load(ctx, id)
	if err != nil {
		return BroadcastPlan{}, err
	}
	if b.Revision != revision {
		return BroadcastPlan{}, ErrPlanChanged
	}
	decisions, _, err := s.decisions(ctx, b, rows)
	if err != nil {
		return BroadcastPlan{}, err
	}
	plan := BroadcastPlan{ID: id, Revision: revision, PlanDigest: PlanDigest(b, decisions), PageID: s.policy.Source.PageID, DailyBudgetCents: b.DailyBudgetCents, WorstDailyCents: float64(b.DailyBudgetCents) * 1.75, WorstWeeklyCents: float64(b.DailyBudgetCents) * 7, CampaignEndsAt: s.now().Add(time.Duration(b.CampaignDays) * 24 * time.Hour), Recipients: decisions}
	saved := make([]store.RecipientDecisionRow, len(decisions))
	for i, d := range decisions {
		if d.Eligible {
			plan.Eligible++
		} else {
			plan.Refused++
		}
		saved[i] = store.RecipientDecisionRow{SubscriptionID: d.SubscriptionID, Eligible: d.Eligible, Reason: d.Reason}
	}
	if err = s.store.SavePlanContext(ctx, id, revision, plan.PlanDigest, saved); err != nil {
		return BroadcastPlan{}, err
	}
	return plan, nil
}

// Provider calls may have completed after cancellation. Commit their receipts
// with a bounded database-only context before returning control to the caller.
func broadcastCommitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
func marketingFailure(err error, permission string) error {
	c := ClassifyMarketingError(err, false)
	if c.Category == CategoryPermissionMissing {
		return fmt.Errorf("%w:%s", ErrPermissionMissing, permission)
	}
	return errors.New(string(c.Category))
}
func (s *BroadcastSender) preflight(ctx context.Context) error {
	permissions, err := s.api.GrantedPermissions(ctx)
	if err != nil {
		var graph *APIError
		if errors.As(err, &graph) && graph.Permission() {
			return nil
		}
		return marketingFailure(err, "ads_management")
	}
	for _, name := range []string{"ads_management", "pages_messaging"} {
		if !permissions[name] {
			return fmt.Errorf("%w:%s", ErrPermissionMissing, name)
		}
	}
	if !permissions["marketing_messages_messenger"] && !permissions["paid_marketing_messages"] {
		return fmt.Errorf("%w:marketing_messages_messenger", ErrPermissionMissing)
	}
	return nil
}
func (s *BroadcastSender) result(ctx context.Context, id string, retry int64, operationErr error) (BroadcastSendResult, error) {
	commit, cancel := broadcastCommitContext(ctx)
	defer cancel()
	b, rows, err := s.store.GetBroadcastContext(commit, id)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	out := BroadcastSendResult{ID: id, State: b.State, CampaignID: b.CampaignID, RetryAfterSeconds: retry, Recipients: []RecipientResult{}}
	if operationErr != nil {
		out.ErrorCode = operationErr.Error()
	}
	subs, err := s.store.ListMarketingSubscriptionsContext(commit, b.SourceID, store.SubscriptionFilter{})
	if err != nil {
		return out, err
	}
	fingerprints := map[int64]string{}
	for _, sub := range subs {
		fingerprints[sub.ID] = sub.Fingerprint
	}
	for _, row := range rows {
		switch row.State {
		case "accepted", "delivered", "read":
			out.Accepted++
		case "failed", "delivery_failed":
			out.Failed++
		case "remote_unknown":
			out.Unknown++
		case "refused":
			out.Refused++
		case "planned", "dispatching":
			out.Remaining++
		}
		out.Recipients = append(out.Recipients, RecipientResult{SubscriptionID: row.SubscriptionID, State: row.State, Reason: row.Reason, Fingerprint: fingerprints[row.SubscriptionID], TrackingID: row.TrackingID})
	}
	return out, operationErr
}
func (s *BroadcastSender) Send(ctx context.Context, req BroadcastSendRequest) (BroadcastSendResult, error) {
	if !s.policy.Enabled || !s.policy.AllowSend {
		return BroadcastSendResult{}, ErrSendsDisabled
	}
	if err := ctx.Err(); err != nil {
		return BroadcastSendResult{}, err
	}
	if req.Limit < 0 || req.Limit > 1000 {
		return BroadcastSendResult{}, errors.New("invalid_limit")
	}
	if req.Limit == 0 {
		req.Limit = 100
	}
	b, rows, err := s.load(ctx, req.ID)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	if b.Revision != req.Revision || req.PlanDigest == "" || b.PlanDigest != req.PlanDigest {
		return BroadcastSendResult{}, ErrPlanChanged
	}
	if b.DailyBudgetCents <= 0 || b.DailyBudgetCents > s.policy.MaxDailyBudgetCents {
		return BroadcastSendResult{}, ErrBudgetOverCap
	}
	if b.State == "cancelled" {
		return s.result(ctx, b.ID, 0, errors.New("broadcast_cancelled"))
	}
	if b.State == "complete" {
		return s.result(ctx, b.ID, 0, nil)
	}
	decisions, _, err := s.decisions(ctx, b, rows)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	for _, d := range decisions {
		if !d.Eligible {
			if err = s.store.RefuseBroadcastRecipientContext(ctx, b.ID, d.SubscriptionID, d.Reason); err != nil {
				return BroadcastSendResult{}, err
			}
		}
	}
	if _, err = s.store.ExpireAbandonedClaimsContext(ctx, b.ID, s.now().Add(-2*time.Minute)); err != nil {
		return BroadcastSendResult{}, err
	}
	b, rows, err = s.load(ctx, b.ID)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	if b.State == "campaign_pending" || b.State == "campaign_unknown" {
		return s.result(ctx, b.ID, 0, ErrCampaignUnknown)
	}
	planned := 0
	for _, r := range rows {
		if r.State == "planned" {
			planned++
		}
	}
	if planned == 0 {
		return s.result(ctx, b.ID, 0, nil)
	}
	if err = s.preflight(ctx); err != nil {
		return s.result(ctx, b.ID, 0, err)
	}
	if b.State == "planned" {
		ends := s.now().Add(time.Duration(b.CampaignDays) * 24 * time.Hour)
		if err = s.store.BeginBroadcastCampaignContext(ctx, b.ID, ends); err != nil {
			return s.result(ctx, b.ID, 0, err)
		}
		campaign, callErr := s.api.CreateMessageCampaign(ctx, b.AdAccountID, s.policy.Source.PageID, b.Name, b.DailyBudgetCents, ends)
		if callErr == nil && (!safeObjectID(campaign.ID) || (campaign.PageID != "" && campaign.PageID != s.policy.Source.PageID)) {
			callErr = errors.New("invalid campaign response")
		}
		commit, cancel := broadcastCommitContext(ctx)
		if callErr != nil {
			category := ClassifyMarketingError(callErr, false)
			next := "planned"
			resultErr := marketingFailure(callErr, "ads_management")
			if category.Ambiguous {
				next = "campaign_unknown"
				resultErr = ErrCampaignUnknown
			}
			err = s.store.TransitionBroadcastFailureContext(commit, b.ID, "campaign_pending", next, string(category.Category))
			cancel()
			if err != nil {
				return s.result(ctx, b.ID, 0, err)
			}
			return s.result(ctx, b.ID, 0, resultErr)
		}
		err = s.store.SetCampaignContext(commit, b.ID, campaign.ID, s.now().Add(time.Hour), ends, campaign.HasBudgetWarning)
		cancel()
		if err != nil {
			return s.result(ctx, b.ID, 0, err)
		}
		b, rows, err = s.load(ctx, b.ID)
		if err != nil {
			return BroadcastSendResult{}, err
		}
	}
	if b.CampaignReadyAt.Valid && s.now().Before(b.CampaignReadyAt.Time) {
		return s.result(ctx, b.ID, int64((b.CampaignReadyAt.Time.Sub(s.now())+time.Second-1)/time.Second), ErrCampaignWarming)
	}
	if !b.CampaignEndsAt.Valid || !s.now().Before(b.CampaignEndsAt.Time) {
		return s.result(ctx, b.ID, 0, errors.New("campaign_expired"))
	}
	if b.State != "sending" {
		if err = s.store.TransitionBroadcastContext(ctx, b.ID, b.State, "sending", ""); err != nil {
			return s.result(ctx, b.ID, 0, err)
		}
	}
	attempts := 0
	var stop error
	var retry int64
	stopCategory := string(CategorySendRejected)
	for _, row := range rows {
		if row.State != "planned" {
			continue
		}
		if err = ctx.Err(); err != nil {
			stop = err
			break
		}
		if attempts >= req.Limit {
			break
		}
		fresh, subs, checkErr := s.decisions(ctx, b, []store.BroadcastRecipient{row})
		if checkErr != nil {
			stop = checkErr
			break
		}
		if len(fresh) != 1 || !fresh[0].Eligible {
			reason := "consent_missing"
			if len(fresh) == 1 {
				reason = fresh[0].Reason
			}
			if err = s.store.RefuseBroadcastRecipientContext(ctx, b.ID, row.SubscriptionID, reason); err != nil {
				stop = err
				break
			}
			continue
		}
		claimed, claimErr := s.store.ClaimRecipientContext(ctx, b.ID, row.SubscriptionID, row.RowRevision, s.now())
		if claimErr != nil {
			stop = claimErr
			break
		}
		if !claimed {
			continue
		}
		attempts++
		sub := subs[row.SubscriptionID]
		tracking, callErr := s.api.SendMarketingMessage(ctx, b.AdAccountID, b.CampaignID, sub.Token, b.MessageJSON, b.MinGapSeconds)
		if callErr == nil && (tracking == "" || strings.Contains(tracking, sub.Token)) {
			callErr = errors.New("invalid marketing receipt")
		}
		state, reason, code := "accepted", "", ""
		classification := ClassifyMarketingError(callErr, b.MinGapSeconds > 0)
		if callErr != nil {
			state = "failed"
			reason = string(classification.Category)
			tracking = ""
			if classification.Ambiguous {
				state = "remote_unknown"
			}
			if graph, ok := errors.AsType[*APIError](callErr); ok {
				code = strconv.Itoa(graph.Code)
			}
		}
		commit, cancel := broadcastCommitContext(ctx)
		err = s.store.FinishRecipientContext(commit, b.ID, row.SubscriptionID, state, reason, code, tracking)
		if err == nil && classification.Category == CategoryRecipientUnavailable {
			err = s.store.RecordConsentEventContext(commit, row.SubscriptionID, "send_refused_recipient", "marketing_send", b.ID, map[string]any{"error_category": reason})
		}
		if err == nil && classification.Category == CategoryFrequencyCap {
			err = s.store.PushNextEligibleContext(commit, row.SubscriptionID, s.now().Add(24*time.Hour))
		}
		cancel()
		if err != nil {
			stop = err
			break
		}
		if classification.StopsRun {
			stop = marketingFailure(callErr, "marketing_messages_messenger")
			stopCategory = string(classification.Category)
			retry = int64(classification.RetryAfter / time.Second)
			break
		}
	}
	commit, cancel := broadcastCommitContext(ctx)
	defer cancel()
	_, current, err := s.store.GetBroadcastContext(commit, b.ID)
	if err != nil {
		return BroadcastSendResult{}, err
	}
	remaining := 0
	for _, row := range current {
		if row.State == "planned" || row.State == "dispatching" {
			remaining++
		}
	}
	next := "complete"
	if remaining > 0 || stop != nil {
		next = "paused"
	}
	if stop != nil {
		if errors.Is(stop, context.Canceled) || errors.Is(stop, context.DeadlineExceeded) {
			stopCategory = "cancelled"
		}
		err = s.store.TransitionBroadcastFailureContext(commit, b.ID, "sending", next, stopCategory)
	} else {
		err = s.store.TransitionBroadcastContext(commit, b.ID, "sending", next, "")
	}
	if err != nil && !errors.Is(err, store.ErrBroadcastFrozen) {
		return s.result(ctx, b.ID, retry, err)
	}
	return s.result(ctx, b.ID, retry, stop)
}
func (s *BroadcastSender) AdoptCampaign(ctx context.Context, id, campaignID string) error {
	b, _, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if b.State != "campaign_unknown" && b.State != "campaign_pending" {
		return ErrPlanChanged
	}
	campaign, err := s.api.CampaignPage(ctx, campaignID)
	if err != nil {
		return marketingFailure(err, "ads_management")
	}
	if campaign.PageID != s.policy.Source.PageID {
		return ErrCampaignForeign
	}
	ready := s.now().Add(time.Hour)
	if campaign.ID != campaignID || campaign.DailyBudgetCents != b.DailyBudgetCents || campaign.LifetimeBudgetCents != 0 || !b.CampaignEndsAt.Valid || !campaign.EndsAt.After(ready) || campaign.EndsAt.After(b.CampaignEndsAt.Time) {
		return ErrPlanChanged
	}
	return s.store.SetCampaignContext(ctx, id, campaignID, ready, campaign.EndsAt, campaign.HasBudgetWarning)
}
