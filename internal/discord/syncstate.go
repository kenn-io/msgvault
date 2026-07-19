package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const SyncStateVersion = 1

// ContainerState holds independent message progress for one channel, thread,
// or forum post.
type ContainerState struct {
	HighWater        string `json:"high_water,omitempty"`
	BackfillBefore   string `json:"backfill_before,omitempty"`
	BackfillUpper    string `json:"backfill_upper,omitempty"`
	BackfillComplete bool   `json:"backfill_complete,omitempty"`
}

// ThreadCatalogState tracks completed archived-thread enumeration for one
// parent channel.
type ThreadCatalogState struct {
	PublicArchiveWatermark  string `json:"public_archive_watermark,omitempty"`
	PrivateArchiveWatermark string `json:"private_archive_watermark,omitempty"`
}

// SyncState is the versioned Discord cursor persisted in sync run state.
type SyncState struct {
	Version       int                           `json:"version"`
	Containers    map[string]ContainerState     `json:"containers"`
	ThreadCatalog map[string]ThreadCatalogState `json:"thread_catalog"`
}

// NewSyncState returns an initialized state at the current format version.
func NewSyncState() *SyncState {
	return &SyncState{
		Version:       SyncStateVersion,
		Containers:    map[string]ContainerState{},
		ThreadCatalog: map[string]ThreadCatalogState{},
	}
}

// LoadSyncState decodes a persisted checkpoint. Empty input represents a new
// source; malformed or unsupported state returns an error rather than silently
// restarting import progress.
func LoadSyncState(blob string) (*SyncState, error) {
	if blob == "" {
		return NewSyncState(), nil
	}

	state := &SyncState{}
	decoder := json.NewDecoder(strings.NewReader(blob))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(state); err != nil {
		return nil, fmt.Errorf("decode Discord sync state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode Discord sync state: %w", err)
	}
	if state.Version != SyncStateVersion {
		return nil, fmt.Errorf("unsupported Discord sync state version %d", state.Version)
	}
	if state.Containers == nil {
		state.Containers = map[string]ContainerState{}
	}
	if state.ThreadCatalog == nil {
		state.ThreadCatalog = map[string]ThreadCatalogState{}
	}
	return state, nil
}

// Marshal encodes the checkpoint after validating its version.
func (s *SyncState) Marshal() (string, error) {
	if s == nil {
		return "", errors.New("marshal Discord sync state: nil state")
	}
	if s.Version != SyncStateVersion {
		return "", fmt.Errorf("marshal Discord sync state: unsupported version %d", s.Version)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal Discord sync state: %w", err)
	}
	return string(encoded), nil
}

// Merge incorporates a newer active-run checkpoint over a completed-run
// baseline. Comparable cursors only advance; opaque incomplete-backfill bounds
// use newer non-empty values and cannot be erased by an unrelated checkpoint.
func (s *SyncState) Merge(other *SyncState) {
	if other == nil {
		return
	}
	if s.Containers == nil {
		s.Containers = map[string]ContainerState{}
	}
	if s.ThreadCatalog == nil {
		s.ThreadCatalog = map[string]ThreadCatalogState{}
	}

	for containerID, checkpoint := range other.Containers {
		baseline := s.Containers[containerID]
		if decimalAfter(checkpoint.HighWater, baseline.HighWater) {
			baseline.HighWater = checkpoint.HighWater
		}
		if checkpoint.BackfillBefore != "" {
			baseline.BackfillBefore = checkpoint.BackfillBefore
		}
		if checkpoint.BackfillUpper != "" {
			baseline.BackfillUpper = checkpoint.BackfillUpper
		}
		baseline.BackfillComplete = baseline.BackfillComplete || checkpoint.BackfillComplete
		s.Containers[containerID] = baseline
	}

	for parentID, checkpoint := range other.ThreadCatalog {
		baseline := s.ThreadCatalog[parentID]
		if timestampAfter(checkpoint.PublicArchiveWatermark, baseline.PublicArchiveWatermark) {
			baseline.PublicArchiveWatermark = checkpoint.PublicArchiveWatermark
		}
		if timestampAfter(checkpoint.PrivateArchiveWatermark, baseline.PrivateArchiveWatermark) {
			baseline.PrivateArchiveWatermark = checkpoint.PrivateArchiveWatermark
		}
		s.ThreadCatalog[parentID] = baseline
	}
}

func decimalAfter(candidate, existing string) bool {
	if candidate == "" {
		return false
	}
	if existing == "" {
		return true
	}
	candidate = strings.TrimLeft(candidate, "0")
	existing = strings.TrimLeft(existing, "0")
	if len(candidate) != len(existing) {
		return len(candidate) > len(existing)
	}
	return candidate > existing
}

func timestampAfter(candidate, existing string) bool {
	if candidate == "" {
		return false
	}
	if existing == "" {
		return true
	}
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate)
	existingTime, existingErr := time.Parse(time.RFC3339Nano, existing)
	if candidateErr == nil && existingErr == nil {
		return candidateTime.After(existingTime)
	}
	return candidate > existing
}
