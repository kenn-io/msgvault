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
// root. The default is nil, which leaves the child with no network path.
type CodexServiceProxy interface {
	Attach(ctx context.Context, workRoot string) (CodexProxySession, error)
}

// CodexProxySession is one attached proxy. Endpoint is what the platform's
// starter hands the child: the Unix socket path on Linux, and the
// credential-bearing loopback proxy URL on macOS.
type CodexProxySession interface {
	Endpoint() string
	Close() error
}

type codexProxyCommandStarter interface {
	StartWithProxy(ctx context.Context, executable CodexExecutable, args []string, env []string, dir string, proxyEndpoint string) (RPCProcess, error)
}

var ErrCodexProxyUnreleased = errors.New("codex service proxy is not released")

type codexOwnedProcess struct {
	RPCProcess

	workRoot   string
	cleanupMu  sync.Mutex
	cleaned    bool
	removeRoot func(string) error
	proxy      CodexProxySession
	// proxyReleased records that the proxy was closed, which happens only
	// once the child has exited: until then it may connect to its address.
	proxyReleased bool
	proxyErr      error
	refreshCommit func() error
	childExited   chan struct{}
	childExitOnce sync.Once
}

func (p *codexOwnedProcess) Wait() error {
	processErr := p.RPCProcess.Wait()
	if p.childExited != nil {
		p.childExitOnce.Do(func() { close(p.childExited) })
	}
	var refreshErr error
	p.cleanupMu.Lock()
	// Authentication refresh is independent of the operation's result. Copy
	// back only after the child has exited, including a nonzero or killed exit.
	if !p.cleaned && p.refreshCommit != nil {
		refreshErr = p.refreshCommit()
	}
	p.cleanupMu.Unlock()
	return errors.Join(processErr, refreshErr, p.cleanup())
}

// cleanup runs once the child has exited, or before it started: it releases
// the proxy and removes the work root.
func (p *codexOwnedProcess) cleanup() error {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	if !p.proxyReleased {
		if p.proxy != nil {
			p.proxyErr = p.proxy.Close()
		}
		p.proxyReleased = true
	}
	if err := p.removeRootLocked(); err != nil {
		return errors.Join(p.proxyErr, err)
	}
	return p.proxyErr
}

// abandon removes the work root, and with it the staged credential, when the
// child's exit could not be confirmed. The proxy stays bound: the pending
// Wait releases it when the child is reaped.
func (p *codexOwnedProcess) abandon() error {
	p.cleanupMu.Lock()
	defer p.cleanupMu.Unlock()
	return p.removeRootLocked()
}

func (p *codexOwnedProcess) removeRootLocked() error {
	if p.cleaned {
		return nil
	}
	remove := p.removeRoot
	if remove == nil {
		remove = os.RemoveAll
	}
	if err := remove(p.workRoot); err != nil {
		return err
	}
	p.cleaned = true
	return nil
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
		process, err = proxyStarter.StartWithProxy(ctx, attestation.VerifiedExecutable(), slices.Clone(codexAppServerArgs), nil, workRoot, proxySession.Endpoint())
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
