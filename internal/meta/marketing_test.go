package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionTokensRequest(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v26.0/100/notification_message_tokens", r.URL.Path)
		want := url.Values{"limit": {"1000"}}
		if calls == 2 {
			want.Set("after", "cursor-2")
		}
		assert.Equal(t, want, r.URL.Query())
		assert.Equal(t, "Bearer synthetic-page-secret", r.Header.Get("Authorization"))
		if calls == 2 {
			_, _ = fmt.Fprint(w, `{"data":[]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"notification_messages_token":"synthetic-mm-token-1","recipient_id":"300","topic_title":"Offers","notification_messages_timezone":"UTC","user_token_status":"REFRESHED","notification_messages_reoptin":"true","creation_timestamp":1700000000,"token_expiry_timestamp":1800000000,"next_eligible_time":1750000000},{"notification_messages_token":"synthetic-mm-token-2","creation_timestamp":"1700000001"}],"paging":{"cursors":{"after":"cursor-2"},"next":"https://evil.example/?access_token=secret"}}`)
	})
	page, err := c.SubscriptionTokens(context.Background(), "100", "")
	requirements.NoError(err)
	requirements.Len(page.Data, 2)
	assertions.Equal("300", page.Data[0].PSID)
	assertions.Equal("Offers", page.Data[0].TopicTitle)
	assertions.Equal("UTC", page.Data[0].Timezone)
	assertions.Equal("REFRESHED", page.Data[0].UserTokenStatus)
	assertions.Equal("true", page.Data[0].Reoptin)
	assertions.Equal(int64(1700000000), page.Data[0].Created.Unix())
	assertions.Equal(int64(1800000000), page.Data[0].Expiry.Unix())
	assertions.Equal(int64(1750000000), page.Data[0].NextEligible.Unix())
	assertions.Empty(page.Data[1].PSID)
	assertions.Equal(int64(1700000001), page.Data[1].Created.Unix())
	_, err = c.SubscriptionTokens(context.Background(), "100", page.After)
	requirements.NoError(err)
	_, err = c.SubscriptionTokens(context.Background(), "100", "synthetic-page-secret")
	requirements.Error(err)
	assertions.Equal(2, calls)
}

