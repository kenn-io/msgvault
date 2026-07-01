package cmd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

// setupVectorFeaturesForRun is a test seam for the build-tag-selected
// setupVectorFeatures implementation.
var setupVectorFeaturesForRun = setupVectorFeatures

// vectorInitHandle tracks the background vector init goroutine so shutdown
// can wait for it and close the opened backend.
type vectorInitHandle struct {
	done chan struct{}
	mu   sync.Mutex
	vf   *vectorFeatures
}

// WaitTimeout blocks until the init goroutine finishes or d elapses.
// Returns false on timeout.
func (h *vectorInitHandle) WaitTimeout(d time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(d):
		return false
	}
}

// CloseFeatures closes the vector backend if the init goroutine opened one.
// Only call after WaitTimeout reports the goroutine finished.
func (h *vectorInitHandle) CloseFeatures() {
	h.mu.Lock()
	vf := h.vf
	h.vf = nil
	h.mu.Unlock()
	if vf != nil && vf.Close != nil {
		if err := vf.Close(); err != nil {
			logger.Warn("closing vectors.db failed", "error", err)
		}
	}
}

// startVectorInit runs the expensive vector backend setup (open, schema
// migrations, embed_gen backfill) in the background so the daemon API can
// serve archive requests immediately. The tracker (idle tracker + operation
// gate) serializes the init's msgvault.db writes against scheduled syncs
// and keeps a background daemon from idle-stopping mid-migration. On
// success the components are installed into apiServer and the embed job is
// registered; on failure the daemon keeps serving with vector endpoints
// reporting the error.
func startVectorInit(
	ctx context.Context,
	s *store.Store,
	dbPath string,
	tracker scheduler.WorkTracker,
	apiServer *api.Server,
	sched *scheduler.Scheduler,
) *vectorInitHandle {
	h := &vectorInitHandle{done: make(chan struct{})}
	if !cfg.Vector.Enabled {
		close(h.done)
		return h
	}
	go func() {
		defer close(h.done)
		logger.Info("daemon startup step",
			"step", "init_vector_backend",
			"detail", "running in background; may run vector schema migrations and embed_gen backfill on large archives")
		if tracker != nil {
			release, ok := tracker.BeginWorkContext(ctx)
			if !ok {
				logger.Info("vector init aborted", "reason", "daemon shutting down")
				return
			}
			defer release()
		}
		vf, err := setupVectorFeaturesForRun(ctx, s, dbPath, false)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("vector init cancelled during daemon shutdown")
				return
			}
			logger.Error("vector init failed; vector search unavailable until fixed",
				"error", err)
			apiServer.SetVectorInitError(err)
			return
		}
		if vf == nil {
			// setupVectorFeaturesForRun returns non-nil whenever
			// cfg.Vector.Enabled is true and err is nil; this guards test
			// seams (and any future caller) that don't uphold that
			// invariant instead of panicking on a nil dereference below.
			logger.Warn("vector init returned no components despite no error; leaving vector search uninitialized")
			return
		}
		h.mu.Lock()
		h.vf = vf
		h.mu.Unlock()
		apiServer.SetVectorFeatures(vf.HybridEngine, vf.Backend, vf.Cfg)
		if err := registerEmbedJob(sched, vf, s); err != nil {
			// Cron was validated in precheckVectorFeatures, so this is an
			// invariant violation, not user error; vector search still works.
			logger.Error("register embed job failed", "error", err)
		}
		logger.Info("daemon startup step complete", "step", "init_vector_backend")
	}()
	return h
}

// registerEmbedJob wires the embed worker into the scheduler (cron-driven
// plus optional post-sync hook). Extracted from runServe so the background
// vector init can register it once the backend is ready.
func registerEmbedJob(sched *scheduler.Scheduler, vf *vectorFeatures, s *store.Store) error {
	embedJob := &scheduler.EmbedJob{
		Worker:           vf.Worker,
		Backend:          vf.Backend,
		Store:            s,
		Fingerprint:      vf.Cfg.GenerationFingerprint(),
		BackstopInterval: vf.Cfg.Embed.BackstopInterval,
		BuildScope:       vf.Cfg.Embed.Scope.BuildScope(),
		Log:              logger,
	}
	schedule := cfg.Vector.Embed.Schedule.Cron
	if err := sched.SetEmbedJob(embedJob, schedule, cfg.Vector.Embed.Schedule.RunAfterSync); err != nil {
		return fmt.Errorf("register embed job: %w", err)
	}
	logger.Info("embed scheduled",
		"cron", schedule,
		"run_after_sync", cfg.Vector.Embed.Schedule.RunAfterSync,
	)
	return nil
}
