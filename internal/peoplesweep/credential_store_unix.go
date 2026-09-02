//go:build linux || darwin

package peoplesweep

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/msgvault/internal/fileutil"
	"golang.org/x/sys/unix"
)

// The pinned namespace lock serializes reuse of this single candidate slot.
const unixCredentialStagingName = ".credential-candidate" //nolint:gosec // A fixed private staging filename is not a credential.

type unixCredentialStoreRoot struct {
	fd                int
	tokensFD          int
	hooks             *credentialStoreHooks
	publishedName     string
	publishedIdentity unix.Stat_t
}

func (s *FileCredentialStore) withCredentialRoot(
	operation string,
	callback func(credentialStoreRoot) error,
) (retErr error) {
	if s == nil || filepath.Clean(s.tokensDir) == "." || s.tokensDir == "" {
		return errors.New("people provider credential tokens directory is required")
	}
	if err := fileutil.SecureMkdirAll(s.tokensDir, 0o700); err != nil {
		return fmt.Errorf("create people provider tokens directory: %w", err)
	}
	tokensFD, err := unix.Open(s.tokensDir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("pin people provider tokens directory without following symlinks: %w", err)
	}
	defer func() {
		if err := unix.Close(tokensFD); err != nil && retErr == nil {
			retErr = fmt.Errorf("close people provider tokens directory: %w", err)
		}
	}()
	if err := validateUnixDirectoryFD(tokensFD, "tokens"); err != nil {
		return err
	}
	if err := unix.Fchmod(tokensFD, 0o700); err != nil {
		return fmt.Errorf("protect pinned people provider tokens directory: %w", err)
	}

	rootFD, created, err := openUnixCredentialNamespace(tokensFD)
	if err != nil {
		return err
	}
	defer func() {
		if err := unix.Close(rootFD); err != nil && retErr == nil {
			retErr = fmt.Errorf("close pinned people provider credential directory: %w", err)
		}
	}()
	if created {
		if err := unix.Fsync(tokensFD); err != nil {
			return fmt.Errorf("sync pinned people provider tokens directory: %w", err)
		}
		if s.hooks != nil && s.hooks.afterNamespaceParentSync != nil {
			s.hooks.afterNamespaceParentSync()
		}
	}
	if err := unix.Flock(rootFD, unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock pinned people provider credential directory: %w", err)
	}
	defer func() {
		if err := unix.Flock(rootFD, unix.LOCK_UN); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock pinned people provider credential directory: %w", err)
		}
	}()
	if err := ensureUnixCredentialLockMarker(rootFD); err != nil {
		return err
	}
	if s.hooks != nil && s.hooks.afterLockAcquired != nil {
		s.hooks.afterLockAcquired()
	}
	if s.hooks != nil && s.hooks.beforeOperation != nil {
		s.hooks.beforeOperation(operation)
	}
	return callback(&unixCredentialStoreRoot{fd: rootFD, tokensFD: tokensFD, hooks: s.hooks})
}

