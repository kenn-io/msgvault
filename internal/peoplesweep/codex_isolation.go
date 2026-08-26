package peoplesweep

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxCodexVersionBytes  = 4096
	codexVersionTimeout   = 500 * time.Millisecond
	codexVersionWaitDelay = 100 * time.Millisecond
)

type verifiedCodexExecutable struct {
	root string
	path string
	once sync.Once
	err  error
}

func (e *verifiedCodexExecutable) Close() error {
	if e == nil {
		return nil
	}
	e.once.Do(func() {
		chmodErr := os.Chmod(e.root, 0o700) //nolint:gosec // Owner-only traversal is required for cleanup.
		removeErr := os.RemoveAll(e.root)
		if (chmodErr != nil && !errors.Is(chmodErr, os.ErrNotExist)) || removeErr != nil {
			e.err = errors.New("release verified codex executable")
		}
	})
	return e.err
}

// ReleasedCodexIsolationGate admits only executable digest/boundary pairs in
// the compiled release registry.
type ReleasedCodexIsolationGate struct{}

// CodexReleaseKey identifies one executable and one proven execution
// boundary. Configuration is not release evidence.
type CodexReleaseKey struct {
	ExecutableSHA256  string
	ExecutionBoundary string
}

// NewReleasedCodexIsolationGate returns the production fail-closed gate.
func NewReleasedCodexIsolationGate() ReleasedCodexIsolationGate {
	return ReleasedCodexIsolationGate{}
}

// Verify resolves and hashes the executable before consulting the compiled
// release registry. It runs --version only after the digest/boundary pair is
// registered.
func (ReleasedCodexIsolationGate) Verify(
	ctx context.Context,
	executable string,
	expectedBoundary string,
) (CodexAttestation, error) {
	return verifyReleasedCodexIsolation(ctx, executable, expectedBoundary, releasedCodexAttestations)
}

// ReverifyForLaunch hashes the same absolute source path and the owned
// verified snapshot immediately before process start. It performs no PATH
// lookup and admits no caller-owned identity fields.
func (ReleasedCodexIsolationGate) ReverifyForLaunch(attestation CodexAttestation) error {
	return reverifyReleasedCodexIsolation(attestation, releasedCodexAttestations)
}

func verifyReleasedCodexIsolation(
	ctx context.Context,
	executable string,
	expectedBoundary string,
	registry map[CodexReleaseKey]CodexAttestation,
) (CodexAttestation, error) {
	if err := ctx.Err(); err != nil {
		return CodexAttestation{}, err
	}
	attestation, err := prepareReleasedCodexIsolation(executable, expectedBoundary, registry)
	if err != nil {
		return CodexAttestation{}, err
	}
	version, err := codexExecutableVersion(ctx, attestation.VerifiedExecutable())
	if err != nil {
		_ = attestation.Close()
		return CodexAttestation{}, err
	}
	if version != attestation.Version {
		_ = attestation.Close()
		return CodexAttestation{}, ErrCodexIsolationUnreleased
	}
	return attestation, nil
}

func prepareReleasedCodexIsolation(
	executable string,
	expectedBoundary string,
	registry map[CodexReleaseKey]CodexAttestation,
) (CodexAttestation, error) {
	resolved, err := resolveCodexExecutable(executable)
	if err != nil {
		return CodexAttestation{}, err
	}
	digest, err := hashCodexExecutable(resolved)
	if err != nil {
		return CodexAttestation{}, err
	}
	key := CodexReleaseKey{
		ExecutableSHA256: digest, ExecutionBoundary: expectedBoundary,
	}
	released, ok := registry[key]
	if !ok || !validReleasedCodexAttestation(key, released) {
		return CodexAttestation{}, ErrCodexIsolationUnreleased
	}
	verified, snapshotDigest, err := snapshotCodexExecutable(resolved)
	if err != nil {
		return CodexAttestation{}, err
	}
	if snapshotDigest != digest {
		_ = verified.Close()
		return CodexAttestation{}, ErrCodexIsolationUnreleased
	}
	if err := validateCodexLaunchArtifact(verified.path, released.LaunchArtifact); err != nil {
		_ = verified.Close()
		return CodexAttestation{}, ErrCodexIsolationUnreleased
	}
	return CodexAttestation{
		ExecutablePath: resolved, Version: released.Version,
		ExecutableSHA256:   released.ExecutableSHA256,
		ExecutionBoundary:  released.ExecutionBoundary,
		LaunchArtifact:     released.LaunchArtifact,
		verifiedExecutable: verified,
	}, nil
}

func reverifyReleasedCodexIsolation(
	attestation CodexAttestation,
	registry map[CodexReleaseKey]CodexAttestation,
) error {
	if attestation.ExecutablePath == "" || !filepath.IsAbs(attestation.ExecutablePath) ||
		filepath.Clean(attestation.ExecutablePath) != attestation.ExecutablePath ||
		attestation.verifiedExecutable == nil {
		return ErrCodexIsolationUnreleased
	}
	key := CodexReleaseKey{
		ExecutableSHA256:  attestation.ExecutableSHA256,
		ExecutionBoundary: attestation.ExecutionBoundary,
	}
	released, ok := registry[key]
	if !ok || !validReleasedCodexAttestation(key, released) ||
		attestation.Version != released.Version ||
		attestation.ExecutableSHA256 != released.ExecutableSHA256 ||
		attestation.ExecutionBoundary != released.ExecutionBoundary ||
		attestation.LaunchArtifact != released.LaunchArtifact {
		return ErrCodexIsolationUnreleased
	}
	sourceDigest, sourceErr := hashCodexExecutable(attestation.ExecutablePath)
	verifiedDigest, verifiedErr := hashCodexExecutable(attestation.verifiedExecutable.path)
	if sourceErr != nil || verifiedErr != nil ||
		sourceDigest != released.ExecutableSHA256 || verifiedDigest != released.ExecutableSHA256 {
		return ErrCodexIsolationUnreleased
	}
	return nil
}

