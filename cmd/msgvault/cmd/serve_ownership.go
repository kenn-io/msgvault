package cmd

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const daemonOwnerLockFile = "daemon.lock"

type serveOwnership struct {
	mu            sync.Mutex
	closed        bool
	dataDir       string
	shutdownToken string
	record        daemon.RuntimeRecord
	daemonLock    *daemonOwnerLock
	lock          *writeOwnerLock
}

func claimServeOwnership(
	ctx context.Context,
	cfg *config.Config,
	host string,
	port int,
	version string,
) (*serveOwnership, error) {
	if cfg == nil {
		return nil, errors.New("nil config")
	}
	daemonLock, err := tryAcquireDaemonOwnerLock(cfg.Data.DataDir)
	if err != nil {
		return nil, err
	}
	var lock *writeOwnerLock
	if !store.IsPostgresURL(cfg.DatabaseDSN()) {
		lock, err = acquireWriteOwnerLock(ctx, cfg.Data.DataDir)
		if err != nil {
			_ = daemonLock.Close()
			return nil, err
		}
	}
	record, shutdownToken, err := writeDaemonRuntime(cfg.Data.DataDir, host, port, version, cfg.Server.APIKey)
	if err != nil {
		_ = lock.Close()
		_ = daemonLock.Close()
		return nil, fmt.Errorf("write daemon runtime: %w", err)
	}
	return &serveOwnership{
		dataDir:       cfg.Data.DataDir,
		shutdownToken: shutdownToken,
		record:        record,
		daemonLock:    daemonLock,
		lock:          lock,
	}, nil
}

// SetStartupPhase publishes what the starting daemon is currently doing in
// its runtime record so `msgvault daemon status` can report progress while
// the HTTP server is not answering pings yet (for example during a long
// analytics cache rebuild). An empty phase marks startup as finished.
func (o *serveOwnership) SetStartupPhase(phase string) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	rec := o.record
	metadata := maps.Clone(rec.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	if phase == "" {
		delete(metadata, runtimeStartupPhase)
	} else {
		metadata[runtimeStartupPhase] = phase
	}
	rec.Metadata = metadata
	if _, err := daemonRuntimeStore(o.dataDir).Write(rec); err != nil {
		return fmt.Errorf("update daemon startup phase: %w", err)
	}
	o.record = rec
	return nil
}

func (o *serveOwnership) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.closed = true
	removeDaemonRuntime(o.dataDir)
	return errors.Join(o.lock.Close(), o.daemonLock.Close())
}

// daemonRuntimeHeartbeatInterval is how often serve re-checks that its
// runtime record is still on disk, republishing it when missing.
const daemonRuntimeHeartbeatInterval = 30 * time.Second

// EnsureRuntimeRecord republishes the daemon's runtime record if the file
// has gone missing from disk. A CLI process with a skewed view of process
// identity (a different PID namespace, or jittery boot-time reads in
// older binaries) can wrongly prune the record; republishing lets the
// daemon self-heal instead of staying undiscoverable until it restarts.
func (o *serveOwnership) EnsureRuntimeRecord() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	store := daemonRuntimeStore(o.dataDir)
	path, err := store.Path(o.record.PID)
	if err != nil {
		return fmt.Errorf("daemon runtime record path: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect daemon runtime record: %w", err)
	}
	if _, err := store.Write(o.record); err != nil {
		return fmt.Errorf("republish daemon runtime record: %w", err)
	}
	return nil
}

// runtimeRecordHeartbeat republishes the runtime record until ctx is
// cancelled. Failures are retried on the next tick rather than surfaced:
// the daemon keeps serving even when the record cannot be rewritten.
// serveOwnership serializes heartbeats with startup-phase updates and
// closure so a tick cannot republish the record after shutdown removes it.
func runtimeRecordHeartbeat(ctx context.Context, ownership *serveOwnership, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = ownership.EnsureRuntimeRecord()
		}
	}
}

type daemonOwnerLock struct {
	path string
	lock *flock.Flock
}

func daemonOwnerLockPath(dataDir string) string {
	return filepath.Join(dataDir, daemonOwnerLockFile)
}

// daemonOwnerLockHeld reports whether another process currently owns the
// archive's daemon lease. A missing lock is cleanly unheld; other filesystem
// errors are returned so safety-critical callers can fail closed. The lease
// can establish that daemon startup is in progress even when process
// create-time reads are temporarily inconsistent; it does not authenticate a
// runtime record's PID or HTTP endpoint.
func daemonOwnerLockHeld(dataDir string) (bool, error) {
	path := daemonOwnerLockPath(dataDir)
	lock := flock.New(path, flock.SetFlag(os.O_RDWR))
	locked, err := lock.TryLock()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("probe daemon lock %s: %w", path, err)
	}
	if err := lock.Close(); err != nil {
		return false, fmt.Errorf("close daemon lock probe %s: %w", path, err)
	}
	return !locked, nil
}

func tryAcquireDaemonOwnerLock(dataDir string) (*daemonOwnerLock, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir for daemon lock: %w", err)
	}
	path := daemonOwnerLockPath(dataDir)
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire daemon lock %s: %w", path, err)
	}
	if !locked {
		return nil, daemonOwnerLockHeldError{path: path}
	}
	return &daemonOwnerLock{path: path, lock: lock}, nil
}

func (l *daemonOwnerLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("release daemon lock %s: %w", l.path, err)
	}
	return nil
}

type daemonOwnerLockHeldError struct {
	path string
}

func (e daemonOwnerLockHeldError) Error() string {
	return fmt.Sprintf(
		"msgvault daemon is already running for this data directory "+
			"(daemon lock %s is held); stop it with `msgvault daemon stop` "+
			"or use `msgvault daemon status` to inspect it",
		e.path,
	)
}