func openUnixCredentialNamespace(tokensFD int) (int, bool, error) {
	rootFD, err := unix.Openat(tokensFD, credentialNamespace,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	created := false
	if err == unix.ENOENT {
		if err := unix.Mkdirat(tokensFD, credentialNamespace, 0o700); err != nil && err != unix.EEXIST {
			return -1, false, fmt.Errorf("create people provider credential directory relative to pinned parent: %w", err)
		}
		created = true
		rootFD, err = unix.Openat(tokensFD, credentialNamespace,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		return -1, false, fmt.Errorf("pin people provider credential directory without following symlinks: %w", err)
	}
	if err := validateUnixDirectoryFD(rootFD, "credential"); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, err
	}
	if err := unix.Fchmod(rootFD, 0o700); err != nil {
		_ = unix.Close(rootFD)
		return -1, false, fmt.Errorf("protect pinned people provider credential directory: %w", err)
	}
	return rootFD, created, nil
}

func validateUnixDirectoryFD(fd int, kind string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("identify pinned people provider %s directory: %w", kind, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("people provider %s path is not a direct directory", kind)
	}
	return nil
}

func ensureUnixCredentialLockMarker(rootFD int) (retErr error) {
	fd, err := unix.Openat(rootFD, ".credentials.lock",
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return fmt.Errorf("open people provider credential lock relative to pinned directory: %w", err)
	}
	defer func() {
		if err := unix.Close(fd); err != nil && retErr == nil {
			retErr = fmt.Errorf("close people provider credential lock: %w", err)
		}
	}()
	if err := validateUnixCredentialFD(fd, "lock"); err != nil {
		return err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return fmt.Errorf("protect opened people provider credential lock: %w", err)
	}
	return nil
}

type unixExistingCredentialDelete struct {
	tokensFD           int
	rootFD             int
	lockFD             int
	credentialFD       int
	tokensIdentity     unix.Stat_t
	rootIdentity       unix.Stat_t
	lockIdentity       unix.Stat_t
	credentialIdentity unix.Stat_t
	credentialName     string
	profileName        string
	locked             bool
}

type unixCredentialDeleteGuard struct {
	mu     sync.Mutex
	owner  *FileCredentialStore
	target *unixExistingCredentialDelete
	state  credentialDeleteGuardState
}

type unixCredentialCleanupGuard struct {
	deletion *unixCredentialDeleteGuard
}

type credentialDeleteGuardState uint8

const (
	credentialDeleteGuardReady credentialDeleteGuardState = iota
	credentialDeleteGuardConsumed
	credentialDeleteGuardClosed
)

func (*unixCredentialDeleteGuard) credentialDeleteGuard() {}

func (*unixCredentialCleanupGuard) credentialCleanupGuard() {}

func (*unixCredentialDeleteGuard) String() string {
	return "people provider credential deletion guard (opaque)"
}

func (g *unixCredentialDeleteGuard) GoString() string {
	return g.String()
}

func (g *unixCredentialDeleteGuard) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, g.String())
}

func (*unixCredentialDeleteGuard) MarshalJSON() ([]byte, error) {
	return nil, errors.New("people provider credential deletion guard cannot serialize")
}

func (*unixCredentialDeleteGuard) MarshalText() ([]byte, error) {
	return nil, errors.New("people provider credential deletion guard cannot serialize")
}

func (g *unixCredentialDeleteGuard) Close() error {
	if g == nil {
		return errors.New("people provider credential deletion guard is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == credentialDeleteGuardClosed {
		return nil
	}
	g.state = credentialDeleteGuardClosed
	if g.target == nil {
		return nil
	}
	return g.target.close()
}

func (*unixCredentialCleanupGuard) String() string {
	return "people provider credential cleanup guard (opaque)"
}

func (g *unixCredentialCleanupGuard) GoString() string { return g.String() }

func (g *unixCredentialCleanupGuard) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, g.String())
}

func (*unixCredentialCleanupGuard) MarshalJSON() ([]byte, error) {
	return nil, errors.New("people provider credential cleanup guard cannot serialize")
}

func (*unixCredentialCleanupGuard) MarshalText() ([]byte, error) {
	return nil, errors.New("people provider credential cleanup guard cannot serialize")
}

func (g *unixCredentialCleanupGuard) Close() error {
	if g == nil || g.deletion == nil {
		return errors.New("people provider credential cleanup guard is invalid")
	}
	return g.deletion.Close()
}

func (s *FileCredentialStore) preflightExistingCredentialDelete(
	profileName string,
) (CredentialDeleteGuard, error) {
	target, err := s.openExistingCredentialDelete(profileName)
	if err != nil {
		return nil, err
	}
	return &unixCredentialDeleteGuard{owner: s, target: target}, nil
}

