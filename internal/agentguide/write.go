package agentguide

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteInstructions explicitly initializes one local instruction file. Parent
// directories must exist. A temporary sibling makes replacement atomic; a new
// destination is linked exclusively so a concurrent creator is never replaced.
func WriteInstructions(path string) (changed bool, retErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return false, fmt.Errorf("open instruction directory: %w", err)
	}
	defer func() {
		if err := root.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close instruction directory: %w", err))
		}
	}()
	name := filepath.Base(path)
	info, err := root.Lstat(name)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect instruction file: %w", err)
	}
	var original []byte
	mode := os.FileMode(0o600)
	if info != nil {
		if !info.Mode().IsRegular() {
			return false, errors.New("instruction file must be regular, not a symlink or directory")
		}
		mode = info.Mode().Perm()
		if mode&0o222 == 0 {
			return false, errors.New("instruction file is read-only")
		}
		original, err = root.ReadFile(name)
		if err != nil {
			return false, fmt.Errorf("read instruction file: %w", err)
		}
	}
	updated, err := UpsertManagedBlock(string(original))
	if err != nil {
		return false, err
	}
	if bytes.Equal(original, []byte(updated)) {
		return false, nil
	}
	tempName := ".msgvault-instructions-" + rand.Text()
	temp, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, fmt.Errorf("create temporary instruction file: %w", err)
	}
	defer func() {
		if err := root.Remove(tempName); err != nil && !os.IsNotExist(err) {
			retErr = errors.Join(retErr, fmt.Errorf("remove temporary instruction file: %w", err))
		}
	}()
	tempOpen := true
	defer func() {
		if tempOpen {
			if err := temp.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close temporary instruction file: %w", err))
			}
		}
	}()
	if _, err := temp.WriteString(updated); err != nil {
		return false, fmt.Errorf("write instructions: %w", err)
	}
	if err := temp.Chmod(mode); err != nil {
		return false, fmt.Errorf("preserve instruction file mode: %w", err)
	}
	closeErr := temp.Close()
	tempOpen = false
	if closeErr != nil {
		return false, fmt.Errorf("close instruction file: %w", closeErr)
	}
	if info == nil {
		if err := root.Link(tempName, name); err != nil {
			return false, fmt.Errorf("create instruction file without replacing an existing entry: %w", err)
		}
		return true, nil
	}
	current, err := root.Lstat(name)
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() || current.Mode().Perm() != mode {
		return false, errors.New("instruction file changed during update; inspect and retry")
	}
	content, err := root.ReadFile(name)
	if err != nil || !bytes.Equal(original, content) {
		return false, errors.New("instruction file changed during update; inspect and retry")
	}
	if err := root.Rename(tempName, name); err != nil {
		return false, fmt.Errorf("replace instruction file: %w", err)
	}
	return true, nil
}
