package store

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"math"
	"slices"
	"time"
)

const (
	XRequestTimeline = "timeline"
	XRequestMentions = "mentions"
	XRequestSearch   = "search_all"
	XRequestLookup   = "lookup"
	XRequestCreate   = "create_reply"
)

var (
	ErrXModelError       = errors.New("X read resource model requires explicit owner acknowledgement")
	ErrXBudgetExceeded   = errors.New("X request allowance exhausted")
	ErrXBudgetCapacity   = errors.New("X budget authority has reached capacity")
	ErrXBudgetIdentity   = errors.New("X request owner, credentials or scopes are invalid")
	ErrXBudgetState      = errors.New("X budget authority is inconsistent")
	ErrXRequestCost      = errors.New("invalid X request cost or shape")
	ErrXReservationUsage = errors.New("X usage exceeds or contradicts its reservation")
	ErrXArchiveScope     = errors.New("X archive job identity or frozen request does not match")
)

// XRequestCost is trusted daemon policy and a verified credential snapshot.
// It contains no token and must not be decoded from an untrusted API request.
// Search requests require a persisted archive job; its fingerprint is pinned by
// the read wrapper. All user expansions are bounded by RequestedResults.
type XRequestCost struct {
	Kind, OwnerID                    string
	Generation                       int64
	GrantedScopes                    []string
	MonthlyBudgetMicros              int64
	RequestedResults                 int
	PostPriceMicros, UserPriceMicros int64
	CreateReservationMicros          int64
	AllowSend                        bool
	JobID, JobFingerprint, Query     string
	After, Before, At                time.Time
}

// XResourceUsage is supplied only after durable response effects. Complete
// means all billable resources are known; partial/empty/error billing is not
// known. Counts include duplicates and deliberately excluded/suppressed objects.
// A durably adopted create uses Created and retains the full create estimate.
type XResourceUsage struct {
	Posts, Users                 int
	Persisted, Complete, Created bool
}

// ReserveXRequestContext commits cost and bounded shape before provider I/O.
func (s *Store) ReserveXRequestContext(ctx context.Context, sourceID int64, spec XRequestCost) (XReservation, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return XReservation{}, err
	}
	var result XReservation
	err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		var err error
		result, err = s.reserveXRequestTx(ctx, tx, sourceID, spec)
		return err
	})
	if err != nil {
		return XReservation{}, err
	}
	return result, nil
}

