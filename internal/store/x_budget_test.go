package store_test

import (
	"context"
	"encoding/json/v2"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func xBudgetFixture(t *testing.T) (*store.Store, int64, store.XRequestCost) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	generation := int64(1)
	_, err = st.UpdateXControlContext(t.Context(), source.ID, 0, store.XControlPatch{OwnerID: "100", Generation: &generation})
	require.NoError(t, err)
	return st, source.ID, store.XRequestCost{Kind: store.XRequestTimeline, OwnerID: "100", Generation: 1, GrantedScopes: []string{"tweet.read", "users.read"}, MonthlyBudgetMicros: 75_000, RequestedResults: 100, PostPriceMicros: 5_000, UserPriceMicros: 10_000, At: time.Date(2026, 10, 31, 23, 59, 0, 0, time.UTC)}
}

func TestXBudgetAdmissionAndSettlement(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	cost.MonthlyBudgetMicros = 74_000
	_, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Empty(state.Reservations)
	cost.MonthlyBudgetMicros = 75_000
	reservation, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testAssert.Equal(5, reservation.MaxPosts)
	_testAssert.Equal(5, reservation.MaxUsers)
	_testAssert.Equal(int64(75_000), reservation.AmountMicros)
	_testAssert.Equal("2026-10", reservation.Month)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{}))
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(75_000), state.SpendBuckets[0].ReservedMicros)
	_testRequire.ErrorIs(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{Posts: 6, Users: 1, Persisted: true, Complete: true}), store.ErrXReservationUsage)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_testRequire.Error(st.SettleXRequestContext(cancelled, id, reservation.ID, store.XResourceUsage{Posts: 2, Users: 1, Persisted: true, Complete: true}))
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{Posts: 2, Users: 1, Persisted: true, Complete: true}))
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{Posts: 5, Users: 5, Persisted: true, Complete: true}))
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(20_000), state.SpendBuckets[0].SpentMicros)
	_testAssert.Zero(state.SpendBuckets[0].ReservedMicros)
	_testAssert.Equal("settled", state.Reservations[0].State)
	// New admission compacts the receipt, never its original bucket's spend.
	cost.RequestedResults = 5
	cost.MonthlyBudgetMicros = 95_000
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{Posts: 5, Users: 5, Persisted: true, Complete: true}))
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(20_000), state.SpendBuckets[0].SpentMicros)
}

func TestBudgetSurvivesRestartAndUTCMonth(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	old, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	peer, err := store.OpenForTest(t.Context(), store.DBPathForTest(st))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, peer.Close()) }()
	cost.At = cost.At.Add(2 * time.Minute)
	cost.Generation = 2
	fresh, err := peer.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testAssert.Equal("2026-11", fresh.Month)
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testRequire.Len(state.SpendBuckets, 2)
	_testAssert.Equal(int64(75_000), state.SpendBuckets[0].ReservedMicros)
	_testAssert.Equal("2026-10", state.Reservations[0].Month)
	_testAssert.Equal(int64(2), state.Generation)
	_testRequire.NoError(peer.SettleXRequestContext(t.Context(), id, old.ID, store.XResourceUsage{Posts: 5, Users: 0, Persisted: true, Complete: true}))
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(25_000), state.SpendBuckets[0].SpentMicros)
	_testAssert.Zero(state.SpendBuckets[0].ReservedMicros)
	_testAssert.Equal(int64(75_000), state.SpendBuckets[1].ReservedMicros)
}

