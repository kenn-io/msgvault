//go:build windows

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var replaceFileProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func moveFileWriteThrough(fromPath, toPath string) error {
	from, err := windows.UTF16PtrFromString(fromPath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(toPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

// beginConfigReplacement uses ReplaceFileW so the destination switches from
// the old config to the candidate atomically. Windows writes the displaced
// file to a separate backup path, which also gives us a native atomic rollback
// operation until the caller completes its durability checks.
func beginConfigReplacement(candidatePath, targetPath string, expectedCandidate, expectedTarget ConfigFile) (configReplacement, error) {
	authority, err := pinWindowsConfigParent(targetPath)
	if err != nil {
		return configReplacement{}, err
	}
	displacedPath := candidatePath + ".displaced"
	targetBefore, targetErr := readPhysicalConfigSnapshot(targetPath)
	if targetErr != nil {
		_ = authority.Release()
		return configReplacement{}, targetErr
	}
	candidateBefore, candidateErr := readPhysicalConfigSnapshot(candidatePath)
	if candidateErr != nil {
		_ = authority.Release()
		return configReplacement{}, candidateErr
	}
	if !sameConfigVersion(targetBefore, expectedTarget) || !sameConfigVersion(candidateBefore, expectedCandidate) {
		_ = authority.Release()
		return configReplacement{}, errors.Join(ErrConfigConflict,
			errors.New("ReplaceFileW identities changed before publication"))
	}
	displaced, err := retainWindowsConfigArtifact(targetPath, targetBefore.identity)
	if err != nil {
		_ = authority.Release()
		return configReplacement{}, err
	}
	replacement := configReplacement{
		displacedPath:     displacedPath,
		preserveCandidate: true,
		recoveryPaths:     []string{targetPath, candidatePath, displacedPath},
		cleanupDisplaced: func() error {
			return retireWindowsConfigArtifact(displacedPath, displaced)
		},
		release: func() error {
			return errors.Join(displaced.Close(), authority.Release())
		},
	}
	if err := replaceFile(targetPath, candidatePath, displacedPath); err != nil {
		reconciled, reconciledErr := reconcileReplaceFileFailure(replacement, targetBefore, candidateBefore, err)
		if reconciled.release == nil {
			_ = displaced.Close()
			_ = authority.Release()
		}
		return reconciled, reconciledErr
	}
	replacement.rollbackPublished = func(expected ConfigFile) error {
		targetBeforeRollback, targetErr := readPhysicalConfigSnapshot(targetPath)
		if targetErr != nil {
			return errors.Join(ErrConfigChanged, targetErr)
		}
		if !sameConfigVersion(targetBeforeRollback, expected) {
			return errors.Join(ErrConfigChanged, ErrConfigConflict,
				errors.New("refusing ReplaceFileW rollback because the published config changed"))
		}
		displacedBeforeRollback, displacedErr := readPhysicalConfigSnapshot(displacedPath)
		if displacedErr != nil {
			return errors.Join(ErrConfigChanged, displacedErr)
		}
		if err := replaceFile(targetPath, displacedPath, candidatePath); err != nil {
			rollbackState := configReplacement{
				displacedPath:     candidatePath,
				preserveCandidate: true,
				recoveryPaths:     []string{targetPath, displacedPath, candidatePath},
			}
			_, reconciledErr := reconcileReplaceFileFailure(
				rollbackState, targetBeforeRollback, displacedBeforeRollback, err,
			)
			return fmt.Errorf("ReplaceFileW rollback did not complete: %w", errors.Join(ErrConfigChanged, reconciledErr))
		}
		return nil
	}
	return replacement, nil
}

func reconcileReplaceFileFailure(
	replacement configReplacement,
	targetBefore, candidateBefore ConfigFile,
	callErr error,
) (configReplacement, error) {
	if windowsPathsMatchOriginal(targetBefore, candidateBefore, replacement.displacedPath) {
		return configReplacement{}, fmt.Errorf("ReplaceFileW left original files unchanged: %w", callErr)
	}

	// ERROR_UNABLE_TO_MOVE_REPLACEMENT_2 documents the dangerous partial
	// outcome: the old target has moved to the backup name while the candidate
	// retains its name. Restore the target with write-through rename, then prove
	// both original identities are back where they started.
	if errors.Is(callErr, windows.ERROR_UNABLE_TO_MOVE_REPLACEMENT_2) &&
		pathMissing(targetBefore.Path) && pathMatchesVersion(replacement.displacedPath, targetBefore) &&
		pathMatchesVersion(candidateBefore.Path, candidateBefore) {
		moveErr := moveFileWriteThrough(replacement.displacedPath, targetBefore.Path)
		if windowsPathsMatchOriginal(targetBefore, candidateBefore, replacement.displacedPath) {
			return configReplacement{}, fmt.Errorf("ReplaceFileW partial outcome was restored: %w", callErr)
		}
		if moveErr != nil {
			callErr = errors.Join(callErr, fmt.Errorf("restore partial ReplaceFileW outcome: %w", moveErr))
		}
	}

	// ERROR_UNABLE_TO_REMOVE_REPLACED and
	// ERROR_UNABLE_TO_MOVE_REPLACEMENT normally leave the two named files
	// unchanged. The identity check above, rather than the error code, is what
	// proves that safe outcome. Every other observable state is ambiguous and
	// must retain all artifacts for operator recovery.
	return replacement, errors.Join(
		fmt.Errorf("%w: ReplaceFileW left an unverified partial state", ErrConfigChanged),
		callErr,
		recoveryArtifactError(replacement),
	)
}

func windowsPathsMatchOriginal(targetBefore, candidateBefore ConfigFile, backupPath string) bool {
	return pathMatchesVersion(targetBefore.Path, targetBefore) &&
		pathMatchesVersion(candidateBefore.Path, candidateBefore) && pathMissing(backupPath)
}

func pathMatchesVersion(path string, before ConfigFile) bool {
	current, err := readPhysicalConfigSnapshot(path)
	return err == nil && sameConfigVersion(current, before)
}

func pathMissing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}

func replaceFile(targetPath, replacementPath, backupPath string) error {
	target, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return fmt.Errorf("encode replacement target: %w", err)
	}
	replacement, err := windows.UTF16PtrFromString(replacementPath)
	if err != nil {
		return fmt.Errorf("encode config candidate: %w", err)
	}
	backup, err := windows.UTF16PtrFromString(backupPath)
	if err != nil {
		return fmt.Errorf("encode displaced config path: %w", err)
	}
	result, _, callErr := replaceFileProc.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(unsafe.Pointer(replacement)),
		uintptr(unsafe.Pointer(backup)),
		0,
		0,
		0,
	)
	if result == 0 {
		return fmt.Errorf("ReplaceFileW: %w", callErr)
	}
	return nil
}
