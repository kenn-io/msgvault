package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func broadcastPolicy(p config.MessengerBroadcastProfile) meta.BroadcastPolicy {
	return meta.BroadcastPolicy{Source: meta.DMSource{AppID: p.AppID, PageID: p.PageID, Platform: meta.PlatformMessenger}, AdAccountID: p.AdAccountID, Enabled: p.Enabled, AllowSend: p.ActionAllowed("send"), MaxDailyBudgetCents: p.MaxDailyBudgetCents, PlanPolicy: meta.PlanPolicy{MaxRecipients: p.MaxRecipients, MinInterval: time.Duration(p.MinIntervalHours) * time.Hour, ConsentMaxAge: time.Duration(p.ConsentMaxAgeHours) * time.Hour}}
}
func configuredBroadcastClient(p config.MessengerBroadcastProfile, ads bool) (*meta.Client, error) {
	name := p.TokenEnv
	if ads {
		name = p.AdTokenEnv
	}
	token := os.Getenv(name)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("broadcast_credentials_missing: set %s in the daemon environment for profile %q", name, p.Identifier)
	}
	return meta.NewClient(token)
}
func broadcastJobName(p config.MessengerBroadcastProfile) string {
	return "messenger_broadcasts:" + broadcastPolicy(p).Source.Identifier()
}
func registerScheduledBroadcastJob(s *scheduler.Scheduler, p config.MessengerBroadcastProfile, run func(context.Context) error) error {
	return s.AddJob(scheduler.Job{Name: broadcastJobName(p), Schedule: p.Schedule, Preemptible: true, Run: run})
}
func runConfiguredBroadcastSync(ctx context.Context, st *store.Store, p config.MessengerBroadcastProfile) error {
	client, err := configuredBroadcastClient(p, false)
	if err != nil {
		return err
	}
	_, err = meta.SyncSubscriptions(ctx, st, client, broadcastPolicy(p).Source, 1000)
	return err
}
func (a *storeAPIAdapter) broadcastProfile(ctx context.Context, in api.BroadcastIntent) (config.MessengerBroadcastProfile, *store.Source, error) {
	if a.config == nil {
		return config.MessengerBroadcastProfile{}, nil, errors.New("configuration is unavailable")
	}
	var p *config.MessengerBroadcastProfile
	if in.Profile != "" {
		p = a.config.GetMessengerBroadcastProfile(in.Profile)
	} else {
		b, _, err := a.store.GetBroadcastContext(ctx, in.ID)
		if err != nil {
			return config.MessengerBroadcastProfile{}, nil, err
		}
		source, err := a.store.GetSourceByIDContext(ctx, b.SourceID)
		if err != nil {
			return config.MessengerBroadcastProfile{}, nil, err
		}
		for _, candidate := range a.config.MessengerBroadcasts {
			if source.SourceType == meta.SourceTypeFacebookMessages && source.Identifier == broadcastPolicy(candidate).Source.Identifier() && b.AdAccountID == candidate.AdAccountID {
				p = &candidate
				break
			}
		}
	}
	if p == nil {
		return config.MessengerBroadcastProfile{}, nil, errors.New("broadcast_profile_not_found")
	}
	source, err := a.store.GetSourceByTypeAndIdentifier(meta.SourceTypeFacebookMessages, broadcastPolicy(*p).Source.Identifier())
	if errors.Is(err, store.ErrSourceNotFound) {
		return *p, nil, errors.New("broadcast_source_not_initialized")
	}
	if err != nil {
		return *p, nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return *p, nil, errors.New("broadcast_source_inactive")
	}
	return *p, source, nil
}
func (a *storeAPIAdapter) runCLIBroadcast(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) (resultErr error) {
	defer func() { resultErr = classifyBroadcastError(resultErr) }()
	if req.Grant != nil {
		return draftReplyError("not_permitted", errors.New("broadcast requires owner access"))
	}
	in, err := api.ParseCLIRunBroadcast(req)
	if err != nil {
		return broadcastArgumentError(err)
	}
	p, source, err := a.broadcastProfile(ctx, in)
	if err != nil {
		return err
	}
	policy := broadcastPolicy(p)
	var out any
	switch in.Sub {
	case "subscriptions":
		var rows []store.MarketingSubscription
		rows, err = a.store.ListMarketingSubscriptionsContext(ctx, source.ID, store.SubscriptionFilter{ParticipantIDs: in.Participants, Status: in.Status})
		out = api.ProjectBroadcastSubscriptions(rows)
	case "list":
		var rows []store.Broadcast
		rows, err = a.store.ListBroadcastsContext(ctx, source.ID)
		list := make([]api.BroadcastSummary, 0, len(rows))
		for _, b := range rows {
			list = append(list, api.ProjectBroadcast(b, p.PageID))
		}
		out = list
	case "show":
		var b store.Broadcast
		var rows []store.BroadcastRecipient
		b, rows, err = a.store.GetBroadcastContext(ctx, in.ID)
		out = api.ProjectBroadcastDetail(b, rows, p.PageID)
	case "create":
		var rows []store.MarketingSubscription
		rows, err = a.store.ListMarketingSubscriptionsContext(ctx, source.ID, store.SubscriptionFilter{ParticipantIDs: in.Participants, SubscriptionIDs: in.Subscriptions})
		if err != nil {
			break
		}
		if len(rows) > 1000 {
			err = errors.New("too_many_recipients")
			break
		}
		found := map[int64]bool{}
		ids := make([]int64, 0, len(rows))
		for _, s := range rows {
			ids = append(ids, s.ID)
			found[s.ID] = true
		}
		for _, id := range in.Subscriptions {
			if !found[id] {
				err = errors.New("subscription_not_found")
				break
			}
		}
		if err != nil {
			break
		}
		var b store.Broadcast
		b, err = a.store.CreateBroadcastContext(ctx, store.Broadcast{SourceID: source.ID, AdAccountID: p.AdAccountID, Name: in.Name, MessageJSON: in.MessageJSON, DailyBudgetCents: in.DailyBudgetCents, MinGapSeconds: in.MinGapSeconds, CampaignDays: int64(p.CampaignDays)}, ids)
		out = api.ProjectBroadcast(b, p.PageID)
	case "plan":
		out, err = meta.NewBroadcastSender(a.store, nil, policy, nil).Plan(ctx, in.ID, in.Revision)
	case "cancel":
		err = a.store.CancelBroadcastContext(ctx, in.ID)
		out = map[string]any{"id": in.ID, "state": "cancelled"}
	case "unsubscribe":
		var client meta.MarketingAPI
		var clientErr error
		if policy.Enabled && policy.AllowSend {
			client, clientErr = configuredBroadcastClient(p, false)
			if clientErr != nil {
				client = nil
			}
		}
		sel := meta.UnsubscribeSelector{}
		if len(in.Participants) > 0 {
			sel.ParticipantID = in.Participants[0]
		} else {
			sel.SubscriptionID = in.Subscriptions[0]
		}
		out, err = meta.Unsubscribe(ctx, a.store, client, policy, sel)
		if err == nil {
			err = clientErr
		}
	case "sync-subscriptions":
		if !p.Enabled {
			return errors.New("broadcast_profile_disabled")
		}
		var client *meta.Client
		client, err = configuredBroadcastClient(p, false)
		if err == nil {
			out, err = meta.SyncSubscriptions(ctx, a.store, client, policy.Source, in.MaxRequests)
		}
	case "send", "adopt-campaign", "preview":
		if in.Sub == "send" && (!policy.Enabled || !policy.AllowSend) {
			return meta.ErrSendsDisabled
		}
		var client *meta.Client
		client, err = configuredBroadcastClient(p, true)
		if err != nil {
			break
		}
		sender := meta.NewBroadcastSender(a.store, client, policy, nil)
		switch in.Sub {
		case "send":
			out, err = sender.Send(ctx, meta.BroadcastSendRequest{ID: in.ID, Revision: in.Revision, PlanDigest: in.PlanDigest, Limit: in.Limit})
		case "adopt-campaign":
			err = sender.AdoptCampaign(ctx, in.ID, in.CampaignID)
			out = map[string]any{"id": in.ID, "campaign_id": in.CampaignID}
		case "preview":
			var b store.Broadcast
			b, _, err = a.store.GetBroadcastContext(ctx, in.ID)
			if err == nil {
				out, err = client.PreviewMarketingMessage(ctx, p.AdAccountID, p.PageID, jsontext.Value(b.MessageJSON))
				if err != nil {
					err = errors.New(string(meta.ClassifyMarketingError(err, false).Category))
				}
			}
		}
	}
	if out != nil && emit != nil {
		data, marshalErr := json.Marshal(out, json.Deterministic(true))
		if marshalErr != nil {
			return marshalErr
		}
		if emitErr := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(data) + "\n"}); emitErr != nil {
			return emitErr
		}
	}
	return err
}

func (a *storeAPIAdapter) ListBroadcastsContext(ctx context.Context, id int64) ([]store.Broadcast, error) {
	return a.store.ListBroadcastsContext(ctx, id)
}
func (a *storeAPIAdapter) GetBroadcastContext(ctx context.Context, id string) (store.Broadcast, []store.BroadcastRecipient, error) {
	return a.store.GetBroadcastContext(ctx, id)
}
func (a *storeAPIAdapter) ListMarketingSubscriptionsContext(ctx context.Context, id int64, f store.SubscriptionFilter) ([]store.MarketingSubscription, error) {
	return a.store.ListMarketingSubscriptionsContext(ctx, id, f)
}
