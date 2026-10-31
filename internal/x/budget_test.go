package x

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// This credential seam isolates admission/retry decisions; real credential
// rotation and bearer integration are also exercised below against real HTTP.
type budgetCredentials struct {
	snapshot               CredentialSnapshot
	prepareErr, refreshErr error
	refreshes              int
}

func (c *budgetCredentials) Prepare(context.Context) (CredentialSnapshot, error) {
	return c.snapshot, c.prepareErr
}
func (c *budgetCredentials) Refresh(context.Context, int64) (CredentialSnapshot, error) {
	c.refreshes++
	if c.refreshErr != nil {
		return CredentialSnapshot{}, c.refreshErr
	}
	c.snapshot.Generation++
	return c.snapshot, nil
}
func (c *budgetCredentials) HTTPClient() *http.Client { return http.DefaultClient }

type budgetFixture struct {
	st     *store.Store
	source int64
	api    *admittedAPI
	creds  *budgetCredentials
	calls  atomic.Int32
	server *httptest.Server
}

func newBudgetFixture(t *testing.T, handler func(*budgetFixture, http.ResponseWriter, *http.Request), handles ...*store.Store) *budgetFixture {
	t.Helper()
	var handle *store.Store
	if len(handles) > 0 {
		handle = handles[0]
	} else {
		handle = testutil.NewTestStore(t)
	}
	f := &budgetFixture{st: handle}
	source, err := f.st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	f.source = source.ID
	generation := int64(1)
	_, err = f.st.UpdateXControlContext(t.Context(), f.source, 0, store.XControlPatch{OwnerID: "100", Generation: &generation})
	require.NoError(t, err)
	f.creds = &budgetCredentials{snapshot: CredentialSnapshot{OwnerID: "100", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if handler != nil {
			handler(f, w, r)
		} else {
			fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
		}
	}))
	t.Cleanup(f.server.Close)
	policy, err := config.NewDefaultConfig().XAccount("100")
	require.NoError(t, err)
	f.api = NewAdmittedAPI(f.st, f.source, NewClient(f.server.URL, f.server.Client()), policy, f.creds, ReadScope{}).(*admittedAPI)
	f.api.now = func() time.Time { return time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC) }
	f.api.wait = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return f
}
func (f *budgetFixture) state(t *testing.T) store.XControlState {
	t.Helper()
	s, err := f.st.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	return s
}
func (f *budgetFixture) archive(t *testing.T) store.XArchiveJob {
	t.Helper()
	now := f.api.now()
	job := store.XArchiveJob{ID: "archive-example", Fingerprint: "frozen-job", Username: "example_user", After: now.Add(-time.Hour), Before: now, CapMicros: 1_000_000,
		Own: &store.XWalk{Fingerprint: "own", Endpoint: "search_all", Query: "from:example_user -is:retweet"}, Mentions: &store.XWalk{Fingerprint: "mentions", Endpoint: "search_all", Query: "to:example_user -is:retweet"}}
	jobs := []store.XArchiveJob{job}
	s := f.state(t)
	_, err := f.st.UpdateXControlContext(t.Context(), f.source, s.Revision, store.XControlPatch{OwnerID: "100", Jobs: &jobs})
	require.NoError(t, err)
	f.api.scope = ReadScope{JobID: job.ID}
	return job
}

func TestRequestBudgetPrecedesHTTP(t *testing.T) {
	for _, budget := range []int64{74_000, 75_000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, r *http.Request) {
				state, err := f.st.LoadXControlContext(r.Context(), f.source)
				if !assert.NoError(t, err) {
					w.WriteHeader(500)
					return
				}
				assert.Equal(t, "5", r.URL.Query().Get("max_results"))
				assert.Equal(t, int64(75_000), state.SpendBuckets[0].ReservedMicros)
				assert.Len(t, state.Reservations, 1)
				fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			})
			f.api.policy.MonthlyBudgetMicros = budget
			page, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 100})
			if budget == 74_000 {
				require.ErrorIs(t, err, store.ErrXBudgetExceeded)
				assert.Zero(t, f.calls.Load())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int32(1), f.calls.Load())
			assert.NotEmpty(t, page.ReservationID)
			assert.Equal(t, 5, page.RequestedResults)
			assert.Equal(t, f.api.now(), page.FetchedAt)
			assert.Equal(t, int64(75_000), f.state(t).SpendBuckets[0].ReservedMicros)
		})
	}
}

