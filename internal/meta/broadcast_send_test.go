package meta

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type senderAPI struct {
	MarketingAPI

	permissions                map[string]bool
	permissionErr, createErr   error
	page                       string
	creates, sends, preflights int
	end                        time.Time
	sendErrors                 []error
	onSend                     func()
}

func (a *senderAPI) GrantedPermissions(context.Context) (map[string]bool, error) {
	a.preflights++
	return a.permissions, a.permissionErr
}
func (a *senderAPI) CreateMessageCampaign(_ context.Context, account, page, _ string, _ int64, end time.Time) (CampaignInfo, error) {
	a.creates++
	a.end = end
	return CampaignInfo{ID: "55", PageID: page}, a.createErr
}
func (a *senderAPI) CampaignPage(_ context.Context, id string) (CampaignInfo, error) {
	// Graph sends and returns campaign end times with whole-second precision.
	return CampaignInfo{ID: id, PageID: a.page, DailyBudgetCents: 500, EndsAt: a.end.Truncate(time.Second)}, nil
}
func (a *senderAPI) SendMarketingMessage(context.Context, string, string, string, jsontext.Value, int64) (string, error) {
	i := a.sends
	a.sends++
	if a.onSend != nil {
		a.onSend()
	}
	if i < len(a.sendErrors) && a.sendErrors[i] != nil {
		return "", a.sendErrors[i]
	}
	return fmt.Sprintf("tracking-%d", a.sends), nil
}

type senderFixture struct {
	st     *store.Store
	api    *senderAPI
	policy BroadcastPolicy
	sender *BroadcastSender
	now    time.Time
	b      store.Broadcast
	req    BroadcastSendRequest
}