func snapshotCodexExecutable(sourcePath string) (_ *verifiedCodexExecutable, _ string, retErr error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return nil, "", errors.New("open codex executable")
	}
	sourceClosed := false
	defer func() {
		if !sourceClosed {
			_ = source.Close()
		}
	}()
	root, err := os.MkdirTemp("", "msgvault-codex-verified-")
	if err != nil {
		return nil, "", errors.New("create verified codex executable root")
	}
	verified := &verifiedCodexExecutable{root: root, path: filepath.Join(root, codexSnapshotFilename(sourcePath))}
	keep := false
	defer func() {
		if !keep {
			_ = verified.Close()
		}
	}()
	target, err := os.OpenFile(verified.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", errors.New("create verified codex executable")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	syncErr := target.Sync()
	chmodErr := target.Chmod(0o500)
	closeErr := target.Close()
	sourceCloseErr := source.Close()
	sourceClosed = true
	if copyErr != nil || syncErr != nil || chmodErr != nil || closeErr != nil || sourceCloseErr != nil {
		return nil, "", errors.New("copy verified codex executable")
	}
	if err := os.Chmod(root, 0o500); err != nil { //nolint:gosec // Owner-only traversal protects the executable snapshot.
		return nil, "", errors.New("protect verified codex executable")
	}
	keep = true
	return verified, hex.EncodeToString(hash.Sum(nil)), nil
}

func validReleasedCodexAttestation(key CodexReleaseKey, released CodexAttestation) bool {
	digest := strings.ToLower(strings.TrimSpace(released.ExecutableSHA256))
	decoded, err := hex.DecodeString(digest)
	return err == nil && len(decoded) == sha256.Size &&
		digest == released.ExecutableSHA256 && digest == key.ExecutableSHA256 &&
		released.ExecutablePath == "" && released.Version != "" &&
		safeCodexVersion(released.Version) &&
		released.ExecutionBoundary != "" && released.ExecutionBoundary == key.ExecutionBoundary &&
		released.LaunchArtifact == CodexLaunchArtifactNativeStandaloneV1
}

func codexSnapshotFilename(sourcePath string) string {
	if strings.EqualFold(filepath.Ext(sourcePath), ".exe") {
		return "codex.exe"
	}
	return "codex"
}

func resolveCodexExecutable(executable string) (string, error) {
	if strings.TrimSpace(executable) == "" {
		return "", errors.New("resolve codex executable")
	}
	resolved, err := exec.LookPath(executable)
	if err != nil {
		return "", errors.New("resolve codex executable")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("resolve codex executable")
	}
	resolved, err = filepath.EvalSymlinks(filepath.Clean(resolved))
	if err != nil {
		return "", errors.New("resolve codex executable")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("resolve codex executable")
	}
	return filepath.Clean(resolved), nil
}

func hashCodexExecutable(executable string) (string, error) {
	file, err := os.Open(executable)
	if err != nil {
		return "", errors.New("hash codex executable")
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.New("hash codex executable")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

type boundedCodexVersionWriter struct {
	bytes    []byte
	overflow bool
}

func (w *boundedCodexVersionWriter) Write(p []byte) (int, error) {
	remaining := maxCodexVersionBytes - len(w.bytes)
	if remaining < len(p) {
		w.overflow = true
		if remaining > 0 {
			w.bytes = append(w.bytes, p[:remaining]...)
		}
		return len(p), nil
	}
	w.bytes = append(w.bytes, p...)
	return len(p), nil
}

func codexExecutableVersion(ctx context.Context, executable CodexExecutable) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if executable.verifiedPath == "" || !filepath.IsAbs(executable.verifiedPath) {
		return "", errors.New("read codex executable version")
	}
	versionCtx, cancel := context.WithTimeout(ctx, codexVersionTimeout)
	defer cancel()
	var stdout boundedCodexVersionWriter
	command := exec.CommandContext(versionCtx, executable.verifiedPath, "--version") //nolint:gosec // The gate owns this absolute private snapshot.
	command.Env = scrubCodexEnvironment(os.Environ())
	command.Stdout = &stdout
	command.Stderr = io.Discard
	command.WaitDelay = codexVersionWaitDelay
	if err := runCodexVersionCommand(command); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		return "", errors.New("read codex executable version")
	}
	version := strings.TrimSpace(string(stdout.bytes))
	if stdout.overflow || !safeCodexVersion(version) {
		return "", errors.New("read codex executable version")
	}
	return version, nil
}

func safeCodexVersion(version string) bool {
	if version == "" || len(version) > maxCodexVersionBytes ||
		strings.TrimSpace(version) != version || !utf8.ValidString(version) {
		return false
	}
	for _, character := range version {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
