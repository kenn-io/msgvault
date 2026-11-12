package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type accountAttributionRepairStore interface {
	GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error)
	RepairAccountAttributionContext(ctx context.Context, id int64, progress func(store.AccountAttributionRepairSummary)) (store.AccountAttributionRepairSummary, error)
}

func (s *Server) handleAccountAttributionRepair(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("source_id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_source_id", "Source ID must be a positive integer")
		return
	}
	st, ok := s.store.(accountAttributionRepairStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "Database not available")
		return
	}
	_, err = st.GetSourceByIDContext(r.Context(), id)
	if errors.Is(err, store.ErrSourceNotFound) {
		writeError(w, http.StatusNotFound, "source_not_found", "Source not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve source")
		return
	}
	if !s.queueAccountAttributionRepair(r.Context(), st, id, false) {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", "Account matching is unavailable")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type accountAttributionRecoveryStore interface {
	accountAttributionRepairStore
	AccountIdentityRevisionContext(ctx context.Context) (int64, error)
	PendingAccountAttributionSourcesContext(ctx context.Context) ([]int64, error)
}

// StartAccountAttributionRecovery observes identity changes for daemon-owned recovery.
func (s *Server) StartAccountAttributionRecovery() {
	st, ok := s.store.(accountAttributionRecoveryStore)
	if !ok {
		return
	}
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importsClosed {
		return
	}
	s.importWG.Go(func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		s.observeAccountAttribution(st, ticker.C)
	})
}

func (s *Server) observeAccountAttribution(st accountAttributionRecoveryStore, ticks <-chan time.Time) {
	var last int64
	var observed bool
	for {
		revision, err := st.AccountIdentityRevisionContext(s.importContext)
		if err == nil && (!observed || revision != last) {
			ids, err := st.PendingAccountAttributionSourcesContext(s.importContext)
			if err == nil {
				for _, id := range ids {
					s.queueAccountAttributionRepair(s.importContext, st, id, true)
				}
				last, observed = revision, true
			} else if s.importContext.Err() == nil {
				s.logger.Warn("discovering pending account matching", "error", err)
			}
		} else if err != nil && s.importContext.Err() == nil {
			s.logger.Warn("reading account identity revision", "error", err)
		}
		select {
		case <-ticks:
		case <-s.importContext.Done():
			return
		}
	}
}

type accountAttributionRepairJob struct {
	dirty bool
	retry bool
}

func (s *Server) accountAttributionJob(id int64) *accountAttributionRepairJob {
	if s.accountRepairs == nil {
		s.accountRepairs = make(map[int64]*accountAttributionRepairJob)
	}
	return s.accountRepairs[id]
}

func (s *Server) queueAccountAttributionRepair(ctx context.Context, st accountAttributionRepairStore, id int64, maintenance bool) bool {
	s.importMu.Lock()
	defer s.importMu.Unlock()
	if s.importsClosed {
		return false
	}
	if existing := s.accountAttributionJob(id); existing != nil {
		if maintenance {
			existing.dirty = true
		} else {
			existing.retry = true
		}
		return true
	}
	job := &accountAttributionRepairJob{}
	s.accountRepairs[id] = job
	var finishIdle func()
	var ok, failed bool
	if maintenance {
		finishIdle, ok = s.idleTracker.BeginMaintenanceWorkContext(ctx)
	} else {
		finishIdle, ok = s.idleTracker.BeginWorkContext(ctx)
	}
	if !ok {
		delete(s.accountRepairs, id)
		return false
	}
	s.importWG.Go(func() {
		defer finishIdle()
		defer func() {
			s.importMu.Lock()
			again := job.dirty || job.retry && failed
			delete(s.accountRepairs, id)
			s.importMu.Unlock()
			if again {
				s.queueAccountAttributionRepair(s.importContext, st, id, true)
			}
		}()
		release, ok := s.beginBackgroundOperationGateWork(s.importContext, "account matching")
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.importContext, 10*time.Minute)
		defer cancel()
		summary, err := st.RepairAccountAttributionContext(ctx, id, nil)
		failed = err != nil
		release()
		if summary.Scanned > 0 {
			s.scheduleAccountIdentityCacheRebuild(ctx)
		}
		if err != nil {
			s.logger.Error("failed to repair account attribution", "source_id", id, "error", err)
		}
	})
	return true
}