func TestXOwnedRateIsEndpointScoped(t *testing.T) {
	for _, kind := range []string{"own", "mentions", "search", "lookup"} {
		for _, price := range []int64{-1, 5000, 7001, 0} {
			t.Run(fmt.Sprintf("%s/%d", kind, price), func(t *testing.T) {
				f := newBudgetFixture(t, nil)
				f.api.policy.OwnedReads = true
				if price >= 0 {
					f.api.policy.PostReadPriceMicros = price
					f.api.policy.PostReadPriceOverride = true
				}
				var err error
				switch kind {
				case "own", "mentions":
					_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", Mentions: kind == "mentions", MaxResults: 5})
				case "search":
					j := f.archive(t)
					_, err = f.api.SearchAll(t.Context(), SearchRequest{Query: j.Own.Query, After: j.After, Before: j.Before, MaxResults: 10})
				case "lookup":
					_, err = f.api.Lookup(t.Context(), []string{"9007199254740993"})
				}
				require.NoError(t, err)
				want := int64(5000)
				if kind == "own" || kind == "mentions" {
					want = 1000
				}
				if price >= 0 {
					want = price
				}
				reservation := f.state(t).Reservations[0]
				assert.Equal(t, want, reservation.PostPriceMicros)
				assert.Equal(t, int64(10000), reservation.UserPriceMicros)
			})
		}
	}
}

func TestXArchiveRetriesChargeSameJob(t *testing.T) {
	f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, r *http.Request) {
		state, err := f.st.LoadXControlContext(r.Context(), f.source)
		if !assert.NoError(t, err) {
			return
		}
		assert.Len(t, state.Reservations, int(f.calls.Load()))
		if f.calls.Load() == 1 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"title":"Transient"}`)
			return
		}
		fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
	})
	job := f.archive(t)
	job.CapMicros = 450_000
	jobs := []store.XArchiveJob{job}
	state := f.state(t)
	_, err := f.st.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Jobs: &jobs})
	require.NoError(t, err)
	f.api.policy.MonthlyBudgetMicros = 1_000_000
	for _, query := range []string{job.Own.Query, job.Mentions.Query} {
		_, err = f.api.SearchAll(t.Context(), SearchRequest{Query: query, After: job.After, Before: job.Before, MaxResults: 10})
		require.NoError(t, err)
	}
	assert.Equal(t, int32(3), f.calls.Load())
	state = f.state(t)
	assert.Equal(t, int64(450_000), state.Jobs[0].ReservedMicros)
	for _, r := range state.Reservations {
		assert.Equal(t, job.ID, r.JobID)
	}
	f.api.now = func() time.Time { return job.Before.Add(time.Hour) }
	f.creds.snapshot.Generation = 2
	_, err = f.api.SearchAll(t.Context(), SearchRequest{Query: job.Own.Query, After: job.After, Before: job.Before, MaxResults: 10})
	require.ErrorIs(t, err, store.ErrXBudgetExceeded)
	assert.Equal(t, int32(3), f.calls.Load())
}

func TestXReadRetryAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status                 int
		header                 string
		wantCalls, wantRefresh int
		wantWait               time.Duration
		wantErr                bool
	}{
		{"401 refresh", 401, "", 2, 1, 0, false},
		{"402 stop", 402, "", 1, 0, 0, true}, {"403 stop", 403, "", 1, 0, 0, true},
		{"500 retry", 500, "", 2, 0, time.Second, false},
		{"500 honors header", 500, "37", 2, 0, 37 * time.Second, false},
		{"429 valid seconds", 429, "37", 2, 0, 37 * time.Second, false},
		{"429 invalid fallback", 429, "bad", 2, 0, time.Second, false},
		{"429 over limit", 429, "901", 1, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, r *http.Request) {
				state, err := f.st.LoadXControlContext(r.Context(), f.source)
				if !assert.NoError(t, err) {
					return
				}
				assert.Len(t, state.Reservations, int(f.calls.Load()))
				if f.calls.Load() == 1 {
					w.Header().Set("Retry-After", tc.header)
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"title":"Synthetic failure"}`)
					return
				}
				fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			})
			var waits []time.Duration
			f.api.wait = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, int32(tc.wantCalls), f.calls.Load())
			assert.Equal(t, tc.wantRefresh, f.creds.refreshes)
			if tc.wantWait > 0 {
				require.Len(t, waits, 1)
				assert.Equal(t, tc.wantWait, waits[0])
			} else {
				assert.Empty(t, waits)
			}
			assert.Equal(t, int64(tc.wantCalls*75000), f.state(t).SpendBuckets[0].ReservedMicros)
		})
	}
}

