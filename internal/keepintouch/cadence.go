package keepintouch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// Eligible uses the existing counted-contact projection; it never counts
// messages itself or assigns a cadence. A short interval retains at least
// one whole day after contact before it enters the approach window.
func Eligible(state store.ContactState, now time.Time, approachDays int) bool {
	if state.Stale || state.LastContactAt == nil || state.CadenceDueAt == nil || approachDays < 0 || approachDays > 3660 {
		return false
	}
	last, due := state.LastContactAt.UTC(), state.CadenceDueAt.UTC()
	if !due.After(last) || last.After(now) {
		return false
	}
	days := (due.Unix() - last.Unix()) / 86400
	if days < 1 {
		return false
	}
	window := min(int64(approachDays), days-1)
	return !now.Before(due.AddDate(0, 0, -int(window)))
}

// CycleKey deliberately excludes the run date, interval, draft and provider.
// The same qualifying-contact gap stays one cycle even after closing its task.
func CycleKey(archiveUID, personUID string, last time.Time) string {
	data, _ := json.Marshal([]string{"msgvault.keep-in-touch.v1", archiveUID, personUID, last.UTC().Format(time.RFC3339Nano)})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
