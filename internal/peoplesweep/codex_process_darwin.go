//go:build darwin

package peoplesweep

import (
	"context"
	"errors"
	"net"
	"net/url"
	"os"
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
// text. The child can read and write only its disposable work root, read the
// system's code and CA bundle, and connect only to the session's proxy port.
// It cannot fork or execute anything but the verified snapshot itself.
const codexSeatbeltProfile = `(version 1)
(deny default)

(allow process-exec (literal (param "CODEX")))
(allow file-read* file-map-executable (literal (param "CODEX")))

; The loader finds the shared cache from the root directory, and system code
; lives under these paths. None of them holds user data.
(allow file-read* (literal "/"))
(allow file-read-metadata (literal "/etc") (path-ancestors (param "WORK")))
(allow file-read* file-map-executable
  (subpath "/usr/lib")
  (subpath "/System/Library/Frameworks")
  (subpath "/System/Library/PrivateFrameworks"))
(allow file-read* (literal "/dev/random") (literal "/dev/urandom") (literal (param "CA_BUNDLE")))
(allow file-read* file-write-data (literal "/dev/null"))
(allow sysctl-read
  (sysctl-name "hw.ephemeral_storage")
  (sysctl-name "hw.machine")
  (sysctl-name "hw.ncpu")
  (sysctl-name "hw.pagesize_compat")
  (sysctl-name-prefix "hw.optional.")
  (sysctl-name "kern.bootargs")
  (sysctl-name "kern.hostname")
  (sysctl-name "kern.iossupportversion")
  (sysctl-name "kern.osproductversion")
  (sysctl-name "kern.osrelease")
  (sysctl-name "kern.ostype")
  (sysctl-name "kern.osvariant_status")
  (sysctl-name "kern.version")
  (sysctl-name-prefix "machdep.cpu.")
  (sysctl-name "security.mac.lockdown_mode_state"))

; The disposable work root holds the staged auth.json and everything Codex
; writes. It is the only user path the child can read.
(allow file-read* file-write* (subpath (param "WORK")))

; Codex refuses to start until it refreshes its managed preferences. It may
; read its own preference domain and no other.
(allow ipc-posix-shm-read* (ipc-posix-name-prefix "apple.cfprefs."))
(allow mach-lookup
  (global-name "com.apple.cfprefsd.daemon")
  (global-name "com.apple.cfprefsd.agent"))
(allow user-preference-read (preference-domain "com.openai.codex"))
`

// codexSeatbeltProxyRule is added only when a proxy session is attached.
const codexSeatbeltProxyRule = `(allow network-outbound (remote ip (string-append "localhost:" (param "PROXY_PORT"))))
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
	if err := requireCodexSystemFile(codexSandboxExec, true); err != nil {
		return nil, errors.New("codex launcher requires a root-owned sandbox-exec")
	}
	if err := requireCodexSystemFile(codexCABundle, false); err != nil {
		return nil, errors.New("codex launcher requires a root-owned system CA bundle")
	}
	codexHome := filepath.Join(workRoot, ".codex")
	tmp := filepath.Join(workRoot, "tmp")
	for _, path := range []string{codexHome, tmp} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, errors.New("create codex launcher work directories")
		}
	}
	profile := codexSeatbeltProfile
	sandboxArgs := []string{
		"-D", "WORK=" + workRoot, "-D", "CODEX=" + codex, "-D", "CA_BUNDLE=" + codexCABundle,
	}
	childEnv := []string{
		"HOME=" + workRoot, "CODEX_HOME=" + codexHome,
		// The app server owns OAuth; model turns must have no execution or
		// filesystem environment that could read its credential store.
		"CODEX_EXEC_SERVER_URL=none",
		"SSL_CERT_FILE=" + codexCABundle, "TMPDIR=" + tmp,
	}
	if proxyEndpoint != "" {
		profile += codexSeatbeltProxyRule
		sandboxArgs = append(sandboxArgs, "-D", "PROXY_PORT="+proxyPort)
		childEnv = append(childEnv,
			"HTTPS_PROXY="+proxyURL, "https_proxy="+proxyURL,
			"ALL_PROXY="+proxyURL, "all_proxy="+proxyURL)
	}
	sandboxArgs = append([]string{"-p", profile}, sandboxArgs...)
	sandboxArgs = append(sandboxArgs, codex)
	sandboxArgs = append(sandboxArgs, args...)
	return execCommandStarter{}.Start(
		ctx, CodexExecutable{verifiedPath: codexSandboxExec}, sandboxArgs, childEnv, workRoot,
	)
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