func TestXReadRetryBoundsCancellationAndShape(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		status, want int
	}{
		{"second 401", `{"title":"Unauthorized"}`, 401, 2},
		{"bounded 5xx", `{"title":"Transient"}`, 503, 3},
		{"malformed success", `{`, 200, 1},
		{"shape discrepancy", `{"data":[` + strings.Repeat(rootJSON+",", 5) + rootJSON + `]}`, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
			require.Error(t, err)
			assert.Equal(t, int32(tc.want), f.calls.Load())
			if tc.name == "shape discrepancy" {
				_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
				require.ErrorIs(t, err, store.ErrXModelError)
				assert.Equal(t, int32(1), f.calls.Load())
			}
		})
	}
	t.Run("cancel wait", func(t *testing.T) {
		f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"title":"Wait"}`)
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f.api.wait = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
		_, err := f.api.Timeline(ctx, TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, int32(1), f.calls.Load())
	})
	t.Run("response loss", func(t *testing.T) {
		f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, _ *http.Request) {
			if f.calls.Load() == 1 {
				c, _, err := w.(http.Hijacker).Hijack()
				if assert.NoError(t, err) {
					assert.NoError(t, c.Close())
				}
				return
			}
			fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
		})
		_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.NoError(t, err)
		assert.Equal(t, int32(2), f.calls.Load())
		assert.Len(t, f.state(t).Reservations, 2)
	})
}

func TestXBudgetRefusesUnadmittedMethodsAndInvalidIdentity(t *testing.T) {
	f := newBudgetFixture(t, nil)
	_, err := f.api.Me(t.Context())
	require.ErrorIs(t, err, ErrConnectVerificationRequired)
	_, err = f.api.CreateReply(t.Context(), "Synthetic reply", "9007199254740993")
	require.ErrorIs(t, err, ErrSendAdmissionRequired)
	_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "101", MaxResults: 5})
	require.Error(t, err)
	f.creds.snapshot.GrantedScopes = []string{"tweet.read"}
	_, err = f.api.Lookup(t.Context(), []string{"9007199254740993"})
	require.Error(t, err)
	f.api.policy.MonthlyBudgetMicros = 0
	f.api.policy.PostReadPriceMicros = 0
	f.api.policy.UserReadPriceMicros = 0
	f.api.policy.PostReadPriceOverride = true
	_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(t, err, store.ErrXBudgetExceeded)
	assert.Zero(t, f.calls.Load())
	assert.Empty(t, f.state(t).Reservations)
}

func TestXLookupBudgetPinsActualRequestedIDs(t *testing.T) {
	f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "9007199254740993", r.URL.Query().Get("ids"))
		fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
	})
	f.api.policy.MonthlyBudgetMicros = 15_000
	ids := []string{"9007199254740993", "9007199254740994"}
	page, err := f.api.Lookup(t.Context(), ids)
	require.NoError(t, err)
	ids[0] = "9007199254740995"
	assert.Equal(t, []string{"9007199254740993"}, page.RequestedIDs)
	assert.Equal(t, 1, page.RequestedResults)
}

func TestXReadRateHeadersAndInvalidGrantGeneration(t *testing.T) {
	for _, dateHeader := range []bool{false, true} {
		t.Run(fmt.Sprint(dateHeader), func(t *testing.T) {
			now := time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)
			f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, _ *http.Request) {
				if f.calls.Load() == 1 {
					if dateHeader {
						w.Header().Set("Retry-After", now.Add(37*time.Second).Format(http.TimeFormat))
					} else {
						w.Header().Set("x-rate-limit-reset", strconv.FormatInt(now.Add(37*time.Second).Unix(), 10))
					}
					w.WriteHeader(429)
					fmt.Fprint(w, `{"title":"Wait"}`)
					return
				}
				fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			})
			f.api.wait = func(ctx context.Context, d time.Duration) error { assert.Equal(t, 37*time.Second, d); return nil }
			_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
			require.NoError(t, err)
			assert.Equal(t, int32(2), f.calls.Load())
		})
	}
	t.Run("old invalid grant", func(t *testing.T) {
		f := newBudgetFixture(t, nil)
		state := f.state(t)
		g := int64(2)
		_, err := f.st.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &g})
		require.NoError(t, err)
		f.creds.prepareErr = &InvalidGrantError{OwnerID: "100", Generation: 1}
		_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.Error(t, err)
		assert.Empty(t, f.state(t).CredentialError)
		f.creds.prepareErr = &InvalidGrantError{OwnerID: "100", Generation: 2}
		_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.Error(t, err)
		assert.Equal(t, "invalid_grant", f.state(t).CredentialError)
		assert.Zero(t, f.calls.Load())
	})
}

func TestXReadRefreshUsesPersistedBearerAndReservation(t *testing.T) {
	var manager *TokenManager
	var resources, tokens atomic.Int32
	f := newBudgetFixture(t, func(f *budgetFixture, w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			tokens.Add(1)
			assert.NoError(t, r.ParseForm())
			assert.Equal(t, "refresh-initial", r.Form.Get("refresh_token"))
			fmt.Fprint(w, `{"access_token":"access-rotated","refresh_token":"refresh-rotated","expires_in":7200,"token_type":"bearer","scope":"tweet.read users.read"}`)
			return
		}
		call := resources.Add(1)
		state, err := f.st.LoadXControlContext(r.Context(), f.source)
		if !assert.NoError(t, err) {
			return
		}
		assert.Len(t, state.Reservations, int(call))
		if call == 1 {
			assert.Equal(t, "Bearer access-initial", r.Header.Get("Authorization"))
			w.WriteHeader(401)
			fmt.Fprint(w, `{"title":"Unauthorized"}`)
			return
		}
		record, err := manager.Load(r.Context(), "100")
		if !assert.NoError(t, err) {
			return
		}
		assert.Equal(t, "refresh-rotated", record.RefreshToken())
		assert.Equal(t, record.Generation, state.Generation)
		assert.Equal(t, "Bearer access-rotated", r.Header.Get("Authorization"))
		fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
	})
	manager = NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: f.server.URL + "/token", HTTPClient: f.server.Client()})
	record := NewTokenRecord("100", "example-client", PublicClient, "access-initial", "refresh-initial", "")
	record.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
	record.Expiry = time.Now().Add(time.Hour)
	require.NoError(t, manager.Save(t.Context(), record))
	credentials := manager.Credentials("100", f.server.Client())
	f.api.credentials = credentials
	f.api.api = NewClient(f.server.URL, credentials.HTTPClient())
	_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resources.Load())
	assert.Equal(t, int32(1), tokens.Load())
	assert.Equal(t, int64(150_000), f.state(t).SpendBuckets[0].ReservedMicros)
}

func TestXReadPreservesAllResourcesAndIncompleteSettlement(t *testing.T) {
	f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[`+rootJSON+`,`+rootJSON+`,`+rootJSON+`],"includes":{"users":[{"id":"1001","username":"example_user","name":"Example User"}]},"errors":[{"resource_id":"9999","title":"Synthetic partial error"}]}`)
	})
	page, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.NoError(t, err)
	assert.Equal(t, 3, page.PostCount)
	assert.Equal(t, 1, page.UserCount)
	assert.Len(t, page.Posts, 3)
	assert.Len(t, page.Errors, 1)
	require.NoError(t, f.st.SettleXRequestContext(t.Context(), f.source, page.ReservationID, store.XResourceUsage{Posts: page.PostCount, Users: page.UserCount, Persisted: true}))
	assert.Equal(t, int64(75_000), f.state(t).SpendBuckets[0].ReservedMicros)
	// Partial/error billing remains unknown. Even a consumer that persisted all
	// valid objects must not claim Complete until billing resources are known.
	assert.Zero(t, f.state(t).SpendBuckets[0].SpentMicros)
}