func newSenderFixture(t *testing.T, n int) *senderFixture {
	t.Helper()
	st, src, sourceID := subscriptionFixture(t)
	tokens := make([]SubscriptionToken, n)
	for i := range tokens {
		tokens[i] = subscriptionToken(fmt.Sprintf("synthetic-mm-token-%d", i+1), strconv.Itoa(300+i))
	}
	_, err := SyncSubscriptions(t.Context(), st, subscriptionPage(tokens...), src, 0)
	require.NoError(t, err)
	subs := subscriptionRows(t, st, sourceID)
	ids := make([]int64, len(subs))
	for i := range subs {
		ids[i] = subs[i].ID
	}
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: sourceID, AdAccountID: "700", Name: "Example offers", MessageJSON: []byte(`{"text":"Example offer"}`), DailyBudgetCents: 500, CampaignDays: 7}, ids)
	require.NoError(t, err)
	f := &senderFixture{st: st, b: b, now: time.Now().UTC().Add(time.Second), policy: BroadcastPolicy{Source: src, AdAccountID: "700", Enabled: true, AllowSend: true, MaxDailyBudgetCents: 1000}}
	f.api = &senderAPI{permissions: map[string]bool{"ads_management": true, "pages_messaging": true, "marketing_messages_messenger": true}, page: "100"}
	f.resetSender()
	plan, err := f.sender.Plan(t.Context(), b.ID, b.Revision)
	require.NoError(t, err)
	assert.Equal(t, n, plan.Eligible)
	assert.Zero(t, f.api.preflights, "planning never calls provider")
	f.req = BroadcastSendRequest{ID: b.ID, Revision: b.Revision, PlanDigest: plan.PlanDigest}
	return f
}
func (f *senderFixture) resetSender() {
	f.sender = NewBroadcastSender(f.st, f.api, f.policy, func() time.Time { return f.now })
}
func (f *senderFixture) warm(t *testing.T) {
	t.Helper()
	r, err := f.sender.Send(t.Context(), f.req)
	require.ErrorIs(t, err, ErrCampaignWarming)
	assert.EqualValues(t, 3600, r.RetryAfterSeconds)
	assert.Zero(t, f.api.sends)
	f.now = f.now.Add(61 * time.Minute)
}
func (f *senderFixture) saved(t *testing.T) (store.Broadcast, []store.BroadcastRecipient) {
	t.Helper()
	b, rows, err := f.st.GetBroadcastContext(t.Context(), f.b.ID)
	require.NoError(t, err)
	return b, rows
}
func TestSendDisabledProfileNoCalls(t *testing.T) {
	for _, mode := range []string{"inactive", "no-send"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)

			f := newSenderFixture(t, 1)
			if mode == "inactive" {
				f.policy.Enabled = false
			} else {
				f.policy.AllowSend = false
			}
			f.resetSender()
			_, err := f.sender.Send(t.Context(), f.req)
			require.ErrorIs(t, err, ErrSendsDisabled)
			assertions.Zero(f.api.preflights)
			assertions.Zero(f.api.creates)
			assertions.Zero(f.api.sends)
		})
	}
}
func TestSendPermissionMissing(t *testing.T) {
	f := newSenderFixture(t, 1)
	delete(f.api.permissions, "marketing_messages_messenger")
	_, err := f.sender.Send(t.Context(), f.req)
	require.EqualError(t, err, "permission_missing:marketing_messages_messenger")
	assert.Zero(t, f.api.creates)
	f.api.permissions["paid_marketing_messages"] = true
	_, err = f.sender.Send(t.Context(), f.req)
	assert.ErrorIs(t, err, ErrCampaignWarming)
}
func TestSendPermissionPreflightDeniedFallsBackToFirstError(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.api.permissionErr = &APIError{Code: 200}
	f.api.createErr = &APIError{Code: 10}
	_, err := f.sender.Send(t.Context(), f.req)
	require.EqualError(t, err, "permission_missing:ads_management")
	assert.Equal(t, 1, f.api.creates)
	assert.Zero(t, f.api.sends)
}
func TestSendBudgetOverCap(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.policy.MaxDailyBudgetCents = 499
	f.resetSender()
	_, err := f.sender.Send(t.Context(), f.req)
	require.ErrorIs(t, err, ErrBudgetOverCap)
	assert.Zero(t, f.api.creates)
	assert.Zero(t, f.api.preflights)
}
func TestSendRefusesChangedRevisionOrDigest(t *testing.T) {
	for _, mode := range []string{"revision", "digest", "message", "source"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			f := newSenderFixture(t, 1)
			switch mode {
			case "revision":
				f.req.Revision++
			case "digest":
				f.req.PlanDigest = "other"
			case "message":
				_, err := f.st.UpdateBroadcastMessageContext(t.Context(), f.b.ID, f.b.Revision, []byte(`{"text":"Changed offer"}`))
				requirements.NoError(err)
			case "source":
				f.policy.Source.PageID = "101"
				f.resetSender()
			}
			_, err := f.sender.Send(t.Context(), f.req)
			requirements.ErrorIs(err, ErrPlanChanged)
			assertions.Zero(f.api.creates)
			assertions.Zero(f.api.sends)
		})
	}
}
func TestSendCreatesCampaignOnce(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.warm(t)
	for range 2 {
		_, err := f.sender.Send(t.Context(), f.req)
		require.NoError(t, err)
	}
	assert.Equal(t, 1, f.api.creates)
	assert.Equal(t, 1, f.api.sends)
}
func TestCampaignTimeoutBecomesUnknownNoRetry(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.api.createErr = context.DeadlineExceeded
	for range 2 {
		_, err := f.sender.Send(t.Context(), f.req)
		require.ErrorIs(t, err, ErrCampaignUnknown)
	}
	b, _ := f.saved(t)
	assert.Equal(t, "campaign_unknown", b.State)
	assert.Equal(t, 1, f.api.creates)
}
func TestCampaignClearRefusalReturnsToPlanned(t *testing.T) {
	for _, code := range []int{2300065, 200} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			f := newSenderFixture(t, 1)
			f.api.createErr = &APIError{Code: code}
			_, err := f.sender.Send(t.Context(), f.req)
			requirements.Error(err)
			b, _ := f.saved(t)
			assertions.Equal("planned", b.State)
			assertions.Empty(b.CampaignID)
			assertions.False(b.CampaignEndsAt.Valid)
			assertions.NotEmpty(b.LastError)
			f.api.createErr = nil
			_, err = f.sender.Send(t.Context(), f.req)
			requirements.ErrorIs(err, ErrCampaignWarming)
			assertions.Equal(2, f.api.creates)
		})
	}
}
func TestCampaignCreateSendsEndTime(t *testing.T) {
	f := newSenderFixture(t, 1)
	created := f.now
	f.warm(t)
	assert.Equal(t, created.Add(7*24*time.Hour), f.api.end)
}
func TestSendWarmupGate(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.warm(t)
	r, err := f.sender.Send(t.Context(), f.req)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Accepted)
}
func TestAdoptCampaignForeignRefused(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.api.createErr = context.DeadlineExceeded
	_, err := f.sender.Send(t.Context(), f.req)
	require.ErrorIs(t, err, ErrCampaignUnknown)
	f.api.page = "101"
	require.ErrorIs(t, f.sender.AdoptCampaign(t.Context(), f.b.ID, "56"), ErrCampaignForeign)
	b, _ := f.saved(t)
	assert.Equal(t, "campaign_unknown", b.State)
}
func TestAdoptSetsWarmup(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	f.api.createErr = context.DeadlineExceeded
	_, err := f.sender.Send(t.Context(), f.req)
	requirements.ErrorIs(err, ErrCampaignUnknown)
	requirements.NoError(f.sender.AdoptCampaign(t.Context(), f.b.ID, "56"))
	b, _ := f.saved(t)
	assertions.Equal("56", b.CampaignID)
	assertions.WithinDuration(f.now.Add(time.Hour), b.CampaignReadyAt.Time, time.Microsecond)
}
func TestSendPerRecipientOutcomes(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 4)
	f.warm(t)
	f.api.sendErrors = []error{nil, &APIError{Code: 1893047}, &APIError{Code: 2300052}, errors.New("provider-secret synthetic-mm-token-4")}
	r, err := f.sender.Send(t.Context(), f.req)
	requirements.Error(err)
	assertions.Equal(1, r.Accepted)
	assertions.Equal(2, r.Failed)
	assertions.Equal(1, r.Unknown)
	assertions.Equal("paused", r.State)
	_, rows := f.saved(t)
	assertions.Equal("recipient_unavailable", rows[1].Reason)
	assertions.Equal("remote_unknown", rows[3].State)
	events, err := f.st.LatestConsentEventsContext(t.Context(), []int64{rows[1].SubscriptionID})
	requirements.NoError(err)
	assertions.Equal("send_refused_recipient", events[rows[1].SubscriptionID][0].Kind)
	encoded, err := json.Marshal(r)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "synthetic-mm-token")
	assertions.NotContains(string(encoded), "provider-secret")
}
func TestSendLoadCapContinues(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 2)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Kind: "WEEKLY_MM_LOAD_CAP_REACHED"}, nil}
	r, err := f.sender.Send(t.Context(), f.req)
	require.NoError(t, err)
	assertions.Equal(1, r.Accepted)
	assertions.Equal(1, r.Failed)
	subs := subscriptionRows(t, f.st, f.b.SourceID)
	assertions.WithinDuration(f.now.Add(24*time.Hour), subs[0].NextEligibleAt.Time, time.Microsecond)
}
func TestSendPacingPausesWithRetryAfter(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 2)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Code: 2300068, RemainingSeconds: 42}}
	r, err := f.sender.Send(t.Context(), f.req)
	require.Error(t, err)
	assertions.EqualValues(42, r.RetryAfterSeconds)
	assertions.Equal(1, r.Remaining)
	assertions.Equal(1, f.api.sends)
}
func TestSendSpendLimitPausesNoRetryAfter(t *testing.T) {
	f := newSenderFixture(t, 2)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Code: 2300072}}
	r, err := f.sender.Send(t.Context(), f.req)
	require.Error(t, err)
	assert.Zero(t, r.RetryAfterSeconds)
	assert.Equal(t, "paused", r.State)
}
func TestSendUnclassifiedTripsBreaker(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 3)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Code: 99}, &APIError{Code: 99}}
	r, err := f.sender.Send(t.Context(), f.req)
	require.Error(t, err)
	assertions.Equal(1, f.api.sends)
	assertions.Equal(1, r.Failed)
	assertions.Equal(2, r.Remaining)
}
func TestSendRerunSkipsNonPlanned(t *testing.T) {
	f := newSenderFixture(t, 2)
	f.warm(t)
	f.req.Limit = 1
	for range 3 {
		_, err := f.sender.Send(t.Context(), f.req)
		require.NoError(t, err)
	}
	assert.Equal(t, 2, f.api.sends)
}
func TestSendRecomputeRefusesNewlyStopped(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	_, rows := f.saved(t)
	requirements.NoError(f.st.SetSubscriptionStatusContext(t.Context(), rows[0].SubscriptionID, "stopped", "stop", "test", "stop-1"))
	r, err := f.sender.Send(t.Context(), f.req)
	requirements.NoError(err)
	assertions.Equal(1, r.Refused)
	assertions.Zero(f.api.creates)
}
func TestSendAbandonedClaimBecomesUnknown(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 1)
	f.warm(t)
	_, rows := f.saved(t)
	claimed, err := f.st.ClaimRecipientContext(t.Context(), f.b.ID, rows[0].SubscriptionID, rows[0].RowRevision, f.now.Add(-3*time.Minute))
	requirements.NoError(err)
	requirements.True(claimed)
	r, err := f.sender.Send(t.Context(), f.req)
	requirements.NoError(err)
	assertions.Equal(1, r.Unknown)
	assertions.Zero(f.api.sends)
}
func TestSendLimitAndCancellation(t *testing.T) {
	assertions := assert.New(t)

	f := newSenderFixture(t, 3)
	f.warm(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.api.onSend = cancel
	r, err := f.sender.Send(ctx, f.req)
	require.ErrorIs(t, err, context.Canceled)
	assertions.Equal(1, f.api.sends)
	assertions.Equal(1, r.Accepted)
	assertions.Equal(2, r.Remaining)
}
func TestSendCredentialInvalidStops(t *testing.T) {
	f := newSenderFixture(t, 2)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Code: 190}}
	r, err := f.sender.Send(t.Context(), f.req)
	require.EqualError(t, err, "token_invalid")
	assert.Equal(t, "paused", r.State)
	assert.Equal(t, 1, f.api.sends)
}
func TestSendClaimsBeforeProviderWithoutOpenTransaction(t *testing.T) {
	f := newSenderFixture(t, 1)
	f.warm(t)
	f.api.onSend = func() { _, rows := f.saved(t); assert.Equal(t, "dispatching", rows[0].State) }
	_, err := f.sender.Send(t.Context(), f.req)
	require.NoError(t, err)
}

