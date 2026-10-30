package threads

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type SyncRequest struct {
	After  time.Time
	Full   bool
	PostID string
}
type SourceSyncConfig struct {
	Version            int       `json:"version"`
	HistoryFloor       time.Time `json:"history_floor"`
	HistoryCeiling     time.Time `json:"history_ceiling"`
	CoverageGeneration uint64    `json:"coverage_generation"`
}
type SyncFingerprint struct {
	Version            int       `json:"version"`
	AppID              string    `json:"app_id"`
	OwnerID            string    `json:"owner_id"`
	CoverageGeneration uint64    `json:"coverage_generation"`
	Floor              time.Time `json:"floor"`
	PostFields         string    `json:"post_fields"`
	ReplyFields        string    `json:"reply_fields"`
	ConversationFields string    `json:"conversation_fields"`
	ListOrder          string    `json:"list_order"`
	ConversationOrder  string    `json:"conversation_order"`
	Mode               string    `json:"mode"`
	RootID             string    `json:"root_id"`
}
type FeedWalk struct {
	CompletedThrough time.Time `json:"completed_through"`
	Since            time.Time `json:"since"`
	Until            time.Time `json:"until"`
	Cursor           string    `json:"cursor"`
	Guard            WalkGuard `json:"guard,omitzero"`
	Done             bool      `json:"done"`
}
type RootPass struct {
	AfterID    int64     `json:"after_id"`
	ThroughID  int64     `json:"through_id"`
	HotSince   time.Time `json:"hot_since"`
	ActiveRoot string    `json:"active_root"`
	ActiveKey  int64     `json:"active_key,omitzero"`
	Guard      WalkGuard `json:"guard,omitzero"`
	Cursor     string    `json:"cursor"`
	Done       bool      `json:"done"`
}
type CoverageCounters struct {
	Unavailable    int64     `json:"unavailable"`
	Incomplete     int64     `json:"incomplete"`
	OldestObserved time.Time `json:"oldest_observed"`
}
type SyncState struct {
	Version           int              `json:"version"`
	Epoch             string           `json:"epoch"`
	Mode              string           `json:"mode"`
	Fingerprint       string           `json:"fingerprint"`
	HistoryCeiling    time.Time        `json:"history_ceiling"`
	OwnPostsHead      FeedWalk         `json:"own_posts_head"`
	OwnRepliesHead    FeedWalk         `json:"own_replies_head"`
	OwnPostsHistory   FeedWalk         `json:"own_posts_history"`
	OwnRepliesHistory FeedWalk         `json:"own_replies_history"`
	HotRoots          RootPass         `json:"hot_roots"`
	AllRoots          RootPass         `json:"all_roots"`
	TargetRoot        RootPass         `json:"target_root"`
	NextLane          int              `json:"next_lane"`
	CompletedLanes    uint8            `json:"completed_lanes,omitzero"`
	Coverage          CoverageCounters `json:"coverage"`
}

