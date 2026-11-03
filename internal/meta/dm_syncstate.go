package meta

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type DMImportSummary struct {
	Threads, MessagesProcessed, MessagesAdded, MessagesSkipped, Requests int
	Partial                                                              bool
	Note                                                                 string
}
type dmSyncState struct {
	Version     string         `json:"version"`
	Source      string         `json:"source"`
	Platform    DMPlatform     `json:"platform"`
	After       string         `json:"after,omitempty"`
	HighWater   string         `json:"high_water,omitempty"`
	ScanStarted string         `json:"scan_started,omitempty"`
	NewHigh     string         `json:"new_high,omitempty"`
	Pending     []Conversation `json:"pending,omitempty"`
	Exhausted   bool           `json:"exhausted,omitempty"`
	Frame       *dmFrame       `json:"frame,omitempty"`
}
type dmFrame struct {
	ThreadID string   `json:"thread_id"`
	IDs      []string `json:"ids"`
	Next     int      `json:"next"`
	Capped   bool     `json:"capped,omitempty"`
}

func (s *dmSyncState) complete() bool { return s.Exhausted && len(s.Pending) == 0 && s.Frame == nil }
func parseDMTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05-0700", s)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("parse inbox timestamp: %w", err)
	}
	return t, nil
}
func (s *dmSyncState) valid(src *store.Source) bool {
	if s.Version != "dm1" || s.Source != src.Identifier || s.Platform.SourceType() != src.SourceType || len(s.Pending) > 25 || len(s.After) > 4096 || strings.IndexFunc(s.After, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return false
	}
	for _, stamp := range []string{s.HighWater, s.NewHigh, s.ScanStarted} {
		if _, err := parseDMTime(stamp); err != nil {
			return false
		}
	}
	for _, c := range s.Pending {
		if !safeObjectID(c.ID) || len(c.Participants) > 2 {
			return false
		}
		if _, err := parseDMTime(c.UpdatedTime); err != nil {
			return false
		}
	}
	if f := s.Frame; f != nil {
		if len(s.Pending) == 0 || f.ThreadID != s.Pending[0].ID || len(f.IDs) > dmBackfillCap || f.Next < 0 || f.Next > len(f.IDs) {
			return false
		}
		for _, id := range f.IDs {
			if !safeObjectID(id) {
				return false
			}
		}
	}
	return true
}
func loadDMState(ctx context.Context, st *store.Store, source *store.Source, full bool, scanStarted time.Time) (*dmSyncState, error) {
	platform := PlatformMessenger
	if source.SourceType == SourceTypeInstagramMessages {
		platform = PlatformInstagram
	}
	fresh := &dmSyncState{Version: "dm1", Source: source.Identifier, Platform: platform, ScanStarted: scanStarted.UTC().Format(time.RFC3339Nano)}
	if full {
		return fresh, nil
	}
	prior, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, source.SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	checkpoint, err := st.GetLatestCheckpointedSyncByTypeContext(ctx, source.ID, source.SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	var blob string
	var priorID int64
	if prior != nil {
		blob, priorID = prior.CursorAfter.String, prior.ID
	}
	if checkpoint != nil && checkpoint.ID > priorID {
		blob = checkpoint.CursorBefore.String
	}
	if blob == "" {
		return fresh, nil
	}
	var state dmSyncState
	if json.Unmarshal([]byte(blob), &state) != nil || !state.valid(source) {
		return nil, errors.New("invalid inbox checkpoint; restart this source with --full")
	}
	if state.complete() {
		fresh.HighWater, fresh.NewHigh = state.NewHigh, state.NewHigh
		return fresh, nil
	}
	// A legacy paused scan has no safe start boundary. Keep its old watermark
	// until a fresh scan can observe all threads again.
	if state.ScanStarted == "" {
		state.NewHigh = state.HighWater
	}
	return &state, nil
}
func dmCheckpoint(state *dmSyncState, sum *DMImportSummary) (*store.Checkpoint, error) {
	blob, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	return &store.Checkpoint{PageToken: string(blob), MessagesAdded: int64(sum.MessagesAdded), MessagesProcessed: int64(sum.MessagesProcessed)}, nil
}
func saveDMState(ctx context.Context, st *store.Store, syncID int64, state *dmSyncState, sum *DMImportSummary) error {
	cp, err := dmCheckpoint(state, sum)
	if err != nil {
		return err
	}
	return st.UpdateSyncCheckpointContext(ctx, syncID, cp)
}
