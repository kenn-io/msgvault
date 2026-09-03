package config

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/robfig/cron/v3"
)

var (
	// ErrConfigConflict indicates that the on-disk content no longer matches
	// the ETag supplied by the caller.
	ErrConfigConflict = errors.New("config file changed")
	// ErrAmbiguousConfigTarget indicates that a targeted key occurs more than
	// once and cannot be edited without guessing.
	ErrAmbiguousConfigTarget = errors.New("ambiguous config edit target")
	// ErrUnsafeConfigTarget indicates a dangling, non-regular, or
	// ownership-unsafe symlink target.
	ErrUnsafeConfigTarget = errors.New("unsafe config target")
	// ErrInvalidConfigCandidate indicates that edited bytes were produced but
	// the complete candidate does not satisfy the config schema or invariants.
	ErrInvalidConfigCandidate = errors.New("invalid config candidate")
	// ErrConfigChanged indicates that the candidate became the live config but
	// a later durability or cleanup step failed and rollback was not possible.
	// Callers must treat the persisted config as changed even though the write
	// operation returned an error.
	ErrConfigChanged = errors.New("config changed despite write error")
	// ErrAtomicReplaceUnsupported indicates that this platform/filesystem cannot
	// provide the conditional exchange required to protect external hand edits.
	ErrAtomicReplaceUnsupported = errors.New("conditional atomic config replacement is unsupported")
)

// Edit is one targeted TOML assignment. Key is a dotted table/key path.
type Edit struct {
	Key   string
	Value any
}

// TableEdit inserts or removes one exact TOML table. Insertions refuse an
// existing semantic table instead of overwriting operator-owned content, and
// when the file defines the table without an explicit header they adopt that
// representation: exact dotted assignments are replaced in place, missing
// keys join the dotted-key family that defines the table, and only otherwise
// is the explicit header appended (which stays valid even when sub-table
// headers already define the table implicitly, because a super-table may
// follow its sub-tables).
type TableEdit struct {
	Path   []string
	Values map[string]any
	Remove bool
	// InsertOnly refuses an exact preexisting table, whether it is encoded
	// as an explicit header or as a dotted assignment family. It is used for
	// named records whose identity must never be overwritten by a concurrent
	// add.
	InsertOnly bool
}

// ConfigFile is an immutable snapshot of one resolved config target.
type ConfigFile struct {
	// LogicalPath is the operator-specified config path. It deliberately keeps
	// symlinks so relative values decode exactly as they do during daemon start.
	LogicalPath string
	// Path is the ownership-verified physical target used for safe replacement.
	Path     string
	Content  []byte
	ETag     string
	Mode     fs.FileMode
	Exists   bool
	identity string
	// retained pins an existing file identity for the lifetime of one edit.
	// Public snapshots never retain a descriptor.
	retained *os.File
	// parentIdentity pins the resolved containing directory for a missing file.
	parentIdentity string
}

// SameConfigFileVersion reports whether two snapshots identify the same exact
// retained file version, including its bytes, mode, resolved path, and inode
// identity. Matching ETags alone are insufficient because a concurrent writer
// can replace a file with byte-identical content.
func SameConfigFileVersion(left, right ConfigFile) bool {
	return left.Exists && right.Exists && left.LogicalPath == right.LogicalPath &&
		left.Path == right.Path && left.ETag == right.ETag &&
		sameConfigModePerm(left.Mode, right.Mode) && bytes.Equal(left.Content, right.Content) &&
		left.identity != "" && left.identity == right.identity
}

var configEditMu sync.Mutex

type syncDirectoryHandle interface {
	Sync() error
	Close() error
}

type configFileOps struct {
	initialRead           func(string) (ConfigFile, error)
	replace               func(string, string, ConfigFile, ConfigFile) (configReplacement, error)
	beforeExchange        func() error
	beforeExistingCleanup func(configReplacement) error
	beforeMissingCleanup  func(configPublication) error
	openDirectory         func(string) (syncDirectoryHandle, error)
	publishNew            func(string, *os.File, ConfigFile) (configPublication, error)
	read                  func(string) (ConfigFile, error)
}

type configReplacement struct {
	displacedPath     string
	rollbackPublished func(ConfigFile) error
	preserveCandidate bool
	recoveryPaths     []string
	published         ConfigFile
	syncDirectory     func() error
	cleanupDisplaced  func() error
	release           func() error
}

type configCandidate struct {
	file     *os.File
	retained *os.File
	path     string
	cleanup  func() error
	release  func() error
}

type configPublication struct {
	candidateRemains bool
	published        ConfigFile
	syncDirectory    func() error
	rollback         func(ConfigFile) error
	cleanupCandidate func() error
	release          func() error
}

func (replacement configReplacement) rollbackInstalledVersion() error {
	if !replacement.published.Exists || replacement.rollbackPublished == nil {
		return errors.Join(ErrConfigChanged, errors.New("config replacement has no verified rollback version"))
	}
	return replacement.rollbackPublished(replacement.published)
}

func defaultConfigFileOps() configFileOps {
	return configFileOps{
		initialRead:   readConfigFileForEdit,
		replace:       beginConfigReplacement,
		openDirectory: openConfigDirectoryForSync,
		publishNew:    publishNewConfig,
		read:          ReadConfigFile,
	}
}

// ReadConfigFile reads the config target without following unsafe symlinks.
// A missing ordinary path is represented as an empty, non-existing snapshot.
func ReadConfigFile(path string) (ConfigFile, error) {
	return readConfigFile(path, false)
}

func readConfigFileForEdit(path string) (ConfigFile, error) {
	return readConfigFile(path, true)
}

func readConfigFile(path string, retain bool) (ConfigFile, error) {
	logicalPath, err := filepath.Abs(path)
	if err != nil {
		return ConfigFile{}, fmt.Errorf("resolve logical config path: %w", err)
	}
	logicalPath = filepath.Clean(logicalPath)
	var snapshot ConfigFile
	if retain {
		snapshot, err = readConfigFileSnapshotForEdit(path)
	} else {
		snapshot, err = readConfigFileSnapshot(path)
	}
	if err != nil {
		return ConfigFile{}, err
	}
	snapshot.LogicalPath = logicalPath
	return snapshot, nil
}

// EditConfigFile applies targeted changes after an optimistic concurrency
// check, validates the complete candidate, and atomically replaces the target.
func EditConfigFile(path, ifMatch string, edits []Edit) (ConfigFile, error) {
	return editConfigFile(path, ifMatch, edits, defaultConfigFileOps())
}

// EditConfigFilePrivate performs the same conditional atomic transaction as
// EditConfigFile, but publishes the candidate owner-only. Remote settings
// surfaces use this boundary because config.toml may already contain secrets
// outside the particular keys being changed.
func EditConfigFilePrivate(path, ifMatch string, edits []Edit) (ConfigFile, error) {
	return editConfigFileWithPrivacy(path, ifMatch, edits, defaultConfigFileOps(), true)
}

func editConfigFile(path, ifMatch string, edits []Edit, ops configFileOps) (ConfigFile, error) {
	return editConfigWithTransform(path, ifMatch, len(edits) > 0, func(content []byte) ([]byte, error) {
		return applyTargetedEdits(content, edits)
	}, ops)
}

func editConfigFileWithPrivacy(
	path, ifMatch string,
	edits []Edit,
	ops configFileOps,
	forcePrivate bool,
) (ConfigFile, error) {
	return editConfigWithMatch(path, ifMatch, nil, len(edits) > 0, func(content []byte) ([]byte, error) {
		return applyTargetedEdits(content, edits)
	}, ops, forcePrivate)
}

// EditConfigTables applies exact table insertions/removals through the same
// ownership, concurrency, validation, durability, rollback, and recovery path
// used by EditConfigFile.
func EditConfigTables(path, ifMatch string, edits []TableEdit) (ConfigFile, error) {
	return editConfigWithTransform(path, ifMatch, len(edits) > 0, func(content []byte) ([]byte, error) {
		return applyTableEdits(content, edits)
	}, defaultConfigFileOps())
}