// PrepareSourceSyncConfigContext persists coverage before starting a run. The
// caller must hold the source lifecycle/execution ownership used by importers.
// Omitting After preserves the selected floor; changing it advances generation
// even when returning to an earlier floor. Unrelated config keys are retained.
func PrepareSourceSyncConfigContext(ctx context.Context, st *store.Store, sourceID int64, request SyncRequest, now time.Time) (SourceSyncConfig, error) {
	if err := request.Validate(); err != nil {
		return SourceSyncConfig{}, err
	}
	source, err := st.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return SourceSyncConfig{}, fmt.Errorf("load Threads coverage config: %w", err)
	}
	if source.SourceType != "threads" {
		return SourceSyncConfig{}, errors.New("Threads coverage requires a Threads source")
	}
	if _, _, err := tokenSourceIDs(source.Identifier); err != nil {
		return SourceSyncConfig{}, err
	}
	if now.Before(earliestHistoryTime()) {
		return SourceSyncConfig{}, errors.New("invalid Threads coverage time")
	}
	fields := map[string]jsontext.Value{}
	cfg := SourceSyncConfig{Version: 1, HistoryFloor: earliestHistoryTime(), HistoryCeiling: source.CreatedAt.UTC(), CoverageGeneration: 1}
	changed := !source.SyncConfig.Valid || source.SyncConfig.String == ""
	if !changed {
		cfg = SourceSyncConfig{}
		if err := json.Unmarshal([]byte(source.SyncConfig.String), &fields); err != nil {
			return SourceSyncConfig{}, fmt.Errorf("decode Threads source config: %w", err)
		}
		if err := json.Unmarshal([]byte(source.SyncConfig.String), &cfg); err != nil {
			return SourceSyncConfig{}, fmt.Errorf("decode Threads coverage: %w", err)
		}
		if err := cfg.validate(); err != nil {
			return SourceSyncConfig{}, err
		}
	}
	if !request.After.IsZero() {
		floor := request.After.UTC()
		if floor.Before(earliestHistoryTime()) {
			return SourceSyncConfig{}, errors.New("Threads history floor precedes the earliest supported time")
		}
		if !floor.Equal(cfg.HistoryFloor) {
			if !changed {
				if cfg.CoverageGeneration == math.MaxUint64 {
					return SourceSyncConfig{}, errors.New("Threads coverage generation exhausted")
				}
				cfg.CoverageGeneration++
			}
			cfg.HistoryFloor = floor
			changed = true
		}
	}
	cfg.HistoryFloor = cfg.HistoryFloor.UTC()
	cfg.HistoryCeiling = cfg.HistoryCeiling.UTC()
	if changed {
		encoded, err := json.Marshal(cfg)
		if err != nil {
			return SourceSyncConfig{}, fmt.Errorf("encode Threads coverage: %w", err)
		}
		var coverage map[string]jsontext.Value
		if err := json.Unmarshal(encoded, &coverage); err != nil {
			return SourceSyncConfig{}, err
		}
		for key, value := range coverage {
			fields[key] = value
		}
		encoded, err = json.Marshal(fields)
		if err != nil {
			return SourceSyncConfig{}, fmt.Errorf("encode Threads source config: %w", err)
		}
		if err := st.UpdateSourceSyncConfigContext(ctx, sourceID, string(encoded)); err != nil {
			return SourceSyncConfig{}, fmt.Errorf("save Threads coverage: %w", err)
		}
	}
	return cfg, nil
}

func earliestHistoryTime() time.Time { return time.Unix(1688540400, 0).UTC() }
func (c SourceSyncConfig) validate() error {
	if c.Version != 1 || c.CoverageGeneration == 0 || c.HistoryFloor.Before(earliestHistoryTime()) || c.HistoryCeiling.Before(earliestHistoryTime()) {
		return errors.New("invalid or unsupported Threads coverage config")
	}
	return nil
}

// Validate rejects ambiguous modes and malformed bounds before any I/O.
func (r SyncRequest) Validate() error {
	if !r.After.IsZero() && r.After.Before(earliestHistoryTime()) {
		return errors.New("Threads history floor precedes the earliest supported time")
	}
	if r.Full && r.PostID != "" {
		return errors.New("Threads full and targeted sync are mutually exclusive")
	}
	if r.PostID != "" && !r.After.IsZero() {
		return errors.New("Threads targeted sync cannot specify after")
	}
	if r.PostID != "" && !validID(r.PostID) {
		return errors.New("invalid Threads targeted root")
	}
	return nil
}

// Fingerprint is canonical versioned JSON, with no credential generation.
// Reauthorization of the same app/owner cannot invalidate committed coverage.
// Fixed interval bounds and epoch belong to the state, not request identity.
func (r SyncRequest) Fingerprint(source string, cfg SourceSyncConfig) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	if err := cfg.validate(); err != nil {
		return "", err
	}
	if !r.After.IsZero() && !r.After.Equal(cfg.HistoryFloor) {
		return "", errors.New("persist Threads requested floor before fingerprinting")
	}
	app, owner, err := tokenSourceIDs(source)
	if err != nil {
		return "", err
	}
	mode := store.ThreadsSyncBaseline
	if r.Full {
		mode = store.ThreadsSyncFull
	}
	if r.PostID != "" {
		mode = store.ThreadsSyncPost
	}
	fp := SyncFingerprint{Version: 1, AppID: app, OwnerID: owner, CoverageGeneration: cfg.CoverageGeneration, Floor: cfg.HistoryFloor.UTC(), PostFields: ownPostFields, ReplyFields: replyFields, ConversationFields: conversationFields, ListOrder: "provider", ConversationOrder: "reverse=true", Mode: mode, RootID: r.PostID}
	encoded, err := json.Marshal(fp)
	if err != nil {
		return "", fmt.Errorf("encode Threads request fingerprint: %w", err)
	}
	return string(encoded), nil
}