// reserveXRequestTx shares a caller's draft admission transaction. Its caller
// locks the source before the scoped sync-generation fence, then takes the
// draft lock in the established attribution/draft order. It never begins a
// transaction or performs network I/O.
func (s *Store) reserveXRequestTx(ctx context.Context, tx *loggedTx, sourceID int64, spec XRequestCost) (XReservation, error) {
	if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
		return XReservation{}, err
	}
	state, err := s.loadXControlTx(ctx, tx, sourceID)
	if err != nil {
		return XReservation{}, err
	}
	if spec.OwnerID != state.OwnerID || spec.Generation <= 0 || spec.Generation < state.Generation || !slices.Contains(spec.GrantedScopes, "tweet.read") || !slices.Contains(spec.GrantedScopes, "users.read") {
		return XReservation{}, ErrXBudgetIdentity
	}
	if state.CredentialError != "" {
		return XReservation{}, ErrXBudgetIdentity
	}
	if state.ModelError != "" && spec.Kind != XRequestCreate {
		return XReservation{}, ErrXModelError
	}
	minimum, maximum := 0, 0
	switch spec.Kind {
	case XRequestTimeline, XRequestMentions:
		minimum, maximum = 5, 100
	case XRequestSearch:
		minimum, maximum = 10, 500
	case XRequestLookup:
		minimum, maximum = 1, 100
	case XRequestCreate:
		if !spec.AllowSend || !slices.Contains(spec.GrantedScopes, "tweet.write") {
			return XReservation{}, ErrXBudgetIdentity
		}
		if spec.CreateReservationMicros < 200_000 || spec.RequestedResults != 0 {
			return XReservation{}, ErrXRequestCost
		}
	default:
		return XReservation{}, ErrXRequestCost
	}
	if spec.At.IsZero() || !validXMonth(spec.At.UTC().Format("2006-01")) || spec.MonthlyBudgetMicros < 0 || spec.PostPriceMicros < 0 || spec.UserPriceMicros < 0 || (minimum > 0 && (spec.RequestedResults < minimum || spec.RequestedResults > maximum)) {
		return XReservation{}, ErrXRequestCost
	}
	if spec.MonthlyBudgetMicros == 0 {
		return XReservation{}, ErrXBudgetExceeded
	}
	if err := validateXBudgetState(state); err != nil {
		return XReservation{}, err
	}
	jobIndex := -1
	if spec.Kind == XRequestSearch && spec.JobID == "" {
		return XReservation{}, ErrXArchiveScope
	}
	if spec.JobID != "" {
		for i := range state.Jobs {
			if state.Jobs[i].ID == spec.JobID {
				jobIndex = i
				break
			}
		}
		if jobIndex < 0 {
			return XReservation{}, ErrXArchiveScope
		}
		job := state.Jobs[jobIndex]
		if job.Complete || spec.JobFingerprint == "" || job.Fingerprint != spec.JobFingerprint {
			return XReservation{}, ErrXArchiveScope
		}
		if spec.Kind == XRequestSearch {
			matches := false
			for _, walk := range []*XWalk{job.Own, job.Mentions} {
				if walk != nil && walk.Query != "" && spec.Query == walk.Query {
					matches = true
				}
			}
			if !matches || !spec.After.Equal(job.After) || !spec.Before.Equal(job.Before) {
				return XReservation{}, ErrXArchiveScope
			}
		}
	}
	month := spec.At.UTC().Format("2006-01")
	bucketIndex := -1
	for i := range state.SpendBuckets {
		if state.SpendBuckets[i].Month == month {
			bucketIndex = i
			break
		}
	}
	if bucketIndex < 0 {
		if len(state.SpendBuckets) >= XMaxSpendBuckets {
			return XReservation{}, ErrXBudgetCapacity
		}
		state.SpendBuckets = append(state.SpendBuckets, XSpendBucket{Month: month})
		bucketIndex = len(state.SpendBuckets) - 1
	}
	bucket := state.SpendBuckets[bucketIndex]
	total, err := xBudgetAdd(bucket.SpentMicros, bucket.ReservedMicros)
	if err != nil {
		return XReservation{}, err
	}
	remaining := spec.MonthlyBudgetMicros - total
	if remaining < 0 {
		return XReservation{}, ErrXBudgetExceeded
	}
	if jobIndex >= 0 {
		job := state.Jobs[jobIndex]
		total, err := xBudgetAdd(job.SpentMicros, job.ReservedMicros)
		if err != nil {
			return XReservation{}, err
		}
		if job.CapMicros == 0 || total > job.CapMicros {
			return XReservation{}, ErrXBudgetExceeded
		}
		remaining = min(remaining, job.CapMicros-total)
	}
	requested, amount := spec.RequestedResults, spec.CreateReservationMicros
	if minimum > 0 {
		unit, err := xBudgetAdd(spec.PostPriceMicros, spec.UserPriceMicros)
		if err != nil {
			return XReservation{}, ErrXRequestCost
		}
		if unit > 0 {
			requested = int(min(int64(requested), remaining/unit))
		}
		if requested < minimum {
			return XReservation{}, ErrXBudgetExceeded
		}
		amount, err = xBudgetMultiply(unit, requested)
		if err != nil {
			return XReservation{}, ErrXRequestCost
		}
	} else if amount > remaining {
		return XReservation{}, ErrXBudgetExceeded
	}
	// Only settled receipts may disappear. Their costs already live in the
	// original bucket/job totals; unknown obligations are never evicted.
	state.Reservations = slices.DeleteFunc(state.Reservations, func(r XReservation) bool { return r.State == "settled" })
	if len(state.Reservations) >= XMaxReservations {
		return XReservation{}, ErrXBudgetCapacity
	}
	reservation := XReservation{ID: "xreq_" + rand.Text(), Kind: spec.Kind, Month: month, JobID: spec.JobID, State: "pending", AmountMicros: amount}
	if minimum > 0 {
		reservation.MaxPosts = requested
		reservation.MaxUsers = requested
		reservation.PostPriceMicros = spec.PostPriceMicros
		reservation.UserPriceMicros = spec.UserPriceMicros
	}
	state.SpendBuckets[bucketIndex].ReservedMicros, err = xBudgetAdd(bucket.ReservedMicros, amount)
	if err != nil {
		return XReservation{}, err
	}
	if jobIndex >= 0 {
		state.Jobs[jobIndex].ReservedMicros, err = xBudgetAdd(state.Jobs[jobIndex].ReservedMicros, amount)
		if err != nil {
			return XReservation{}, err
		}
	}
	state.Reservations = append(state.Reservations, reservation)
	// Admission must leave enough bounded authority to record its outcome.
	// In particular an observed model excess must not fail merely because this
	// request consumed the last bytes/revision needed for its durable read hold.
	if state.Revision > math.MaxInt64-2 {
		return XReservation{}, ErrXBudgetCapacity
	}
	capacityProbe := state
	capacityProbe.Revision += 2
	capacityProbe.Generation = spec.Generation
	if spec.Kind != XRequestCreate {
		capacityProbe.ModelError = XModelErrorResourceShape
	}
	encoded, err := json.Marshal(capacityProbe)
	if err != nil {
		return XReservation{}, err
	}
	if len(encoded) > XMaxControlBytes {
		return XReservation{}, ErrXBudgetCapacity
	}
	patch := XControlPatch{OwnerID: state.OwnerID, SpendBuckets: &state.SpendBuckets, Reservations: &state.Reservations, Jobs: &state.Jobs}
	if spec.Generation > state.Generation {
		patch.Generation = &spec.Generation
	}
	if _, err = s.updateXControlTx(ctx, tx, sourceID, state.Revision, patch); err != nil {
		return XReservation{}, err
	}
	return reservation, nil
}

