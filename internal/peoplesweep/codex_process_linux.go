//go:build linux

package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// bubblewrapCodexStarter confines the pinned, standalone Codex binary to a
// disposable work root and an empty network namespace. The bound launcher
// stages only an explicitly supplied auth.json in that root.
type bubblewrapCodexStarter struct {
	bridgePath   string
	bridgeDigest string
}

// codexBridgeSHA256 is linked into Linux daemon builds from the adjacent
// static bridge artifact. An unlinked development binary fails closed.
var codexBridgeSHA256 string

func codexAuthOwnedByDaemon(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	uid := os.Geteuid()
	return ok && uid >= 0 && uint64(stat.Uid) == uint64(uid)
}

func NewCodexCommandStarter() CommandStarter {
	return bubblewrapCodexStarter{bridgeDigest: codexBridgeSHA256}
}

// Tests supply the built helper and its digest through the same launcher path.
func newCodexCommandStarterWithBridge(path, digest string) CommandStarter {
	return bubblewrapCodexStarter{bridgePath: path, bridgeDigest: digest}
}

func (s bubblewrapCodexStarter) Start(
	ctx context.Context,
	executable CodexExecutable,
	args []string,
	env []string,
	dir string,
) (RPCProcess, error) {
	return s.start(ctx, executable, args, env, dir, "")
}

func (s bubblewrapCodexStarter) StartWithProxy(
	ctx context.Context, executable CodexExecutable, args, env []string, dir, socketPath string,
) (RPCProcess, error) {
	return s.start(ctx, executable, args, env, dir, socketPath)
}

func (s bubblewrapCodexStarter) start(
	ctx context.Context, executable CodexExecutable, args, env []string, dir, socketPath string,
) (RPCProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !slices.Equal(args, codexAppServerArgs) {
		return nil, errors.New("codex launcher only accepts the reviewed app-server command")
	}
	if len(env) != 0 {
		return nil, errors.New("codex launcher does not accept an inherited environment")
	}
	if executable.verifiedPath == "" || !filepath.IsAbs(executable.verifiedPath) ||
		!filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, errors.New("codex launcher requires absolute verified executable and work root")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, errors.New("codex launcher work root is unavailable")
	}
	var bridge *verifiedCodexExecutable
	if socketPath != "" {
		if len(s.bridgeDigest) != 64 {
			return nil, errors.New("codex proxy bridge digest is not linked into the daemon")
		}
		if socketPath != filepath.Join(dir, codexProxySocketName) {
			return nil, errors.New("codex proxy socket must be inside the work root")
		}
		info, err := os.Lstat(socketPath)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(info) {
			return nil, errors.New("codex proxy socket is unavailable")
		}
		path := s.bridgePath
		if path == "" {
			self, err := os.Executable()
			if err != nil {
				return nil, errors.New("locate codex proxy bridge")
			}
			path = filepath.Join(filepath.Dir(self), "msgvault-codex-bridge")
		}
		info, err = os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("codex proxy bridge is missing: %w", os.ErrNotExist)
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || !codexAuthOwnedByDaemon(info) {
			return nil, errors.New("codex proxy bridge must be daemon-owned and not group writable")
		}
		var actualDigest string
		bridge, actualDigest, err = snapshotCodexExecutable(path)
		if err != nil {
			return nil, errors.New("snapshot codex proxy bridge")
		}
		if actualDigest != s.bridgeDigest {
			_ = bridge.Close()
			return nil, errors.New("codex proxy bridge digest does not match daemon build")
		}
		if err := validateCodexLaunchArtifact(bridge.path, CodexLaunchArtifactNativeStandaloneV1); err != nil {
			_ = bridge.Close()
			return nil, errors.New("codex proxy bridge must be a static native executable")
		}
	}
	keepBridge := false
	defer func() {
		if bridge != nil && !keepBridge {
			_ = bridge.Close()
		}
	}()
	const bubblewrap = "/usr/bin/bwrap"
	if info, err := os.Stat(bubblewrap); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("codex launcher requires Bubblewrap")
	}
	const caBundle = "/etc/ssl/certs/ca-certificates.crt"
	caInfo, err := os.Stat(caBundle)
	if err != nil || !caInfo.Mode().IsRegular() || caInfo.Size() == 0 ||
		caInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("codex launcher requires a readable system CA bundle")
	}
	caStat, ok := caInfo.Sys().(*syscall.Stat_t)
	if !ok || caStat.Uid != 0 {
		return nil, errors.New("codex launcher requires a root-owned system CA bundle")
	}
	bwrapArgs := []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--die-with-parent", "--new-session",
		"--clearenv", "--tmpfs", "/tmp", "--dir", "/work", "--bind", dir, "/work",
		"--dir", "/work/.codex",
		"--dir", "/etc", "--dir", "/etc/ssl", "--dir", "/etc/ssl/certs",
		"--ro-bind", caBundle, caBundle,
		"--ro-bind", executable.verifiedPath, "/codex", "--proc", "/proc", "--dev", "/dev",
		"--setenv", "HOME", "/work", "--setenv", "CODEX_HOME", "/work/.codex",
		"--setenv", "SSL_CERT_FILE", caBundle,
		"--setenv", "TMPDIR", "/tmp", "--chdir", "/work",
	}
	entrypoint := "/codex"
	if bridge != nil {
		bwrapArgs = append(bwrapArgs, "--ro-bind", bridge.path, "/bridge",
			"--setenv", "HTTPS_PROXY", "http://127.0.0.1:3128",
			"--setenv", "https_proxy", "http://127.0.0.1:3128",
			"--setenv", "ALL_PROXY", "http://127.0.0.1:3128",
			"--setenv", "all_proxy", "http://127.0.0.1:3128")
		entrypoint = "/bridge"
	}
	bwrapArgs = append(bwrapArgs, "--", entrypoint)
	bwrapArgs = append(bwrapArgs, args...)
	process, err := execCommandStarter{}.Start(
		ctx, CodexExecutable{verifiedPath: bubblewrap}, bwrapArgs, []string{"PATH=/usr/bin:/bin"}, dir,
	)
	if err != nil {
		return nil, err
	}
	if bridge != nil {
		keepBridge = true
		return &codexBridgeOwnedProcess{RPCProcess: process, bridge: bridge}, nil
	}
	return process, nil
}

type codexBridgeOwnedProcess struct {
	RPCProcess

	bridge *verifiedCodexExecutable
}

func (p *codexBridgeOwnedProcess) Wait() error {
	return errors.Join(p.RPCProcess.Wait(), p.bridge.Close())
}

func (p *codexBridgeOwnedProcess) Kill() error {
	return errors.Join(p.RPCProcess.Kill(), p.bridge.Close())
}
