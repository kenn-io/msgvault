package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	XControlVersion          = 1
	XModelErrorResourceShape = "resource_shape_exceeded"
	XMaxControlBytes         = 1 << 20
	XMaxReservations         = 256
	XMaxArchiveJobs          = 64
	XMaxSpendBuckets         = 120
	XMaxUnresolvedIDs        = 1000
)

var ErrXControlConflict = errors.New("X control revision conflict")

// XControlState is the bounded authority in sources.sync_cursor. Sync-run
// checkpoints are diagnostic only and must never replace this state.
type XControlState struct {
	Version         int            `json:"version"`
	Revision        int64          `json:"revision"`
	OwnerID         string         `json:"owner_id"`
	Username        string         `json:"username,omitempty"`
	InstanceID      string         `json:"instance_id,omitempty"`
	Generation      int64          `json:"generation"`
	CredentialError string         `json:"credential_error,omitempty"`
	ModelError      string         `json:"model_error,omitempty"`
	Own             *XWalk         `json:"own,omitempty"`
	Mentions        *XWalk         `json:"mentions,omitempty"`
	Overlap         *XWalk         `json:"overlap,omitempty"`
	Refresh         XRefreshState  `json:"refresh"`
	Jobs            []XArchiveJob  `json:"jobs,omitempty"`
	SpendBuckets    []XSpendBucket `json:"spend_buckets,omitempty"`
	Reservations    []XReservation `json:"reservations,omitempty"`
}

// XWalk freezes endpoint/query and bounds for every page, including replay.
// Watermark is published only after exhaustion and all obligations resolve.
type XWalk struct {
	Fingerprint        string    `json:"fingerprint"`
	Endpoint           string    `json:"endpoint"`
	Query              string    `json:"query,omitempty"`
	After              time.Time `json:"after,omitzero"`
	Before             time.Time `json:"before,omitzero"`
	BaseWatermark      string    `json:"base_watermark,omitempty"`
	Watermark          string    `json:"watermark,omitempty"`
	Token              string    `json:"token,omitempty"`
	SeenTokens         []string  `json:"seen_tokens,omitempty"`
	LowestCommittedID  string    `json:"lowest_committed_id,omitempty"`
	MaxObservedID      string    `json:"max_observed_id,omitempty"`
	ObservedResults    int64     `json:"observed_results,omitempty"`
	UnresolvedIDs      []string  `json:"unresolved_ids,omitempty"`
	UnresolvedPage     bool      `json:"unresolved_page,omitempty"`
	Complete           bool      `json:"complete"`
	HistoryGapPossible bool      `json:"history_gap_possible,omitempty"`
}

type XRefreshState struct {
	Cursor            string    `json:"cursor,omitempty"`
	SweepStartedAt    time.Time `json:"sweep_started_at,omitzero"`
	LastCompleteAt    time.Time `json:"last_complete_at,omitzero"`
	OldestUncheckedAt time.Time `json:"oldest_unchecked_at,omitzero"`
	Outstanding       int64     `json:"outstanding"`
	PendingRemoval    bool      `json:"pending_removal"`
}

// Closed archive jobs retain their identity and cap. Repeated invocations must
// reuse them instead of minting a new lifetime allowance.
type XArchiveJob struct {
	ID             string    `json:"id"`
	Fingerprint    string    `json:"fingerprint"`
	Username       string    `json:"username"`
	After          time.Time `json:"after,omitzero"`
	Before         time.Time `json:"before,omitzero"`
	CapMicros      int64     `json:"cap_micros"`
	SpentMicros    int64     `json:"spent_micros"`
	ReservedMicros int64     `json:"reserved_micros"`
	Own            *XWalk    `json:"own,omitempty"`
	Mentions       *XWalk    `json:"mentions,omitempty"`
	Complete       bool      `json:"complete"`
}

type XArchiveJobSpec struct {
	OwnerID         string
	After, Before   time.Time
	CapMicros       *int64
	JobID           string
	NewJob, Preview bool
}