// ValidateConfigTableEdits applies and validates an exact table plan against
// one retained snapshot without publishing it. Callers use this before an
// irreversible side effect; EditConfigTables repeats the plan under ETag.
func ValidateConfigTableEdits(snapshot ConfigFile, edits []TableEdit) error {
	if snapshot.LogicalPath == "" || snapshot.Path == "" || snapshot.ETag != configETag(snapshot.Content) {
		return errors.New("config edit snapshot is invalid")
	}
	candidate, err := applyTableEdits(snapshot.Content, edits)
	if err != nil {
		return err
	}
	mode := snapshot.Mode.Perm()
	if !snapshot.Exists {
		mode = 0o600
	}
	loaded, err := LoadConfigFile(ConfigFile{
		LogicalPath: snapshot.LogicalPath,
		Path:        snapshot.Path,
		Content:     candidate,
		ETag:        configETag(candidate),
		Mode:        mode,
		Exists:      true,
	}, "")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfigCandidate, err)
	}
	if err := validateEditableCandidate(loaded); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfigCandidate, err)
	}
	return nil
}

// RestoreConfigFile restores before only while the live target is the exact
// filesystem object published by the transaction. The post-write snapshot is
// an identity guard, not merely an ETag: byte-identical concurrent replacement
// files are preserved.
func RestoreConfigFile(path string, published, before ConfigFile) (ConfigFile, error) {
	return restoreConfigFile(path, published, before, defaultConfigFileOps())
}

func restoreConfigFile(path string, published, before ConfigFile, ops configFileOps) (ConfigFile, error) {
	if err := validatePublishedConfigSnapshot(published); err != nil {
		return ConfigFile{}, err
	}
	if !before.Exists {
		if before.ETag != configETag(nil) || before.Path == "" || before.parentIdentity == "" {
			return ConfigFile{}, errors.New("config rollback snapshot is invalid")
		}
		return restoreMissingConfigFile(path, published, before, ops)
	}
	if before.ETag == "" || before.ETag != configETag(before.Content) {
		return ConfigFile{}, errors.New("config rollback snapshot is invalid")
	}
	return editConfigWithExpectedTransform(path, published, true, func([]byte) ([]byte, error) {
		return append([]byte(nil), before.Content...), nil
	}, ops)
}

func validatePublishedConfigSnapshot(published ConfigFile) error {
	if !published.Exists || published.LogicalPath == "" || published.Path == "" ||
		published.identity == "" || published.ETag == "" ||
		published.ETag != configETag(published.Content) {
		return errors.New("published config rollback identity is invalid")
	}
	return nil
}

func restoreMissingConfigFile(
	path string,
	published, before ConfigFile,
	ops configFileOps,
) (ConfigFile, error) {
	configEditMu.Lock()
	defer configEditMu.Unlock()
	readInitial := ops.initialRead
	if readInitial == nil {
		readInitial = ops.read
	}
	current, err := readInitial(path)
	if err != nil {
		return ConfigFile{}, err
	}
	if current.retained != nil {
		defer func() { _ = current.retained.Close() }()
	}
	if !SameConfigFileVersion(current, published) {
		return ConfigFile{}, errors.Join(ErrConfigConflict,
			errors.New("config rollback target is not the published transaction object"))
	}
	if ops.beforeExchange != nil {
		if err := ops.beforeExchange(); err != nil {
			return ConfigFile{}, fmt.Errorf("before config rollback retirement: %w", err)
		}
	}
	if err := retireExactConfigForMissingRestore(current, before); err != nil {
		return ConfigFile{}, err
	}
	read := ops.read
	if read == nil {
		read = ReadConfigFile
	}
	restored, err := read(path)
	if err != nil {
		return ConfigFile{}, errors.Join(ErrConfigChanged, fmt.Errorf("verify missing config rollback: %w", err))
	}
	if restored.Exists || restored.ETag != before.ETag || restored.Path != before.Path {
		return ConfigFile{}, errors.Join(ErrConfigChanged, errors.New("missing config rollback could not be verified"))
	}
	return restored, nil
}

func editConfigWithTransform(
	path, ifMatch string,
	hasChanges bool,
	transform func([]byte) ([]byte, error),
	ops configFileOps,
) (result ConfigFile, resultErr error) {
	return editConfigWithMatch(path, ifMatch, nil, hasChanges, transform, ops, false)
}

func editConfigWithExpectedTransform(
	path string,
	published ConfigFile,
	hasChanges bool,
	transform func([]byte) ([]byte, error),
	ops configFileOps,
) (result ConfigFile, resultErr error) {
	return editConfigWithMatch(path, "", &published, hasChanges, transform, ops, false)
}

