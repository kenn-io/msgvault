package telegram

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"
)

const syncStateVersion = 1

// HistoryProgress identifies a snapshot being walked newest to oldest. Offset
// is the next exclusive page boundary, committed only after page writes.
type HistoryProgress struct {
	Ceiling    int            `json:"ceiling"`
	Offset     int            `json:"offset,omitempty"`
	MinID      int            `json:"min_id,omitempty"`
	Ranges     []HistoryRange `json:"ranges,omitempty"`
	RangeIndex int            `json:"range_index,omitempty"`
	After      time.Time      `json:"after,omitzero"`
	Full       bool           `json:"full,omitzero"`
}

// HistoryRange is a Telegram takeout split range used for one history pass.
type HistoryRange struct {
	MinID int `json:"min_id"`
	MaxID int `json:"max_id"`
}

// DialogState keeps completed history separate from any interrupted scan.
type DialogState struct {
	Frontier   int              `json:"frontier,omitempty"`
	History    *HistoryProgress `json:"history,omitempty"`
	Peer       *DialogPeer      `json:"peer,omitempty"`
	ChannelPTS *int             `json:"channel_pts,omitempty"`
}

// DialogPeer preserves the catalog identity needed to resume a dialog.
type DialogPeer struct {
	NativeID          int64      `json:"native_id"`
	Kind              string     `json:"kind"`
	AccessHash        int64      `json:"access_hash,omitempty"`
	Title             string     `json:"title,omitempty"`
	TopMessage        int        `json:"top_message"`
	ObservedPTS       *int       `json:"observed_pts,omitempty"`
	EnabledByDefault  bool       `json:"enabled_by_default"`
	Self              bool       `json:"self,omitempty"`
	InaccessibleSince *time.Time `json:"inaccessible_since,omitempty"`
	MissingSince      *time.Time `json:"missing_since,omitempty"`
}

// CommonUpdateState is the last durably applied account-wide update position.
type CommonUpdateState struct {
	Pts  int `json:"pts"`
	Qts  int `json:"qts"`
	Date int `json:"date"`
	Seq  int `json:"seq"`
}

// SyncState is the versioned Telegram source checkpoint.
type SyncState struct {
	Version int                    `json:"version"`
	Dialogs map[string]DialogState `json:"dialogs"`
	Common  *CommonUpdateState     `json:"common,omitempty"`
	Gaps    map[string]RecoveryGap `json:"gaps,omitempty"`
	Counts  SyncCounts             `json:"-"`
}

// RecoveryGap records a bounded rescan that cannot prove older edits or
// deletions. The key is "common" or the marked channel ID.
type RecoveryGap struct {
	FirstDetected time.Time `json:"first_detected"`
	LastDetected  time.Time `json:"last_detected"`
	WindowStart   time.Time `json:"window_start"`
	Count         int       `json:"count"`
}

// WithRecoveryGap returns a copy so a failed save cannot publish the warning.
func (s *SyncState) WithRecoveryGap(key string, detectedAt, windowStart time.Time) (*SyncState, error) {
	if s == nil || !validRecoveryGapKey(key) || detectedAt.IsZero() || windowStart.IsZero() || windowStart.After(detectedAt) {
		return nil, errors.New("invalid Telegram recovery gap")
	}
	next := *s
	next.Gaps = make(map[string]RecoveryGap, len(s.Gaps)+1)
	for k, gap := range s.Gaps {
		next.Gaps[k] = gap
	}
	gap := next.Gaps[key]
	if gap.Count > 0 && detectedAt.Before(gap.LastDetected) {
		return nil, errors.New("Telegram recovery gap time moved backwards")
	}
	if gap.Count == math.MaxInt {
		return nil, errors.New("Telegram recovery gap count overflow")
	}
	if gap.Count == 0 {
		gap.FirstDetected = detectedAt.UTC()
	}
	gap.LastDetected, gap.WindowStart = detectedAt.UTC(), windowStart.UTC()
	gap.Count++
	next.Gaps[key] = gap
	return &next, nil
}

func validRecoveryGapKey(key string) bool {
	if key == "common" {
		return true
	}
	id, err := strconv.ParseInt(key, 10, 64)
	return err == nil && strconv.FormatInt(id, 10) == key && constant.TDLibPeerID(id).IsChannel()
}

// SyncCounts covers completed work in the current run, not inherited cursors.
type SyncCounts struct {
	Processed int64
	Added     int64
	Updated   int64
}

func NewSyncState() *SyncState {
	return &SyncState{Version: syncStateVersion, Dialogs: make(map[string]DialogState)}
}