func (s *Store) PrepareXArchiveJobContext(ctx context.Context, sourceID int64, spec XArchiveJobSpec) (XArchiveJob, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return XArchiveJob{}, err
	}
	if !validXControlID(spec.OwnerID) || spec.After.IsZero() || (spec.JobID != "" && spec.NewJob) || len(spec.JobID) > 128 || (!spec.Preview && spec.CapMicros == nil) || (spec.CapMicros != nil && *spec.CapMicros < 0) {
		return XArchiveJob{}, ErrXArchiveScope
	}
	spec.After = spec.After.UTC()
	var result XArchiveJob
	err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error { return s.lockSyncSourceTx(ctx, tx, sourceID) }, func(tx *loggedTx) error {
		if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
			return err
		}
		state, err := s.loadXControlTx(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if state.OwnerID != spec.OwnerID {
			return ErrXArchiveScope
		}
		if err := validateXBudgetState(state); err != nil {
			return err
		}
		index := -1
		if !spec.NewJob {
			for i := len(state.Jobs) - 1; i >= 0; i-- {
				job := state.Jobs[i]
				if (spec.JobID != "" && job.ID == spec.JobID) || (spec.JobID == "" && job.After.Equal(spec.After)) {
					index = i
					break
				}
			}
		}
		if index >= 0 {
			job := state.Jobs[index]
			if !job.After.Equal(spec.After) || !validXArchiveJob(sourceID, state.OwnerID, job) {
				return ErrXArchiveScope
			}
			if spec.CapMicros != nil && *spec.CapMicros < job.CapMicros {
				return ErrXArchiveScope
			}
			if !spec.Preview && *spec.CapMicros > job.CapMicros {
				job.CapMicros = *spec.CapMicros
				state.Jobs[index] = job
				if _, err := s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: state.OwnerID, Jobs: &state.Jobs}); err != nil {
					return err
				}
			}
			result = job
			return nil
		}
		if spec.JobID != "" {
			return ErrXArchiveScope
		}
		// Choose a new ceiling and cached handle only after looking for a
		// durable job. Reconnects and wall-clock changes cannot renew its cap.
		before := spec.Before.UTC().Truncate(time.Second)
		if before.IsZero() || !spec.After.Before(before) {
			return ErrXArchiveScope
		}
		if !validXArchiveUsername(state.Username) {
			return errors.New("X archive requires a cached verified username; reconnect the account")
		}
		job := XArchiveJob{Username: state.Username, After: spec.After, Before: before}
		job.Own = &XWalk{Endpoint: XRequestSearch, Query: "from:" + job.Username + " -is:retweet", After: job.After, Before: job.Before}
		job.Mentions = &XWalk{Endpoint: XRequestSearch, Query: "to:" + job.Username + " -is:retweet", After: job.After, Before: job.Before}
		job.Own.Fingerprint = XWalkFingerprint(job.Own)
		job.Mentions.Fingerprint = XWalkFingerprint(job.Mentions)
		job.Fingerprint = xArchiveFingerprint(sourceID, state.OwnerID, job)
		if spec.CapMicros != nil {
			job.CapMicros = *spec.CapMicros
		}
		if !spec.Preview {
			if len(state.Jobs) >= XMaxArchiveJobs {
				return ErrXBudgetCapacity
			}
			job.ID = "xjob_" + rand.Text()
			state.Jobs = append(state.Jobs, job)
			if _, err := s.updateXControlTx(ctx, tx, sourceID, state.Revision, XControlPatch{OwnerID: state.OwnerID, Jobs: &state.Jobs}); err != nil {
				return err
			}
		}
		result = job
		return nil
	})
	return result, err
}