func (s *FileCredentialStore) deleteExistingCredential(
	profileName string,
	opaque CredentialDeleteGuard,
) error {
	guard, ok := opaque.(*unixCredentialDeleteGuard)
	if !ok || guard == nil {
		return errors.New("people provider credential deletion guard is invalid")
	}
	guard.mu.Lock()
	defer guard.mu.Unlock()
	switch guard.state {
	case credentialDeleteGuardClosed:
		return errors.New("people provider credential deletion guard is closed")
	case credentialDeleteGuardConsumed:
		return errors.New("people provider credential deletion guard was already consumed")
	case credentialDeleteGuardReady:
		guard.state = credentialDeleteGuardConsumed
	default:
		guard.state = credentialDeleteGuardConsumed
		return errors.New("people provider credential deletion guard has invalid state")
	}
	if guard.owner != s {
		return errors.New("people provider credential deletion guard belongs to a different store")
	}
	if guard.target == nil || guard.target.profileName != profileName {
		return errors.New("people provider credential deletion guard belongs to a different profile")
	}
	if !guard.target.locked {
		if err := unix.Flock(guard.target.rootFD, unix.LOCK_EX); err != nil {
			return fmt.Errorf("lock pinned people provider credential directory for guarded deletion: %w", err)
		}
		guard.target.locked = true
	}
	if s.hooks != nil && s.hooks.beforeCredentialRetire != nil {
		s.hooks.beforeCredentialRetire()
	}
	if err := guard.target.validateLive(s.tokensDir, "guarded deletion", true); err != nil {
		return err
	}
	wipeErr := wipeUnixCredentialFD(guard.target.credentialFD)
	if err := guard.target.validateLive(s.tokensDir, "guarded deletion", false); err != nil {
		return err
	}
	return wipeErr
}

func (s *FileCredentialStore) cleanupNewCredential(
	profileName string,
	opaque CredentialCleanupGuard,
) error {
	guard, ok := opaque.(*unixCredentialCleanupGuard)
	if !ok || guard == nil || guard.deletion == nil {
		return errors.New("people provider credential cleanup guard is invalid")
	}
	return s.deleteExistingCredential(profileName, guard.deletion)
}

