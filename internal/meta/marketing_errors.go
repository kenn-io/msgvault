package meta

import (
	"encoding/json/v2"
	"errors"
	"sort"
	"time"
)

type FailureCategory string

const (
	CategoryPermissionMissing    FailureCategory = "permission_missing"
	CategoryCredentialInvalid    FailureCategory = "token_invalid"
	CategoryThrottled            FailureCategory = "rate_limited"
	CategoryRecipientUnavailable FailureCategory = "recipient_unavailable"
	CategoryConversationGap      FailureCategory = "conversation_gap"
	CategoryFrequencyCap         FailureCategory = "frequency_cap"
	CategoryPacing               FailureCategory = "pacing"
	CategorySpendLimit           FailureCategory = "spend_limit"
	CategoryCampaignRejected     FailureCategory = "campaign_rejected"
	CategorySendRejected         FailureCategory = "send_rejected"
)

type Classification struct {
	Category            FailureCategory
	RetryAfter          time.Duration
	Ambiguous, StopsRun bool
}

func ClassifyMarketingError(err error, gapSent bool) Classification {
	if err == nil {
		return Classification{}
	}
	var e *APIError
	if !errors.As(err, &e) {
		return Classification{Category: CategorySendRejected, Ambiguous: true, StopsRun: true}
	}
	// A gateway/server failure cannot establish that a write was rejected.
	if e.Status >= 500 || e.Status == 408 || (e.Status > 0 && e.Status < 400 && e.Code == 0) {
		return Classification{Category: CategorySendRejected, Ambiguous: true, StopsRun: true}
	}
	out := Classification{Category: CategorySendRejected, StopsRun: true}
	switch {
	case e.Permission():
		out.Category = CategoryPermissionMissing
	case e.TokenExpired():
		out.Category = CategoryCredentialInvalid
	case e.Throttled():
		out.Category = CategoryThrottled
		out.RetryAfter = time.Hour
	case e.Code == 1893047 || e.Kind == "RECIPIENT_NOT_AVAILABLE" || e.Kind == "RECIPIENT_UNAVAILABLE":
		out.Category = CategoryRecipientUnavailable
		out.StopsRun = false
	case e.Code == 2300052:
		out.Category = CategoryRecipientUnavailable
		if gapSent {
			out.Category = CategoryConversationGap
		}
		out.StopsRun = false
	case e.Kind == "WEEKLY_MM_LOAD_CAP_REACHED" || e.Kind == "DAILY_MM_LOAD_CAP_REACHED" || e.Kind == "MIN_ORGANIC_THREAD_NOT_REACHED":
		out.Category = CategoryFrequencyCap
		out.StopsRun = false
	case e.Code >= 2300064 && e.Code <= 2300067:
		out.Category = CategoryCampaignRejected
		out.StopsRun = false
	case e.Code >= 2300072 && e.Code <= 2300079:
		out.Category = CategorySpendLimit
	case e.Code == 2300028 || (e.Code >= 2300068 && e.Code <= 2300071) || e.Code == 2300080:
		out.Category = CategoryPacing
		out.RetryAfter = time.Hour
	}
	if out.RetryAfter > 0 && e.RemainingSeconds > 0 {
		out.RetryAfter = time.Duration(e.RemainingSeconds) * time.Second
	}
	return out
}

// marketingErrorFields projects only exact documented enum values and a
// numeric delay. Arbitrary provider strings never escape the decoder.
func marketingErrorFields(data []byte) (string, int) {
	var envelope struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return "", 0
	}
	var delay struct {
		Error struct {
			Data struct {
				Seconds int `json:"remaining_seconds"`
			} `json:"error_data"`
		} `json:"error"`
	}
	remaining := 0
	if json.Unmarshal(data, &delay) == nil {
		remaining = delay.Error.Data.Seconds
	}
	var visit func(any) string
	visit = func(v any) string {
		switch x := v.(type) {
		case string:
			switch x {
			case "WEEKLY_MM_LOAD_CAP_REACHED", "DAILY_MM_LOAD_CAP_REACHED", "MIN_ORGANIC_THREAD_NOT_REACHED", "RECIPIENT_UNAVAILABLE", "RECIPIENT_NOT_AVAILABLE":
				return x
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if s := visit(x[key]); s != "" {
					return s
				}
			}
		case []any:
			for _, value := range x {
				if s := visit(value); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return visit(envelope.Error), remaining
}
