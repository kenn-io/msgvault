//go:build darwin

package peoplesweep

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	codexSandboxExec = "/usr/bin/sandbox-exec"
	codexCABundle    = "/private/etc/ssl/cert.pem"
)

// codexSeatbeltProfile denies everything the app server does not need. The
// launcher passes every path as a parameter, so no path is parsed as profile
// text. Each grant below is one Codex 0.167.0 fails without on macOS 27 (Apple
// silicon): it aborts in dyld, exits naming the refused path, or panics in the
// Rust runtime. Codex starts no process; the profile lets it exec only itself.
const codexSeatbeltProfile = `(version 1)
(deny default)

(allow process-exec (literal (param "CODEX")))
(allow file-read* file-map-executable (literal (param "CODEX")))

; dyld opens the root directory to find the shared cache, and aborts without
; it. This exposes the names of the top-level directories, not their contents.
(allow file-read-data (literal "/"))
; /etc is a symlink Codex resolves to look for /etc/codex/requirements.toml, and
; it canonicalizes CODEX_HOME, which stats each ancestor of the work root.
(allow file-read-metadata (literal "/etc") (path-ancestors (param "WORK")))
(allow file-read* file-map-executable
  (subpath "/usr/lib")
  (subpath "/System/Library/Frameworks")
  (subpath "/System/Library/PrivateFrameworks"))
(allow file-read* (literal "/dev/random") (literal "/dev/urandom") (literal (param "CA_BUNDLE")))
(allow file-read* file-write-data (literal "/dev/null"))
; The Rust runtime reads the page size at start.
(allow sysctl-read (sysctl-name "hw.pagesize_compat"))

; The disposable work root holds the staged auth.json and everything Codex
; writes. It is the only user path the child can read.
(allow file-read* file-write* (subpath (param "WORK")))

; Codex refuses to start until CFPreferences synchronizes its managed
; preferences. That needs cfprefsd and the two change counters it publishes
; in shared memory, read-only. No preference domain is readable: cfprefsd
; answers every read as empty.
(allow mach-lookup
  (global-name "com.apple.cfprefsd.daemon")
  (global-name "com.apple.cfprefsd.agent"))
(allow ipc-posix-shm-read-data
  (ipc-posix-name "apple.cfprefs.daemonv1")
  (ipc-posix-name (string-append "apple.cfprefs." (param "UID") "v1")))
`

// codexSeatbeltProxyRule is added only when a proxy session is attached. It
// admits TCP alone: the proxy holds that port's TCP listeners, and nothing
// stops another process from receiving UDP on the same port number.
const codexSeatbeltProxyRule = `(allow network-outbound (remote tcp (string-append "localhost:" (param "PROXY_PORT"))))
`

// seatbeltCodexStarter confines the pinned, standalone Codex binary with a
// deny-by-default Seatbelt profile. The bound launcher stages only an
// explicitly supplied auth.json in the disposable work root.
type seatbeltCodexStarter struct{}

func NewCodexCommandStarter() CommandStarter { return seatbeltCodexStarter{} }

func (s seatbeltCodexStarter) Start(
	ctx context.Context,
	executable CodexExecutable,
	args []string,
	env []string,
	dir string,
) (RPCProcess, error) {
	return s.start(ctx, executable, args, env, dir, "")
}

func (s seatbeltCodexStarter) StartWithProxy(
	ctx context.Context, executable CodexExecutable, args, env []string, dir, proxyEndpoint string,
) (RPCProcess, error) {
	if proxyEndpoint == "" {
		return nil, errors.New("codex proxy endpoint is required")
	}
	return s.start(ctx, executable, args, env, dir, proxyEndpoint)
}

func (seatbeltCodexStarter) start(
	ctx context.Context, executable CodexExecutable, args, env []string, dir, proxyEndpoint string,
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
	// Seatbelt matches resolved paths, and the default temporary directory
	// sits behind the /var symlink.
	workRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, errors.New("codex launcher work root is unavailable")
	}
	info, err := os.Lstat(workRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !codexAuthOwnedByDaemon(info) {
		return nil, errors.New("codex launcher work root must be a private daemon-owned directory")
	}
	codex, err := filepath.EvalSymlinks(executable.verifiedPath)
	if err != nil {
		return nil, errors.New("codex launcher verified executable is unavailable")
	}
	if info, err := os.Lstat(codex); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("codex launcher verified executable must be a regular file")
	}
	var proxyURL, proxyPort string
	if proxyEndpoint != "" {
		proxyURL, proxyPort, err = codexSeatbeltProxyEndpoint(proxyEndpoint)
		if err != nil {
			return nil, err
		}
	}
	sandboxArgs, childEnv, err := codexSeatbeltInvocation(workRoot, codex, proxyURL, proxyPort, args)
	if err != nil {
		return nil, err
	}
	return execCommandStarter{}.Start(
		ctx, CodexExecutable{verifiedPath: codexSandboxExec}, sandboxArgs, childEnv, workRoot,
	)
}

