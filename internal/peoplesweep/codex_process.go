package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

type codexBoundLauncher struct {
	gate    CodexIsolationGate
	starter CommandStarter
	proxy   CodexServiceProxy
}

// CodexServiceProxy attaches a bounded host-side service to a disposable work
// root. The default is nil, which keeps the child's network namespace disconnected.
type CodexServiceProxy interface {
	Attach(ctx context.Context, workRoot string) (CodexProxySession, error)
}

type CodexProxySession interface {
	SocketPath() string
	Close() error
}

type codexProxyCommandStarter interface {
	StartWithProxy(ctx context.Context, executable CodexExecutable, args []string, env []string, dir string, socketPath string) (RPCProcess, error)
}

var ErrCodexProxyUnreleased = errors.New("codex service proxy is not released")

type codexOwnedProcess struct {
	RPCProcess

	workRoot         string
	cleanupMu        sync.Mutex
	cleaned          bool
	removeRoot       func(string) error
	proxy            CodexProxySession
	cleanupErr       error
	refreshCommit    func() error
	commitAuth       bool
	refreshCommitted bool
	childExited      chan struct{}
	childExitOnce    sync.Once
}

func (p *codexOwnedProcess) Wait() error {
	processErr := p.RPCProcess.Wait()
	if p.childExited != nil {
		p.childExitOnce.Do(func() { close(p.childExited) })
	}
	var refreshErr error
	p.cleanupMu.Lock()
	if processErr == nil && !p.cleaned && p.commitAuth && p.refreshCommit != nil {
		refreshErr = p.refreshCommit()
		p.refreshCommitted = refreshErr == nil
	}
	p.cleanupMu.Unlock()
	return errors.Join(processErr, refreshErr, p.cleanup())
}

func (p *codexOwnedProcess) refreshSkipped() bool {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	return p.commitAuth && !p.refreshCommitted
}

func (p *codexOwnedProcess) allowRefreshCommit() error {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	if p.cleaned || p.refreshCommit == nil {
		return ErrCodexAuthRefreshUnsafe
	}
	p.commitAuth = true
	return nil
}

func (p *codexOwnedProcess) Kill() error {
	processErr := p.RPCProcess.Kill()
	return errors.Join(processErr, p.cleanup())
}

func (p *codexOwnedProcess) cleanup() error {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	if p.cleaned {
		return p.cleanupErr
	}
	remove := p.removeRoot
	if remove == nil {
		remove = os.RemoveAll
	}
	var proxyErr error
	if p.proxy != nil {
		proxyErr = p.proxy.Close()
	}
	if err := remove(p.workRoot); err != nil {
		return errors.Join(proxyErr, err)
	}
	p.cleaned = true
	p.cleanupErr = proxyErr
	return p.cleanupErr
}

func (l codexBoundLauncher) Verify(ctx context.Context, executable string) (CodexAttestation, error) {
	return l.gate.Verify(ctx, executable, CodexExecutionBoundaryV1)
}

func (l codexBoundLauncher) Start(
	ctx context.Context,
	attestation CodexAttestation,
	authHome string,
) (RPCProcess, error) {
	return l.start(ctx, attestation, authHome, false)
}