func validXArchiveUsername(username string) bool {
	if len(username) < 1 || len(username) > 15 {
		return false
	}
	for _, c := range []byte(username) {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

func xArchiveFingerprint(sourceID int64, owner string, job XArchiveJob) string {
	identity := strings.Join([]string{strconv.FormatInt(sourceID, 10), owner, job.Username, job.After.UTC().Format(time.RFC3339Nano), job.Before.UTC().Format(time.RFC3339Nano), job.Own.Query, job.Mentions.Query}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func validXArchiveJob(sourceID int64, owner string, job XArchiveJob) bool {
	if !validXArchiveUsername(job.Username) || job.Own == nil || job.Mentions == nil || job.Before.IsZero() || job.After.IsZero() || !job.After.Before(job.Before) {
		return false
	}
	if job.Own.Query != "from:"+job.Username+" -is:retweet" || job.Mentions.Query != "to:"+job.Username+" -is:retweet" {
		return false
	}
	for _, walk := range []*XWalk{job.Own, job.Mentions} {
		if walk.Endpoint != XRequestSearch || !walk.After.Equal(job.After) || !walk.Before.Equal(job.Before) || walk.BaseWatermark != "" || walk.Fingerprint != XWalkFingerprint(walk) {
			return false
		}
	}
	return job.Fingerprint == xArchiveFingerprint(sourceID, owner, job) && job.Complete == (job.Own.Complete && job.Mentions.Complete)
}

// XWalkFingerprint binds an opaque continuation to its exact frozen request.
func XWalkFingerprint(walk *XWalk) string {
	identity := strings.Join([]string{walk.Endpoint, walk.Query, walk.After.UTC().Format(time.RFC3339Nano), walk.Before.UTC().Format(time.RFC3339Nano), walk.BaseWatermark}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

type XSpendBucket struct {
	Month          string `json:"month"`
	SpentMicros    int64  `json:"spent_micros"`
	ReservedMicros int64  `json:"reserved_micros"`
}

// Reservations retain their charged UTC month across rollover. Settlement
// compacts known costs into that original month/job, never a later month.
type XReservation struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Month           string `json:"month"`
	AmountMicros    int64  `json:"amount_micros"`
	MaxPosts        int    `json:"max_posts"`
	MaxUsers        int    `json:"max_users"`
	PostPriceMicros int64  `json:"post_price_micros"`
	UserPriceMicros int64  `json:"user_price_micros"`
	JobID           string `json:"job_id,omitempty"`
	State           string `json:"state"`
}

// XControlPatch names owned segments. Nil preserves a segment; a pointer to
// an empty slice explicitly clears it. OwnerID is required. ExpectedRevision
// is used by CompleteXSyncContext; UpdateXControlContext takes it separately.
type XControlPatch struct {
	InitializeInstance bool
	OwnerID            string
	ExpectedRevision   int64
	Generation         *int64
	Username           *string
	CredentialError    *string
	ModelError         *string
	Own                *XWalk
	Mentions           *XWalk
	Overlap            *XWalk
	Refresh            *XRefreshState
	Jobs               *[]XArchiveJob
	SpendBuckets       *[]XSpendBucket
	Reservations       *[]XReservation
}

// LoadXControlContext reads authority without changing malformed or future state.
func (s *Store) LoadXControlContext(ctx context.Context, sourceID int64) (XControlState, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return XControlState{}, err
	}
	return s.loadXControlWith(boundQuerier{ctx: ctx, q: s.db}, sourceID)
}

// loadXControlTx reads within a caller's existing short transaction. Mutating
// callers must lockSyncSourceTx before the sync-generation fence, then load.
func (s *Store) loadXControlTx(ctx context.Context, tx *loggedTx, sourceID int64) (XControlState, error) {
	return s.loadXControlWith(boundQuerier{ctx: ctx, q: tx}, sourceID)
}

func (s *Store) loadXControlWith(q querier, sourceID int64) (XControlState, error) {
	var sourceType, ownerID string
	var cursor sql.NullString
	if err := q.QueryRow("SELECT source_type, identifier, sync_cursor FROM sources WHERE id=?", sourceID).Scan(&sourceType, &ownerID, &cursor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return XControlState{}, fmt.Errorf("load X control: %w", ErrSourceNotFound)
		}
		return XControlState{}, fmt.Errorf("load X control: %w", err)
	}
	if sourceType != "x" || !validXControlID(ownerID) {
		return XControlState{}, errors.New("X control requires a numeric X source")
	}
	state := XControlState{Version: XControlVersion, OwnerID: ownerID}
	if cursor.Valid {
		if len(cursor.String) > XMaxControlBytes {
			return XControlState{}, errors.New("X control exceeds capacity")
		}
		// Decode into zero state so missing version/owner cannot inherit defaults.
		state = XControlState{}
		if xControlHasNull(cursor.String) {
			return XControlState{}, errors.New("invalid null in X control JSON")
		}
		if err := json.Unmarshal([]byte(cursor.String), &state, json.RejectUnknownMembers(true)); err != nil {
			return XControlState{}, errors.New("invalid X control JSON")
		}
	}
	if err := validateXControl(state, ownerID); err != nil {
		return XControlState{}, err
	}
	return state, nil
}

// UpdateXControlContext compares the current revision before changing only the
// named segments. Orchestration holds source execution before credentials and
// this short transaction; no provider I/O belongs inside a Store transaction.
func (s *Store) UpdateXControlContext(ctx context.Context, sourceID, expectedRevision int64, patch XControlPatch) (XControlState, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return XControlState{}, err
	}
	var result XControlState
	err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
		return s.lockSyncSourceTx(ctx, tx, sourceID)
	}, func(tx *loggedTx) error {
		var err error
		result, err = s.updateXControlTx(ctx, tx, sourceID, expectedRevision, patch)
		return err
	})
	if err != nil {
		return XControlState{}, err
	}
	return result, nil
}

