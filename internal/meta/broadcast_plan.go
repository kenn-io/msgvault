package meta

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type RecipientDecision struct {
	SubscriptionID, ParticipantID int64
	Eligible                      bool
	Reason                        string
}
type PlanPolicy struct {
	MaxRecipients              int
	MinInterval, ConsentMaxAge time.Duration
}

// PlanRecipients makes one decision per subscription, without writes or
// provider calls. Complete-walk evidence and freshness are separate checks.
func PlanRecipients(now time.Time, policy PlanPolicy, subs []store.MarketingSubscription, events map[int64][]store.ConsentEvent, lastSent map[int64]time.Time) []RecipientDecision {
	if policy.MaxRecipients <= 0 {
		policy.MaxRecipients = 100
	}
	if policy.MaxRecipients > 1000 {
		policy.MaxRecipients = 1000
	}
	if policy.MinInterval <= 0 {
		policy.MinInterval = 24 * time.Hour
	}
	if policy.MinInterval < 12*time.Hour {
		policy.MinInterval = 12 * time.Hour
	}
	if policy.ConsentMaxAge <= 0 {
		policy.ConsentMaxAge = 24 * time.Hour
	}
	ordered := append([]store.MarketingSubscription(nil), subs...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seenIDs := map[int64]bool{}
	participants := map[int64]bool{}
	psids := map[string]bool{}
	out := make([]RecipientDecision, 0, len(ordered))
	count := 0
	for _, sub := range ordered {
		if seenIDs[sub.ID] {
			continue
		}
		seenIDs[sub.ID] = true
		d := RecipientDecision{SubscriptionID: sub.ID}
		if sub.ParticipantID.Valid {
			d.ParticipantID = sub.ParticipantID.Int64
		}
		d.Reason = consentRefusal(now, policy, sub, events[sub.ID], lastSent[sub.ID])
		if d.Reason == "" {
			switch {
			case (sub.ParticipantID.Valid && participants[sub.ParticipantID.Int64]) || (sub.PSID.Valid && sub.PSID.String != "" && psids[sub.PSID.String]):
				d.Reason = "duplicate"
			case count >= policy.MaxRecipients:
				d.Reason = "over_cap"
			default:
				d.Eligible = true
				count++
				if sub.ParticipantID.Valid {
					participants[sub.ParticipantID.Int64] = true
				}
				if sub.PSID.Valid && sub.PSID.String != "" {
					psids[sub.PSID.String] = true
				}
			}
		}
		out = append(out, d)
	}
	return out
}
func consentRefusal(now time.Time, policy PlanPolicy, sub store.MarketingSubscription, events []store.ConsentEvent, last time.Time) string {
	if sub.Status == "stopped" || sub.Status == "unsubscribed" {
		return "consent_stopped"
	}
	var optin, stop time.Time
	evidence := false
	for _, ev := range events {
		switch ev.Kind {
		case "optin", "resume":
			if ev.OccurredAt.IsZero() || ev.OccurredAt.After(now) {
				continue
			}
			evidence = true
			if ev.OccurredAt.After(optin) {
				optin = ev.OccurredAt
			}
		case "stop", "local_unsubscribe":
			if ev.OccurredAt.After(stop) {
				stop = ev.OccurredAt
			}
		case "listed":
			if ev.WalkComplete && !ev.OccurredAt.After(now) {
				evidence = true
			}
		}
	}
	if !stop.IsZero() && !optin.After(stop) {
		return "consent_stopped"
	}
	if sub.Status != "active" || sub.Token == "" || !evidence {
		return "consent_missing"
	}
	if !sub.LastConfirmedAt.Valid || sub.LastConfirmedAt.Time.After(now) || now.Sub(sub.LastConfirmedAt.Time) > policy.ConsentMaxAge {
		return "consent_stale"
	}
	if !sub.TokenExpiryAt.Valid || !sub.TokenExpiryAt.Time.After(now) {
		return "consent_expired"
	}
	if (!last.IsZero() && now.Sub(last) < policy.MinInterval) || (sub.NextEligibleAt.Valid && sub.NextEligibleAt.Time.After(now)) {
		return "frequency_cap"
	}
	return ""
}

// PlanDigest binds owner confirmation to content, spend, source and recipient
// decisions. Raw subscription tokens never enter the digest's input encoding.
func PlanDigest(b store.Broadcast, decisions []RecipientDecision) string {
	ordered := append([]RecipientDecision(nil), decisions...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].SubscriptionID < ordered[j].SubscriptionID })
	input := struct {
		ID                                            string
		SourceID                                      int64
		AdAccountID                                   string
		Revision                                      int64
		MessageSHA256                                 string
		DailyBudgetCents, CampaignDays, MinGapSeconds int64
		Decisions                                     []RecipientDecision
	}{b.ID, b.SourceID, b.AdAccountID, b.Revision, b.MessageSHA256, b.DailyBudgetCents, b.CampaignDays, b.MinGapSeconds, ordered}
	data, _ := json.Marshal(input)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