func TestXReadRejectsInvalidArgumentsWithoutReservation(t *testing.T) {
	f := newBudgetFixture(t, nil)
	for _, r := range []TimelineRequest{{OwnerID: "100", MaxResults: 4}, {OwnerID: "100", MaxResults: 101}, {OwnerID: "100", MaxResults: 5, SinceID: "01"}, {OwnerID: "100", MaxResults: 5, SinceID: "1", After: f.api.now()}, {OwnerID: "100", MaxResults: 5, After: f.api.now(), Before: f.api.now()}} {
		_, err := f.api.Timeline(t.Context(), r)
		require.Error(t, err)
	}
	for _, ids := range [][]string{nil, {"01"}, {"1", "1"}} {
		_, err := f.api.Lookup(t.Context(), ids)
		require.Error(t, err)
	}
	_, err := f.api.SearchAll(t.Context(), SearchRequest{Query: "", MaxResults: 10})
	require.Error(t, err)
	_, err = f.api.SearchAll(t.Context(), SearchRequest{Query: "from:example_user", MaxResults: 10})
	require.ErrorIs(t, err, store.ErrXArchiveScope)
	assert.Zero(t, f.calls.Load())
	assert.Empty(t, f.state(t).Reservations)
}

func TestXReadCumulativeWaitAndCredentialDisable(t *testing.T) {
	t.Run("cumulative wait", func(t *testing.T) {
		f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "900")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"title":"Rate limited"}`)
		})
		waits := 0
		f.api.wait = func(context.Context, time.Duration) error { waits++; return nil }
		_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.ErrorIs(t, err, ErrReadRetryDelay)
		assert.Equal(t, 1, waits)
		assert.Equal(t, int32(2), f.calls.Load())
	})
	t.Run("second 401 disables only current generation", func(t *testing.T) {
		f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"title":"Unauthorized"}`)
		})
		_, err := f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.Error(t, err)
		assert.Equal(t, "unauthorized", f.state(t).CredentialError)
		_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
		require.Error(t, err)
		assert.Equal(t, int32(2), f.calls.Load())
		assert.Equal(t, 1, f.creds.refreshes)
	})
}

