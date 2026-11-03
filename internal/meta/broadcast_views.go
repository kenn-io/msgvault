package meta

import (
	"encoding/json/jsontext"
	"time"
)

type BroadcastSubscriptionView struct {
	ID               int64      `json:"id"`
	SourceID         int64      `json:"source_id"`
	ParticipantID    *int64     `json:"participant_id,omitempty"`
	TokenFingerprint string     `json:"token_fingerprint"`
	TopicTitle       string     `json:"topic_title"`
	Timezone         string     `json:"timezone"`
	Status           string     `json:"status"`
	Reoptin          string     `json:"reoptin"`
	TokenExpiryAt    *time.Time `json:"token_expiry_at,omitempty"`
	NextEligibleAt   *time.Time `json:"next_eligible_at,omitempty"`
	LastConfirmedAt  *time.Time `json:"last_confirmed_at,omitempty"`
}
type BroadcastSummary struct {
	ID               string         `json:"id"`
	SourceID         int64          `json:"source_id"`
	PageID           string         `json:"page_id"`
	AdAccountID      string         `json:"ad_account_id"`
	Name             string         `json:"name"`
	MessageJSON      jsontext.Value `json:"message_json"`
	Revision         int64          `json:"revision"`
	DailyBudgetCents int64          `json:"daily_budget_cents"`
	WorstDailyCents  float64        `json:"worst_daily_cents"`
	WorstWeeklyCents float64        `json:"worst_weekly_cents"`
	CampaignDays     int64          `json:"campaign_days"`
	MinGapSeconds    int64          `json:"min_conversation_gap_seconds"`
	State            string         `json:"state"`
	CampaignID       string         `json:"campaign_id"`
	PlanDigest       string         `json:"plan_digest"`
	LastError        string         `json:"last_error"`
	CampaignReadyAt  *time.Time     `json:"campaign_ready_at,omitempty"`
	CampaignEndsAt   *time.Time     `json:"campaign_ends_at,omitempty"`
	BudgetWarning    bool           `json:"budget_warning"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}
type BroadcastRecipientView struct {
	SubscriptionID int64      `json:"subscription_id"`
	ParticipantID  *int64     `json:"participant_id,omitempty"`
	State          string     `json:"state"`
	Reason         string     `json:"reason"`
	ErrorCode      string     `json:"error_code"`
	TrackingID     string     `json:"tracking_id"`
	MID            string     `json:"mid"`
	MessageID      *int64     `json:"message_id,omitempty"`
	AttemptedAt    *time.Time `json:"attempted_at,omitempty"`
	RowRevision    int64      `json:"row_revision"`
}
type BroadcastDetail struct {
	BroadcastSummary

	Recipients []BroadcastRecipientView `json:"recipients"`
}
type BroadcastListResponse struct {
	Broadcasts []BroadcastSummary `json:"broadcasts"`
}
type BroadcastSubscriptionsResponse struct {
	Subscriptions []BroadcastSubscriptionView `json:"subscriptions"`
}