func (s *FileCredentialStore) openExistingCredentialDelete(
	profileName string,
) (_ *unixExistingCredentialDelete, retErr error) {
	if s == nil || filepath.Clean(s.tokensDir) == "." || s.tokensDir == "" {
		return nil, errors.New("people provider credential tokens directory is required")
	}
	target := &unixExistingCredentialDelete{
		tokensFD: -1, rootFD: -1, lockFD: -1, credentialFD: -1,
		profileName: profileName,
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, target.close())
		}
	}()
	target.tokensFD, retErr = unix.Open(s.tokensDir,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if retErr != nil {
		return nil, fmt.Errorf("open existing people provider tokens directory without following symlinks: %w", retErr)
	}
	target.tokensIdentity, retErr = validateUnixPrivateDirectoryFD(target.tokensFD, "tokens")
	if retErr != nil {
		return nil, retErr
	}
	if !unixPrivateDirectoryPathMatches(s.tokensDir, target.tokensIdentity) {
		return nil, errors.New("people provider tokens directory changed during deletion preflight")
	}

	target.rootFD, retErr = unix.Openat(target.tokensFD, credentialNamespace,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if retErr != nil {
		return nil, fmt.Errorf("open existing people provider credential directory without following symlinks: %w", retErr)
	}
	target.rootIdentity, retErr = validateUnixPrivateDirectoryFD(target.rootFD, "credential")
	if retErr != nil {
		return nil, retErr
	}
	if !unixPrivateDirectoryEntryMatches(target.tokensFD, credentialNamespace, target.rootIdentity) {
		return nil, errors.New("people provider credential directory changed during deletion preflight")
	}

	// Hold the pinned namespace lock until the guard is explicitly closed.
	// This serializes ordinary store operations while external pathname swaps
	// remain harmless because guarded deletion revalidates every identity.
	if retErr = unix.Flock(target.rootFD, unix.LOCK_EX); retErr != nil {
		return nil, fmt.Errorf("lock existing people provider credential directory: %w", retErr)
	}
	target.locked = true
	target.lockFD, target.lockIdentity, retErr = openExistingUnixCredentialDeleteFile(
		target.rootFD, ".credentials.lock", "lock",
	)
	if retErr != nil {
		return nil, retErr
	}
	if err := target.validateInfrastructure(s.tokensDir, "deletion preflight"); err != nil {
		return nil, err
	}
	if s.hooks != nil && s.hooks.afterLockAcquired != nil {
		s.hooks.afterLockAcquired()
	}
	if s.hooks != nil && s.hooks.beforeOperation != nil {
		s.hooks.beforeOperation("preflight-delete")
	}
	if err := target.validateInfrastructure(s.tokensDir, "deletion preflight"); err != nil {
		return nil, err
	}

	target.credentialName = profileName + ".json"
	target.credentialFD, target.credentialIdentity, retErr = openExistingUnixCredentialDeleteFile(
		target.rootFD, target.credentialName, "file",
	)
	if errors.Is(retErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	if retErr != nil {
		return nil, retErr
	}
	if s.hooks != nil && s.hooks.afterCredentialOpen != nil {
		s.hooks.afterCredentialOpen("preflight-delete")
	}
	if err := target.validateLive(s.tokensDir, "deletion preflight", true); err != nil {
		return nil, err
	}
	return target, nil
}

func (r *unixCredentialStoreRoot) pinCleanup(
	owner *FileCredentialStore,
	profileName string,
) (_ CredentialCleanupGuard, retErr error) {
	if r.publishedName != profileName || r.publishedIdentity.Dev == 0 || r.publishedIdentity.Ino == 0 {
		return nil, errors.New("new people provider credential publication identity is unavailable")
	}
	if r.hooks != nil && r.hooks.failedCleanupPin != nil {
		if err := r.hooks.failedCleanupPin(); err != nil {
			return nil, err
		}
	}
	target := &unixExistingCredentialDelete{
		tokensFD: -1, rootFD: -1, lockFD: -1, credentialFD: -1,
		profileName: profileName,
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, target.close())
		}
	}()

	// Open new file descriptions through the exact directories used by SaveNew.
	// Unlike dup, these do not retain the namespace flock after SaveNew returns.
	target.tokensFD, retErr = unix.Openat(r.tokensFD, ".",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if retErr != nil {
		return nil, fmt.Errorf("pin new people provider tokens directory for cleanup: %w", retErr)
	}
	target.tokensIdentity, retErr = validateUnixPrivateDirectoryFD(target.tokensFD, "tokens")
	if retErr != nil {
		return nil, retErr
	}
	target.rootFD, retErr = unix.Openat(r.fd, ".",
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if retErr != nil {
		return nil, fmt.Errorf("pin new people provider credential directory for cleanup: %w", retErr)
	}
	target.rootIdentity, retErr = validateUnixPrivateDirectoryFD(target.rootFD, "credential")
	if retErr != nil {
		return nil, retErr
	}
	target.lockFD, target.lockIdentity, retErr = openExistingUnixCredentialDeleteFile(
		target.rootFD, ".credentials.lock", "lock",
	)
	if retErr != nil {
		return nil, retErr
	}
	target.credentialName = profileName + ".json"
	target.credentialFD, target.credentialIdentity, retErr = openExistingUnixCredentialDeleteFile(
		target.rootFD, target.credentialName, "file",
	)
	if retErr != nil {
		return nil, retErr
	}
	if target.credentialIdentity.Dev != r.publishedIdentity.Dev ||
		target.credentialIdentity.Ino != r.publishedIdentity.Ino {
		return nil, errors.New("new people provider credential changed before cleanup identity was pinned")
	}
	if err := target.validateLive(owner.tokensDir, "new credential cleanup preflight", true); err != nil {
		return nil, err
	}
	return &unixCredentialCleanupGuard{deletion: &unixCredentialDeleteGuard{
		owner: owner, target: target,
	}}, nil
}

// retirePublished durably retires only the exact credential identity this
// root published, while the pinned namespace lock and directory FD are still
// held. SaveNew calls it when pinCleanup fails, so a publication that cannot
// carry a cleanup guard never strands a live credential. Like guarded
// deletion it leaves a durable empty record instead of unlinking a pathname.
func (r *unixCredentialStoreRoot) retirePublished(profileName string) (retErr error) {
	if r.publishedName != profileName || r.publishedIdentity.Dev == 0 || r.publishedIdentity.Ino == 0 {
		return errors.New("people provider credential publication identity is unavailable")
	}
	targetName := profileName + ".json"
	fd, identity, err := openExistingUnixCredentialDeleteFile(r.fd, targetName, "file")
	if err != nil {
		return err
	}
	defer func() {
		if err := unix.Close(fd); err != nil && retErr == nil {
			retErr = fmt.Errorf("close retired people provider credential: %w", err)
		}
	}()
	if identity.Dev != r.publishedIdentity.Dev || identity.Ino != r.publishedIdentity.Ino {
		return errors.New("people provider credential changed before publication rollback")
	}
	if err := wipeUnixCredentialFD(fd); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return fmt.Errorf("identify retired people provider credential during publication rollback: %w", err)
	}
	if identity.Dev != current.Dev || identity.Ino != current.Ino ||
		!unixCredentialEntryMatches(r.fd, targetName, identity) {
		return errors.New("people provider credential changed during publication rollback")
	}
	if err := unix.Fsync(r.fd); err != nil {
		return fmt.Errorf("sync pinned people provider credential directory after publication rollback: %w", err)
	}
	return nil
}

func (target *unixExistingCredentialDelete) close() error {
	if target == nil {
		return nil
	}
	var closeErrors []error
	if target.credentialFD >= 0 {
		fd := target.credentialFD
		target.credentialFD = -1
		if err := unix.Close(fd); err != nil {
			closeErrors = append(closeErrors, errors.New("close pinned people provider credential failed"))
		}
	}
	if target.lockFD >= 0 {
		fd := target.lockFD
		target.lockFD = -1
		if err := unix.Close(fd); err != nil {
			closeErrors = append(closeErrors, errors.New("close pinned people provider credential lock failed"))
		}
	}
	if target.rootFD >= 0 {
		fd := target.rootFD
		target.rootFD = -1
		if target.locked {
			target.locked = false
			if err := unix.Flock(fd, unix.LOCK_UN); err != nil {
				closeErrors = append(closeErrors, errors.New("unlock pinned people provider credential directory failed"))
			}
		}
		if err := unix.Close(fd); err != nil {
			closeErrors = append(closeErrors, errors.New("close pinned people provider credential directory failed"))
		}
	}
	if target.tokensFD >= 0 {
		fd := target.tokensFD
		target.tokensFD = -1
		if err := unix.Close(fd); err != nil {
			closeErrors = append(closeErrors, errors.New("close pinned people provider tokens directory failed"))
		}
	}
	return errors.Join(closeErrors...)
}

func (target *unixExistingCredentialDelete) validateInfrastructure(tokensDir, phase string) error {
	if !unixPrivateDirectoryPathMatches(tokensDir, target.tokensIdentity) {
		return fmt.Errorf("people provider tokens directory changed during %s", phase)
	}
	if !unixPrivateDirectoryEntryMatches(target.tokensFD, credentialNamespace, target.rootIdentity) {
		return fmt.Errorf("people provider credential directory changed during %s", phase)
	}
	if !unixCredentialEntryMatches(target.rootFD, ".credentials.lock", target.lockIdentity) {
		return fmt.Errorf("people provider credential lock changed during %s", phase)
	}
	return nil
}

func (target *unixExistingCredentialDelete) validateLive(
	tokensDir, phase string,
	requireNonEmpty bool,
) error {
	if err := target.validateInfrastructure(tokensDir, phase); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Fstat(target.credentialFD, &current); err != nil {
		return fmt.Errorf("identify existing people provider credential during %s: %w", phase, err)
	}
	if target.credentialIdentity.Dev != current.Dev ||
		target.credentialIdentity.Ino != current.Ino ||
		validateUnixCredentialStat(current, "file") != nil ||
		!unixCredentialEntryMatches(target.rootFD, target.credentialName, target.credentialIdentity) {
		return fmt.Errorf("people provider credential changed during %s", phase)
	}
	if requireNonEmpty && current.Size == 0 {
		return fmt.Errorf("%w for profile %q", ErrCredentialNotFound, target.profileName)
	}
	return nil
}

func openExistingUnixCredentialDeleteFile(
	rootFD int,
	name, kind string,
) (int, unix.Stat_t, error) {
	var entry unix.Stat_t
	if err := unix.Fstatat(rootFD, name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf(
			"inspect existing people provider credential %s without following symlinks: %w", kind, err,
		)
	}
	if err := validateUnixCredentialStat(entry, kind); err != nil {
		return -1, unix.Stat_t{}, err
	}
	fd, err := unix.Openat(rootFD, name,
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ELOOP || err == unix.EISDIR {
		return -1, unix.Stat_t{}, fmt.Errorf("people provider credential %s is not a regular file", kind)
	}
	if err != nil {
		return -1, unix.Stat_t{}, fmt.Errorf(
			"open existing people provider credential %s without following symlinks: %w", kind, err,
		)
	}
	var identity unix.Stat_t
	if err := unix.Fstat(fd, &identity); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("identify existing people provider credential %s: %w", kind, err)
	}
	if err := validateUnixCredentialStat(identity, kind); err != nil {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, err
	}
	if entry.Dev != identity.Dev || entry.Ino != identity.Ino {
		_ = unix.Close(fd)
		return -1, unix.Stat_t{}, fmt.Errorf("people provider credential %s changed while opening for deletion", kind)
	}
	return fd, identity, nil
}

func validateUnixPrivateDirectoryFD(fd int, kind string) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return unix.Stat_t{}, fmt.Errorf("identify existing people provider %s directory: %w", kind, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return unix.Stat_t{}, fmt.Errorf("people provider %s path is not a direct directory", kind)
	}
	if stat.Mode&0o777 != 0o700 {
		return unix.Stat_t{}, fmt.Errorf("people provider %s directory permissions are not private", kind)
	}
	if stat.Uid != unixEffectiveUID() {
		return unix.Stat_t{}, fmt.Errorf("people provider %s directory owner does not match the current user", kind)
	}
	return stat, nil
}