func (l codexBoundLauncher) start(
	ctx context.Context,
	attestation CodexAttestation,
	authHome string,
	loginOnly bool,
) (RPCProcess, error) {
	if err := l.gate.ReverifyForLaunch(attestation); err != nil {
		return nil, fmt.Errorf("reverify codex app-server isolation: %w", err)
	}
	var proxyStarter codexProxyCommandStarter
	if l.proxy != nil {
		var ok bool
		proxyStarter, ok = l.starter.(codexProxyCommandStarter)
		if !ok {
			return nil, ErrCodexProxyUnreleased
		}
	}
	workRoot, err := os.MkdirTemp("", "msgvault-codex-work-")
	if err != nil {
		return nil, fmt.Errorf("create codex work root: %w", err)
	}
	cleanup := func(err error) error {
		return errors.Join(err, os.RemoveAll(workRoot))
	}
	var refresh *codexRefreshState
	if loginOnly {
		if err := os.Mkdir(filepath.Join(workRoot, ".codex"), 0o700); err != nil {
			return nil, cleanup(fmt.Errorf("create codex login state: %w", err))
		}
	} else if authHome != "" {
		if err := stageCodexAuth(authHome, workRoot); err != nil {
			return nil, cleanup(fmt.Errorf("stage codex auth: %w", err))
		}
		refresh, err = prepareCodexRefresh(authHome, workRoot)
		if err != nil {
			return nil, cleanup(fmt.Errorf("snapshot codex auth: %w", err))
		}
	}
	var proxySession CodexProxySession
	if l.proxy != nil {
		proxySession, err = l.proxy.Attach(ctx, workRoot)
		if err != nil {
			return nil, cleanup(fmt.Errorf("attach codex service proxy: %w", err))
		}
		if proxySession == nil {
			return nil, cleanup(errors.New("codex service proxy did not create a session"))
		}
	}
	cleanupStart := func(err error) error {
		if proxySession != nil {
			err = errors.Join(err, proxySession.Close())
		}
		return cleanup(err)
	}
	var process RPCProcess
	if proxySession == nil {
		process, err = l.starter.Start(ctx, attestation.VerifiedExecutable(), slices.Clone(codexAppServerArgs), nil, workRoot)
	} else {
		process, err = proxyStarter.StartWithProxy(ctx, attestation.VerifiedExecutable(), slices.Clone(codexAppServerArgs), nil, workRoot, proxySession.SocketPath())
	}
	if err != nil {
		return nil, cleanupStart(err)
	}
	owned := &codexOwnedProcess{RPCProcess: process, workRoot: workRoot, proxy: proxySession, childExited: make(chan struct{})}
	if refresh != nil {
		owned.refreshCommit = refresh.commit
	}
	return owned, nil
}

func stageCodexAuth(authHome, workRoot string) error {
	if !filepath.IsAbs(authHome) || filepath.Clean(authHome) != authHome {
		return errors.New("codex auth home must be an absolute clean path")
	}
	info, err := os.Lstat(authHome)
	if err != nil {
		return errors.New("codex auth home is unavailable")
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(info) {
		return errors.New("codex auth home must be a private directory")
	}
	root, err := os.OpenRoot(authHome)
	if err != nil {
		return errors.New("codex auth home cannot be opened")
	}
	defer func() { _ = root.Close() }()
	info, err = root.Lstat("auth.json")
	if err != nil {
		return errors.New("codex auth.json is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() > 1<<20 || !codexAuthOwnedByDaemon(info) {
		return errors.New("codex auth.json must be a private regular file under 1 MiB")
	}
	source, err := root.Open("auth.json")
	if err != nil {
		return errors.New("codex auth.json cannot be opened")
	}
	defer func() { _ = source.Close() }()
	opened, err := source.Stat()
	if err != nil {
		return errors.New("codex auth.json cannot be inspected")
	}
	if !os.SameFile(info, opened) || !codexAuthOwnedByDaemon(opened) {
		return errors.New("codex auth.json changed during open")
	}
	destDir := filepath.Join(workRoot, ".codex")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		return err
	}
	dest, err := os.OpenFile(filepath.Join(destDir, "auth.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(dest, source, 1<<20+1)
	closeErr := dest.Close()
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		return errors.New("codex auth.json cannot be staged")
	}
	if closeErr != nil {
		return errors.New("codex staged auth.json cannot be closed")
	}
	if copied, err := os.Stat(filepath.Join(destDir, "auth.json")); err != nil || copied.Size() > 1<<20 {
		return errors.New("codex auth.json exceeds 1 MiB")
	}
	return nil
}