func editConfigWithMatch(
	path, ifMatch string,
	published *ConfigFile,
	hasChanges bool,
	transform func([]byte) ([]byte, error),
	ops configFileOps,
	forcePrivate bool,
) (result ConfigFile, resultErr error) {
	var expected ConfigFile
	defer func() {
		if errors.Is(resultErr, ErrConfigChanged) && expected.Exists {
			result = expected
		}
	}()
	configEditMu.Lock()
	defer configEditMu.Unlock()

	readInitial := ops.initialRead
	if readInitial == nil {
		readInitial = ops.read
	}
	before, err := readInitial(path)
	if err != nil && configParentIsMissing(path) {
		// Windows cannot snapshot a missing final parent. Do not create it
		// until the caller has proved knowledge of the empty snapshot ETag.
		if ifMatch == "" || ifMatch != configETag(nil) {
			return ConfigFile{}, fmt.Errorf("%w: current ETag is %s", ErrConfigConflict, configETag(nil))
		}
		if !hasChanges {
			return ConfigFile{}, err
		}
		if err := ensureConfigParentDirectories(path); err != nil {
			return ConfigFile{}, err
		}
		before, err = readInitial(path)
	}
	if err != nil {
		return ConfigFile{}, err
	}
	if before.retained != nil {
		defer func() { _ = before.retained.Close() }()
	}
	if published != nil && !SameConfigFileVersion(before, *published) {
		return ConfigFile{}, errors.Join(ErrConfigConflict,
			errors.New("config target is not the published transaction object"))
	}
	if published == nil && (ifMatch == "" || ifMatch != before.ETag) {
		return ConfigFile{}, fmt.Errorf("%w: current ETag is %s", ErrConfigConflict, before.ETag)
	}
	if !hasChanges {
		return before, nil
	}
	if !before.Exists {
		if err := ensureConfigParentDirectories(path, before.parentIdentity); err != nil {
			return ConfigFile{}, err
		}
		refreshed, refreshErr := readInitial(path)
		if refreshErr != nil {
			return ConfigFile{}, refreshErr
		}
		if refreshed.Exists || refreshed.ETag != before.ETag || refreshed.Path != before.Path {
			if refreshed.retained != nil {
				_ = refreshed.retained.Close()
			}
			return ConfigFile{}, fmt.Errorf("%w: config target changed while creating its directory", ErrConfigConflict)
		}
		before = refreshed
	}

	candidate, err := transform(before.Content)
	if err != nil {
		return ConfigFile{}, err
	}
	dir := filepath.Dir(before.Path)
	created, err := createConfigCandidate(dir)
	if err != nil {
		return ConfigFile{}, fmt.Errorf("create config candidate: %w", err)
	}
	tmp, tmpPath := created.file, created.path
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = created.cleanup()
		}
		if created.release != nil {
			_ = created.release()
		}
	}()

	mode := before.Mode.Perm()
	if !before.Exists || forcePrivate {
		mode = 0o600
	}
	expected = ConfigFile{
		LogicalPath: before.LogicalPath, Path: before.Path,
		Content: append([]byte(nil), candidate...), ETag: configETag(candidate),
		Mode: mode, Exists: true,
	}
	if err := secureConfigCandidate(tmp, tmpPath, mode); err != nil {
		return ConfigFile{}, fmt.Errorf("set config candidate permissions: %w", err)
	}
	if _, err := tmp.Write(candidate); err != nil {
		return ConfigFile{}, fmt.Errorf("write config candidate: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return ConfigFile{}, fmt.Errorf("sync config candidate: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return ConfigFile{}, fmt.Errorf("close config candidate: %w", err)
	}

	loaded, err := LoadConfigFile(ConfigFile{
		LogicalPath: before.LogicalPath,
		Path:        tmpPath,
		Content:     candidate,
		ETag:        configETag(candidate),
		Mode:        mode,
		Exists:      true,
	}, "")
	if err != nil {
		return ConfigFile{}, fmt.Errorf("%w: %w", ErrInvalidConfigCandidate, err)
	}
	if err := validateEditableCandidate(loaded); err != nil {
		return ConfigFile{}, fmt.Errorf("%w: %w", ErrInvalidConfigCandidate, err)
	}

	if ops.beforeExchange != nil {
		if err := ops.beforeExchange(); err != nil {
			return ConfigFile{}, fmt.Errorf("before config replacement: %w", err)
		}
	}
	if before.Exists {
		replacement, replaceErr := conditionalReplace(path, tmpPath, before, ops.replace, ops.read)
		capturePublishedConfigVersion(&expected, replacement.published, "")
		if replacement.release != nil {
			defer func() { _ = replacement.release() }()
		}
		if replaceErr != nil {
			if errors.Is(replaceErr, ErrConfigChanged) {
				keep = replacement.preserveCandidate || replacement.displacedPath == tmpPath
			}
			return ConfigFile{}, replaceErr
		}
		if syncErr := syncReplacementDirectory(replacement, dir, ops.openDirectory); syncErr != nil {
			if rollbackErr := rollbackConfigReplacement(path, before, replacement, ops.read); rollbackErr != nil {
				keep = replacement.preserveCandidate || replacement.displacedPath == tmpPath
				return ConfigFile{}, errors.Join(
					fmt.Errorf("%w: config directory sync failed after publication", ErrConfigChanged),
					syncErr,
					rollbackErr,
					recoveryArtifactError(replacement),
				)
			}
			if rollbackSyncErr := syncReplacementDirectory(replacement, dir, ops.openDirectory); rollbackSyncErr != nil {
				keep = true
				return ConfigFile{}, errors.Join(
					fmt.Errorf("%w: rollback could not be made durable", ErrConfigChanged),
					syncErr,
					rollbackSyncErr,
				)
			}
			return ConfigFile{}, syncErr
		}
		if ops.beforeExistingCleanup != nil {
			if err := ops.beforeExistingCleanup(replacement); err != nil {
				keep = true
				return ConfigFile{}, errors.Join(fmt.Errorf("%w: replacement cleanup preparation failed", ErrConfigChanged), err)
			}
		}
		var removeErr error
		if replacement.cleanupDisplaced != nil {
			removeErr = replacement.cleanupDisplaced()
		} else {
			removeErr = errors.New("config replacement has no retirement operation")
		}
		if removeErr != nil {
			keep = true
			return ConfigFile{}, errors.Join(
				fmt.Errorf("%w: replacement cleanup failed", ErrConfigChanged),
				removeErr,
			)
		}
		// The candidate descriptor now names the live published config, while
		// its former pathname has been retired as the displaced artifact. It
		// must not be treated as a temporary file on later durability errors.
		keep = true
		if syncErr := syncReplacementDirectory(replacement, dir, ops.openDirectory); syncErr != nil {
			return ConfigFile{}, errors.Join(
				fmt.Errorf("%w: cleanup durability failed", ErrConfigChanged),
				syncErr,
			)
		}
	} else {
		publication, err := ops.publishNew(tmpPath, created.retained, before)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return ConfigFile{}, fmt.Errorf("%w: config file was created", ErrConfigConflict)
			}
			return ConfigFile{}, fmt.Errorf("create config file conditionally: %w", err)
		}
		if publication.release != nil {
			defer func() { _ = publication.release() }()
		}
		capturePublishedConfigVersion(&expected, publication.published, before.Path)
		syncPublication := func() error {
			if publication.syncDirectory != nil {
				return publication.syncDirectory()
			}
			return syncConfigDirectory(dir, ops.openDirectory)
		}
		if syncErr := syncPublication(); syncErr != nil {
			var rollbackErr error
			if publication.rollback != nil {
				rollbackErr = publication.rollback(publication.published)
			} else {
				rollbackErr = errors.Join(ErrConfigChanged,
					errors.New("config publication has no rollback retirement operation"))
			}
			if rollbackErr != nil {
				return ConfigFile{}, errors.Join(
					fmt.Errorf("%w: config directory sync failed after creation", ErrConfigChanged),
					syncErr,
					fmt.Errorf("rollback created config: %w", rollbackErr),
				)
			}
			if rollbackSyncErr := syncPublication(); rollbackSyncErr != nil {
				keep = true
				return ConfigFile{}, errors.Join(
					fmt.Errorf("%w: creation rollback could not be made durable", ErrConfigChanged),
					syncErr,
					rollbackSyncErr,
				)
			}
			return ConfigFile{}, syncErr
		}
		if !publication.candidateRemains {
			keep = true
		} else {
			if ops.beforeMissingCleanup != nil {
				if err := ops.beforeMissingCleanup(publication); err != nil {
					keep = true
					return ConfigFile{}, errors.Join(fmt.Errorf("%w: candidate cleanup preparation failed", ErrConfigChanged), err)
				}
			}
			var cleanupErr error
			if publication.cleanupCandidate != nil {
				cleanupErr = publication.cleanupCandidate()
			} else {
				cleanupErr = errors.New("config publication has no candidate retirement operation")
			}
			if cleanupErr != nil {
				keep = true
				return ConfigFile{}, errors.Join(
					fmt.Errorf("%w: candidate-link cleanup failed", ErrConfigChanged),
					cleanupErr,
				)
			}
		}
		if syncErr := syncPublication(); syncErr != nil {
			return ConfigFile{}, errors.Join(
				fmt.Errorf("%w: cleanup durability failed", ErrConfigChanged),
				syncErr,
			)
		}
	}
	keep = true
	after, readErr := ops.read(path)
	if readErr != nil {
		return ConfigFile{}, errors.Join(
			fmt.Errorf("%w: committed config could not be read", ErrConfigChanged),
			readErr,
		)
	}
	if !SameConfigFileVersion(after, expected) {
		return ConfigFile{}, errors.Join(
			fmt.Errorf("%w: committed config identity changed before return", ErrConfigChanged),
			ErrConfigConflict,
		)
	}
	return after, nil
}

func capturePublishedConfigVersion(expected *ConfigFile, published ConfigFile, pathOverride string) {
	if pathOverride != "" {
		published.Path = pathOverride
	}
	published.LogicalPath = expected.LogicalPath
	if published.Exists && published.Path == expected.Path && published.ETag == expected.ETag &&
		sameConfigModePerm(published.Mode, expected.Mode) && bytes.Equal(published.Content, expected.Content) &&
		published.identity != "" {
		*expected = published
	}
}

func configParentIsMissing(path string) bool {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Dir(absolute))
	return errors.Is(err, fs.ErrNotExist)
}

func syncReplacementDirectory(replacement configReplacement, path string, open func(string) (syncDirectoryHandle, error)) error {
	if replacement.syncDirectory != nil {
		return replacement.syncDirectory()
	}
	return syncConfigDirectory(path, open)
}

