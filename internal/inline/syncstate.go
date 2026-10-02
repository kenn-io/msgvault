package inline

import (
	"encoding/json/v2"
	"fmt"
)

// ChatState separates incomplete older history from a pinned newest-message
// scan. Cursor positions move only after each message snapshot is durable.
type ChatState struct {
	Initialized   bool  `json:"initialized,omitempty"`
	Head          int64 `json:"head,omitempty"`
	HistoryBefore int64 `json:"history_before,omitempty"`
	HistoryDone   bool  `json:"history_done,omitempty"`
	ScanActive    bool  `json:"scan_active,omitempty"`
	ScanBefore    int64 `json:"scan_before,omitempty"`
	ScanHead      int64 `json:"scan_head,omitempty"`
	ScanFloor     int64 `json:"scan_floor,omitempty"`
	RepairActive  bool  `json:"repair_active,omitempty"`
	RepairBefore  int64 `json:"repair_before,omitempty"`
	RepairHead    int64 `json:"repair_head,omitempty"`
}

type SyncState struct {
	Account       string                `json:"account"`
	RepairPending bool                  `json:"repair_pending,omitempty"`
	RepairScope   []int64               `json:"repair_scope,omitempty"`
	Chats         map[string]*ChatState `json:"chats"`
}

func LoadSyncState(blob, account string) (*SyncState, error) {
	state := &SyncState{Account: account, Chats: map[string]*ChatState{}}
	if blob == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(blob), state); err != nil {
		return nil, fmt.Errorf("decode Inline checkpoint: %w", err)
	}
	if state.Account != account || state.Chats == nil {
		return nil, fmt.Errorf("Inline checkpoint belongs to another account or is incomplete")
	}
	seen := map[int64]bool{}
	for _, id := range state.RepairScope {
		if !validID(id) || seen[id] {
			return nil, fmt.Errorf("invalid Inline repair scope")
		}
		seen[id] = true
	}
	for key, chat := range state.Chats {
		if chat == nil {
			return nil, fmt.Errorf("Inline checkpoint chat %q is null", key)
		}
		for _, id := range []int64{chat.Head, chat.HistoryBefore, chat.ScanBefore, chat.ScanHead, chat.ScanFloor, chat.RepairBefore, chat.RepairHead} {
			if id < 0 || id > MaxID {
				return nil, fmt.Errorf("invalid Inline checkpoint cursor")
			}
		}
		if chat.ScanActive && (!chat.Initialized || (chat.ScanHead > 0 && chat.ScanHead < chat.ScanFloor)) {
			return nil, fmt.Errorf("invalid Inline scan bounds")
		}
	}
	return state, nil
}

func (s *SyncState) Marshal() (string, error) {
	data, err := json.Marshal(s, json.Deterministic(true))
	return string(data), err
}
func (s *SyncState) chat(id int64) *ChatState {
	key := chatKey(id)
	if s.Chats[key] == nil {
		s.Chats[key] = &ChatState{}
	}
	return s.Chats[key]
}
