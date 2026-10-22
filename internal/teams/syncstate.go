package teams

import (
	"encoding/json/v2"
	"maps"
	"time"
)

// SyncState holds per-conversation incremental cursors, persisted as JSON in
// sync_runs.cursor_after. Chats use a max-lastModifiedDateTime timestamp;
// channels use an @odata.deltaLink.
type SyncState struct {
	Chats             map[string]string `json:"chats"`    // chatID -> max lastModifiedDateTime (RFC3339)
	Channels          map[string]string `json:"channels"` // "teamID/channelID" -> deltaLink
	CoveredChats      map[string]bool   `json:"covered_chats,omitempty"`
	ResetChatBaseline bool              `json:"reset_chat_baseline,omitempty"`
}

func NewSyncState() *SyncState {
	return &SyncState{Chats: map[string]string{}, Channels: map[string]string{}, CoveredChats: map[string]bool{}}
}

func LoadSyncState(blob string) (*SyncState, error) {
	s := NewSyncState()
	if blob == "" {
		return s, nil
	}
	if err := json.Unmarshal([]byte(blob), s); err != nil {
		return nil, err
	}
	if s.Chats == nil {
		s.Chats = map[string]string{}
	}
	if s.Channels == nil {
		s.Channels = map[string]string{}
	}
	if s.CoveredChats == nil {
		s.CoveredChats = map[string]bool{}
	}
	return s, nil
}

func (s *SyncState) Marshal() (string, error) {
	b, err := json.Marshal(s, json.Deterministic(true))
	return string(b), err
}

func (s *SyncState) ChatCursor(chatID string) string     { return s.Chats[chatID] }
func (s *SyncState) SetChatCursor(chatID, cursor string) { s.Chats[chatID] = cursor }
func (s *SyncState) ChannelDelta(key string) string      { return s.Channels[key] }
func (s *SyncState) SetChannelDelta(key, link string)    { s.Channels[key] = link }

// ChatCovered accepts completed empty history or a valid legacy chat cursor.
func (s *SyncState) ChatCovered(chatID string) bool {
	if s.CoveredChats[chatID] {
		return true
	}
	_, err := time.Parse(time.RFC3339Nano, s.Chats[chatID])
	return err == nil
}

// Merge incorporates cursors from other into s, keeping the more-advanced value for
// each conversation. A full-repair checkpoint replaces chat coverage instead.
// If other is nil or s itself it is silently ignored.
//
// Chat cursors are RFC3339Nano timestamps. They are parsed before comparison
// because RFC3339Nano omits trailing fractional zeroes, so string ordering is
// not a reliable proxy for time ordering.
//
// Channel deltaLinks are opaque Graph tokens that cannot be compared by value; we
// always prefer other's link when it is non-empty, on the assumption that other
// represents a more recent (checkpoint) run whose cursor is at least as advanced.
func (s *SyncState) Merge(other *SyncState) {
	if other == nil || other == s {
		return
	}
	if other.ResetChatBaseline {
		// An interrupted full repair must not inherit an older successful
		// run's coverage. Channel tokens keep their existing merge semantics.
		s.Chats = map[string]string{}
		s.CoveredChats = map[string]bool{}
		maps.Copy(s.Chats, other.Chats)
		maps.Copy(s.CoveredChats, other.CoveredChats)
		s.ResetChatBaseline = true
	} else {
		for chatID, cursor := range other.Chats {
			if chatCursorAfter(cursor, s.Chats[chatID]) {
				s.Chats[chatID] = cursor
			}
		}
		if s.CoveredChats == nil {
			s.CoveredChats = map[string]bool{}
		}
		for chatID, covered := range other.CoveredChats {
			if covered {
				s.CoveredChats[chatID] = true
			}
		}
	}
	for key, link := range other.Channels {
		if link != "" {
			s.Channels[key] = link
		}
	}
}

func chatCursorAfter(candidate, existing string) bool {
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