func conditionalReplace(
	requestedPath, candidatePath string,
	before ConfigFile,
	replace func(string, string, ConfigFile, ConfigFile) (configReplacement, error),
	read func(string) (ConfigFile, error),
) (configReplacement, error) {
	current, err := read(requestedPath)
	if err != nil {
		return configReplacement{}, err
	}
	if !sameConfigSnapshot(current, before) {
		return configReplacement{}, fmt.Errorf("%w: config target changed before replacement", ErrConfigConflict)
	}
	candidateBefore, err := readPhysicalConfigSnapshot(candidatePath)
	if err != nil {
		return configReplacement{}, fmt.Errorf("inspect config candidate before replacement: %w", err)
	}
	replacement, err := replace(candidatePath, before.Path, candidateBefore, before)
	if err != nil {
		if errors.Is(err, ErrConfigChanged) {
			return replacement, errors.Join(
				fmt.Errorf("replace config file conditionally: %w", err),
				recoveryArtifactError(replacement),
			)
		}
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrExist) {
			return configReplacement{}, fmt.Errorf("%w: config target changed during replacement: %w", ErrConfigConflict, err)
		}
		return configReplacement{}, fmt.Errorf("replace config file conditionally: %w", err)
	}
	published, publishedErr := readPhysicalConfigSnapshot(before.Path)
	if publishedErr != nil || !sameConfigVersion(published, candidateBefore) {
		replacement.published = published
		return replacement, errors.Join(
			fmt.Errorf("%w: live config changed during replacement verification", ErrConfigChanged),
			fmt.Errorf("%w: published config is not the verified candidate", ErrConfigConflict),
			publishedErr,
			recoveryArtifactError(replacement),
		)
	}
	replacement.published = published
	displaced, displacedErr := readPhysicalConfigSnapshot(replacement.displacedPath)
	if displacedErr != nil {
		return replacement, errors.Join(
			fmt.Errorf("%w: displaced config could not be verified", ErrConfigChanged),
			displacedErr,
			recoveryArtifactError(replacement),
		)
	}
	if sameConfigVersion(displaced, before) {
		mapped, mapErr := read(requestedPath)
		if mapErr != nil || !mapped.Exists || mapped.Path != before.Path || !sameConfigVersion(mapped, candidateBefore) {
			if rollbackErr := replacement.rollbackInstalledVersion(); rollbackErr != nil {
				return replacement, errors.Join(
					fmt.Errorf("%w: rollback failed after config symlink race", ErrConfigChanged),
					fmt.Errorf("%w: config symlink target changed during replacement", ErrConfigConflict),
					fmt.Errorf("restore config after symlink race: %w", rollbackErr),
					recoveryArtifactError(replacement),
				)
			}
			restored, restoreErr := readPhysicalConfigSnapshot(before.Path)
			if restoreErr != nil || !sameConfigVersion(restored, before) {
				return replacement, errors.Join(
					fmt.Errorf("%w: config rollback could not be verified", ErrConfigChanged),
					restoreErr,
					recoveryArtifactError(replacement),
				)
			}
			return replacement, fmt.Errorf("%w: config symlink target changed during replacement", ErrConfigConflict)
		}
		return replacement, nil
	}
	rollbackErr := replacement.rollbackInstalledVersion()
	if rollbackErr != nil {
		return replacement, errors.Join(
			fmt.Errorf("%w: rollback failed after replacement conflict", ErrConfigChanged),
			fmt.Errorf("%w: displaced config no longer matched expected snapshot identity", ErrConfigConflict),
			fmt.Errorf("restore raced config after conditional exchange: %w", rollbackErr),
			recoveryArtifactError(replacement),
		)
	}
	restored, restoreErr := readPhysicalConfigSnapshot(before.Path)
	if restoreErr != nil || !sameConfigVersion(restored, displaced) {
		return replacement, errors.Join(
			fmt.Errorf("%w: raced config rollback could not be verified", ErrConfigChanged),
			restoreErr,
			recoveryArtifactError(replacement),
		)
	}
	return replacement, fmt.Errorf("%w: displaced config no longer matched the original snapshot", ErrConfigConflict)
}

func rollbackConfigReplacement(requestedPath string, before ConfigFile, replacement configReplacement, read func(string) (ConfigFile, error)) error {
	current, err := readPhysicalConfigSnapshot(before.Path)
	if err != nil || !sameConfigVersion(current, replacement.published) {
		return errors.Join(
			errors.New("refusing rollback because the published config changed"),
			err,
		)
	}
	if err := replacement.rollbackInstalledVersion(); err != nil {
		return fmt.Errorf("rollback config replacement: %w", err)
	}
	restored, err := read(requestedPath)
	if err != nil {
		return fmt.Errorf("verify rolled-back config: %w", err)
	}
	if !sameConfigSnapshot(restored, before) {
		return errors.New("rolled-back config does not match the original snapshot")
	}
	return nil
}

func sameConfigSnapshot(left, right ConfigFile) bool {
	if left.Path != right.Path || left.Exists != right.Exists {
		return false
	}
	if !left.Exists {
		return left.parentIdentity != "" && left.parentIdentity == right.parentIdentity && left.ETag == right.ETag
	}
	return sameConfigVersion(left, right)
}

func sameConfigVersion(left, right ConfigFile) bool {
	return left.Exists && right.Exists && left.ETag == right.ETag && left.identity != "" && left.identity == right.identity
}

func readPhysicalConfigSnapshot(path string) (ConfigFile, error) {
	content, mode, identity, err := readVerifiedPhysicalConfig(path)
	if err != nil {
		return ConfigFile{}, err
	}
	return ConfigFile{Path: path, Content: content, ETag: configETag(content), Mode: mode, Exists: true, identity: identity}, nil
}

func readVerifiedPhysicalConfig(path string) ([]byte, fs.FileMode, string, error) {
	file, err := openConfigNoFollow(path)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = file.Close() }()
	return readVerifiedOpenedConfig(file)
}

func readVerifiedOpenedConfig(file *os.File) ([]byte, fs.FileMode, string, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, 0, "", fmt.Errorf("stat opened config: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, "", fmt.Errorf("%w: opened config is not regular", ErrUnsafeConfigTarget)
	}
	if euid, supported := effectiveUserID(); supported {
		uid, verified := fileOwner(info)
		if !verified || uid != euid {
			return nil, 0, "", fmt.Errorf("%w: opened config is not owned by the effective user", ErrUnsafeConfigTarget)
		}
	}
	if err := validateOpenedConfigSecurity(file); err != nil {
		return nil, 0, "", fmt.Errorf("%w: opened config security is not safe: %w", ErrUnsafeConfigTarget, err)
	}
	identity, ok := openedFileIdentity(file, info)
	if !ok {
		return nil, 0, "", fmt.Errorf("%w: opened config identity is unavailable", ErrUnsafeConfigTarget)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, "", err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, 0, "", fmt.Errorf("restat opened config: %w", err)
	}
	afterIdentity, ok := openedFileIdentity(file, after)
	if !ok || afterIdentity != identity {
		return nil, 0, "", fmt.Errorf("%w: opened config identity changed while reading", ErrUnsafeConfigTarget)
	}
	return content, info.Mode(), identity, nil
}

func recoveryArtifactError(replacement configReplacement) error {
	paths := append([]string(nil), replacement.recoveryPaths...)
	if replacement.displacedPath != "" {
		paths = append(paths, replacement.displacedPath)
	}
	if len(paths) == 0 {
		return nil
	}
	return fmt.Errorf("config recovery artifact preserved at %s", strings.Join(paths, ", "))
}

func syncConfigDirectory(path string, open func(string) (syncDirectoryHandle, error)) error {
	handle, err := open(path)
	if err != nil {
		return fmt.Errorf("open config directory for sync: %w", err)
	}
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if syncErr != nil || closeErr != nil {
		var errs []error
		if syncErr != nil {
			errs = append(errs, fmt.Errorf("sync config directory: %w", syncErr))
		}
		if closeErr != nil {
			errs = append(errs, fmt.Errorf("close config directory: %w", closeErr))
		}
		return errors.Join(errs...)
	}
	return nil
}