func TestXBudgetEmptyPollsPreserveCostAcrossRollover(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, id, cost := xBudgetFixture(t)
	state, err := st.LoadXControlContext(t.Context(), id)
	requirements.NoError(err)
	job := store.XArchiveJob{ID: "archive-empty", Fingerprint: "frozen", Username: "example_user", After: cost.At.Add(-24 * time.Hour), Before: cost.At, CapMicros: 30_000_000,
		Own:      &store.XWalk{Fingerprint: "own", Endpoint: "search_all", Query: "from:example_user -is:retweet", After: cost.At.Add(-24 * time.Hour), Before: cost.At},
		Mentions: &store.XWalk{Fingerprint: "mentions", Endpoint: "search_all", Query: "to:example_user -is:retweet", After: cost.At.Add(-24 * time.Hour), Before: cost.At}}
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Jobs: &[]store.XArchiveJob{job}})
	requirements.NoError(err)

	cost.MonthlyBudgetMicros = 30_000_000
	send := cost
	send.Kind, send.RequestedResults, send.CreateReservationMicros, send.AllowSend = store.XRequestCreate, 0, 200_000, true
	send.GrantedScopes = append(send.GrantedScopes, "tweet.write")
	uncertain, err := st.ReserveXRequestContext(t.Context(), id, send)
	requirements.NoError(err)
	requirements.NoError(st.SettleXRequestContext(t.Context(), id, uncertain.ID, store.XResourceUsage{Persisted: true, Complete: true}))
	cost.RequestedResults = 5
	cost.JobID, cost.JobFingerprint = job.ID, job.Fingerprint
	const perMonth = 130
	for range 2 {
		for range perMonth {
			reservation, err := st.ReserveXRequestContext(t.Context(), id, cost)
			requirements.NoError(err)
			requirements.NoError(st.SettleXRequestContext(t.Context(), id, reservation.ID, store.XResourceUsage{Persisted: true, Complete: true}))
		}
		cost.At = cost.At.Add(2 * time.Minute)
	}
	state, err = st.LoadXControlContext(t.Context(), id)
	requirements.NoError(err)
	requirements.Len(state.SpendBuckets, 2)
	assertions.Equal(int64(perMonth*75_000), state.SpendBuckets[0].SpentMicros)
	assertions.Equal(int64(200_000), state.SpendBuckets[0].ReservedMicros)
	assertions.Equal(int64(perMonth*75_000), state.SpendBuckets[1].SpentMicros)
	assertions.Zero(state.SpendBuckets[1].ReservedMicros)
	requirements.Len(state.Jobs, 1)
	assertions.Equal(int64(2*perMonth*75_000), state.Jobs[0].SpentMicros)
	assertions.Zero(state.Jobs[0].ReservedMicros)
	requirements.Len(state.Reservations, 2)
	assertions.Equal(uncertain.ID, state.Reservations[0].ID)
	assertions.Equal("pending", state.Reservations[0].State)
}

func TestXBudgetConcurrentAdmissionHasOneWinner(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	peer, err := store.OpenForTest(t.Context(), store.DBPathForTest(st))
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, peer.Close()) }()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, handle := range []*store.Store{st, peer} {
		wg.Go(func() {
			<-start
			_, err := handle.ReserveXRequestContext(t.Context(), id, cost)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
		}
	}
	_testAssert.Equal(1, winners)
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Len(state.Reservations, 1)
	_testAssert.Equal(int64(75_000), state.SpendBuckets[0].ReservedMicros)
}

func TestXBudgetRejectsInvalidIdentityCostAndZero(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, base := xBudgetFixture(t)
	for _, tc := range []struct {
		name   string
		change func(*store.XRequestCost)
	}{
		{"wrong owner", func(c *store.XRequestCost) { c.OwnerID = "101" }},
		{"stale generation", func(c *store.XRequestCost) { c.Generation = 0 }},
		{"missing scopes", func(c *store.XRequestCost) { c.GrantedScopes = nil }},
		{"negative budget", func(c *store.XRequestCost) { c.MonthlyBudgetMicros = -1 }},
		{"negative price", func(c *store.XRequestCost) { c.PostPriceMicros = -1 }},
		{"overflow unit", func(c *store.XRequestCost) { c.PostPriceMicros = math.MaxInt64 }},
		{"invalid kind", func(c *store.XRequestCost) { c.Kind = "anything" }},
		{"too few results", func(c *store.XRequestCost) { c.RequestedResults = 4 }},
		{"too many results", func(c *store.XRequestCost) { c.RequestedResults = 101 }},
		{"zero budget and free estimates", func(c *store.XRequestCost) { c.MonthlyBudgetMicros = 0; c.PostPriceMicros = 0; c.UserPriceMicros = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := base
			tc.change(&cost)
			_, err := st.ReserveXRequestContext(t.Context(), id, cost)
			require.Error(t, err)
		})
	}
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Empty(state.Reservations)
	base.MonthlyBudgetMicros = 1
	base.PostPriceMicros = 0
	base.UserPriceMicros = 0
	res, err := st.ReserveXRequestContext(t.Context(), id, base)
	_testRequire.NoError(err)
	_testAssert.Equal(100, res.MaxPosts)
	_testAssert.Zero(res.AmountMicros)
}