func unixPrivateDirectoryEntryMatches(parentFD int, name string, opened unix.Stat_t) bool {
	var current unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return unixPrivateDirectoryStatMatches(opened, current)
}

func unixPrivateDirectoryPathMatches(path string, opened unix.Stat_t) bool {
	var current unix.Stat_t
	if err := unix.Lstat(path, &current); err != nil {
		return false
	}
	return unixPrivateDirectoryStatMatches(opened, current)
}

func unixPrivateDirectoryStatMatches(opened, current unix.Stat_t) bool {
	return opened.Dev == current.Dev && opened.Ino == current.Ino &&
		current.Mode&unix.S_IFMT == unix.S_IFDIR &&
		current.Mode&0o777 == 0o700 && current.Uid == unixEffectiveUID()
}

func unixEffectiveUID() uint32 {
	return uint32(os.Geteuid()) //nolint:gosec // Unix effective UIDs are non-negative uint32 values.
}

func (r *unixCredentialStoreRoot) save(profileName string, data []byte) (retErr error) {
	targetName := profileName + ".json"
	if err := inspectUnixCredentialEntry(r.fd, targetName, true); err != nil {
		return err
	}
	candidateName, candidate, err := createUnixCredentialCandidate(r.fd)
	if err != nil {
		return err
	}
	var candidateIdentity unix.Stat_t
	if err := unix.Fstat(int(candidate.Fd()), &candidateIdentity); err != nil {
		identityErr := fmt.Errorf("identify opened people provider credential candidate: %w", err)
		if closeErr := candidate.Close(); closeErr != nil {
			return errors.Join(identityErr, fmt.Errorf("close people provider credential candidate: %w", closeErr))
		}
		return identityErr
	}
	committed := false
	defer func() {
		if !committed {
			retErr = errors.Join(retErr, wipeFailedUnixCredentialCandidate(candidate, r.hooks))
			if err := candidate.Close(); err != nil {
				retErr = errors.Join(retErr, errors.New("close wiped people provider credential candidate failed"))
			}
			return
		}
		if err := candidate.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close published people provider credential: %w", err))
		}
	}()
	if r.hooks != nil && r.hooks.afterCandidateOpen != nil {
		r.hooks.afterCandidateOpen(candidateName)
	}
	if err := validateUnixCredentialFD(int(candidate.Fd()), "candidate"); err != nil {
		return err
	}
	if _, err := candidate.Write(data); err != nil {
		return fmt.Errorf("write pinned people provider credential candidate: %w", err)
	}
	if err := candidate.Sync(); err != nil {
		return fmt.Errorf("sync pinned people provider credential candidate: %w", err)
	}
	if err := inspectUnixCredentialEntry(r.fd, targetName, true); err != nil {
		return err
	}
	var currentCandidate unix.Stat_t
	if err := unix.Fstatat(r.fd, candidateName, &currentCandidate, unix.AT_SYMLINK_NOFOLLOW); err != nil ||
		candidateIdentity.Dev != currentCandidate.Dev || candidateIdentity.Ino != currentCandidate.Ino ||
		validateUnixCredentialStat(currentCandidate, "candidate") != nil {
		return errors.New("people provider credential candidate changed before publication")
	}
	if r.hooks != nil && r.hooks.beforeCandidatePublish != nil {
		r.hooks.beforeCandidatePublish(candidateName)
	}
	if err := unix.Renameat(r.fd, candidateName, r.fd, targetName); err != nil {
		return fmt.Errorf("publish people provider credential relative to pinned directory: %w", err)
	}
	publishedFD, err := unix.Openat(r.fd, targetName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errors.New("people provider credential candidate changed during publication")
	}
	defer func() {
		if err := unix.Close(publishedFD); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close published people provider credential: %w", err))
		}
	}()
	var publishedIdentity unix.Stat_t
	if err := unix.Fstat(publishedFD, &publishedIdentity); err != nil ||
		candidateIdentity.Dev != publishedIdentity.Dev || candidateIdentity.Ino != publishedIdentity.Ino ||
		validateUnixCredentialStat(publishedIdentity, "candidate") != nil {
		return errors.New("people provider credential candidate changed during publication")
	}
	if err := unix.Fsync(r.fd); err != nil {
		return fmt.Errorf("sync pinned people provider credential directory: %w", err)
	}
	r.publishedName = profileName
	r.publishedIdentity = publishedIdentity
	committed = true
	return nil
}