// codexVersionCommand runs the snapshot's --version under the same
// profile, without a proxy, in a disposable root.
func codexVersionCommand(ctx context.Context, verifiedPath, root string) (*exec.Cmd, error) {
	workRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errors.New("codex version root is unavailable")
	}
	codex, err := filepath.EvalSymlinks(verifiedPath)
	if err != nil {
		return nil, errors.New("codex verified executable is unavailable")
	}
	sandboxArgs, childEnv, err := codexSeatbeltInvocation(workRoot, codex, "", "", []string{"--version"})
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, codexSandboxExec, sandboxArgs...) //nolint:gosec // A fixed system binary; the launcher builds every argument.
	command.Env = childEnv
	command.Dir = workRoot
	return command, nil
}

// codexSeatbeltInvocation checks the system files the profile relies on,
// prepares the work root, and returns sandbox-exec's arguments and the
// child's whole environment. An empty proxyURL leaves the child no network.
func codexSeatbeltInvocation(workRoot, codex, proxyURL, proxyPort string, args []string) ([]string, []string, error) {
	if err := requireCodexSystemFile(codexSandboxExec, true); err != nil {
		return nil, nil, errors.New("codex launcher requires a root-owned sandbox-exec")
	}
	if err := requireCodexSystemFile(codexCABundle, false); err != nil {
		return nil, nil, errors.New("codex launcher requires a root-owned system CA bundle")
	}
	codexHome := filepath.Join(workRoot, ".codex")
	tmp := filepath.Join(workRoot, "tmp")
	for _, path := range []string{codexHome, tmp} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, nil, errors.New("create codex launcher work directories")
		}
	}
	profile := codexSeatbeltProfile
	sandboxArgs := []string{
		"-D", "WORK=" + workRoot, "-D", "CODEX=" + codex, "-D", "CA_BUNDLE=" + codexCABundle,
		"-D", "UID=" + strconv.Itoa(os.Geteuid()),
	}
	childEnv := []string{
		"HOME=" + workRoot, "CODEX_HOME=" + codexHome,
		// The app server owns OAuth; model turns must have no execution or
		// filesystem environment that could read its credential store.
		"CODEX_EXEC_SERVER_URL=none",
		"SSL_CERT_FILE=" + codexCABundle, "TMPDIR=" + tmp,
	}
	if proxyURL != "" {
		profile += codexSeatbeltProxyRule
		sandboxArgs = append(sandboxArgs, "-D", "PROXY_PORT="+proxyPort)
		childEnv = append(childEnv,
			"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL,
			"ALL_PROXY="+proxyURL, "all_proxy="+proxyURL)
	}
	sandboxArgs = append([]string{"-p", profile}, sandboxArgs...)
	sandboxArgs = append(sandboxArgs, codex)
	return append(sandboxArgs, args...), childEnv, nil
}

// codexSeatbeltProxyEndpoint accepts only the loopback proxy URL the darwin
// proxy issues: http, 127.0.0.1, a port, and the session credential.
func codexSeatbeltProxyEndpoint(endpoint string) (string, string, error) {
	invalid := errors.New("codex proxy endpoint must be the session's loopback proxy")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User == nil ||
		parsed.User.Username() != codexProxyUser || parsed.Hostname() != "127.0.0.1" ||
		parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return "", "", invalid
	}
	password, ok := parsed.User.Password()
	if !ok || len(password) != 64 || strings.Trim(password, "0123456789abcdef") != "" {
		return "", "", invalid
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 || port > 65535 || strconv.Itoa(port) != parsed.Port() {
		return "", "", invalid
	}
	return (&url.URL{
		Scheme: "http", User: url.UserPassword(codexProxyUser, password),
		Host: net.JoinHostPort("127.0.0.1", parsed.Port()),
	}).String(), parsed.Port(), nil
}

func requireCodexSystemFile(path string, executable bool) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Mode().Perm()&0o022 != 0 ||
		(executable && info.Mode()&0o111 == 0) {
		return errors.New("system file is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return errors.New("system file is not root-owned")
	}
	return nil
}