// LoadSyncState refuses unknown or malformed checkpoints instead of losing progress.
func LoadSyncState(blob string) (*SyncState, error) {
	if blob == "" {
		return NewSyncState(), nil
	}
	var state SyncState
	decoder := jsontext.NewDecoder(strings.NewReader(blob), json.RejectUnknownMembers(true))
	if err := json.UnmarshalDecode(decoder, &state); err != nil {
		return nil, fmt.Errorf("decode Telegram sync state: %w", err)
	}
	var trailing any
	if err := json.UnmarshalDecode(decoder, &trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid trailing Telegram sync state")
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *SyncState) Marshal() (string, error) {
	if err := s.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(s, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode Telegram sync state: %w", err)
	}
	return string(data), nil
}

func (s *SyncState) validate() error {
	if s == nil || s.Version != syncStateVersion || s.Dialogs == nil {
		return errors.New("invalid Telegram sync state")
	}
	if c := s.Common; c != nil && (c.Pts < 0 || int64(c.Pts) > math.MaxInt32 || c.Qts < 0 || int64(c.Qts) > math.MaxInt32 || c.Date <= 0 || int64(c.Date) > math.MaxInt32 || c.Seq < 0 || int64(c.Seq) > math.MaxInt32) {
		return errors.New("invalid Telegram update state")
	}
	for key, gap := range s.Gaps {
		if !validRecoveryGapKey(key) || gap.Count <= 0 || gap.FirstDetected.IsZero() || gap.LastDetected.Before(gap.FirstDetected) || gap.WindowStart.IsZero() || gap.WindowStart.After(gap.LastDetected) {
			return errors.New("invalid Telegram recovery gap")
		}
	}
	for key, dialog := range s.Dialogs {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id == 0 || strconv.FormatInt(id, 10) != key {
			return errors.New("invalid Telegram dialog state")
		}
		if err := dialog.Validate(); err != nil {
			return err
		}
		if dialog.Peer != nil {
			peer := dialog.Peer
			if peer.NativeID <= 0 || peer.TopMessage < 0 || int64(peer.TopMessage) > math.MaxInt32 || (peer.ObservedPTS != nil && (*peer.ObservedPTS < 0 || int64(*peer.ObservedPTS) > math.MaxInt32)) || (peer.Self && peer.Kind != "user") || (peer.MissingSince != nil && peer.MissingSince.IsZero()) || (peer.InaccessibleSince != nil && peer.InaccessibleSince.IsZero()) {
				return errors.New("invalid Telegram catalog peer")
			}
			var native tg.PeerClass
			switch peer.Kind {
			case "user":
				native = &tg.PeerUser{UserID: peer.NativeID}
			case "group":
				native = &tg.PeerChat{ChatID: peer.NativeID}
			case "supergroup", "channel":
				native = &tg.PeerChannel{ChannelID: peer.NativeID}
			default:
				return errors.New("invalid Telegram catalog peer kind")
			}
			marked, err := MarkedPeerID(native)
			if err != nil || marked != id {
				return errors.New("Telegram catalog peer does not match dialog key")
			}
		}
	}
	return nil
}

// Validate rejects inconsistent completed and in-progress history bounds.
func (d DialogState) Validate() error {
	if d.Frontier < 0 || int64(d.Frontier) > math.MaxInt32 {
		return errors.New("invalid Telegram dialog state")
	}
	if d.ChannelPTS != nil && (*d.ChannelPTS < 0 || int64(*d.ChannelPTS) > math.MaxInt32) {
		return errors.New("invalid Telegram channel pts")
	}
	if p := d.History; p != nil {
		if p.Ceiling <= 0 || int64(p.Ceiling) > math.MaxInt32 || p.MinID < 0 || p.MinID >= p.Ceiling || p.Offset < 0 || p.Offset > p.Ceiling || (p.Offset != 0 && p.Offset <= p.MinID) || (p.Full && p.MinID != 0) || p.RangeIndex < 0 || (len(p.Ranges) == 0 && p.RangeIndex != 0) || (len(p.Ranges) > 0 && p.RangeIndex >= len(p.Ranges)) {
			return errors.New("invalid Telegram history checkpoint")
		}
		for _, messageRange := range p.Ranges {
			if messageRange.MinID < 0 || messageRange.MaxID <= messageRange.MinID || int64(messageRange.MaxID) > math.MaxInt32 {
				return errors.New("invalid Telegram history checkpoint")
			}
		}
	}
	return nil
}