func createUnixCredentialCandidate(rootFD int) (string, *os.File, error) {
	for range 2 {
		created := false
		fd, err := unix.Openat(rootFD, unixCredentialStagingName,
			unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err == unix.ENOENT {
			fd, err = unix.Openat(rootFD, unixCredentialStagingName,
				unix.O_RDWR|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
			if err == unix.EEXIST {
				continue
			}
			created = err == nil
		}
		if err != nil {
			return "", nil, fmt.Errorf("open people provider credential candidate relative to pinned directory: %w", err)
		}
		if err := validateUnixCredentialFD(fd, "candidate"); err != nil {
			_ = unix.Close(fd)
			return "", nil, err
		}
		if !created {
			if err := unix.Ftruncate(fd, 0); err != nil {
				_ = unix.Close(fd)
				return "", nil, errors.New("reset pinned people provider credential candidate")
			}
			if err := unix.Fsync(fd); err != nil {
				_ = unix.Close(fd)
				return "", nil, errors.New("sync reset people provider credential candidate")
			}
		}
		return unixCredentialStagingName, os.NewFile(uintptr(fd), unixCredentialStagingName), nil
	}
	return "", nil, errors.New("could not open the people provider credential candidate")
}

func (r *unixCredentialStoreRoot) load(profileName string) (data []byte, retErr error) {
	name := profileName + ".json"
	fd, err := unix.Openat(r.fd, name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	if err != nil {
		return nil, fmt.Errorf("open people provider credential for profile %q without following symlinks: %w", profileName, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close pinned people provider credential for profile %q: %w", profileName, err))
		}
	}()
	if err := validateUnixCredentialFD(fd, "file"); err != nil {
		return nil, err
	}
	if r.hooks != nil && r.hooks.afterCredentialOpen != nil {
		r.hooks.afterCredentialOpen("load")
	}
	data, err = io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read pinned people provider credential for profile %q: %w", profileName, err)
	}
	if len(data) == 0 {
		// Delete leaves a durable empty record so it never has to unlink an
		// attacker-replaceable pathname.
		return nil, fmt.Errorf("%w for profile %q", ErrCredentialNotFound, profileName)
	}
	return data, nil
}

func inspectUnixCredentialEntry(rootFD int, name string, allowMissing bool) (retErr error) {
	fd, err := unix.Openat(rootFD, name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err == unix.ENOENT && allowMissing {
		return nil
	}
	if err == unix.ELOOP {
		return errors.New("people provider credential path is not a regular file")
	}
	if err != nil {
		return fmt.Errorf("inspect people provider credential entry without following symlinks: %w", err)
	}
	defer func() {
		if err := unix.Close(fd); err != nil && retErr == nil {
			retErr = fmt.Errorf("close inspected people provider credential: %w", err)
		}
	}()
	return validateUnixCredentialFD(fd, "file")
}

func wipeUnixCredentialFD(fd int) error {
	if err := unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("wipe pinned people provider credential during deletion: %w", err)
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync wiped people provider credential during deletion: %w", err)
	}
	return nil
}

func wipeFailedUnixCredentialCandidate(candidate *os.File, hooks *credentialStoreHooks) error {
	if err := validateUnixCredentialFD(int(candidate.Fd()), "candidate"); err != nil {
		return errors.New("people provider credential candidate wipe refused")
	}
	var truncateErr error
	if hooks != nil && hooks.failedCandidateTruncate != nil {
		truncateErr = hooks.failedCandidateTruncate()
	} else {
		truncateErr = candidate.Truncate(0)
	}
	var syncErr error
	if hooks != nil && hooks.failedCandidateSync != nil {
		syncErr = hooks.failedCandidateSync()
	} else {
		syncErr = candidate.Sync()
	}
	var cleanupErrors []error
	if truncateErr != nil {
		cleanupErrors = append(cleanupErrors,
			errors.New("people provider credential candidate wipe failed"))
	}
	if syncErr != nil {
		cleanupErrors = append(cleanupErrors,
			errors.New("people provider credential candidate durable wipe failed"))
	}
	return errors.Join(cleanupErrors...)
}

func validateUnixCredentialFD(fd int, kind string) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return fmt.Errorf("identify opened people provider credential %s: %w", kind, err)
	}
	return validateUnixCredentialStat(stat, kind)
}

func validateUnixCredentialStat(stat unix.Stat_t, kind string) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("people provider credential %s is not a regular file", kind)
	}
	if stat.Mode&0o777 != 0o600 {
		return fmt.Errorf("people provider credential %s permissions are not private", kind)
	}
	if stat.Uid != unixEffectiveUID() {
		return fmt.Errorf("people provider credential %s owner does not match the current user", kind)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("people provider credential %s has an unsafe number of links", kind)
	}
	return nil
}

func unixCredentialEntryMatches(rootFD int, name string, opened unix.Stat_t) bool {
	var current unix.Stat_t
	if err := unix.Fstatat(rootFD, name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return opened.Dev == current.Dev && opened.Ino == current.Ino &&
		validateUnixCredentialStat(current, "file") == nil
}

// StoredCredentialsSupported reports whether this build can keep a
// profile-specific secret in the private tokens directory.
func StoredCredentialsSupported() bool { return true }
