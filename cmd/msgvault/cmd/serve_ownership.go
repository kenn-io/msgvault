package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const daemonOwnerLockFile = "daemon.lock"

type serveOwnership struct {
	dataDir       string
	shutdownToken string
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
	_, shutdownToken, err := writeDaemonRuntime(cfg.Data.DataDir, host, port, version, cfg.Server.APIKey)
	if err != nil {
		_ = lock.Close()
		_ = daemonLock.Close()
		return nil, fmt.Errorf("write daemon runtime: %w", err)
	}
	return &serveOwnership{
		dataDir:       cfg.Data.DataDir,
		shutdownToken: shutdownToken,
		daemonLock:    daemonLock,
		lock:          lock,
	}, nil
}

func (o *serveOwnership) Close() error {
	if o == nil {
		return nil
	}
	removeDaemonRuntime(o.dataDir)
	return errors.Join(o.lock.Close(), o.daemonLock.Close())
}

type daemonOwnerLock struct {
	path string
	lock *flock.Flock
}

func daemonOwnerLockPath(dataDir string) string {
	return filepath.Join(dataDir, daemonOwnerLockFile)
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
			"(daemon lock %s is held); stop it with `msgvault serve stop` "+
			"or use `msgvault serve status` to inspect it",
		e.path,
	)
}