func validateEditableCandidate(cfg *Config) error {
	if err := cfg.Server.ValidateSecure(); err != nil {
		return err
	}
	if cfg.Vector.AnyLaneEnabled() {
		if err := cfg.Vector.Validate(); err != nil {
			return fmt.Errorf("vector config: %w", err)
		}
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedules := map[string]string{
		"vector.embed.schedule.cron":      cfg.Vector.Embed.Schedule.Cron,
		"vector.multimodal.schedule.cron": cfg.Vector.Multimodal.Schedule.Cron,
		"beeper.schedule":                 cfg.Beeper.Schedule,
		"slack.schedule":                  cfg.Slack.Schedule,
	}
	for index, account := range cfg.Accounts {
		schedules[fmt.Sprintf("accounts[%d].schedule", index)] = account.Schedule
	}
	for index, source := range cfg.SynctechSMS.Sources {
		schedules[fmt.Sprintf("synctech_sms.sources[%d].schedule", index)] = source.Schedule
	}
	for index, source := range cfg.GCal {
		schedules[fmt.Sprintf("gcal[%d].schedule", index)] = source.Schedule
	}
	for index, source := range cfg.Granola {
		schedules[fmt.Sprintf("granola[%d].schedule", index)] = source.Schedule
	}
	for index, source := range cfg.Circleback {
		schedules[fmt.Sprintf("circleback[%d].schedule", index)] = source.Schedule
	}
	for index, source := range cfg.NotionMeetings {
		schedules[fmt.Sprintf("notion_meetings[%d].schedule", index)] = source.Schedule
	}
	for key, expression := range schedules {
		if expression == "" {
			continue
		}
		if _, err := parser.Parse(expression); err != nil {
			return fmt.Errorf("invalid %s %q: %w", key, expression, err)
		}
	}
	return nil
}

func resolveConfigTargetWithOwner(path string, owner func(fs.FileInfo) (uint64, bool)) (string, fs.FileMode, bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", 0, false, fmt.Errorf("resolve config path: %w", err)
	}
	requestedInfo, requestedErr := os.Lstat(absolute)
	requestedSymlink := requestedErr == nil && requestedInfo.Mode()&os.ModeSymlink != 0
	resolved, resolveErr := resolveOwnedSymlinks(absolute, owner)
	if resolveErr != nil {
		return "", 0, false, fmt.Errorf("%w: resolve config path: %w", ErrUnsafeConfigTarget, resolveErr)
	}
	absolute = resolved
	info, err := os.Lstat(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		if requestedSymlink {
			return "", 0, false, fmt.Errorf("%w: config symlink target does not exist", ErrUnsafeConfigTarget)
		}
		return absolute, 0o600, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("inspect config path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, false, fmt.Errorf("%w: config path is not a regular file", ErrUnsafeConfigTarget)
	}
	if euid, supported := effectiveUserID(); supported {
		uid, verified := owner(info)
		if !verified || uid != euid {
			return "", 0, false, fmt.Errorf("%w: config target is not owned by the effective user", ErrUnsafeConfigTarget)
		}
	}
	return absolute, info.Mode(), true, nil
}

// The resolved-path result is consumed on fallback platforms; native pinned
// resolver builds retain this helper for identical regression coverage.
//
//nolint:unparam
func resolveOwnedSymlinksWithReadlink(
	path string,
	owner func(fs.FileInfo) (uint64, bool),
	readlink func(string) (string, error),
) (string, error) {
	euid, supported := effectiveUserID()
	current := filepath.Clean(path)
	for range 255 {
		volume := filepath.VolumeName(current)
		root := volume + string(filepath.Separator)
		relative := strings.TrimPrefix(current, root)
		parts := strings.Split(relative, string(filepath.Separator))
		prefix := root
		followed := false
		for index, part := range parts {
			if part == "" {
				continue
			}
			candidate := filepath.Join(prefix, part)
			info, err := os.Lstat(candidate)
			if errors.Is(err, fs.ErrNotExist) {
				return filepath.Join(append([]string{candidate}, parts[index+1:]...)...), nil
			}
			if err != nil {
				return "", err
			}
			if info.Mode()&os.ModeSymlink == 0 {
				prefix = candidate
				continue
			}
			if !supported {
				return "", errors.New("symlink ownership verification is unavailable")
			}
			uid, ok := owner(info)
			if !ok || (uid != euid && uid != 0) {
				return "", errors.New("symlink hop is not owned by the effective user or root")
			}
			beforeIdentity, beforeIdentityOK := pathEntryIdentity(candidate, info)
			target, err := readlink(candidate)
			if err != nil {
				return "", err
			}
			after, err := os.Lstat(candidate)
			if err != nil {
				return "", errors.Join(ErrUnsafeConfigTarget,
					errors.New("symlink changed while its target was inspected"), err)
			}
			afterIdentity, afterIdentityOK := pathEntryIdentity(candidate, after)
			if after.Mode()&os.ModeSymlink == 0 || !os.SameFile(info, after) ||
				!beforeIdentityOK || !afterIdentityOK || beforeIdentity != afterIdentity {
				return "", errors.Join(ErrUnsafeConfigTarget,
					errors.New("symlink changed while its target was inspected"))
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(candidate), target)
			}
			current = filepath.Join(append([]string{target}, parts[index+1:]...)...)
			current = filepath.Clean(current)
			followed = true
			break
		}
		if !followed {
			return current, nil
		}
	}
	return "", errors.New("too many config symlink hops")
}

func configETag(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf("\"sha256-%x\"", sum)
}

type tomlLine struct {
	body string
	eol  string
}

var bareTOMLTableSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func applyTargetedEdits(content []byte, edits []Edit) ([]byte, error) {
	lines := splitTOMLLines(string(content))
	seenEdits := make(map[string]struct{}, len(edits))
	for _, edit := range edits {
		if _, duplicate := seenEdits[edit.Key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate requested key %q", ErrAmbiguousConfigTarget, edit.Key)
		}
		seenEdits[edit.Key] = struct{}{}
		if provider, ok := edit.Value.(personEnrichmentProviderEdit); ok {
			if edit.Key != personEnrichmentProviderEditKey {
				return nil, fmt.Errorf("%w: invalid dedicated provider target", ErrUnsafeConfigTarget)
			}
			var err error
			lines, err = replacePersonEnrichmentProviderLines(lines, provider)
			if err != nil {
				return nil, err
			}
			continue
		}
		section, key, ok := strings.Cut(edit.Key, ".")
		if !ok || section == "" || key == "" {
			return nil, fmt.Errorf("invalid config edit key %q", edit.Key)
		}
		if lastDot := strings.LastIndex(edit.Key, "."); lastDot >= 0 {
			section, key = edit.Key[:lastDot], edit.Key[lastDot+1:]
		}
		value, err := encodeTOMLValue(edit.Value)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", edit.Key, err)
		}
		lines, err = editTOMLLines(lines, section, key, value)
		if err != nil {
			return nil, err
		}
	}
	return joinTOMLLines(lines), nil
}

func applyTableEdits(content []byte, edits []TableEdit) ([]byte, error) {
	lines := splitTOMLLines(string(content))
	seen := make(map[string]struct{}, len(edits))
	for _, edit := range edits {
		path, encodedPath, err := validateTableEdit(edit)
		if err != nil {
			return nil, err
		}
		identity := strings.Join(path, "\x00")
		if _, duplicate := seen[identity]; duplicate {
			return nil, fmt.Errorf("%w: duplicate requested table %s", ErrAmbiguousConfigTarget, encodedPath)
		}
		seen[identity] = struct{}{}

		matches := exactTOMLTableHeaders(lines, path)
		if len(matches) > 1 {
			return nil, fmt.Errorf("%w: duplicate table %s", ErrAmbiguousConfigTarget, encodedPath)
		}
		if edit.Remove {
			if len(matches) == 0 {
				return nil, fmt.Errorf("config table %s does not exist", encodedPath)
			}
			if tomlPathHasDescendantContent(lines, path) {
				return nil, fmt.Errorf("%w: table %s has descendant content", ErrAmbiguousConfigTarget, encodedPath)
			}
			lines = removeTOMLTable(lines, matches[0], path)
			continue
		}

		if len(matches) == 0 {
			if edit.InsertOnly && (len(dottedTOMLFamily(lines, path)) > 0 || tomlPathAssignedAsValue(lines, path)) {
				return nil, fmt.Errorf("%w: table %s already exists", ErrAmbiguousConfigTarget, encodedPath)
			}
			lines, err = insertTOMLTableValues(lines, path, encodedPath, edit.Values)
			if err != nil {
				return nil, err
			}
			continue
		}
		if edit.InsertOnly {
			return nil, fmt.Errorf("%w: table %s already exists", ErrAmbiguousConfigTarget, encodedPath)
		}
		keys := make([]string, 0, len(edit.Values))
		for key := range edit.Values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines, err = editExactTOMLTableValue(lines, matches[0], path, key, edit.Values[key])
			if err != nil {
				return nil, err
			}
		}
	}
	return joinTOMLLines(lines), nil
}

func validateTableEdit(edit TableEdit) ([]string, string, error) {
	if len(edit.Path) == 0 {
		return nil, "", errors.New("config table path is required")
	}
	path := append([]string(nil), edit.Path...)
	for _, segment := range path {
		if segment == "" || strings.TrimSpace(segment) != segment || strings.ContainsAny(segment, "\r\n\x00") {
			return nil, "", errors.New("config table path contains an invalid segment")
		}
	}
	encodedPath := encodeTOMLPathSuffix(path)
	if parsed, ok := parseTOMLKey(encodedPath); !ok || !equalPath(parsed, path) {
		return nil, "", errors.New("config table path is not representable in TOML")
	}
	if edit.Remove {
		if edit.InsertOnly {
			return nil, "", errors.New("removed config table cannot be insert-only")
		}
		if len(edit.Values) != 0 {
			return nil, "", errors.New("removed config table cannot also contain values")
		}
		return path, encodedPath, nil
	}
	if len(edit.Values) == 0 {
		return nil, "", errors.New("inserted config table requires values")
	}
	for key := range edit.Values {
		parsed, ok := parseTOMLKey(key)
		if !ok || len(parsed) != 1 || parsed[0] != key {
			return nil, "", fmt.Errorf("invalid config table key %q", key)
		}
	}
	return path, encodedPath, nil
}