// updateXControlTx merges under the caller's existing source-locked, generation-
// fenced transaction. Task-specific admission can write its other rows in that
// same transaction. This helper never begins/commits a transaction or does I/O.
func (s *Store) updateXControlTx(ctx context.Context, tx *loggedTx, sourceID, expectedRevision int64, patch XControlPatch) (XControlState, error) {
	if err := s.requireSyncSource(sourceID); err != nil {
		return XControlState{}, err
	}
	if err := requireSourceWritableWith(boundQuerier{ctx: ctx, q: tx}, sourceID); err != nil {
		return XControlState{}, err
	}
	state, err := s.loadXControlTx(ctx, tx, sourceID)
	if err != nil {
		return XControlState{}, err
	}
	if patch.OwnerID != state.OwnerID {
		return XControlState{}, errors.New("X control owner mismatch")
	}
	if expectedRevision != state.Revision {
		return XControlState{}, ErrXControlConflict
	}
	if state.Revision == math.MaxInt64 {
		return XControlState{}, errors.New("X control revision exhausted")
	}
	if patch.InitializeInstance && state.InstanceID == "" {
		instance := make([]byte, 32)
		if _, err := rand.Read(instance); err != nil {
			return XControlState{}, fmt.Errorf("create X source instance: %w", err)
		}
		state.InstanceID = hex.EncodeToString(instance)
	}
	if patch.Generation != nil {
		if *patch.Generation < state.Generation {
			return XControlState{}, errors.New("X credential generation cannot decrease")
		}
		state.Generation = *patch.Generation
	}
	if patch.Username != nil {
		state.Username = *patch.Username
	}
	if patch.CredentialError != nil {
		state.CredentialError = *patch.CredentialError
	}
	if patch.ModelError != nil {
		state.ModelError = *patch.ModelError
	}
	if patch.Own != nil {
		state.Own = patch.Own
	}
	if patch.Mentions != nil {
		state.Mentions = patch.Mentions
	}
	if patch.Overlap != nil {
		state.Overlap = patch.Overlap
	}
	if patch.Refresh != nil {
		state.Refresh = *patch.Refresh
	}
	if patch.Jobs != nil {
		state.Jobs = *patch.Jobs
	}
	if patch.SpendBuckets != nil {
		state.SpendBuckets = *patch.SpendBuckets
	}
	if patch.Reservations != nil {
		state.Reservations = *patch.Reservations
	}
	state.Revision++
	if err := validateXControl(state, state.OwnerID); err != nil {
		return XControlState{}, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return XControlState{}, errors.New("encode X control JSON")
	}
	if len(data) > XMaxControlBytes {
		return XControlState{}, errors.New("X control exceeds capacity")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE sources SET sync_cursor=? WHERE id=?", string(data), sourceID); err != nil {
		return XControlState{}, fmt.Errorf("update X control: %w", err)
	}
	// Return detached data: callers may reuse/mutate patch slices after return.
	var result XControlState
	if err := json.Unmarshal(data, &result); err != nil {
		return XControlState{}, errors.New("decode saved X control JSON")
	}
	return result, nil
}

// CompleteXSyncContext requires the exact ScopedToSync view. Control authority
// and terminal run publication commit together; diagnostic checkpoints never
// supply the source cursor. Every terminal attempt releases run-owned execution,
// including conflicts and database failures, just like generic completion.
func (s *Store) CompleteXSyncContext(ctx context.Context, runID int64, patch XControlPatch) error {
	if s.syncGeneration == nil {
		return errors.New("complete X sync requires sync-scoped Store")
	}
	if runID != s.syncGeneration.runID {
		return ErrSyncRunSuperseded
	}
	sourceID := s.syncGeneration.sourceID
	base := s.withoutSyncScope()
	err := base.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.validateCurrentSyncGeneration(ctx, tx, sourceID, runID, SyncStatusRunning); err != nil {
			return err
		}
		state, err := s.updateXControlTx(ctx, tx, sourceID, patch.ExpectedRevision, patch)
		if err != nil {
			return err
		}
		data, err := json.Marshal(state)
		if err != nil {
			return errors.New("encode completed X control")
		}
		return base.completeSyncSourceTx(ctx, tx, runID, sourceID, string(data), false)
	})
	if err == nil {
		base.optimizeAfterSync(ctx)
	}
	return base.finalizeSyncExecution(runID, err)
}