func TestXBudgetArchiveCapAndFingerprint(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	after := cost.At.Add(-24 * time.Hour)
	before := cost.At
	jobs := []store.XArchiveJob{{ID: "archive-1", Fingerprint: "frozen-job", Username: "example_user", After: after, Before: before, CapMicros: 300_000,
		Own:      &store.XWalk{Fingerprint: "own", Endpoint: "search_all", Query: "from:example_user -is:retweet", After: after, Before: before},
		Mentions: &store.XWalk{Fingerprint: "mentions", Endpoint: "search_all", Query: "to:example_user -is:retweet", After: after, Before: before}}}
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Jobs: &jobs})
	_testRequire.NoError(err)
	cost.Kind = store.XRequestSearch
	cost.RequestedResults = 10
	cost.MonthlyBudgetMicros = 1_000_000
	cost.JobID = "archive-1"
	cost.JobFingerprint = "wrong"
	cost.After = after
	cost.Before = before
	cost.Query = jobs[0].Own.Query
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXArchiveScope)
	cost.JobFingerprint = "frozen-job"
	first, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	cost.Query = jobs[0].Mentions.Query
	second, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testAssert.Equal(first.JobID, second.JobID)
	cost.At = cost.At.Add(time.Hour)
	cost.Generation = 2
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, first.ID, store.XResourceUsage{Posts: 10, Users: 0, Persisted: true, Complete: true}))
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXBudgetExceeded)
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(50_000), state.Jobs[0].SpentMicros)
	_testAssert.Equal(int64(150_000), state.Jobs[0].ReservedMicros)
	cost.Query = "from:other_user -is:retweet"
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXArchiveScope)
}

func TestXBudgetCapacityPreservesUnknownAndSendReservation(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	reservations := make([]store.XReservation, store.XMaxReservations)
	for i := range reservations {
		reservations[i] = store.XReservation{ID: strconv.Itoa(i), Kind: "lookup", Month: "2026-10", State: "unknown"}
	}
	buckets := []store.XSpendBucket{{Month: "2026-10"}}
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Reservations: &reservations, SpendBuckets: &buckets})
	_testRequire.NoError(err)
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXBudgetCapacity)
	state, err = st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(reservations, state.Reservations)
	reservations[0].State = "settled"
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Reservations: &reservations})
	_testRequire.NoError(err)
	cost.Kind = store.XRequestCreate
	cost.RequestedResults = 0
	cost.MonthlyBudgetMicros = 200_000
	cost.CreateReservationMicros = 200_000
	cost.AllowSend = true
	cost.GrantedScopes = append(cost.GrantedScopes, "tweet.write")
	res, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testAssert.Equal(int64(200_000), res.AmountMicros)
	_testAssert.Zero(res.MaxPosts)
}