func tomlPathHasDescendantContent(lines []tomlLine, target []string) bool {
	structural := tomlStructuralLines(lines)
	var table []string
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if parsed, _, ok := parseTOMLTable(line.body); ok {
			table = parsed
			if len(parsed) > len(target) && pathPrefix(target, parsed) {
				return true
			}
			continue
		}
		assignment, ok := assignmentKey(line.body)
		if !ok {
			continue
		}
		fullPath := appendPath(table, assignment)
		if len(fullPath) > len(target)+1 && pathPrefix(target, fullPath) {
			return true
		}
	}
	return false
}

func exactTOMLTableHeaders(lines []tomlLine, path []string) []int {
	structural := tomlStructuralLines(lines)
	var matches []int
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		parsed, array, ok := parseTOMLTable(line.body)
		if ok && !array && equalPath(parsed, path) {
			matches = append(matches, index)
		}
	}
	return matches
}

// encodeTOMLPathSuffix renders bare segments verbatim and quotes the rest,
// yielding one dotted TOML key for the exact path segments given.
func encodeTOMLPathSuffix(segments []string) string {
	encoded := make([]string, len(segments))
	for index, segment := range segments {
		if bareTOMLTableSegment.MatchString(segment) {
			encoded[index] = segment
		} else {
			encoded[index] = strconv.Quote(segment)
		}
	}
	return strings.Join(encoded, ".")
}

// insertTOMLTableValues publishes values for a table that has no explicit
// TOML header, choosing the one representation that cannot conflict with how
// the file already defines the table:
//
//   - an exact dotted assignment, such as a root-level
//     `people.sweep.provider = { ... }`, is replaced in place, so an existing
//     inline value becomes the new scalar without touching other lines;
//   - remaining keys join the dotted-key family that defines the table
//     (adjacent lines, same table context), because a table defined through
//     dotted keys must not be redefined by an explicit header;
//   - otherwise the explicit header is appended, which stays valid even when
//     sub-table headers such as [people.sweep.budgets] already define the
//     table implicitly: TOML permits defining a super-table after its
//     sub-tables.
//
// A table path that is itself assigned as a value (for example
// `people.sweep = { ... }`) cannot gain keys in any representation — neither
// a header nor a dotted key may extend an inline value — and stays ambiguous.
func insertTOMLTableValues(
	lines []tomlLine,
	path []string,
	encodedPath string,
	values map[string]any,
) ([]tomlLine, error) {
	if tomlPathAssignedAsValue(lines, path) {
		return nil, fmt.Errorf("%w: table %s has a non-table representation", ErrAmbiguousConfigTarget, encodedPath)
	}
	family := dottedTOMLFamily(lines, path)
	if len(family) == 0 {
		return appendTOMLTable(lines, encodedPath, values)
	}
	return editDottedTOMLFamily(lines, family, path, encodedPath, values)
}

// dottedTOMLFamilyAssignment is one structural assignment whose table context
// is a strict prefix of the family's table path and whose full key path
// extends that path, i.e. one member of the dotted-key representation that
// defines the table.
type dottedTOMLFamilyAssignment struct {
	line  int
	table []string
	key   []string
}

func (member dottedTOMLFamilyAssignment) fullPath() []string {
	return appendPath(member.table, member.key)
}

func dottedTOMLFamily(lines []tomlLine, path []string) []dottedTOMLFamilyAssignment {
	structural := tomlStructuralLines(lines)
	var family []dottedTOMLFamilyAssignment
	var table []string
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if parsed, _, ok := parseTOMLTable(line.body); ok {
			table = parsed
			continue
		}
		assignment, ok := assignmentKey(line.body)
		if !ok {
			continue
		}
		if len(table) < len(path) && pathPrefix(table, path) &&
			pathPrefix(path, appendPath(table, assignment)) {
			family = append(family, dottedTOMLFamilyAssignment{
				line: index, table: append([]string(nil), table...), key: assignment,
			})
		}
	}
	return family
}

func tomlPathAssignedAsValue(lines []tomlLine, path []string) bool {
	structural := tomlStructuralLines(lines)
	var table []string
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if parsed, _, ok := parseTOMLTable(line.body); ok {
			table = parsed
			continue
		}
		assignment, ok := assignmentKey(line.body)
		if !ok {
			continue
		}
		full := appendPath(table, assignment)
		if len(full) <= len(path) && pathPrefix(full, path) {
			return true
		}
	}
	return false
}