// SettleXRequestContext uses the reservation's immutable admitted prices and
// original month/job. An absent, already compacted receipt is a no-op: it can
// never release money. Pending/unknown entries are never compacted.
func (s *Store) SettleXRequestContext(ctx context.Context, sourceID int64, reservationID string, usage XResourceUsage) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error { return s.settleXRequestTx(ctx, tx, sourceID, reservationID, usage) })
}

// settleXRequestTx is reusable by atomic message/receipt finalization.
func (s *Store) settleXRequestTx(ctx context.Context, tx *loggedTx, sourceID int64, id string, usage XResourceUsage) error {
	state, err := s.loadXControlTx(ctx, tx, sourceID)
	if err != nil {
		return err
	}
	if err := validateXBudgetState(state); err != nil {
		return err
	}
	index := -1
	for i := range state.Reservations {
		if state.Reservations[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	reservation := state.Reservations[index]
	if reservation.State == "settled" {
		return nil
	}
	if usage.Posts < 0 || usage.Users < 0 || usage.Posts > reservation.MaxPosts || usage.Users > reservation.MaxUsers || (usage.Created && reservation.Kind != XRequestCreate) {
		return ErrXReservationUsage
	}
	if !usage.Persisted || !usage.Complete {
		return nil
	}
	actual := reservation.AmountMicros
	if reservation.Kind == XRequestCreate {
		if !usage.Created {
			return nil
		}
	} else {
		if usage.Posts == 0 && usage.Users == 0 {
			return nil
		}
		posts, err := xBudgetMultiply(reservation.PostPriceMicros, usage.Posts)
		if err != nil {
			return ErrXReservationUsage
		}
		users, err := xBudgetMultiply(reservation.UserPriceMicros, usage.Users)
		if err != nil {
			return ErrXReservationUsage
		}
		actual, err = xBudgetAdd(posts, users)
		if err != nil || actual > reservation.AmountMicros {
			return ErrXReservationUsage
		}
	}
	for i := range state.SpendBuckets {
		if state.SpendBuckets[i].Month == reservation.Month {
			state.SpendBuckets[i].ReservedMicros -= reservation.AmountMicros
			state.SpendBuckets[i].SpentMicros, err = xBudgetAdd(state.SpendBuckets[i].SpentMicros, actual)
			if err != nil {
				return err
			}
		}
	}
	for i := range state.Jobs {
		if state.Jobs[i].ID == reservation.JobID {
			state.Jobs[i].ReservedMicros -= reservation.AmountMicros
			state.Jobs[i].SpentMicros, err = xBudgetAdd(state.Jobs[i].SpentMicros, actual)
			if err != nil {
				return err
			}
		}
	}
	state.Reservations[index].State = "settled"
	_, err = s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: state.OwnerID, Reservations: &state.Reservations, SpendBuckets: &state.SpendBuckets, Jobs: &state.Jobs})
	return err
}