func validXControlID(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] == '0' {
		return false
	}
	for i := range len(value) {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func validXMonth(value string) bool {
	_, err := time.Parse("2006-01", value)
	return err == nil && len(value) == 7
}

func validateXWalk(walk *XWalk) error {
	if walk == nil {
		return nil
	}
	if walk.Fingerprint == "" || walk.Endpoint == "" {
		return errors.New("X walk requires fingerprint and endpoint")
	}
	if walk.ObservedResults < 0 {
		return errors.New("invalid X observed result count")
	}
	if !walk.After.IsZero() && !walk.Before.IsZero() && !walk.After.Before(walk.Before) {
		return errors.New("invalid X walk bounds")
	}
	if len(walk.UnresolvedIDs) > XMaxUnresolvedIDs || len(walk.SeenTokens) > 1000 {
		return errors.New("X walk exceeds capacity")
	}
	for _, id := range []string{walk.BaseWatermark, walk.Watermark, walk.LowestCommittedID, walk.MaxObservedID} {
		if id != "" && !validXControlID(id) {
			return errors.New("invalid X walk ID")
		}
	}
	for _, id := range walk.UnresolvedIDs {
		if !validXControlID(id) {
			return errors.New("invalid unresolved X ID")
		}
	}
	if walk.Complete && (len(walk.UnresolvedIDs) > 0 || walk.UnresolvedPage || walk.Token != "") {
		return errors.New("completed X walk has unresolved work")
	}
	return nil
}

func validateXControl(state XControlState, ownerID string) error {
	if state.Version != XControlVersion {
		return errors.New("unsupported X control version")
	}
	if state.OwnerID != ownerID || !validXControlID(state.OwnerID) {
		return errors.New("X control owner mismatch")
	}
	if state.Revision < 0 || state.Generation < 0 || state.Refresh.Outstanding < 0 {
		return errors.New("invalid X control counters")
	}
	if len(state.Username) > 256 {
		return errors.New("X cached username exceeds capacity")
	}
	if state.InstanceID != "" && !validXRemovalDigest(state.InstanceID) {
		return errors.New("invalid X source instance")
	}
	if len(state.CredentialError) > 256 {
		return errors.New("X credential status exceeds capacity")
	}
	if len(state.ModelError) > 64 || (state.ModelError != "" && state.ModelError != XModelErrorResourceShape) {
		return errors.New("unsupported X read-model error")
	}
	for _, walk := range []*XWalk{state.Own, state.Mentions, state.Overlap} {
		if err := validateXWalk(walk); err != nil {
			return err
		}
	}
	if len(state.Jobs) > XMaxArchiveJobs || len(state.SpendBuckets) > XMaxSpendBuckets || len(state.Reservations) > XMaxReservations {
		return errors.New("X control exceeds capacity")
	}
	jobs := make(map[string]bool, len(state.Jobs))
	for _, job := range state.Jobs {
		if job.ID == "" || job.Fingerprint == "" || jobs[job.ID] || job.CapMicros < 0 || job.SpentMicros < 0 || job.ReservedMicros < 0 {
			return errors.New("invalid X archive job")
		}
		jobs[job.ID] = true
		if !job.After.IsZero() && !job.Before.IsZero() && !job.After.Before(job.Before) {
			return errors.New("invalid X archive job bounds")
		}
		for _, walk := range []*XWalk{job.Own, job.Mentions} {
			if err := validateXWalk(walk); err != nil {
				return err
			}
		}
	}
	months := make(map[string]bool, len(state.SpendBuckets))
	for _, bucket := range state.SpendBuckets {
		if !validXMonth(bucket.Month) || months[bucket.Month] || bucket.SpentMicros < 0 || bucket.ReservedMicros < 0 {
			return errors.New("invalid X spend bucket")
		}
		months[bucket.Month] = true
	}
	reservations := make(map[string]bool, len(state.Reservations))
	for _, reservation := range state.Reservations {
		if reservation.ID == "" || reservation.Kind == "" || reservations[reservation.ID] || !validXMonth(reservation.Month) || reservation.AmountMicros < 0 || reservation.MaxPosts < 0 || reservation.MaxUsers < 0 || reservation.PostPriceMicros < 0 || reservation.UserPriceMicros < 0 {
			return errors.New("invalid X reservation")
		}
		if reservation.State != "pending" && reservation.State != "unknown" && reservation.State != "settled" {
			return errors.New("invalid X reservation state")
		}
		if reservation.JobID != "" && !jobs[reservation.JobID] {
			return errors.New("X reservation archive job missing")
		}
		reservations[reservation.ID] = true
	}
	return nil
}

// No stored field is nullable: optional segments are omitted. In particular,
// decoding null into a scalar must never silently turn reserved spend into zero.
func xControlHasNull(data string) bool {
	decoder := jsontext.NewDecoder(strings.NewReader(data))
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil || token.Kind() == 'n' {
			return true
		}
	}
}
