//go:build linux

package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
			return nil, errors.New("codex proxy bridge must be a regular file and not group writable")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 && !codexAuthOwnedByDaemon(info) {
			return nil, errors.New("codex proxy bridge must be owned by root or the daemon")
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
	bwrapArgs, err := codexBubblewrapArgs(dir, executable.verifiedPath)
	if err != nil {
		return nil, err
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
		ctx, CodexExecutable{verifiedPath: codexBubblewrap}, bwrapArgs, []string{"PATH=/usr/bin:/bin"}, dir,
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

const (
	codexBubblewrap = "/usr/bin/bwrap"
	codexCABundle   = "/etc/ssl/certs/ca-certificates.crt"
)

// codexBubblewrapArgs confines the executable to empty user, PID and network
// namespaces whose only host files are dir (as /work), the system CA bundle
// and the executable (as /codex). The caller appends "--" and the command.
func codexBubblewrapArgs(dir, executable string) ([]string, error) {
	if info, err := os.Stat(codexBubblewrap); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("codex launcher requires Bubblewrap")
	}
	caInfo, err := os.Stat(codexCABundle)
	if err != nil || !caInfo.Mode().IsRegular() || caInfo.Size() == 0 ||
		caInfo.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("codex launcher requires a readable system CA bundle")
	}
	caStat, ok := caInfo.Sys().(*syscall.Stat_t)
	if !ok || caStat.Uid != 0 {
		return nil, errors.New("codex launcher requires a root-owned system CA bundle")
	}
	return []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--die-with-parent", "--new-session",
		"--clearenv", "--tmpfs", "/tmp", "--dir", "/work", "--bind", dir, "/work",
		"--dir", "/work/.codex",
		"--dir", "/etc", "--dir", "/etc/ssl", "--dir", "/etc/ssl/certs",
		"--ro-bind", codexCABundle, codexCABundle,
		"--ro-bind", executable, "/codex", "--proc", "/proc", "--dev", "/dev",
		"--setenv", "HOME", "/work", "--setenv", "CODEX_HOME", "/work/.codex",
		// The app server owns OAuth; model turns must have no execution or
		// filesystem environment that could read its credential store.
		"--setenv", "CODEX_EXEC_SERVER_URL", "none",
		"--setenv", "SSL_CERT_FILE", codexCABundle,
		"--setenv", "TMPDIR", "/tmp", "--chdir", "/work",
	}, nil
}

// codexVersionCommand runs the snapshot's --version in the same
// namespaces as the app server, in a disposable root.
func codexVersionCommand(ctx context.Context, verifiedPath, root string) (*exec.Cmd, error) {
	args, err := codexBubblewrapArgs(root, verifiedPath)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, codexBubblewrap, append(args, "--", "/codex", "--version")...) //nolint:gosec // A fixed system binary; the launcher builds every argument.
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.Dir = root
	return command, nil
}
