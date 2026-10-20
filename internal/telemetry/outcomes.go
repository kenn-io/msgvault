package telemetry

import (
	"context"
	"errors"
	"sync"

	"go.kenn.io/msgvault/internal/syncerr"
)

type SyncFailures struct {
	Capture func(string, map[string]any) error
	failing sync.Map
}

func (s *SyncFailures) Record(sourceType, identifier string, err error) {
	if s == nil || errors.Is(err, context.Canceled) {
		return
	}
	key := [2]string{sourceType, identifier}
	if err == nil {
		s.failing.Delete(key)
		return
	}
	if _, loaded := s.failing.LoadOrStore(key, true); !loaded && s.Capture != nil {
		_ = s.Capture(EventSyncFailed, map[string]any{"sync_source": sourceType, "reason": syncerr.Reason(err)})
	}
}