// ResumeQuery selects source-wide normal/full history together; an explicit
// full request decides whether to start a fresh epoch after loading that state.
func (r SyncRequest) ResumeQuery(source string, cfg SourceSyncConfig) (store.ThreadsResumeQuery, error) {
	fp, err := r.Fingerprint(source, cfg)
	if err != nil {
		return store.ThreadsResumeQuery{}, err
	}
	if r.PostID != "" {
		return store.ThreadsResumeQuery{PostFingerprint: fp}, nil
	}
	baseline, err := (SyncRequest{}).Fingerprint(source, cfg)
	if err != nil {
		return store.ThreadsResumeQuery{}, err
	}
	full, err := (SyncRequest{Full: true}).Fingerprint(source, cfg)
	if err != nil {
		return store.ThreadsResumeQuery{}, err
	}
	return store.ThreadsResumeQuery{BaselineFingerprint: baseline, FullFingerprint: full}, nil
}

func decodeSyncFingerprint(raw string) (SyncFingerprint, error) {
	var fp SyncFingerprint
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		return fp, fmt.Errorf("decode Threads fingerprint: %w", err)
	}
	if fp.Mode != store.ThreadsSyncBaseline && fp.Mode != store.ThreadsSyncFull && fp.Mode != store.ThreadsSyncPost {
		return fp, errors.New("invalid Threads fingerprint mode")
	}
	request := SyncRequest{Full: fp.Mode == store.ThreadsSyncFull, PostID: fp.RootID}
	if (fp.Mode == store.ThreadsSyncPost) != (fp.RootID != "") {
		return fp, errors.New("invalid Threads fingerprint root")
	}
	expected, err := request.Fingerprint("threads:"+fp.AppID+":"+fp.OwnerID, SourceSyncConfig{Version: 1, HistoryFloor: fp.Floor, HistoryCeiling: fp.Floor, CoverageGeneration: fp.CoverageGeneration})
	if err != nil {
		return fp, err
	}
	if expected != raw {
		return fp, errors.New("unsupported or noncanonical Threads fingerprint")
	}
	return fp, nil
}

// NewSyncState freezes an epoch's initial intervals and both root-pass bounds.
// A targeted state services only TargetRoot. The importer owns later page
// commits, lane scheduling and advancement of completed watermarks.
func NewSyncState(epoch, fingerprint string, cfg SourceSyncConfig, now time.Time, upperKey int64) (*SyncState, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	fp, err := decodeSyncFingerprint(fingerprint)
	if err != nil {
		return nil, err
	}
	if epoch == "" || now.Before(cfg.HistoryCeiling) || upperKey < 0 || !fp.Floor.Equal(cfg.HistoryFloor) || fp.CoverageGeneration != cfg.CoverageGeneration {
		return nil, errors.New("invalid Threads epoch bounds or coverage")
	}
	state := &SyncState{Version: 1, Epoch: epoch, Mode: fp.Mode, Fingerprint: fingerprint, HistoryCeiling: cfg.HistoryCeiling.UTC()}
	if fp.Mode == store.ThreadsSyncPost {
		state.TargetRoot.ActiveRoot = fp.RootID
		state.OwnPostsHead.Done = true
		state.OwnRepliesHead.Done = true
		state.OwnPostsHistory.Done = true
		state.OwnRepliesHistory.Done = true
		state.HotRoots.Done = true
		state.AllRoots.Done = true
		return state, nil
	}
	history := FeedWalk{Since: cfg.HistoryFloor.UTC(), Until: cfg.HistoryCeiling.UTC(), Done: !cfg.HistoryFloor.Before(cfg.HistoryCeiling)}
	head := FeedWalk{CompletedThrough: cfg.HistoryCeiling.UTC(), Done: true}
	if err := head.PinHead(cfg.HistoryFloor, now); err != nil {
		return nil, err
	}
	state.OwnPostsHistory = history
	state.OwnRepliesHistory = history
	state.OwnPostsHead = head
	state.OwnRepliesHead = head
	state.HotRoots = RootPass{ThroughID: upperKey, HotSince: now.UTC().Add(-7 * 24 * time.Hour)}
	state.AllRoots = RootPass{ThroughID: upperKey}
	return state, nil
}