func TestSendResumedRunClearsFailure(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 2)
	f.warm(t)
	f.api.sendErrors = []error{&APIError{Code: 190}, nil}
	_, err := f.sender.Send(t.Context(), f.req)
	requirements.EqualError(err, "token_invalid")
	_, err = f.sender.Send(t.Context(), f.req)
	requirements.NoError(err)
	b, _ := f.saved(t)
	assertions.Equal("complete", b.State)
	assertions.Empty(b.LastError)
}

func TestSendCancellationDuringProviderPersistsUnknown(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	f := newSenderFixture(t, 2)
	f.warm(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.api.onSend = cancel
	f.api.sendErrors = []error{context.Canceled, nil}
	r, err := f.sender.Send(ctx, f.req)
	requirements.Error(err)
	assertions.Equal(1, r.Unknown)
	assertions.Equal(1, r.Remaining)
	f.api.onSend = nil
	r, err = f.sender.Send(t.Context(), f.req)
	requirements.NoError(err)
	assertions.Equal(1, r.Unknown)
	assertions.Equal(1, r.Accepted)
	assertions.Equal(2, f.api.sends)
}

func TestCampaignServerErrorNeverRecreates(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			f := newSenderFixture(t, 1)
			creates := 0
			f.sender.api = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"data":[{"permission":"ads_management","status":"granted"},{"permission":"pages_messaging","status":"granted"},{"permission":"marketing_messages_messenger","status":"granted"}]}`)
					return
				}
				creates++
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "upstream unavailable")
			})
			for range 2 {
				_, err := f.sender.Send(t.Context(), f.req)
				require.ErrorIs(t, err, ErrCampaignUnknown)
			}
			b, _ := f.saved(t)
			assert.Equal(t, "campaign_unknown", b.State)
			assert.Equal(t, 1, creates)
		})
	}
}

func TestAdoptCampaignEnforcesSpendAndExpiry(t *testing.T) {
	for _, mode := range []string{"matching", "higher-budget", "lower-budget", "lifetime-budget", "missing-budget", "expired", "missing-end", "extended-end"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			f := newSenderFixture(t, 1)
			f.api.createErr = context.DeadlineExceeded
			_, err := f.sender.Send(t.Context(), f.req)
			requirements.ErrorIs(err, ErrCampaignUnknown)
			b, _ := f.saved(t)
			assertions.True(b.CampaignEndsAt.Valid, "persist the requested end before the ambiguous POST")
			originalEnd := f.now.Add(7 * 24 * time.Hour)
			f.now = f.now.Add(24 * time.Hour)
			budget, lifetime := "500", "0"
			end := originalEnd.Add(-24 * time.Hour).Truncate(time.Second)
			switch mode {
			case "higher-budget":
				budget = "501"
			case "lower-budget":
				budget = "499"
			case "lifetime-budget":
				lifetime = "3500"
			case "missing-budget":
				budget = "0"
			case "expired":
				end = f.now.Add(-time.Hour)
			case "extended-end":
				end = originalEnd.Add(time.Hour)
			}
			endText := end.Format("2006-01-02T15:04:05-0700")
			if mode == "missing-end" {
				endText = ""
			}
			f.sender.api = fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = fmt.Fprintf(w, `{"id":"56","creative":{"actor_id":"100"},"adset":{"daily_budget":%q,"lifetime_budget":%q,"end_time":%q}}`, budget, lifetime, endText)
			})
			err = f.sender.AdoptCampaign(t.Context(), f.b.ID, "56")
			if mode == "matching" {
				requirements.NoError(err)
				b, _ = f.saved(t)
				assertions.WithinDuration(end, b.CampaignEndsAt.Time, time.Microsecond)
			} else {
				requirements.Error(err)
				b, _ = f.saved(t)
				assertions.Equal("campaign_unknown", b.State)
				assertions.Empty(b.CampaignID)
			}
		})
	}
}

func TestSendEarlyWebhookPreservesConclusiveReceipt(t *testing.T) {
	for kind, want := range map[string]string{"echo": "accepted", "delivered": "delivered", "read": "read", "click": "accepted", "delivery_failed": "delivery_failed"} {
		for _, timeout := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/timeout=%t", kind, timeout), func(t *testing.T) {
				assertions := assert.New(t)

				f := newSenderFixture(t, 1)
				f.warm(t)
				var rows []store.BroadcastRecipient
				sub := subscriptionRows(t, f.st, f.b.SourceID)[0]
				f.api.onSend = func() {
					ok, err := f.st.ApplyMarketingEventContext(t.Context(), store.MarketingEvent{SourceID: f.b.SourceID, Kind: kind, CampaignID: "55", Fingerprint: sub.Fingerprint, TrackingID: "early-tracking", MID: "early-mid"})
					require.NoError(t, err)
					require.True(t, ok)
				}
				if timeout {
					f.api.sendErrors = []error{context.DeadlineExceeded}
				}
				_, _ = f.sender.Send(t.Context(), f.req)
				_, rows = f.saved(t)
				assertions.Equal(want, rows[0].State)
				assertions.Equal("early-tracking", rows[0].TrackingID)
				if kind == "delivery_failed" {
					assertions.Equal("send_rejected", rows[0].Reason)
				} else {
					assertions.Empty(rows[0].Reason)
				}
				f.api.onSend = nil
				_, _ = f.sender.Send(t.Context(), f.req)
				assertions.Equal(1, f.api.sends)
			})
		}
	}
}