// RecordXCredentialErrorContext records a sanitized credential failure for only the
// failing generation. A late failure cannot disable replacement credentials.
func (s *Store) RecordXCredentialErrorContext(ctx context.Context, sourceID int64, ownerID string, generation int64, code string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	if code != "invalid_grant" && code != "unauthorized" {
		return ErrXBudgetIdentity
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		state, err := s.loadXControlTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if ownerID != state.OwnerID {
			return ErrXBudgetIdentity
		}
		if generation != state.Generation {
			return nil
		}
		_, err = s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: ownerID, CredentialError: &code})
		return err
	})
}

func xBudgetAdd(a, b int64) (int64, error) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, ErrXBudgetState
	}
	return a + b, nil
}
func xBudgetMultiply(price int64, count int) (int64, error) {
	if price < 0 || count < 0 || (count > 0 && price > math.MaxInt64/int64(count)) {
		return 0, ErrXBudgetState
	}
	return price * int64(count), nil
}

// Check that a damaged or unrelated cursor patch cannot erase live costs.
func validateXBudgetState(state XControlState) error {
	months := map[string]int64{}
	jobs := map[string]int64{}
	for _, r := range state.Reservations {
		if r.State != "settled" {
			var err error
			months[r.Month], err = xBudgetAdd(months[r.Month], r.AmountMicros)
			if err != nil {
				return err
			}
			if r.JobID != "" {
				jobs[r.JobID], err = xBudgetAdd(jobs[r.JobID], r.AmountMicros)
				if err != nil {
					return err
				}
			}
		}
	}
	for _, b := range state.SpendBuckets {
		if b.ReservedMicros != months[b.Month] {
			return ErrXBudgetState
		}
		if _, err := xBudgetAdd(b.SpentMicros, b.ReservedMicros); err != nil {
			return err
		}
		delete(months, b.Month)
	}
	for _, j := range state.Jobs {
		if j.ReservedMicros != jobs[j.ID] {
			return ErrXBudgetState
		}
		if _, err := xBudgetAdd(j.SpentMicros, j.ReservedMicros); err != nil {
			return err
		}
		delete(jobs, j.ID)
	}
	if len(months) > 0 || len(jobs) > 0 {
		return ErrXBudgetState
	}
	return nil
}

// RecordXModelErrorContext durably fences later reads without changing their
// reservations, counters, credentials or cursors. The evidence must name an
// admitted read. Caller retains source execution ownership until this returns.
func (s *Store) RecordXModelErrorContext(ctx context.Context, sourceID int64, ownerID, reservationID string) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		state, err := s.loadXControlTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if ownerID != state.OwnerID {
			return ErrXBudgetIdentity
		}
		evidence := false
		for _, reservation := range state.Reservations {
			if reservation.ID == reservationID && reservation.Kind != XRequestCreate && reservation.State != "settled" {
				evidence = true
			}
		}
		if !evidence {
			return ErrXReservationUsage
		}
		if state.ModelError == XModelErrorResourceShape {
			return nil
		}
		code := XModelErrorResourceShape
		_, err = s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: ownerID, ModelError: &code})
		return err
	})
}

// AcknowledgeXReadModelContext is an explicit, network-free owner recovery
// after reviewing the adapter/pricing. The authenticated owner adapter holds
// source execution and passes the reviewed revision. It clears only the model
// hold: it cannot refund spend, reset cursors/generations or unfreeze a send.
func (s *Store) AcknowledgeXReadModelContext(ctx context.Context, sourceID int64, ownerID string, expectedRevision int64) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	return s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		state, err := s.loadXControlTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if ownerID != state.OwnerID {
			return ErrXBudgetIdentity
		}
		if state.ModelError == "" {
			return nil
		}
		if state.Revision != expectedRevision {
			return ErrXControlConflict
		}
		clear := ""
		_, err = s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: ownerID, ModelError: &clear})
		return err
	})
}