func TestXBudgetModelAcknowledgementDoesNotUnfenceSend(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), id)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	cost.MonthlyBudgetMicros = 1_000_000
	cost.RequestedResults = 5
	sendCost := cost
	sendCost.Kind = store.XRequestCreate
	sendCost.RequestedResults = 0
	sendCost.AllowSend = true
	sendCost.CreateReservationMicros = 200_000
	sendCost.GrantedScopes = append(sendCost.GrantedScopes, "tweet.write")
	send, err := st.ReserveXRequestContext(t.Context(), id, sendCost)
	_testRequire.NoError(err)
	// Schema fixture: Task14 owns attempt admission; this existing uncertain row
	// proves a read-model acknowledgement cannot erase its separate send fence.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,reserved_micros) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), id, "100", "model-draft", "attempt-1", 1, "201", "200", "synthetic-digest", "remote_unknown", send.ID, send.AmountMicros)
	_testRequire.NoError(err)
	read, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.RecordXModelErrorContext(t.Context(), id, "100", read.ID))
	before, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(store.XModelErrorResourceShape, before.ModelError)
	_testRequire.NoError(st.AcknowledgeXReadModelContext(t.Context(), id, "100", before.Revision))
	after, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	_testAssert.Equal(before.Reservations, after.Reservations)
	_testAssert.Equal(before.SpendBuckets, after.SpendBuckets)
	_testAssert.Equal(int64(275_000), after.SpendBuckets[0].ReservedMicros)
	var state string
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT state FROM x_reply_attempts WHERE source_id=? AND draft_id=? AND attempt_id=?"), id, "model-draft", "attempt-1").Scan(&state))
	_testAssert.Equal("remote_unknown", state)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id) VALUES(?,?,?,?,?,?,?,?,?,?)`), id, "100", "model-draft", "attempt-2", 1, "201", "200", "synthetic-digest", "admitted", "new-attempt")
	_testRequire.Error(err)
}

func TestXBudgetReservesCapacityForDurableModelHold(t *testing.T) {
	_testRequire := require.New(t)

	st, id, cost := xBudgetFixture(t)
	cost.MonthlyBudgetMicros = 5_000_000
	admitted, err := st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.NoError(err)
	_testRequire.NoError(st.SettleXRequestContext(t.Context(), id, admitted.ID, store.XResourceUsage{Posts: 5, Users: 5, Persisted: true, Complete: true}))
	state, err := st.LoadXControlContext(t.Context(), id)
	_testRequire.NoError(err)
	state.Own = &store.XWalk{Fingerprint: "capacity-window", Endpoint: "own"}
	encoded, err := json.Marshal(state)
	_testRequire.NoError(err)
	state.Own.Query = strings.Repeat("x", store.XMaxControlBytes-len(encoded)-20)
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Own: state.Own})
	_testRequire.NoError(err)
	// A settled receipt can be compacted/replaced within the byte limit, but
	// admitting I/O here would leave no space for its required durable hold.
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	_testRequire.ErrorIs(err, store.ErrXBudgetCapacity)
}

func TestXReadAdmissionRequiresRoomForNextPage(t *testing.T) {
	checks := assert.New(t)
	needs := require.New(t)
	st, id, cost := xBudgetFixture(t)
	state, err := st.LoadXControlContext(t.Context(), id)
	needs.NoError(err)

	// Required current tokens alone leave less than one maximally escaped
	// response token's space. They and the frozen jobs must never be pruned.
	token := strings.Repeat("a", 8192)
	jobs := make([]store.XArchiveJob, 60)
	for i := range jobs {
		jobs[i] = store.XArchiveJob{ID: "job-" + strconv.Itoa(i), Fingerprint: "frozen", CapMicros: 1000000,
			Own:      &store.XWalk{Fingerprint: "own", Endpoint: "search", Token: token},
			Mentions: &store.XWalk{Fingerprint: "mentions", Endpoint: "search", Token: token}}
	}
	_, err = st.UpdateXControlContext(t.Context(), id, state.Revision, store.XControlPatch{OwnerID: "100", Jobs: &jobs})
	needs.NoError(err)
	before, err := st.LoadXControlContext(t.Context(), id)
	needs.NoError(err)
	_, err = st.ReserveXRequestContext(t.Context(), id, cost)
	needs.ErrorIs(err, store.ErrXBudgetCapacity)
	after, err := st.LoadXControlContext(t.Context(), id)
	needs.NoError(err)
	checks.Equal(before, after)
	checks.Len(after.Jobs, 60)
	checks.Equal(token, after.Jobs[59].Mentions.Token)
	checks.Empty(after.Reservations)
}