// PinHead preserves a pending interval, or opens the next interval with an
// exact one-hour overlap. Local bounds retain nanoseconds and equal times.
// API whole-second handling belongs to the importer, never watermark rounding.
// Empty intervals finish this run's lane without advancing CompletedThrough;
// the next invocation can open an interval once its floor has been reached.
func (w *FeedWalk) PinHead(floor, until time.Time) error {
	if !w.Done && !w.Since.IsZero() && !w.Until.IsZero() {
		return nil
	}
	if floor.Before(earliestHistoryTime()) || w.CompletedThrough.IsZero() || until.Before(w.CompletedThrough) {
		return errors.New("invalid Threads head bounds")
	}
	since := w.CompletedThrough.Add(-time.Hour)
	if since.Before(floor) {
		since = floor
	}
	w.Since = since.UTC()
	w.Until = until.UTC()
	w.Cursor = ""
	w.Guard = WalkGuard{}
	w.Done = !since.Before(until)
	return nil
}

func (s SyncState) validate() error {
	fp, err := decodeSyncFingerprint(s.Fingerprint)
	if err != nil {
		return err
	}
	if s.Version != 1 || s.Epoch == "" || s.Mode != fp.Mode || s.HistoryCeiling.Before(earliestHistoryTime()) || s.NextLane < 0 || s.NextLane >= 6 || s.CompletedLanes > 63 || s.Coverage.Unavailable < 0 || s.Coverage.Incomplete < 0 {
		return errors.New("invalid or unsupported Threads sync state")
	}
	for _, pass := range []RootPass{s.HotRoots, s.AllRoots, s.TargetRoot} {
		if pass.AfterID < 0 || pass.ThroughID < pass.AfterID || (pass.ActiveRoot != "" && !validID(pass.ActiveRoot)) || !validCursor(pass.Cursor) {
			return errors.New("invalid Threads root pass")
		}
	}
	for _, walk := range []FeedWalk{s.OwnPostsHead, s.OwnRepliesHead, s.OwnPostsHistory, s.OwnRepliesHistory} {
		if !validCursor(walk.Cursor) || (!walk.Done && (walk.Since.IsZero() || walk.Until.IsZero() || !walk.Since.Before(walk.Until))) {
			return errors.New("invalid Threads feed walk")
		}
	}
	if fp.Mode == store.ThreadsSyncPost && s.TargetRoot.ActiveRoot != fp.RootID {
		return errors.New("Threads target state disagrees with fingerprint")
	}
	return nil
}

func (s SyncState) Encode() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode Threads sync state: %w", err)
	}
	return string(encoded), nil
}
func DecodeSyncState(raw string) (*SyncState, error) {
	var state SyncState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, fmt.Errorf("decode Threads sync state: %w", err)
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

// ResumeSyncState chooses an entire checkpoint or baseline, never individual
// lanes from different rows. Explicit full requests resume a pending full
// epoch; absent one, nil tells the importer to create a fresh full epoch.
func ResumeSyncState(request SyncRequest, baseline, checkpoint *store.SyncRun) (*SyncState, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	raw := ""
	if checkpoint != nil && checkpoint.CursorBefore.Valid {
		raw = checkpoint.CursorBefore.String
	}
	if raw == "" {
		if request.Full || baseline == nil || !baseline.CursorAfter.Valid {
			return nil, nil //nolint:nilnil // A fresh epoch is required.
		}
		raw = baseline.CursorAfter.String
	}
	if raw == "" {
		return nil, nil //nolint:nilnil // No saved state.
	}
	state, err := DecodeSyncState(raw)
	if err != nil {
		return nil, err
	}
	if request.PostID != "" {
		if state.Mode != store.ThreadsSyncPost || state.TargetRoot.ActiveRoot != request.PostID {
			return nil, errors.New("Threads targeted resume state mismatch")
		}
	} else if state.Mode == store.ThreadsSyncPost {
		return nil, errors.New("Threads targeted state cannot replace source-wide state")
	}
	if request.Full && state.Mode != store.ThreadsSyncFull {
		return nil, nil //nolint:nilnil // Ordinary checkpoint is superseded by an explicit full epoch.
	}
	return state, nil
}
