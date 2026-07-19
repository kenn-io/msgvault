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
	if err := state.validate(); err != nil {
		return nil, fmt.Errorf("validate Discord sync state: %w", err)
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
	if err := s.validate(); err != nil {
		return "", fmt.Errorf("marshal Discord sync state: %w", err)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal Discord sync state: %w", err)
	}
	return string(encoded), nil
}

func (s *SyncState) validate() error {
	if s == nil {
		return errors.New("nil state")
	}
	if s.Version != SyncStateVersion {
		return fmt.Errorf("unsupported version %d", s.Version)
	}
	for containerID, container := range s.Containers {
		fields := []struct {
			name  string
			value string
		}{
			{name: "high_water", value: container.HighWater},
			{name: "backfill_before", value: container.BackfillBefore},
			{name: "backfill_upper", value: container.BackfillUpper},
		}
		for _, field := range fields {
			if field.value == "" {
				continue
			}
			if _, err := ParseSnowflake(field.value); err != nil {
				return fmt.Errorf("containers[%q].%s: %w", containerID, field.name, err)
			}
		}
	}

	for parentID, catalog := range s.ThreadCatalog {
		fields := []struct {
			name  string
			value string
		}{
			{name: "public_archive_watermark", value: catalog.PublicArchiveWatermark},
			{name: "private_archive_watermark", value: catalog.PrivateArchiveWatermark},
		}
		for _, field := range fields {
			if field.value == "" {
				continue
			}
			if _, err := time.Parse(time.RFC3339Nano, field.value); err != nil {
				return fmt.Errorf("thread_catalog[%q].%s: %w", parentID, field.name, err)
			}
		}
	}
	return nil
}

// Merge incorporates a newer active-run checkpoint over a completed-run
// baseline. Comparable cursors only advance; opaque incomplete-backfill bounds
// use newer non-empty values and cannot be erased by an unrelated checkpoint.
func (s *SyncState) Merge(other *SyncState) error {
	if err := s.validate(); err != nil {
		return fmt.Errorf("validate Discord sync baseline: %w", err)
	}
	if other == nil {
		return nil
	}
	if err := other.validate(); err != nil {
		return fmt.Errorf("validate Discord sync checkpoint: %w", err)
	}
	if s.Containers == nil {
		s.Containers = map[string]ContainerState{}
	}
	if s.ThreadCatalog == nil {
		s.ThreadCatalog = map[string]ThreadCatalogState{}
	}

	for containerID, checkpoint := range other.Containers {
		baseline := s.Containers[containerID]
		after, err := snowflakeAfter(checkpoint.HighWater, baseline.HighWater)
		if err != nil {
			return fmt.Errorf("compare containers[%q].high_water: %w", containerID, err)
		}
		if after {
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
		publicAfter, err := timestampAfter(checkpoint.PublicArchiveWatermark, baseline.PublicArchiveWatermark)
		if err != nil {
			return fmt.Errorf("compare thread_catalog[%q].public_archive_watermark: %w", parentID, err)
		}
		if publicAfter {
			baseline.PublicArchiveWatermark = checkpoint.PublicArchiveWatermark
		}
		privateAfter, err := timestampAfter(checkpoint.PrivateArchiveWatermark, baseline.PrivateArchiveWatermark)
		if err != nil {
			return fmt.Errorf("compare thread_catalog[%q].private_archive_watermark: %w", parentID, err)
		}
		if privateAfter {
			baseline.PrivateArchiveWatermark = checkpoint.PrivateArchiveWatermark
		}
		s.ThreadCatalog[parentID] = baseline
	}
	return nil
}

func snowflakeAfter(candidate, existing string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	if existing == "" {
		return true, nil
	}
	candidateValue, err := ParseSnowflake(candidate)
	if err != nil {
		return false, err
	}
	existingValue, err := ParseSnowflake(existing)
	if err != nil {
		return false, err
	}
	return candidateValue > existingValue, nil
}

func timestampAfter(candidate, existing string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	if existing == "" {
		return true, nil
	}
	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate)
	existingTime, existingErr := time.Parse(time.RFC3339Nano, existing)
	if candidateErr != nil {
		return false, fmt.Errorf("parse candidate timestamp: %w", candidateErr)
	}
	if existingErr != nil {
		return false, fmt.Errorf("parse existing timestamp: %w", existingErr)
	}
	return candidateTime.After(existingTime), nil
}