func TestSubscriptionTokensRejectsSecretCursor(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"notification_messages_token":"synthetic-mm-token-1"}],"paging":{"cursors":{"after":"synthetic-mm-token-1"},"next":"ignored"}}`)
	})
	_, err := c.SubscriptionTokens(context.Background(), "100", "")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "synthetic-mm-token-1")
}

func TestCreateMessageCampaignRequest(t *testing.T) {
	end := time.Unix(1800000000, 0)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v26.0/act_700/message_campaign", r.URL.Path)
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		assert.Equal(t, url.Values{"name": {"Example"}, "page_id": {"100"}, "daily_budget": {"500"}, "end_time": {"1800000000"}}, r.PostForm)
		_, _ = fmt.Fprint(w, `{"id":"55","warning":"provider text"}`)
	})
	got, err := c.CreateMessageCampaign(context.Background(), "700", "100", "Example", 500, end)
	require.NoError(t, err)
	assert.Equal(t, "55", got.ID)
	assert.True(t, got.HasBudgetWarning)
}
func TestCampaignPageRequest(t *testing.T) {
	assertions := assert.New(t)

	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/55", r.URL.Path)
		assert.Equal(t, "id,campaign{name},creative{actor_id},adset{daily_budget,lifetime_budget,end_time}", r.URL.Query().Get("fields"))
		_, _ = fmt.Fprint(w, `{"id":"55","creative":{"actor_id":"100"},"adset":{"daily_budget":"500","lifetime_budget":0,"end_time":"2027-01-15T08:00:00+0000"}}`)
	})
	got, err := c.CampaignPage(context.Background(), "55")
	require.NoError(t, err)
	assertions.Equal("100", got.PageID)
	assertions.Equal(int64(500), got.DailyBudgetCents)
	assertions.Zero(got.LifetimeBudgetCents)
	assertions.Equal(time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC), got.EndsAt)
}
func TestPreviewRequest(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/act_700/generatepreviews", r.URL.Path)
		assert.Equal(t, "MESSENGER_MARKETING_MESSAGES_MEDIA", r.URL.Query().Get("ad_format"))
		assert.JSONEq(t, `{"marketing_message_structured_spec":{},"object_story_spec":{"page_id":"100","link_data":{}}}`, r.URL.Query().Get("creative"))
		assert.JSONEq(t, `{"text":"Example"}`, r.URL.Query().Get("message"))
		_, _ = fmt.Fprint(w, `{"data":[{"body":"<iframe></iframe>"}]}`)
	})
	got, err := c.PreviewMarketingMessage(context.Background(), "700", "100", []byte(`{"text":"Example"}`))
	require.NoError(t, err)
	assert.JSONEq(t, `{"data":[{"body":"<iframe></iframe>"}]}`, string(got))
}
func TestSendMarketingMessageRequest(t *testing.T) {
	for _, gap := range []int64{0, 3600} {
		t.Run(strconv.FormatInt(gap, 10), func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/v26.0/act_700/messages", r.URL.Path)
				if !assert.NoError(t, r.ParseForm()) {
					return
				}
				assert.Equal(t, "55", r.PostForm.Get("message_id"))
				assert.JSONEq(t, `{"subscription_token":"synthetic-mm-token-1"}`, r.PostForm.Get("messenger_delivery_data"))
				assert.JSONEq(t, `{"text":"Example"}`, r.PostForm.Get("message"))
				if gap == 0 {
					assert.NotContains(t, r.PostForm, "min_conversation_gap_seconds")
				} else {
					assert.Equal(t, "3600", r.PostForm.Get("min_conversation_gap_seconds"))
				}
				assert.Empty(t, r.URL.RawQuery)
				_, _ = fmt.Fprint(w, `{"success":true,"marketing_message_tracking_id":"tracking-1"}`)
			})
			got, err := c.SendMarketingMessage(context.Background(), "700", "55", "synthetic-mm-token-1", []byte(`{"text":"Example"}`), gap)
			require.NoError(t, err)
			assert.Equal(t, "tracking-1", got)
		})
	}
}
func TestUnsubscribeRowsNeverMixed(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	calls := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v26.0/100/messaging_customer_base/unsubscribe", r.URL.Path)
		if !assert.NoError(t, r.ParseForm()) {
			return
		}
		var rows []map[string]string
		if !assert.NoError(t, json.Unmarshal([]byte(r.PostForm.Get("data")), &rows)) || !assert.Len(t, rows, 1) {
			return
		}
		assert.Len(t, rows[0], 1)
		if calls == 1 {
			assert.Equal(t, "300", rows[0]["psid"])
		} else {
			assert.Equal(t, "synthetic-mm-token-1", rows[0]["subscriber_token"])
		}
		_, _ = fmt.Fprint(w, `{"invalid_rows_count":1}`)
	})
	n, err := c.UnsubscribePSIDs(context.Background(), "100", []string{"300"})
	requirements.NoError(err)
	assertions.Equal(1, n)
	n, err = c.UnsubscribeTokens(context.Background(), "100", []string{"synthetic-mm-token-1"})
	requirements.NoError(err)
	assertions.Equal(1, n)
}
func TestMarketingErrorsRedactTokens(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"code":190,"message":"synthetic-page-secret synthetic-mm-token-1"}}`)
	})
	_, err := c.SendMarketingMessage(context.Background(), "700", "55", "synthetic-mm-token-1", []byte(`{}`), 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "synthetic-page-secret")
	assert.NotContains(t, err.Error(), "synthetic-mm-token-1")
}
func TestAPIErrorKindAllowlistOnly(t *testing.T) {
	for _, s := range []string{"WEEKLY_MM_LOAD_CAP_REACHED", "DAILY_MM_LOAD_CAP_REACHED", "MIN_ORGANIC_THREAD_NOT_REACHED", "RECIPIENT_UNAVAILABLE", "RECIPIENT_NOT_AVAILABLE", "WEEKLY_MM_LOAD_CAP_REACHED: token", "synthetic-mm-token-1"} {
		t.Run(s, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.MarshalWrite(w, map[string]any{"error": map[string]any{"code": 2300070, "message": s, "error_data": map[string]any{"remaining_seconds": 90}}})
			})
			var out any
			err := c.get(context.Background(), "me", nil, &out)
			var api *APIError
			require.ErrorAs(t, err, &api)
			want := s
			if s == "WEEKLY_MM_LOAD_CAP_REACHED: token" || s == "synthetic-mm-token-1" {
				want = ""
			}
			assert.Equal(t, want, api.Kind)
			assert.Equal(t, 90, api.RemainingSeconds)
		})
	}
}
func TestClassifyMarketingError(t *testing.T) {
	for _, tt := range []struct {
		code  int
		kind  string
		gap   bool
		cat   FailureCategory
		stop  bool
		retry time.Duration
	}{
		{10, "", false, CategoryPermissionMissing, true, 0}, {200, "", false, CategoryPermissionMissing, true, 0}, {299, "", false, CategoryPermissionMissing, true, 0},
		{190, "", false, CategoryCredentialInvalid, true, 0}, {4, "", false, CategoryThrottled, true, time.Hour}, {17, "", false, CategoryThrottled, true, time.Hour}, {32, "", false, CategoryThrottled, true, time.Hour}, {613, "", false, CategoryThrottled, true, time.Hour},
		{1893047, "", false, CategoryRecipientUnavailable, false, 0}, {0, "RECIPIENT_NOT_AVAILABLE", false, CategoryRecipientUnavailable, false, 0}, {2300052, "", false, CategoryRecipientUnavailable, false, 0}, {2300052, "", true, CategoryConversationGap, false, 0},
		{2300028, "", false, CategoryPacing, true, time.Hour}, {2300070, "", false, CategoryPacing, true, 90 * time.Second}, {2300074, "", false, CategorySpendLimit, true, 0}, {2300080, "", false, CategoryPacing, true, time.Hour},
		{2300065, "", false, CategoryCampaignRejected, false, 0}, {0, "WEEKLY_MM_LOAD_CAP_REACHED", false, CategoryFrequencyCap, false, 0}, {99, "", false, CategorySendRejected, true, 0},
	} {
		t.Run(fmt.Sprintf("%d-%s-%t", tt.code, tt.kind, tt.gap), func(t *testing.T) {
			assertions := assert.New(t)

			e := &APIError{Status: 400, Code: tt.code, Kind: tt.kind}
			if tt.code == 2300070 {
				e.RemainingSeconds = 90
			}
			got := ClassifyMarketingError(fmt.Errorf("wrapped: %w", e), tt.gap)
			assertions.Equal(tt.cat, got.Category)
			assertions.Equal(tt.stop, got.StopsRun)
			assertions.Equal(tt.retry, got.RetryAfter)
			assertions.False(got.Ambiguous)
		})
	}
	assert.True(t, ClassifyMarketingError(errors.New("transport"), false).Ambiguous)
}
func TestAdAccountPathRequiresDecimal(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) { assert.Fail(t, "invalid account made a request") })
	for _, id := range []string{"act_700", "7/0"} {
		_, err := c.CreateMessageCampaign(context.Background(), id, "100", "test", 500, time.Now())
		require.Error(t, err)
	}
}
func TestGrantedPermissions(t *testing.T) {
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v26.0/me/permissions", r.URL.Path)
		_, _ = fmt.Fprint(w, `{"data":[{"permission":"ads_management","status":"granted"},{"permission":"pages_messaging","status":"declined"}]}`)
	})
	got, err := c.GrantedPermissions(context.Background())
	require.NoError(t, err)
	assert.True(t, got["ads_management"])
	assert.False(t, got["pages_messaging"])
}

func TestAPIErrorKindAcrossEnvelopeFields(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"DAILY_MM_LOAD_CAP_REACHED","error_data":{"remaining_seconds":90}}}`,
		`{"error":{"code":2300070,"error_data":{"reason":"DAILY_MM_LOAD_CAP_REACHED","remaining_seconds":90}}}`,
	} {
		c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, body)
		})
		var out any
		err := c.get(t.Context(), "me", nil, &out)
		var api *APIError
		require.ErrorAs(t, err, &api)
		assert.Equal(t, "DAILY_MM_LOAD_CAP_REACHED", api.Kind)
		assert.Equal(t, 90, api.RemainingSeconds)
	}
}