func TestXDisabledCredentialsPreflightBeforeRefresh(t *testing.T) {
	for _, floor := range []int64{1, 100} {
		t.Run(fmt.Sprint(floor), func(t *testing.T) {
			var tokenCalls, resourceCalls atomic.Int32
			f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					tokenCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"access_token":"implicit-refresh","refresh_token":"implicit-refresh-token","expires_in":7200,"token_type":"bearer","scope":"tweet.read users.read"}`)
					return
				}
				resourceCalls.Add(1)
				assert.Equal(t, "Bearer explicit-replacement", r.Header.Get("Authorization"))
				fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			})
			execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
			require.NoError(t, err)
			defer func() { assert.NoError(t, execution.Release()) }()
			manager := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: f.server.URL + "/token", HTTPClient: f.server.Client()})
			old := NewTokenRecord("100", "example-client", PublicClient, "old-access", "old-refresh", "")
			old.Expiry = time.Now().Add(-time.Hour)
			old.GrantedScopes = []string{"tweet.read", "users.read", "offline.access"}
			require.NoError(t, manager.WithLifecycleLock(t.Context(), "100", func() error { return manager.Save(t.Context(), old) }))
			state := f.state(t)
			_, err = f.st.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &floor})
			require.NoError(t, err)
			prior, err := f.st.ReserveXRequestContext(t.Context(), f.source, store.XRequestCost{Kind: store.XRequestTimeline, OwnerID: "100", Generation: floor, GrantedScopes: old.GrantedScopes, MonthlyBudgetMicros: 5_000_000, RequestedResults: 5, PostPriceMicros: 5000, UserPriceMicros: 10000, At: f.api.now()})
			require.NoError(t, err)
			require.NoError(t, f.st.RecordXCredentialErrorContext(t.Context(), f.source, "100", floor, "invalid_grant"))
			credentials := manager.Credentials("100", f.server.Client())
			f.api.credentials = credentials
			f.api.api = NewClient(f.server.URL, credentials.HTTPClient())
			_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
			stopped := assert.ErrorIs(t, err, store.ErrXBudgetIdentity)
			noTokens := assert.Zero(t, tokenCalls.Load(), "disabled credentials must be refused before expiring Prepare")
			noResources := assert.Zero(t, resourceCalls.Load())
			if !stopped || !noTokens || !noResources {
				return
			}
			assert.Equal(t, floor, f.state(t).Generation)
			assert.Equal(t, "invalid_grant", f.state(t).CredentialError)
			// A caller-asserted higher generation is not an explicit verified install.
			_, err = f.st.ReserveXRequestContext(t.Context(), f.source, store.XRequestCost{Kind: store.XRequestTimeline, OwnerID: "100", Generation: floor + 1, GrantedScopes: old.GrantedScopes, MonthlyBudgetMicros: 5_000_000, RequestedResults: 5, PostPriceMicros: 5000, UserPriceMicros: 10000, At: f.api.now()})
			require.ErrorIs(t, err, store.ErrXBudgetIdentity)
			replacement := NewTokenRecord("100", "example-client", PublicClient, "explicit-replacement", "explicit-refresh", "")
			replacement.GrantedScopes = old.GrantedScopes
			replacement.Expiry = time.Now().Add(time.Hour)
			require.NoError(t, manager.WithLifecycleLock(t.Context(), "100", func() error {
				state := f.state(t)
				file, err := manager.Load(t.Context(), "100")
				if err != nil {
					return err
				}
				installed, err := manager.InstallVerified(t.Context(), replacement, file.Generation, state.Generation)
				if err != nil {
					return err
				}
				assert.Equal(t, floor+1, installed.Generation)
				clear := ""
				_, err = f.st.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &installed.Generation, CredentialError: &clear})
				return err
			}))
			_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
			require.NoError(t, err)
			assert.Zero(t, tokenCalls.Load())
			assert.Equal(t, int32(1), resourceCalls.Load())
			state = f.state(t)
			assert.Equal(t, floor+1, state.Generation)
			assert.Empty(t, state.CredentialError)
			assert.Equal(t, int64(150_000), state.SpendBuckets[0].ReservedMicros)
			assert.Equal(t, prior.ID, state.Reservations[0].ID)
		})
	}
}

func TestXReadModelExcessPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model.db")
	handle := testutil.NewSQLiteTestStoreAt(t, path)
	var oversized atomic.Bool
	oversized.Store(true)
	f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
		if oversized.Load() {
			fmt.Fprint(w, `{"data":[`+strings.Repeat(rootJSON+",", 5)+rootJSON+`]}`)
		} else {
			fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
		}
	}, handle)
	state := f.state(t)
	walk := store.XWalk{Endpoint: "own", Fingerprint: "frozen-walk", Token: "page-2", BaseWatermark: "100", LowestCommittedID: "101"}
	_, err := handle.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Own: &walk})
	require.NoError(t, err)
	originalNow := f.api.now
	_, err = f.api.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(t, err, ErrReadModelExceeded)
	assert.Equal(t, int32(1), f.calls.Load())
	state = f.state(t)
	assert.Equal(t, store.XModelErrorResourceShape, state.ModelError)
	assert.Empty(t, state.CredentialError)
	require.Len(t, state.Reservations, 1)
	original := state.Reservations[0]
	require.NoError(t, handle.Close())
	reopened, err := store.OpenForTest(path)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	execution, err := reopened.AcquireSyncExecutionContext(t.Context(), f.source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	fresh := NewAdmittedAPI(reopened, f.source, NewClient(f.server.URL, f.server.Client()), f.api.policy, f.creds, ReadScope{}).(*admittedAPI)
	fresh.now = originalNow
	oversized.Store(false)
	_, err = fresh.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(t, err, store.ErrXModelError)
	assert.Equal(t, int32(1), f.calls.Load())
	// Credential registration and ordinary refresh generations cannot clear a
	// read-model hold. No credential error is manufactured for this condition.
	state, err = reopened.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	generation := int64(2)
	clear := ""
	state, err = reopened.UpdateXControlContext(t.Context(), f.source, state.Revision, store.XControlPatch{OwnerID: "100", Generation: &generation, CredentialError: &clear})
	require.NoError(t, err)
	f.creds.snapshot.Generation = 2
	_, err = fresh.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(t, err, store.ErrXModelError)
	assert.Equal(t, int32(1), f.calls.Load())
	require.ErrorIs(t, reopened.AcknowledgeXReadModelContext(t.Context(), f.source, "101", state.Revision), store.ErrXBudgetIdentity)
	require.ErrorIs(t, reopened.AcknowledgeXReadModelContext(t.Context(), f.source, "100", state.Revision-1), store.ErrXControlConflict)
	before := state
	require.NoError(t, reopened.AcknowledgeXReadModelContext(t.Context(), f.source, "100", state.Revision))
	after, err := reopened.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	assert.Empty(t, after.ModelError)
	assert.Equal(t, before.Generation, after.Generation)
	assert.Equal(t, before.Own, after.Own)
	assert.Equal(t, before.Reservations, after.Reservations)
	assert.Equal(t, before.SpendBuckets, after.SpendBuckets)
	assert.Equal(t, int64(75_000), after.SpendBuckets[0].ReservedMicros)
	// Acknowledgement itself is network free and does not settle the unknown read.
	assert.Equal(t, int32(1), f.calls.Load())
	_, err = fresh.Timeline(t.Context(), TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.NoError(t, err)
	after, err = reopened.LoadXControlContext(t.Context(), f.source)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.calls.Load())
	assert.Equal(t, int64(150_000), after.SpendBuckets[0].ReservedMicros)
	assert.Equal(t, original, after.Reservations[0])
	assert.Equal(t, &walk, after.Own)
}

func TestXOrdinaryShapeFailuresDoNotHoldReadModel(t *testing.T) {
	for _, tc := range []struct{ name, body, id string }{
		{"unexpected lookup ID", `{"data":[` + rootJSON + `]}`, "42"},
		{"malformed JSON", `{`, "9007199254740993"},
		{"unexpected author", `{"data":[` + rootJSON + `],"includes":{"users":[{"id":"1002","username":"example_other","name":"Example Other"}]}}`, "9007199254740993"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, tc.body) })
			for range 2 {
				_, err := f.api.Lookup(t.Context(), []string{tc.id})
				require.Error(t, err)
				assert.NotErrorIs(t, err, ErrReadModelExceeded)
			}
			assert.Equal(t, int32(2), f.calls.Load())
			assert.Empty(t, f.state(t).ModelError)
			assert.Empty(t, f.state(t).CredentialError)
		})
	}
}

type cancelAfterModelResponse struct {
	API
	cancel context.CancelFunc
}

func (c cancelAfterModelResponse) Timeline(ctx context.Context, r TimelineRequest) (Page, error) {
	page, err := c.API.Timeline(ctx, r)
	if errors.Is(err, ErrReadModelExceeded) {
		c.cancel()
	}
	return page, err
}

func TestXReadModelPersistsAfterCallerCancellation(t *testing.T) {
	f := newBudgetFixture(t, func(_ *budgetFixture, w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[`+strings.Repeat(rootJSON+",", 5)+rootJSON+`]}`)
	})
	execution, err := f.st.AcquireSyncExecutionContext(t.Context(), f.source)
	require.NoError(t, err)
	defer func() { assert.NoError(t, execution.Release()) }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.api.api = cancelAfterModelResponse{API: f.api.api, cancel: cancel}
	_, err = f.api.Timeline(ctx, TimelineRequest{OwnerID: "100", MaxResults: 5})
	require.ErrorIs(t, err, ErrReadModelExceeded)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, int32(1), f.calls.Load())
	state := f.state(t)
	assert.Equal(t, store.XModelErrorResourceShape, state.ModelError)
	assert.Equal(t, int64(75_000), state.SpendBuckets[0].ReservedMicros)
}