func editDottedTOMLFamily(
	lines []tomlLine,
	family []dottedTOMLFamilyAssignment,
	path []string,
	encodedPath string,
	values map[string]any,
) ([]tomlLine, error) {
	context := family[0].table
	for _, member := range family[1:] {
		if !equalPath(member.table, context) {
			return nil, fmt.Errorf(
				"%w: table %s has mixed dotted representations", ErrAmbiguousConfigTarget, encodedPath)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type replacement struct {
		line    int
		encoded string
	}
	var replacements []replacement
	pending := make([]string, 0, len(keys))
	pendingEncoded := make(map[string]string, len(keys))
	for _, key := range keys {
		encoded, err := encodeTOMLValue(values[key])
		if err != nil {
			return nil, fmt.Errorf("encode %s.%s: %w", encodedPath, key, err)
		}
		target := appendPath(path, []string{key})
		var matches []int
		for _, member := range family {
			if equalPath(member.fullPath(), target) {
				matches = append(matches, member.line)
			}
		}
		switch len(matches) {
		case 1:
			replacements = append(replacements, replacement{line: matches[0], encoded: encoded})
		case 0:
			pending = append(pending, key)
			pendingEncoded[key] = encoded
		default:
			return nil, fmt.Errorf("%w: %s.%s", ErrAmbiguousConfigTarget, encodedPath, key)
		}
	}
	// Replacing from the last line upward keeps earlier match indices valid
	// when a multiline value splices out its continuation lines.
	sort.Slice(replacements, func(left, right int) bool {
		return replacements[left].line > replacements[right].line
	})
	for _, pendingReplacement := range replacements {
		updated, err := replaceTOMLAssignment(lines, pendingReplacement.line, pendingReplacement.encoded)
		if err != nil {
			return nil, fmt.Errorf("edit %s: %w", encodedPath, err)
		}
		lines = updated
	}
	if len(pending) == 0 {
		return lines, nil
	}
	// Line indices may have shifted when replacements spliced multiline
	// values, so the insertion anchor is resolved from a fresh scan: the new
	// keys extend the same dotted family, directly after its last member.
	family = dottedTOMLFamily(lines, path)
	if len(family) == 0 {
		return nil, fmt.Errorf(
			"%w: table %s lost its dotted representation", ErrAmbiguousConfigTarget, encodedPath)
	}
	anchor := family[len(family)-1]
	spanEnd, _, _, err := assignmentSpan(lines, anchor.line)
	if err != nil {
		return nil, fmt.Errorf("edit %s: %w", encodedPath, err)
	}
	insertAt := spanEnd + 1
	eol := preferredEOL(lines)
	lastLineEOL := eol
	if lines[insertAt-1].eol == "" {
		lines[insertAt-1].eol = eol
		if insertAt == len(lines) {
			lastLineEOL = ""
		}
	}
	localPrefix := encodeTOMLPathSuffix(path[len(anchor.table):])
	inserted := make([]tomlLine, len(pending))
	for index, key := range pending {
		lineEOL := eol
		if index == len(pending)-1 {
			lineEOL = lastLineEOL
		}
		inserted[index] = tomlLine{
			body: localPrefix + "." + key + " = " + pendingEncoded[key], eol: lineEOL,
		}
	}
	lines = append(lines, tomlLine{})
	copy(lines[insertAt+len(inserted):], lines[insertAt:])
	copy(lines[insertAt:], inserted)
	return lines, nil
}

func appendTOMLTable(lines []tomlLine, encodedPath string, values map[string]any) ([]tomlLine, error) {
	eol := preferredEOL(lines)
	hadFinalEOL := len(lines) == 0 || lines[len(lines)-1].eol != ""
	if len(lines) > 0 {
		if lines[len(lines)-1].eol == "" {
			lines[len(lines)-1].eol = eol
		}
		if lines[len(lines)-1].body != "" {
			lines = append(lines, tomlLine{eol: eol})
		}
	}
	lines = append(lines, tomlLine{body: "[" + encodedPath + "]", eol: eol})
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for index, key := range keys {
		value, err := encodeTOMLValue(values[key])
		if err != nil {
			return nil, fmt.Errorf("encode %s.%s: %w", encodedPath, key, err)
		}
		lineEOL := eol
		if index == len(keys)-1 && !hadFinalEOL {
			lineEOL = ""
		}
		lines = append(lines, tomlLine{body: key + " = " + value, eol: lineEOL})
	}
	return lines, nil
}

func editExactTOMLTableValue(
	lines []tomlLine,
	header int,
	path []string,
	key string,
	value any,
) ([]tomlLine, error) {
	encoded, err := encodeTOMLValue(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s.%s: %w", strings.Join(path, "."), key, err)
	}
	structural := tomlStructuralLines(lines)
	end := len(lines)
	var matches []int
	for index := header + 1; index < len(lines); index++ {
		if !structural[index] {
			continue
		}
		if _, _, ok := parseTOMLTable(lines[index].body); ok {
			end = index
			break
		}
		assignment, ok := assignmentKey(lines[index].body)
		if ok && len(assignment) == 1 && assignment[0] == key {
			matches = append(matches, index)
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%w: %s.%s", ErrAmbiguousConfigTarget, strings.Join(path, "."), key)
	}
	if len(matches) == 1 {
		return replaceTOMLAssignment(lines, matches[0], encoded)
	}

	eol := preferredEOL(lines)
	insertAt := end
	for insertAt > header+1 && strings.TrimSpace(lines[insertAt-1].body) == "" {
		insertAt--
	}
	if insertAt > 0 && lines[insertAt-1].eol == "" {
		lines[insertAt-1].eol = eol
	}
	lineEOL := eol
	if insertAt == len(lines) && len(lines) > 0 && lines[len(lines)-1].eol == "" {
		lineEOL = ""
	}
	lines = append(lines, tomlLine{})
	copy(lines[insertAt+1:], lines[insertAt:])
	lines[insertAt] = tomlLine{body: key + " = " + encoded, eol: lineEOL}
	return lines, nil
}

func removeTOMLTable(lines []tomlLine, header int, path []string) []tomlLine {
	structural := tomlStructuralLines(lines)
	end := len(lines)
	for index := header + 1; index < len(lines); index++ {
		if !structural[index] {
			continue
		}
		parsed, _, ok := parseTOMLTable(lines[index].body)
		if ok && !pathPrefix(path, parsed) {
			end = index
			break
		}
	}
	preserveFrom := end
	for preserveFrom > header+1 {
		line := strings.TrimSpace(lines[preserveFrom-1].body)
		if line != "" && !strings.HasPrefix(line, "#") {
			break
		}
		preserveFrom--
	}
	return append(lines[:header], lines[preserveFrom:]...)
}

func splitTOMLLines(content string) []tomlLine {
	if content == "" {
		return nil
	}
	var lines []tomlLine
	for len(content) > 0 {
		if index := strings.IndexByte(content, '\n'); index >= 0 {
			body := content[:index]
			eol := "\n"
			if withoutCR, ok := strings.CutSuffix(body, "\r"); ok {
				body = withoutCR
				eol = "\r\n"
			}
			lines = append(lines, tomlLine{body: body, eol: eol})
			content = content[index+1:]
			continue
		}
		lines = append(lines, tomlLine{body: content})
		break
	}
	return lines
}

func joinTOMLLines(lines []tomlLine) []byte {
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(line.body)
		out.WriteString(line.eol)
	}
	return []byte(out.String())
}

func editTOMLLines(lines []tomlLine, section, key, value string) ([]tomlLine, error) {
	target := strings.Split(section+"."+key, ".")
	currentTable := []string(nil)
	type insertionTarget struct {
		path       []string
		start, end int
	}
	var candidates []insertionTarget
	rootEnd := len(lines)
	rootDottedFamily := false
	var matches []int
	structural := tomlStructuralLines(lines)
	for index, line := range lines {
		if !structural[index] {
			continue
		}
		if parsed, array, ok := parseTOMLTable(line.body); ok {
			if rootEnd == len(lines) {
				rootEnd = index
			}
			for candidateIndex := range candidates {
				if candidates[candidateIndex].end == len(lines) {
					candidates[candidateIndex].end = index
				}
			}
			currentTable = parsed
			if !array && pathPrefix(parsed, target) && len(parsed) < len(target) {
				candidates = append(candidates, insertionTarget{path: parsed, start: index, end: len(lines)})
			}
			continue
		}
		assignment, ok := assignmentKey(line.body)
		if ok && currentTable == nil && len(assignment) > 1 && len(target) > 1 &&
			equalPath(assignment[:len(assignment)-1], target[:len(target)-1]) {
			rootDottedFamily = true
		}
		if ok && equalPath(appendPath(currentTable, assignment), target) {
			matches = append(matches, index)
		}
	}
	if rootDottedFamily {
		candidates = append(candidates, insertionTarget{start: -1, end: rootEnd})
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%w: %s.%s", ErrAmbiguousConfigTarget, section, key)
	}
	if len(matches) == 1 {
		updated, err := replaceTOMLAssignment(lines, matches[0], value)
		if err != nil {
			return nil, fmt.Errorf("edit %s.%s: %w", section, key, err)
		}
		return updated, nil
	}

	eol := preferredEOL(lines)
	hadFinalEOL := len(lines) == 0 || lines[len(lines)-1].eol != ""
	if len(candidates) > 0 {
		bestPrefix := -1
		best := -1
		ambiguous := false
		for index, candidate := range candidates {
			if len(candidate.path) > bestPrefix {
				bestPrefix = len(candidate.path)
				best = index
				ambiguous = false
			} else if len(candidate.path) == bestPrefix && equalPath(candidate.path, candidates[best].path) {
				ambiguous = true
			}
		}
		if ambiguous {
			return nil, fmt.Errorf("%w: %s.%s", ErrAmbiguousConfigTarget, section, key)
		}
		candidate := candidates[best]
		insertAt := candidate.end
		for insertAt > candidate.start+1 && strings.TrimSpace(lines[insertAt-1].body) == "" {
			insertAt--
		}
		if insertAt > 0 && lines[insertAt-1].eol == "" {
			lines[insertAt-1].eol = eol
		}
		assignment := strings.Join(target[len(candidate.path):], ".")
		lineEOL := eol
		if insertAt == len(lines) && !hadFinalEOL {
			lineEOL = ""
		}
		line := tomlLine{body: assignment + " = " + value, eol: lineEOL}
		lines = append(lines, tomlLine{})
		copy(lines[insertAt+1:], lines[insertAt:])
		lines[insertAt] = line
		return lines, nil
	}

	if len(lines) > 0 {
		if lines[len(lines)-1].eol == "" {
			lines[len(lines)-1].eol = eol
		}
		if lines[len(lines)-1].body != "" {
			lines = append(lines, tomlLine{eol: eol})
		}
	}
	finalEOL := eol
	if len(lines) > 0 && !hadFinalEOL {
		finalEOL = ""
	}
	lines = append(lines,
		tomlLine{body: "[" + section + "]", eol: eol},
		tomlLine{body: key + " = " + value, eol: finalEOL},
	)
	return lines, nil
}

// replaceTOMLAssignment substitutes one assignment's value in place,
// splicing out continuation lines of the previous multiline value while
// preserving the operator's trailing comment and final line ending.
func replaceTOMLAssignment(lines []tomlLine, index int, encoded string) ([]tomlLine, error) {
	spanEnd, suffix, multiline, err := assignmentSpan(lines, index)
	if err != nil {
		return nil, err
	}
	replaced, err := replaceAssignmentValue(lines[index].body, encoded)
	if err != nil {
		return nil, err
	}
	if multiline {
		replaced += suffix
		lines[index].eol = lines[spanEnd].eol
		lines = append(lines[:index+1], lines[spanEnd+1:]...)
	}
	lines[index].body = replaced
	return lines, nil
}

type tomlMultilineString byte

const (
	tomlNotMultiline tomlMultilineString = iota
	tomlBasicMultiline
	tomlLiteralMultiline
)

// tomlStructuralLines marks physical lines whose leading TOML syntax belongs
// to the document rather than to a multiline string value. The opening
// assignment remains structural; every continuation line is ignored until a
// valid unescaped triple-quote delimiter closes the value.
func tomlStructuralLines(lines []tomlLine) []bool {
	result := make([]bool, len(lines))
	state := tomlNotMultiline
	for index, line := range lines {
		result[index] = state == tomlNotMultiline
		state = scanTOMLMultilineState(line.body, state)
	}
	return result
}

func scanTOMLMultilineState(line string, state tomlMultilineString) tomlMultilineString {
	for index := 0; index < len(line); {
		if state != tomlNotMultiline {
			delimiter := byte('\'')
			if state == tomlBasicMultiline {
				delimiter = '"'
			}
			if line[index] != delimiter {
				index++
				continue
			}
			run := quoteRunLength(line[index:], delimiter)
			if run >= 3 && (state != tomlBasicMultiline || !tomlQuoteEscaped(line, index)) {
				state = tomlNotMultiline
				index += run
				continue
			}
			index += run
			continue
		}

		switch line[index] {
		case '#':
			return state
		case '"', '\'':
			quote := line[index]
			run := quoteRunLength(line[index:], quote)
			if run >= 3 {
				if quote == '"' {
					state = tomlBasicMultiline
				} else {
					state = tomlLiteralMultiline
				}
				index += 3
				continue
			}
			index++
			for index < len(line) {
				if quote == '"' && line[index] == '\\' {
					index += 2
					continue
				}
				if line[index] == quote {
					index++
					break
				}
				index++
			}
		default:
			index++
		}
	}
	return state
}

func quoteRunLength(value string, quote byte) int {
	count := 0
	for count < len(value) && value[count] == quote {
		count++
	}
	return count
}

func tomlQuoteEscaped(line string, quoteIndex int) bool {
	backslashes := 0
	for index := quoteIndex - 1; index >= 0 && line[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func assignmentSpan(lines []tomlLine, start int) (int, string, bool, error) {
	equal := indexOutsideTOMLEquals(lines[start].body)
	if equal < 0 {
		return start, "", false, errors.New("assignment has no equals sign")
	}
	if end, suffix, multiline, ok, err := multilineStringAssignmentSpan(lines, start, equal+1); ok || err != nil {
		return end, suffix, multiline, err
	}
	depth := 0
	opened := false
	var quote byte
	escaped := false
	for lineIndex := start; lineIndex < len(lines); lineIndex++ {
		body := lines[lineIndex].body
		position := 0
		if lineIndex == start {
			position = equal + 1
		}
		for ; position < len(body); position++ {
			char := body[position]
			if escaped {
				escaped = false
				continue
			}
			if quote == '"' && char == '\\' {
				escaped = true
				continue
			}
			if quote != 0 {
				if char == quote {
					quote = 0
				}
				continue
			}
			switch char {
			case '"', '\'':
				quote = char
			case '#':
				position = len(body)
			case '[', '{':
				opened = true
				depth++
			case ']', '}':
				if depth == 0 {
					return start, "", false, errors.New("unbalanced TOML value")
				}
				depth--
				if opened && depth == 0 {
					suffix := body[position+1:]
					if strings.TrimSpace(suffix) != "" && !strings.HasPrefix(strings.TrimSpace(suffix), "#") {
						return start, "", false, errors.New("unexpected text after multiline value")
					}
					return lineIndex, suffix, lineIndex > start, nil
				}
			}
		}
		if !opened {
			return start, "", false, nil
		}
		if depth == 0 {
			return lineIndex, "", lineIndex > start, nil
		}
	}
	return start, "", false, errors.New("unterminated multiline TOML value")
}

func multilineStringAssignmentSpan(lines []tomlLine, start, valueStart int) (int, string, bool, bool, error) {
	opening := strings.TrimLeft(lines[start].body[valueStart:], " \t")
	if len(opening) < 3 || (opening[:3] != `"""` && opening[:3] != `'''`) {
		return start, "", false, false, nil
	}
	delimiter := opening[0]
	openingOffset := len(lines[start].body) - len(opening)
	for lineIndex := start; lineIndex < len(lines); lineIndex++ {
		position := 0
		if lineIndex == start {
			position = openingOffset + 3
		}
		body := lines[lineIndex].body
		for position < len(body) {
			if body[position] != delimiter {
				position++
				continue
			}
			run := quoteRunLength(body[position:], delimiter)
			if run < 3 || (delimiter == '"' && tomlQuoteEscaped(body, position)) {
				position += run
				continue
			}
			suffix := body[position+3:]
			trimmed := strings.TrimSpace(suffix)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				return start, "", false, true, errors.New("unexpected text after multiline string value")
			}
			return lineIndex, suffix, lineIndex > start, true, nil
		}
	}
	return start, "", false, true, errors.New("unterminated multiline TOML string value")
}

func preferredEOL(lines []tomlLine) string {
	for _, line := range lines {
		if line.eol != "" {
			return line.eol
		}
	}
	return "\n"
}

func parseTOMLTable(line string) ([]string, bool, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false, false
	}
	array := strings.HasPrefix(trimmed, "[[")
	open, closeDelimiter := 1, "]"
	if array {
		open, closeDelimiter = 2, "]]"
	}
	end := strings.Index(trimmed[open:], closeDelimiter)
	if end < 1 {
		return nil, false, false
	}
	end += open
	remainder := strings.TrimSpace(trimmed[end+len(closeDelimiter):])
	if remainder != "" && !strings.HasPrefix(remainder, "#") {
		return nil, false, false
	}
	path, ok := parseTOMLKey(strings.TrimSpace(trimmed[open:end]))
	return path, array, ok
}

func assignmentKey(line string) ([]string, bool) {
	equal := indexOutsideTOMLEquals(line)
	if equal < 0 {
		return nil, false
	}
	key := strings.TrimSpace(line[:equal])
	if key == "" || strings.HasPrefix(key, "#") {
		return nil, false
	}
	return parseTOMLKey(key)
}

func parseTOMLKey(key string) ([]string, bool) {
	var document map[string]any
	if _, err := toml.Decode(key+" = true", &document); err != nil {
		return nil, false
	}
	path := make([]string, 0, 4)
	current := document
	for {
		if len(current) != 1 {
			return nil, false
		}
		for segment, value := range current {
			path = append(path, segment)
			nested, ok := value.(map[string]any)
			if !ok {
				return path, true
			}
			current = nested
		}
	}
}

func appendPath(prefix, suffix []string) []string {
	result := make([]string, 0, len(prefix)+len(suffix))
	result = append(result, prefix...)
	return append(result, suffix...)
}

func pathPrefix(prefix, path []string) bool {
	return len(prefix) <= len(path) && equalPath(prefix, path[:len(prefix)])
}

func equalPath(left, right []string) bool {
	return slices.Equal(left, right)
}

func replaceAssignmentValue(line, value string) (string, error) {
	equal := indexOutsideTOMLEquals(line)
	if equal < 0 {
		return "", errors.New("assignment has no equals sign")
	}
	start := equal + 1
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	comment := commentOutsideTOMLValue(line, start)
	end := len(line)
	if comment >= 0 {
		end = comment
		for end > start && (line[end-1] == ' ' || line[end-1] == '\t') {
			end--
		}
	}
	return line[:start] + value + line[end:], nil
}

func indexOutsideTOMLEquals(line string) int {
	var quote byte
	escaped := false
	for index := range len(line) {
		char := line[index]
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && char == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '"' || char == '\'' {
			quote = char
			continue
		}
		if char == '=' {
			return index
		}
	}
	return -1
}

func commentOutsideTOMLValue(line string, start int) int {
	var quote byte
	escaped := false
	depth := 0
	for index := start; index < len(line); index++ {
		char := line[index]
		if escaped {
			escaped = false
			continue
		}
		if quote == '"' && char == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '"', '\'':
			quote = char
		case '[', '{':
			depth++
		case ']', '}':
			if depth > 0 {
				depth--
			}
		case '#':
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func encodeTOMLValue(value any) (string, error) {
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(map[string]any{"value": value}); err != nil {
		return "", fmt.Errorf("encode TOML value: %w", err)
	}
	line := strings.TrimSuffix(encoded.String(), "\n")
	const prefix = "value = "
	if !strings.HasPrefix(line, prefix) || strings.ContainsRune(line, '\n') {
		return "", errors.New("value is not representable as one TOML assignment")
	}
	return strings.TrimPrefix(line, prefix), nil
}
